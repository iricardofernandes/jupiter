package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/platform/service"
	"github.com/iricardofernandes/jupiter/internal/sim/pix"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

const readHeaderTimeout = 5 * time.Second

// The Pix simulator (a bank with the API Pix, the SPI, DICT and payers), for tests and
// local development only.
//
//   - JUPITER_HTTP_ADDR (default 127.0.0.1:8586): the API Pix and payload locations, over
//     TLS with JUPITER_SIM_TLS_CERT and JUPITER_SIM_TLS_KEY; client certificates from
//     JUPITER_SIM_TLS_CA are accepted, and payers trust that CA too.
//   - JUPITER_SIM_HOST: the host[:port] BR Codes point payers to (default: the address).
//   - JUPITER_SIM_WEBHOOK_CERT and JUPITER_SIM_WEBHOOK_KEY: the certificate notifications
//     are sent with; receivers must have certificates from JUPITER_SIM_TLS_CA.
//   - JUPITER_SIM_PIX_CLIENTS: the bank's clients, as id:secret:key:balance, comma
//     separated, balance in reais.
//   - JUPITER_SIM_ADMIN_ADDR (default 127.0.0.1:8587): unauthenticated operator controls
//     over plain HTTP; keep it on loopback.
func main() {
	service.Main("sim-pix", "127.0.0.1:8586", func(_ context.Context, cfg service.Config, logger *slog.Logger) (service.App, error) {
		serverTLS, err := mtls.LoadServerConfig(os.Getenv("JUPITER_SIM_TLS_CERT"), os.Getenv("JUPITER_SIM_TLS_KEY"), os.Getenv("JUPITER_SIM_TLS_CA"))
		if err != nil {
			return service.App{}, err
		}
		webhookTLS, err := mtls.LoadClientConfig(os.Getenv("JUPITER_SIM_WEBHOOK_CERT"), os.Getenv("JUPITER_SIM_WEBHOOK_KEY"), os.Getenv("JUPITER_SIM_TLS_CA"))
		if err != nil {
			return service.App{}, err
		}
		clients, err := parseClients(os.Getenv("JUPITER_SIM_PIX_CLIENTS"))
		if err != nil {
			return service.App{}, err
		}
		host := os.Getenv("JUPITER_SIM_HOST")
		if host == "" {
			host = cfg.HTTPAddr
		}
		sim, err := pix.New(pix.Config{
			Logger: logger, Host: host, Clients: clients, WebhookTLS: webhookTLS,
			PayerTLS: &tls.Config{RootCAs: serverTLS.ClientCAs, MinVersion: tls.VersionTLS12},
		})
		if err != nil {
			return service.App{}, err
		}
		adminAddr := os.Getenv("JUPITER_SIM_ADMIN_ADDR")
		if adminAddr == "" {
			adminAddr = "127.0.0.1:8587"
		}
		return service.App{
			Handler:    sim.Handler(),
			TLS:        pix.ServerTLS(serverTLS.Certificates[0], serverTLS.ClientCAs),
			Background: []func(context.Context) error{func(ctx context.Context) error { return serveAdmin(ctx, adminAddr, sim, logger) }},
			Close:      sim.Close,
		}, nil
	})
}

func serveAdmin(ctx context.Context, addr string, sim *pix.Sim, logger *slog.Logger) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: sim.AdminHandler(), ReadHeaderTimeout: readHeaderTimeout}
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

func parseClients(s string) ([]pix.Client, error) {
	var clients []pix.Client
	for spec := range strings.SplitSeq(s, ",") {
		if spec = strings.TrimSpace(spec); spec == "" {
			continue
		}
		parts := strings.Split(spec, ":")
		if len(parts) != 4 {
			return nil, fmt.Errorf("JUPITER_SIM_PIX_CLIENTS: %q is not id:secret:key:balance", spec)
		}
		balance, err := pixapi.ParseValor(parts[3])
		if err != nil {
			if n, errInt := strconv.ParseInt(parts[3], 10, 64); errInt == nil {
				balance, err = n*100, nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("JUPITER_SIM_PIX_CLIENTS: balance %q", parts[3])
		}
		clients = append(clients, pix.Client{
			ID: parts[0], Secret: parts[1], Keys: []string{parts[2]}, Balance: balance,
			Name: "Jupiter Pagamentos", TaxID: "12345678000195",
		})
	}
	if len(clients) == 0 {
		return nil, errors.New("JUPITER_SIM_PIX_CLIENTS names no client")
	}
	return clients, nil
}
