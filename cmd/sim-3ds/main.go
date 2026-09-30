package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"

	"github.com/iricardofernandes/jupiter/internal/platform/service"
	threedssim "github.com/iricardofernandes/jupiter/internal/sim/3ds"
)

// The 3-D Secure directory server and access control server simulator, for tests and
// local development: HTTP on JUPITER_HTTP_ADDR (default 127.0.0.1:8585).
// JUPITER_SIM_PUBLIC_URL is where browsers reach it; JUPITER_SIM_AUTHENTICATION_KEY
// (base64, shared with sim-card-network) makes authentication values;
// JUPITER_3DS_RESULTS_SECRET signs results sent to 3DS servers.
func main() {
	service.Main("sim-3ds", "127.0.0.1:8585", func(context.Context, service.Config, *slog.Logger) (service.App, error) {
		key, err := base64.StdEncoding.DecodeString(os.Getenv("JUPITER_SIM_AUTHENTICATION_KEY"))
		if err != nil || len(key) == 0 {
			return service.App{}, errors.New("JUPITER_SIM_AUTHENTICATION_KEY must be a base64 key")
		}
		secret := os.Getenv("JUPITER_3DS_RESULTS_SECRET")
		if secret == "" {
			return service.App{}, errors.New("JUPITER_3DS_RESULTS_SECRET is required")
		}
		publicURL := os.Getenv("JUPITER_SIM_PUBLIC_URL")
		if publicURL == "" {
			publicURL = "http://127.0.0.1:8585"
		}
		sim := threedssim.New(threedssim.Config{PublicURL: publicURL, AuthenticationKey: key, ResultsSecret: secret})
		return service.App{Handler: sim.Handler()}, nil
	})
}
