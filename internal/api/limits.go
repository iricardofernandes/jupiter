package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/platform/ratelimit"
)

// Limits bound how fast one key may call, in each mode, and how many keys that do not
// exist one address may present before it is refused without a database lookup. The
// zero value sets no limit.
type Limits struct {
	Live, Test, Unknown ratelimit.Rate
	Clients             ratelimit.Clients
}

// DefaultLimits are a hundred requests a second for a live key and 25 for a test key, as
// payment APIs commonly allow, and one unknown key a second from an address.
var DefaultLimits = Limits{
	Live:    ratelimit.Rate{PerSecond: 100, Burst: 100},
	Test:    ratelimit.Rate{PerSecond: 25, Burst: 25},
	Unknown: ratelimit.Rate{PerSecond: 1, Burst: 20},
}

// LimitsFromEnv reads JUPITER_API_RATE_LIVE and JUPITER_API_RATE_TEST, requests a second
// per key, JUPITER_API_RATE_UNKNOWN, unknown keys a second per address, and
// JUPITER_TRUSTED_PROXIES, the proxies whose X-Forwarded-For names the client.
func LimitsFromEnv(getenv func(string) string) (Limits, error) {
	l := DefaultLimits
	for name, rate := range map[string]*ratelimit.Rate{
		"JUPITER_API_RATE_LIVE": &l.Live, "JUPITER_API_RATE_TEST": &l.Test, "JUPITER_API_RATE_UNKNOWN": &l.Unknown,
	} {
		v := getenv(name)
		if v == "" {
			continue
		}
		perSecond, err := strconv.ParseFloat(v, 64)
		if err != nil || perSecond < 0 {
			return Limits{}, fmt.Errorf("%s must be a number of requests a second: %q", name, v)
		}
		rate.PerSecond = perSecond
		rate.Burst = max(rate.Burst, int(perSecond))
	}
	trusted, err := ratelimit.ParseTrusted(getenv("JUPITER_TRUSTED_PROXIES"))
	if err != nil {
		return Limits{}, err
	}
	l.Clients.Trusted = trusted
	return l, nil
}

type limiters struct {
	live, test, unknown *ratelimit.Buckets
	clients             ratelimit.Clients
}

func (a *API) newLimiters(l Limits) limiters {
	return limiters{
		live:    ratelimit.New(l.Live, a.deps.Now),
		test:    ratelimit.New(l.Test, a.deps.Now),
		unknown: ratelimit.New(l.Unknown, a.deps.Now),
		clients: l.Clients,
	}
}

func (l limiters) of(p merchant.Principal) *ratelimit.Buckets {
	if p.Livemode {
		return l.live
	}
	return l.test
}

func (a *API) tooManyRequests(w http.ResponseWriter, r *http.Request, b *ratelimit.Buckets) {
	w.Header().Set("Retry-After", strconv.Itoa(max(b.RetryAfter(), 1)))
	a.writeError(w, r, &Error{
		Status: http.StatusTooManyRequests, Type: openapi.RateLimitError, Code: "rate_limit",
		Message: "Too many requests. Wait the seconds in the Retry-After header and try again.",
	})
}
