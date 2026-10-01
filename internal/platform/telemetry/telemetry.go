// Package telemetry sends a service's metrics to the OpenTelemetry Collector, which
// exposes them to Prometheus (ADR 0002). It is on when OTEL_EXPORTER_OTLP_ENDPOINT names
// the Collector's OTLP/HTTP address, such as http://127.0.0.1:54318; without it the
// instruments record nothing.
package telemetry

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// interval is how often metrics are sent; Prometheus scrapes the Collector every 15
// seconds.
const interval = 15 * time.Second

// latencyBuckets, in seconds, are fine where payments live, a few milliseconds, and wide
// enough to see a rail time out.
var latencyBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Start sends the process's metrics to the Collector, named for service, until the
// returned function is called.
func Start(ctx context.Context, service string, getenv func(string) string) (func(context.Context) error, error) {
	endpoint := getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(strings.TrimSuffix(endpoint, "/")+"/v1/metrics"))
	if err != nil {
		return nil, err
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(interval))),
		sdkmetric.WithResource(resource.NewSchemaless(attribute.String("service.name", service))),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Kind: sdkmetric.InstrumentKindHistogram, Unit: "s"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: latencyBuckets}},
		)),
	)
	otel.SetMeterProvider(provider)
	return provider.Shutdown, nil
}

// Meter is Jupiter's meter.
func Meter() metric.Meter { return otel.Meter("github.com/iricardofernandes/jupiter") }

// Reading is one value of a gauge a module samples, such as how many payments have been
// processing for too long.
type Reading struct {
	Gauge string
	Value int64
	Attrs []attribute.KeyValue
}

// Gauge names a sampled value and says what it counts.
type Gauge struct {
	Name, Description string
}
