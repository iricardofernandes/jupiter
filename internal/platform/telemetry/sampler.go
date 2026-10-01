package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Probe reads some of a module's gauges.
type Probe func(context.Context) ([]Reading, error)

// Sampler keeps the latest readings of a set of gauges, taken by Sample and observed by
// the meter whenever it exports. A gauge that has never been read reports nothing.
type Sampler struct {
	probes []Probe
	gauges map[string]metric.Int64ObservableGauge
	mu     sync.Mutex
	latest []Reading
}

// NewSampler registers the gauges with the meter and reads them with probes.
func NewSampler(meter metric.Meter, gauges []Gauge, probes ...Probe) (*Sampler, error) {
	s := &Sampler{probes: probes, gauges: map[string]metric.Int64ObservableGauge{}}
	instruments := make([]metric.Observable, 0, len(gauges))
	for _, g := range gauges {
		gauge, err := meter.Int64ObservableGauge(g.Name, metric.WithDescription(g.Description))
		if err != nil {
			return nil, err
		}
		s.gauges[g.Name] = gauge
		instruments = append(instruments, gauge)
	}
	if _, err := meter.RegisterCallback(s.observe, instruments...); err != nil {
		return nil, err
	}
	return s, nil
}

// Sample runs every probe and keeps what they read. A probe that fails keeps its
// gauges' previous readings out: a gauge that cannot be read is absent, not zero.
func (s *Sampler) Sample(ctx context.Context) error {
	var readings []Reading
	var failures []error
	for _, probe := range s.probes {
		got, err := probe(ctx)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, r := range got {
			if _, ok := s.gauges[r.Gauge]; !ok {
				failures = append(failures, fmt.Errorf("telemetry: a reading of %q, which is no registered gauge", r.Gauge))
				continue
			}
			readings = append(readings, r)
		}
	}
	s.mu.Lock()
	s.latest = readings
	s.mu.Unlock()
	return errors.Join(failures...)
}

// Latest is what the last sample read.
func (s *Sampler) Latest() []Reading {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Reading(nil), s.latest...)
}

func (s *Sampler) observe(_ context.Context, o metric.Observer) error {
	for _, r := range s.Latest() {
		o.ObserveInt64(s.gauges[r.Gauge], r.Value, metric.WithAttributeSet(attribute.NewSet(r.Attrs...)))
	}
	return nil
}
