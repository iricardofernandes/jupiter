//go:build integration

package ledger_test

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

// Set JUPITER_LEDGER_BENCH=1 for the larger run recorded in docs/benchmarks.
func hotAccountLoad() (writers, perWriter int) {
	if os.Getenv("JUPITER_LEDGER_BENCH") != "" {
		return 64, 250
	}
	return 16, 25
}

// Many writers pay into one platform account at once. Each writer has its own wallet,
// as merchants and customers do. Transactions run without InTx's retry, so a deadlock
// or serialization failure fails the test instead of being absorbed.
func TestConcurrentWritersToAHotAccount(t *testing.T) {
	for _, target := range []target{hotBatched, hotSynchronous} {
		t.Run(string(target), func(t *testing.T) {
			writers, perWriter := hotAccountLoad()
			result := runPostings(t, target, writers, perWriter)
			t.Logf("hot account batched=%t: %d writers × %d postings in %v = %.0f postings/s; latency p50 %v p99 %v max %v",
				target == hotBatched, writers, perWriter, result.elapsed.Round(time.Millisecond), result.throughput(),
				result.percentile(50), result.percentile(99), result.percentile(100))
		})
	}
}

// With JUPITER_LEDGER_SWEEP=1, postings at rising concurrency until the ledger saturates:
// each writer into an account of its own, and all into one hot account, batched or not.
func TestLedgerSaturation(t *testing.T) {
	if os.Getenv("JUPITER_LEDGER_SWEEP") == "" {
		t.Skip("set JUPITER_LEDGER_SWEEP=1 to sweep")
	}
	const postings = 8000
	for _, target := range []target{independent, hotBatched, hotSynchronous} {
		t.Run(string(target), func(t *testing.T) {
			t.Logf("| Writers | Postings/s | p50 | p95 | p99 | Max |")
			t.Logf("|---|---|---|---|---|---|")
			for _, writers := range []int{1, 2, 4, 8, 16, 32, 64, 128, 256} {
				r := runPostings(t, target, writers, max(20, postings/writers))
				t.Logf("| %d | %.0f | %v | %v | %v | %v |", writers, r.throughput(), r.percentile(50), r.percentile(95), r.percentile(99), r.percentile(100))
			}
		})
	}
}

// maxConns keeps the pool under PostgreSQL's default of 100 connections; writers beyond
// it queue for one, as a service's requests would.
const maxConns = 64

// target is where writers post: into accounts of their own, or one shared account.
type target string

const (
	independent    target = "independent"
	hotBatched     target = "hot_batched"
	hotSynchronous target = "hot_synchronous"
)

type loadResult struct {
	elapsed   time.Duration
	latencies []time.Duration
}

func (r loadResult) throughput() float64 {
	return float64(len(r.latencies)) / r.elapsed.Seconds()
}

func (r loadResult) percentile(p int) time.Duration {
	sorted := slices.Clone(r.latencies)
	slices.Sort(sorted)
	i := (len(sorted) - 1) * p / 100
	return sorted[i].Round(10 * time.Microsecond)
}

func runPostings(t *testing.T, to target, writers, perWriter int) loadResult {
	t.Helper()
	const unit = 10
	pool, err := postgres.Connect(t.Context(), server.URL(t)+"&pool_max_conns="+strconv.Itoa(min(writers+4, maxConns)))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	f := &fixture{t: t, pool: pool, ledger: ledger.New(), clock: newClock()}
	cash := f.cash()
	platform := f.account(ledger.AccountSpec{Code: "platform", Normal: ledger.CreditNormal, Batched: to == hotBatched})
	wallets, sinks := make([]ledger.Account, writers), make([]ledger.Account, writers)
	for i := range wallets {
		wallets[i], sinks[i] = f.wallet(), platform
		if to == independent {
			sinks[i] = f.account(ledger.AccountSpec{Code: "sink", Normal: ledger.CreditNormal})
		}
		f.mustPost(ledger.Debit(cash.ID, brl(t, unit*int64(perWriter))), ledger.Credit(wallets[i].ID, brl(t, unit*int64(perWriter))))
	}

	latencies := make([][]time.Duration, writers)
	errs := make(chan error, writers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			<-start
			for range perWriter {
				began := time.Now()
				err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
					_, err := f.ledger.Post(t.Context(), tx, ledger.Posting{Legs: []ledger.Leg{
						ledger.Debit(wallets[w].ID, brl(t, unit)), ledger.Credit(sinks[w].ID, brl(t, unit)),
					}})
					return err
				})
				if err != nil {
					errs <- fmt.Errorf("writer %d: %w", w, err)
					return
				}
				latencies[w] = append(latencies[w], time.Since(began))
			}
		})
	}
	began := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(began)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	for {
		applied, err := f.ledger.ApplyQueued(t.Context(), pool, 10_000)
		if err != nil {
			t.Fatal(err)
		}
		if applied == 0 {
			break
		}
	}
	if to == independent {
		for _, sink := range sinks {
			if got, want := f.fields(sink), [4]int64{0, unit * int64(perWriter), 0, 0}; got != want {
				t.Fatalf("sink = %v, want %v: an update was lost", got, want)
			}
		}
	} else if got, want := f.fields(platform), [4]int64{0, unit * int64(writers*perWriter), 0, 0}; got != want {
		t.Fatalf("platform = %v, want %v: an update was lost", got, want)
	}
	for _, w := range wallets {
		if posted, _ := f.balance(w).Posted(); !posted.IsZero() {
			t.Fatalf("wallet %s = %v, want 0", w.ID, posted)
		}
	}
	f.requireClean()

	all := make([]time.Duration, 0, writers*perWriter)
	for _, l := range latencies {
		all = append(all, l...)
	}
	return loadResult{elapsed: elapsed, latencies: all}
}
