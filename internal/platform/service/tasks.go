package service

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
)

// The metrics of the background tasks Every runs:
//   - jupiter.task.runs, by task and outcome (ok, error);
//   - jupiter.task.duration, by task;
//   - jupiter.task.last_success, the Unix time a task last succeeded;
//   - jupiter.task.interval, in seconds, how often it runs.
//
// A task whose last success is several intervals old has stalled or keeps failing.
var tasks = &taskSet{byName: map[string]*taskState{}}

type taskSet struct {
	once     sync.Once
	mu       sync.Mutex
	byName   map[string]*taskState
	runs     metric.Int64Counter
	duration metric.Float64Histogram
}

type taskState struct {
	set         *taskSet
	attrs       attribute.Set
	interval    time.Duration
	mu          sync.Mutex
	lastSuccess time.Time
}

func (t *taskSet) add(name string, interval time.Duration) *taskState {
	t.once.Do(t.register)
	t.mu.Lock()
	defer t.mu.Unlock()
	// Until its first success a task counts from its start, so one that hangs on its first
	// run stalls like any other.
	state := &taskState{set: t, attrs: attribute.NewSet(attribute.String("task", name)), interval: interval, lastSuccess: time.Now()}
	t.byName[name] = state
	return state
}

func (t *taskSet) register() {
	meter := telemetry.Meter()
	t.runs, _ = meter.Int64Counter("jupiter.task.runs", metric.WithDescription("Runs of a background task, by outcome."))
	t.duration, _ = meter.Float64Histogram("jupiter.task.duration", metric.WithUnit("s"), metric.WithDescription("How long a background task's run took."))
	lastSuccess, _ := meter.Int64ObservableGauge("jupiter.task.last_success", metric.WithDescription("When a background task last succeeded, in Unix seconds."))
	interval, _ := meter.Int64ObservableGauge("jupiter.task.interval", metric.WithDescription("How often a background task runs, in seconds."))
	_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		t.mu.Lock()
		defer t.mu.Unlock()
		for _, s := range t.byName {
			s.mu.Lock()
			o.ObserveInt64(lastSuccess, s.lastSuccess.Unix(), metric.WithAttributeSet(s.attrs))
			s.mu.Unlock()
			o.ObserveInt64(interval, int64(s.interval.Seconds()), metric.WithAttributeSet(s.attrs))
		}
		return nil
	}, lastSuccess, interval)
}

func (s *taskState) ran(ctx context.Context, began time.Time, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	} else {
		s.mu.Lock()
		s.lastSuccess = time.Now()
		s.mu.Unlock()
	}
	if s.set.runs == nil || s.set.duration == nil {
		return // the meter refused the instruments; the task runs unmeasured
	}
	ctx = context.WithoutCancel(ctx)
	s.set.runs.Add(ctx, 1, metric.WithAttributeSet(s.attrs), metric.WithAttributes(attribute.String("outcome", outcome)))
	s.set.duration.Record(ctx, time.Since(began).Seconds(), metric.WithAttributeSet(s.attrs))
}
