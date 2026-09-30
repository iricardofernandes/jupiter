package main

import (
	"context"
	"log/slog"

	"github.com/iricardofernandes/jupiter/internal/platform/service"
)

func main() {
	service.Main("api", ":8080", func(context.Context, service.Config, *slog.Logger) (service.App, error) {
		return service.App{}, nil
	})
}
