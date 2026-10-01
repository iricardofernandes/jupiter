package telemetry_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
)

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string][]metricdata.DataPoint[int64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string][]metricdata.DataPoint[int64]{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if g, ok := m.Data.(metricdata.Gauge[int64]); ok {
				out[m.Name] = g.DataPoints
			}
		}
	}
	return out
}

// A gauge reports what the last sample read; a probe that fails leaves its gauges absent,
// not zero; and a reading of no registered gauge is an error.
func TestSampler(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
	failing := false
	gauges := []telemetry.Gauge{{Name: "a"}, {Name: "b"}}
	s, err := telemetry.NewSampler(meter, gauges,
		func(context.Context) ([]telemetry.Reading, error) {
			return []telemetry.Reading{
				{Gauge: "a", Value: 3, Attrs: []attribute.KeyValue{attribute.String("counterparty", "bank")}},
				{Gauge: "a", Value: 4, Attrs: []attribute.KeyValue{attribute.String("counterparty", "slc")}},
			}, nil
		},
		func(context.Context) ([]telemetry.Reading, error) {
			if failing {
				return nil, errors.New("the database is down")
			}
			return []telemetry.Reading{{Gauge: "b", Value: 7}}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := collect(t, reader); len(got) != 0 {
		t.Fatalf("before a sample: %v", got)
	}
	if err := s.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := collect(t, reader)
	if len(got["a"]) != 2 || len(got["b"]) != 1 || got["b"][0].Value != 7 {
		t.Fatalf("after a sample: %v", got)
	}
	failing = true
	if err := s.Sample(context.Background()); err == nil {
		t.Fatal("a failing probe reported nothing")
	}
	if got := collect(t, reader); len(got["a"]) != 2 || len(got["b"]) != 0 {
		t.Fatalf("after a probe failed: %v", got)
	}

	unknown, err := telemetry.NewSampler(meter, nil, func(context.Context) ([]telemetry.Reading, error) {
		return []telemetry.Reading{{Gauge: "c", Value: 1}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := unknown.Sample(context.Background()); err == nil {
		t.Fatal("a reading of an unregistered gauge was taken")
	}
}

func TestStartIsOffWithoutAnEndpoint(t *testing.T) {
	shutdown, err := telemetry.Start(context.Background(), "test", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
