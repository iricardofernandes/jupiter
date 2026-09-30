//go:build integration

package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/api"
)

// processor stands for a counterparty called between atomic phases, such as a card
// network. It honours idempotency keys, as the real ones do.
type processor struct {
	mu      sync.Mutex
	charged map[string]bool
	calls   int
}

func (p *processor) charge(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.charged[key] = true
	return len(p.charged)
}

type twoPhase struct {
	h         *harness
	handler   http.Handler
	processor *processor
	failOnce  atomic.Bool
}

// newTwoPhase registers an operation shaped like a payment: reserve (atomic), call the
// processor (foreign), record the result (atomic).
func newTwoPhase(h *harness) *twoPhase {
	h.t.Helper()
	if _, err := h.pool.Exec(h.t.Context(), "CREATE TABLE effects (step text NOT NULL)"); err != nil {
		h.t.Fatal(err)
	}
	tp := &twoPhase{h: h, processor: &processor{charged: map[string]bool{}}}
	tp.handler = h.api.RegisterTestOperation("two_phase",
		api.NewTestPhase("started", nil, func(ctx context.Context, tx pgx.Tx) (string, any, error) {
			_, err := tx.Exec(ctx, "INSERT INTO effects VALUES ('reserved')")
			return "reserved", nil, err
		}),
		api.NewTestPhase("reserved",
			func(context.Context) error {
				tp.processor.charge("operation-1")
				return nil
			},
			func(ctx context.Context, tx pgx.Tx) (string, any, error) {
				if tp.failOnce.CompareAndSwap(true, false) {
					return "", nil, errors.New("database hiccup")
				}
				_, err := tx.Exec(ctx, "INSERT INTO effects VALUES ('captured')")
				return "", map[string]any{"object": "result", "status": "captured"}, err
			}),
	)
	return tp
}

func (tp *twoPhase) post(key string) (int, string, error) {
	srv := httptest.NewServer(tp.handler)
	defer srv.Close()
	req, err := http.NewRequestWithContext(tp.h.t.Context(), http.MethodPost, srv.URL, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+tp.h.keys["sk_test_"])
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var buf [512]byte
	n, _ := resp.Body.Read(buf[:])
	return resp.StatusCode, string(buf[:n]), nil
}

func (tp *twoPhase) effects() map[string]int {
	tp.h.t.Helper()
	rows, err := tp.h.pool.Query(tp.h.t.Context(), "SELECT step, count(*) FROM effects GROUP BY step")
	if err != nil {
		tp.h.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var step string
		var n int
		if err := rows.Scan(&step, &n); err != nil {
			tp.h.t.Fatal(err)
		}
		out[step] = n
	}
	return out
}

// Phase 2 exit criterion: the process dies between atomic phases, and the completer
// finishes the request correctly. The handler's goroutine exits right after the first
// phase commits, leaving the database exactly as a killed process would: the key
// locked, its recovery point advanced, no response stored.
func TestTheCompleterFinishesARequestAbandonedBetweenPhases(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	tp := newTwoPhase(h)
	h.api.SetIdempotencyTiming(time.Minute, 200*time.Millisecond)
	var crashed atomic.Bool
	h.api.SetAfterPhase(func(point string) {
		if point == "reserved" && crashed.CompareAndSwap(false, true) {
			runtime.Goexit()
		}
	})

	if _, _, err := tp.post("op-1"); err == nil {
		t.Fatal("the request completed although its process died")
	}
	if got := tp.effects(); got["reserved"] != 1 || got["captured"] != 0 {
		t.Fatalf("after the crash, effects = %v", got)
	}

	// A client retry while the dead runner's lock is fresh waits, then gives up.
	if status, body, err := tp.post("op-1"); err != nil || status != http.StatusConflict {
		t.Fatalf("retry during the lock = %d %s %v, want 409", status, body, err)
	}
	if n, err := h.api.CompleteAbandoned(t.Context()); err != nil || n != 0 {
		t.Fatalf("CompleteAbandoned before the lock is stale = %d, %v; want 0", n, err)
	}

	h.clock.Advance(2 * time.Minute)
	if n, err := h.api.CompleteAbandoned(t.Context()); err != nil || n != 1 {
		t.Fatalf("CompleteAbandoned = %d, %v; want 1", n, err)
	}
	if got := tp.effects(); got["reserved"] != 1 || got["captured"] != 1 {
		t.Fatalf("after completion, effects = %v, want each step once", got)
	}
	if len(tp.processor.charged) != 1 {
		t.Fatalf("the processor charged %d operations, want 1", len(tp.processor.charged))
	}

	status, body, err := tp.post("op-1")
	if err != nil || status != http.StatusOK || body != `{"object":"result","status":"captured"}` {
		t.Fatalf("retry after completion = %d %s %v", status, body, err)
	}
	if got := tp.effects(); got["captured"] != 1 {
		t.Fatalf("the retry acted again: %v", got)
	}
}

