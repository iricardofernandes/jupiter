package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/internal/platform/service"
	"github.com/iricardofernandes/jupiter/internal/sim/slc"
	"github.com/iricardofernandes/jupiter/pkg/taxid"
)

const (
	readHeaderTimeout = 5 * time.Second
	tickEvery         = 10 * time.Second
	minToken          = 16
)

// The centralized settlement simulator, for tests and local development only.
//
//   - JUPITER_HTTP_ADDR (default 127.0.0.1:8592): the settlement system's API.
//   - JUPITER_SIM_SLC_PARTICIPANTS: who takes part, as token:cnpj:ispb, comma separated;
//     the ISPB is the participant's settlement account's.
//   - JUPITER_SIM_ADMIN_ADDR (default 127.0.0.1:8593): unauthenticated operator controls
//     over plain HTTP, running the settlement window; keep it on loopback.
//   - JUPITER_SIM_SLC_CREDIT_URL: where what a grade credits is paid into the
//     participant's account, sim-bank's /admin/credits on this machine; unset, nothing
//     reaches a bank.
func main() {
	service.Main("sim-slc", "127.0.0.1:8592", func(_ context.Context, _ service.Config, logger *slog.Logger) (service.App, error) {
		participants, err := parseParticipants(os.Getenv("JUPITER_SIM_SLC_PARTICIPANTS"))
		if err != nil {
			return service.App{}, err
		}
		credit, err := creditTo(os.Getenv("JUPITER_SIM_SLC_CREDIT_URL"), logger)
		if err != nil {
			return service.App{}, err
		}
		sim := slc.New(slc.Config{Logger: logger, Participants: participants, Credit: credit})
		adminAddr := os.Getenv("JUPITER_SIM_ADMIN_ADDR")
		if adminAddr == "" {
			adminAddr = "127.0.0.1:8593"
		}
		return service.App{
			Handler: sim.Handler(),
			Background: []func(context.Context) error{
				func(ctx context.Context) error { return serveAdmin(ctx, adminAddr, sim.AdminHandler(), logger) },
				func(ctx context.Context) error { return every(ctx, tickEvery, sim.Tick) },
			},
		}, nil
	})
}

func every(ctx context.Context, d time.Duration, f func()) error {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			f()
		}
	}
}

func serveAdmin(ctx context.Context, addr string, h http.Handler, logger *slog.Logger) error {
	// The controls are unauthenticated: they listen on this machine only.
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
		return fmt.Errorf("JUPITER_SIM_ADMIN_ADDR %q is not a loopback address", addr)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	// A JSON content type cannot come from a plain cross-site form.
	jsonOnly := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "the admin takes application/json", http.StatusUnsupportedMediaType)
			return
		}
		h.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: jsonOnly, ReadHeaderTimeout: readHeaderTimeout}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	logger.InfoContext(ctx, "admin listening", "addr", ln.Addr().String())
	if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func parseParticipants(s string) ([]slc.Participant, error) {
	var out []slc.Participant
	tokens := map[string]bool{}
	for spec := range strings.SplitSeq(s, ",") {
		if spec = strings.TrimSpace(spec); spec == "" {
			continue
		}
		parts := strings.Split(spec, ":")
		if len(parts) != 3 {
			return nil, errors.New("JUPITER_SIM_SLC_PARTICIPANTS: an entry is not token:cnpj:ispb")
		}
		if len(parts[0]) < minToken || tokens[parts[0]] || len(parts[1]) != 14 || !taxid.Valid(parts[1]) || len(parts[2]) != 8 {
			return nil, fmt.Errorf("JUPITER_SIM_SLC_PARTICIPANTS: each needs a token of %d characters or more of its own, a valid CNPJ and an 8-digit ISPB", minToken)
		}
		tokens[parts[0]] = true
		out = append(out, slc.Participant{Token: parts[0], TaxID: parts[1], ISPB: parts[2]})
	}
	if len(out) == 0 {
		return nil, errors.New("JUPITER_SIM_SLC_PARTICIPANTS names no participant")
	}
	return out, nil
}

// creditTo pays settled grades into the participant's account through the bank
// simulator's operator controls, on this machine; with no URL, nowhere.
func creditTo(rawURL string, logger *slog.Logger) (func(slc.Participant, string, int64), error) {
	if rawURL == "" {
		return func(slc.Participant, string, int64) {}, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("JUPITER_SIM_SLC_CREDIT_URL: %w", err)
	}
	if ip := net.ParseIP(u.Hostname()); u.Scheme != "http" || u.Path != "/admin/credits" || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
		return nil, errors.New("JUPITER_SIM_SLC_CREDIT_URL must be sim-bank's /admin/credits on this machine")
	}
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(p slc.Participant, date string, amount int64) {
		body, _ := json.Marshal(map[string]any{
			"tax_id": p.TaxID, "date": date, "amount": amount, "reference": slc.CreditReference(date), "description": "LIQUIDACAO SLC",
		})
		resp, err := client.Post(rawURL, "application/json", bytes.NewReader(body)) //nolint:noctx,gosec // a background tick, to the loopback address checked above
		if err != nil {
			logger.Error("crediting a settled grade", "date", date, "error", err)
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			logger.Error("the bank refused a settled grade's credit", "date", date, "status", resp.StatusCode)
		}
	}, nil
}
