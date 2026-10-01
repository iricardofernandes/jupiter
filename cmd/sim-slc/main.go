package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
func main() {
	service.Main("sim-slc", "127.0.0.1:8592", func(_ context.Context, _ service.Config, logger *slog.Logger) (service.App, error) {
		participants, err := parseParticipants(os.Getenv("JUPITER_SIM_SLC_PARTICIPANTS"))
		if err != nil {
			return service.App{}, err
		}
		sim := slc.New(slc.Config{Logger: logger, Participants: participants})
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
	json := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "the admin takes application/json", http.StatusUnsupportedMediaType)
			return
		}
		h.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: json, ReadHeaderTimeout: readHeaderTimeout}
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