// A failure that is not the client's releases the key without a response; the retry
// resumes from the recovery point instead of starting over.
func TestARetryAfterAServerErrorResumesFromTheRecoveryPoint(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	tp := newTwoPhase(h)
	tp.failOnce.Store(true)

	status, _, err := tp.post("op-2")
	if err != nil || status != http.StatusInternalServerError {
		t.Fatalf("first attempt = %d, %v; want 500", status, err)
	}
	status, body, err := tp.post("op-2")
	if err != nil || status != http.StatusOK {
		t.Fatalf("retry = %d %s %v", status, body, err)
	}
	if got := tp.effects(); got["reserved"] != 1 || got["captured"] != 1 {
		t.Fatalf("effects = %v, want each step once", got)
	}
	// The foreign call ran on both attempts, and its own idempotency kept it to one charge.
	if tp.processor.calls != 2 || len(tp.processor.charged) != 1 {
		t.Fatalf("processor calls %d, charges %d; want 2 calls and 1 charge", tp.processor.calls, len(tp.processor.charged))
	}
}

func TestTheReaperKeepsUnfinishedWork(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	tp := newTwoPhase(h)
	h.api.SetIdempotencyTiming(time.Hour, 50*time.Millisecond)
	if status, _, err := tp.post("finished"); err != nil || status != http.StatusOK {
		t.Fatal(status, err)
	}
	var crashed atomic.Bool
	h.api.SetAfterPhase(func(string) {
		if crashed.CompareAndSwap(false, true) {
			runtime.Goexit()
		}
	})
	_, _, _ = tp.post("abandoned")

	h.clock.Advance(25 * time.Hour)
	n, err := h.api.ReapIdempotencyKeys(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("reaped %d, %v; want only the finished key", n, err)
	}
	var left string
	if err := h.pool.QueryRow(t.Context(), "SELECT key FROM api.idempotency_keys").Scan(&left); err != nil || left != "abandoned" {
		t.Fatalf("remaining key = %q, %v", left, err)
	}
}

// A client that hangs up mid-request must not leave its key locked: the request runs on
// to the end detached from the connection, and the retry gets its response.
func TestAClientThatHangsUpCanRetryAtOnce(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	h.api.SetIdempotencyTiming(time.Hour, 5*time.Second)
	handler := h.api.RegisterTestOperation("slow",
		api.NewTestPhase("started", nil, func(ctx context.Context, _ pgx.Tx) (string, any, error) {
			select {
			case <-ctx.Done():
				return "", nil, ctx.Err()
			case <-time.After(300 * time.Millisecond):
			}
			return "", map[string]any{"object": "result"}, nil
		}))
	srv := httptest.NewServer(handler)
	defer srv.Close()
	send := func(timeout time.Duration) (int, error) {
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
		req.Header.Set("Authorization", "Bearer "+h.keys["sk_test_"])
		req.Header.Set("Idempotency-Key", "hangup")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	if _, err := send(50 * time.Millisecond); err == nil {
		t.Fatal("the first request was expected to time out on the client")
	}
	began := time.Now()
	status, err := send(5 * time.Second)
	if err != nil || status != http.StatusOK {
		t.Fatalf("retry = %d, %v; want 200", status, err)
	}
	if waited := time.Since(began); waited > 2*time.Second {
		t.Fatalf("the retry waited %v: the key stayed locked after the client hung up", waited)
	}
}

func TestAPanicReleasesTheKey(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	h.api.SetIdempotencyTiming(time.Hour, 200*time.Millisecond)
	var panicked atomic.Bool
	handler := h.api.RegisterTestOperation("panics",
		api.NewTestPhase("started", nil, func(context.Context, pgx.Tx) (string, any, error) {
			if panicked.CompareAndSwap(false, true) {
				panic("bug")
			}
			return "", map[string]any{"object": "result"}, nil
		}))
	tp := &twoPhase{h: h, handler: handler}
	if status, _, err := tp.post("panic"); err != nil || status != http.StatusInternalServerError {
		t.Fatalf("first attempt = %d, %v; want 500", status, err)
	}
	if status, body, err := tp.post("panic"); err != nil || status != http.StatusOK {
		t.Fatalf("retry after the panic = %d %s %v; want 200", status, body, err)
	}
}

// A runner that dies before committing anything leaves the key at 'started'. There is
// nothing to complete, so the completer unlocks it and the client's retry runs it.
func TestAKeyAbandonedBeforeAnyWorkIsUnlocked(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	h.api.SetIdempotencyTiming(time.Minute, 100*time.Millisecond)
	var died atomic.Bool
	handler := h.api.RegisterTestOperation("dies_early",
		api.NewTestPhase("started",
			func(context.Context) error {
				if died.CompareAndSwap(false, true) {
					runtime.Goexit()
				}
				return nil
			},
			func(context.Context, pgx.Tx) (string, any, error) {
				return "", map[string]any{"object": "result"}, nil
			}))
	tp := &twoPhase{h: h, handler: handler}
	_, _, _ = tp.post("early")
	if status, _, _ := tp.post("early"); status != http.StatusConflict {
		t.Fatalf("retry while locked = %d, want 409", status)
	}
	h.clock.Advance(2 * time.Minute)
	if _, err := h.api.CompleteAbandoned(t.Context()); err != nil {
		t.Fatal(err)
	}
	if status, body, err := tp.post("early"); err != nil || status != http.StatusOK {
		t.Fatalf("retry after unlocking = %d %s %v; want 200", status, body, err)
	}
}
