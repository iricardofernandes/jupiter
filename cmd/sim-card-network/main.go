package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/iricardofernandes/jupiter/internal/platform/service"
	"github.com/iricardofernandes/jupiter/internal/sim/cardnetwork"
)

const dayCheck = time.Minute

// The card network and issuer simulator, for tests and local development only: ISO 8583
// over TCP on JUPITER_CARDNET_ADDR (default 127.0.0.1:8583), and clearing files and
// unauthenticated operator controls over HTTP on JUPITER_HTTP_ADDR (default
// 127.0.0.1:8584).
func main() {
	service.Main("sim-card-network", "127.0.0.1:8584", func(ctx context.Context, _ service.Config, logger *slog.Logger) (service.App, error) {
		addr := os.Getenv("JUPITER_CARDNET_ADDR")
		if addr == "" {
			addr = "127.0.0.1:8583"
		}
		network := cardnetwork.New(cardnetwork.Config{Logger: logger})
		if err := network.Start(addr); err != nil {
			return service.App{}, err
		}
		logger.InfoContext(ctx, "card network listening", "addr", network.Addr())
		return service.App{
			Handler: network.Handler(),
			Background: []func(context.Context) error{
				func(ctx context.Context) error { return network.RunDays(ctx, dayCheck) },
			},
			Close: network.Close,
		}, nil
	})
}
