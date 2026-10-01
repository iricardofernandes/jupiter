package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/api/migrations"
	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/receivables"
	"github.com/iricardofernandes/jupiter/internal/recipients"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
	"github.com/iricardofernandes/jupiter/internal/risk"
	"github.com/iricardofernandes/jupiter/internal/subscriptions"
)

const (
	versionHeader   = "Jupiter-Version"
	requestIDHeader = "Request-Id"
)

var requestPrefix = id.MustPrefix("req")

type Deps struct {
	Pool      *pgxpool.Pool
	Merchants *merchant.Service
	Events    *events.Service
	Payments  *payments.Service
	Vault     Vault
	Risk      *risk.Service
	// Subscriptions charge customers by Pix Automático; nil leaves them out.
	Subscriptions *subscriptions.Service
	// Receivables keeps merchants' card receivables and their agenda; nil leaves it out.
	Receivables *receivables.Service
	// Recipients are those payments are split to; nil leaves them out.
	Recipients *recipients.Service
	// Disputes keeps chargebacks, MED claims and fraud reports; nil leaves them out.
	Disputes *disputes.Service
	// Reconciliation reports what matched with every counterparty; nil leaves it out.
	Reconciliation *reconciliation.Service
	Box            *secretbox.Box
	Logger         *slog.Logger
	Now            func() time.Time
	// AfterPhase, if set, runs after each atomic phase of an idempotent request commits.
	// It is the seam through which simulations kill requests between phases.
	AfterPhase func(recoveryPoint string)
	// IdempotencyWait bounds how long a request waits for a concurrent one holding the
	// same Idempotency-Key; zero keeps the default of ten seconds.
	IdempotencyWait time.Duration
}

type API struct {
	deps       Deps
	idem       idempotencyConfig
	operations map[string]operation
	// afterPhase runs after each atomic phase commits, before the next begins. Tests
	// use it to abandon a request between phases, as a crash would.
	afterPhase func(recoveryPoint string)
}

func New(d Deps) *API {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	a := &API{deps: d, idem: defaultIdempotencyConfig, afterPhase: d.AfterPhase}
	if d.IdempotencyWait > 0 {
		a.idem.wait = d.IdempotencyWait
	}
	a.operations = a.registerOperations()
	return a
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "api", migrations.FS)
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	openapi.HandlerWithOptions(a, openapi.StdHTTPServerOptions{
		BaseRouter: mux,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			a.writeError(w, r, invalidRequest("parameter_invalid", "", "%s", err.Error()))
		},
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		a.writeError(w, r, &Error{
			Status: http.StatusNotFound, Type: openapi.InvalidRequestError, Code: "url_invalid",
			Message: "Unrecognized request URL (" + r.Method + " " + r.URL.Path + ").",
		})
	})
	return a.withRequestID(a.withLogging(a.withRecovery(a.withAuthentication(a.withVersion(withRoute(mux))))))
}

type contextKey int

const (
	principalKey contextKey = iota
	requestIDKey
	versionKey
)

func principalFrom(ctx context.Context) merchant.Principal {
	p, _ := ctx.Value(principalKey).(merchant.Principal)
	return p
}

func requestIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
}

func versionFrom(ctx context.Context) string {
	if v, ok := ctx.Value(versionKey).(string); ok {
		return v
	}
	return CurrentVersion
}

func owner(p merchant.Principal) events.Owner {
	return events.Owner{Merchant: p.Merchant, Livemode: p.Livemode}
}

func (a *API) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := requestPrefix.New().String()
		w.Header().Set(requestIDHeader, requestID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, requestID)))
	})
}

func (a *API) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { //nolint:contextcheck // the deferred handler uses the request's own context
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // a sentinel compared by identity, as net/http does
					panic(v)
				}
				a.deps.Logger.ErrorContext(r.Context(), "panic serving request", "panic", v, "request_id", requestIDFrom(r.Context()))
				a.writeError(w, r, errInternal)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

func (a *API) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		began := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		route := new(string)
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), routeKey{}, route)))
		measure(r.Context(), *route, rec.status, time.Since(began))
		p := principalFrom(r.Context())
		a.deps.Logger.InfoContext(r.Context(), "request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(began).Milliseconds(), "request_id", requestIDFrom(r.Context()),
			"merchant", p.Merchant.String(), "livemode", p.Livemode)
	})
}

func (a *API) withAuthentication(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || value == "" {
			a.writeError(w, r, unauthenticated("No API key provided. Send it as a bearer token in the Authorization header."))
			return
		}
		p, err := a.deps.Merchants.Authenticate(r.Context(), a.deps.Pool, value)
		switch {
		case errors.Is(err, merchant.ErrInvalidKey):
			a.writeError(w, r, unauthenticated("Invalid API key provided: "+redact(value)))
			return
		case err != nil:
			a.fail(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(payments.WithMemo(context.WithValue(r.Context(), principalKey, p))))
	})
}

func (a *API) withVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := principalFrom(r.Context()).APIVersion
		if requested := r.Header.Get(versionHeader); requested != "" {
			if !knownVersion(requested) {
				a.writeError(w, r, invalidRequest("api_version_invalid", versionHeader,
					"Unknown API version %q. Known versions: %s.", requested, strings.Join(Versions, ", ")))
				return
			}
			version = requested
		}
		w.Header().Set(versionHeader, version)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), versionKey, version)))
	})
}

func redact(key string) string {
	if len(key) <= 12 {
		return "***"
	}
	return key[:8] + "…" + key[len(key)-4:]
}

func (a *API) writeJSON(w http.ResponseWriter, r *http.Request, body any) {
	raw, err := render(body, versionFrom(r.Context()))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, raw)
}

func writeRaw(w http.ResponseWriter, status int, raw []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw) //nolint:gosec // raw is JSON served as application/json, not HTML
}

func (a *API) writeError(w http.ResponseWriter, r *http.Request, e *Error) {
	raw, err := render(e.body(requestIDFrom(r.Context())), CurrentVersion)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeRaw(w, e.Status, raw)
}

// fail answers with the client error inside err, or logs err and answers 500.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	if apiErr, ok := asError(err); ok {
		a.writeError(w, r, apiErr)
		return
	}
	a.deps.Logger.ErrorContext(r.Context(), "request failed", "error", err, "request_id", requestIDFrom(r.Context()))
	a.writeError(w, r, errInternal)
}

// RenderEvent is the body of a webhook delivery: the same event object the API returns.
func RenderEvent(e events.Event, version string) ([]byte, error) {
	return render(eventJSON(e, nil), version)
}
