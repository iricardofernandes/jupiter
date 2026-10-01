package main

import (
	"context"
	"encoding/base64"
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
		key, err := base64.StdEncoding.DecodeString(os.Getenv("JUPITER_SIM_AUTHENTICATION_KEY"))
		if err != nil {
			return service.App{}, err
		}
		network := cardnetwork.New(cardnetwork.Config{
			Logger: logger, AuthenticationKey: key,
			TokenEventsURL: os.Getenv("JUPITER_CARDNET_EVENTS_URL"), TokenEventsSecret: os.Getenv("JUPITER_CARDNET_EVENTS_SECRET"),
			DisputeEventsURL: os.Getenv("JUPITER_CARDNET_DISPUTE_EVENTS_URL"), DisputeEventsSecret: os.Getenv("JUPITER_CARDNET_DISPUTE_EVENTS_SECRET"),
			AcquirerToken: os.Getenv("JUPITER_CARDNET_TOKEN"),
		})
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
