package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
)

// requestDuration is jupiter.api.request.duration: how long the API took to answer, by
// route and status.
var requestDuration = sync.OnceValue(func() metric.Float64Histogram {
	h, _ := telemetry.Meter().Float64Histogram("jupiter.api.request.duration",
		metric.WithUnit("s"), metric.WithDescription("How long the API took to answer a request, by route and status."))
	return h
})

type routeKey struct{}

// withRoute notes the route the mux matched, for the request's metric: the mux sets it on
// the request it is given.
func withRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if route, ok := r.Context().Value(routeKey{}).(*string); ok {
			*route = r.Pattern
		}
	})
}

func measure(ctx context.Context, route string, status int, took time.Duration) {
	if route == "" || route == "/" {
		route = "unmatched"
	}
	requestDuration().Record(ctx, took.Seconds(), metric.WithAttributes(
		attribute.String("http.route", route), attribute.Int("http.response.status_code", status),
	))
}
