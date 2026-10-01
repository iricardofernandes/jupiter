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
	"github.com/iricardofernandes/jupiter/internal/sim/bank"
	"github.com/iricardofernandes/jupiter/pkg/cnab240"
	"github.com/iricardofernandes/jupiter/pkg/taxid"
)

const (
	readHeaderTimeout = 5 * time.Second
	tickEvery         = 10 * time.Second
	minToken          = 16
)

// The bank simulator (boleto collection by CNAB 240, transfers and statements), for tests
// and local development only.
//
//   - JUPITER_HTTP_ADDR (default 127.0.0.1:8590): the bank's API.
//   - JUPITER_SIM_HOST: the host[:port] hybrid boletos' Pix QR codes point to.
//   - JUPITER_SIM_BANK_CLIENTS: the bank's collection clients, as
//     token:cnpj:agreement:branch-digit:account-digit, comma separated.
//   - JUPITER_SIM_ADMIN_ADDR (default 127.0.0.1:8591): unauthenticated operator controls
//     over plain HTTP, paying boletos and closing the day; keep it on loopback.
func main() {
	service.Main("sim-bank", "127.0.0.1:8590", func(_ context.Context, cfg service.Config, logger *slog.Logger) (service.App, error) {
		clients, err := parseClients(os.Getenv("JUPITER_SIM_BANK_CLIENTS"))
		if err != nil {
			return service.App{}, err
		}
		host := os.Getenv("JUPITER_SIM_HOST")
		if host == "" {
			host = cfg.HTTPAddr
		}
		sim := bank.New(bank.Config{Logger: logger, Host: host, Clients: clients})
		adminAddr := os.Getenv("JUPITER_SIM_ADMIN_ADDR")
		if adminAddr == "" {
			adminAddr = "127.0.0.1:8591"
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

func parseClients(s string) ([]bank.Client, error) {
	var out []bank.Client
	tokens := map[string]bool{}
	for spec := range strings.SplitSeq(s, ",") {
		if spec = strings.TrimSpace(spec); spec == "" {
			continue
		}
		parts := strings.Split(spec, ":")
		if len(parts) != 5 {
			return nil, errors.New("JUPITER_SIM_BANK_CLIENTS: an entry is not token:cnpj:agreement:branch-digit:account-digit")
		}
		if len(parts[0]) < minToken || tokens[parts[0]] || len(parts[1]) != 14 || !taxid.Valid(parts[1]) || parts[2] == "" {
			return nil, fmt.Errorf("JUPITER_SIM_BANK_CLIENTS: each needs a token of %d characters or more of its own, a valid CNPJ and an agreement", minToken)
		}
		branch, branchDV, ok1 := strings.Cut(parts[3], "-")
		number, numberDV, ok2 := strings.Cut(parts[4], "-")
		if !ok1 || !ok2 || len(branch) > 5 || len(number) > 12 || len(branchDV) != 1 || len(numberDV) != 1 {
			return nil, errors.New("JUPITER_SIM_BANK_CLIENTS: branch and account are number-digit, of up to 5 and 12 digits")
		}
		tokens[parts[0]] = true
		out = append(out, bank.Client{
			Token: parts[0], TaxID: parts[1], Name: "Jupiter Pagamentos", Agreement: parts[2],
			Account: cnab240.Account{Branch: fmt.Sprintf("%05s", branch), BranchDV: branchDV, Number: fmt.Sprintf("%012s", number), NumberDV: numberDV},
		})
	}
	if len(out) == 0 {
		return nil, errors.New("JUPITER_SIM_BANK_CLIENTS names no client")
	}
	return out, nil
}
