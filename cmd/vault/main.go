package main

import (
	"context"
	"log/slog"

	"github.com/iricardofernandes/jupiter/internal/platform/service"
)

func main() {
	service.Main("vault", ":8082", func(context.Context, service.Config, *slog.Logger) (service.App, error) {
		return service.App{}, nil
	})
}
