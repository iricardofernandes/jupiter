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
	for _, batched := range []bool{true, false} {
		t.Run(fmt.Sprintf("batched=%t", batched), func(t *testing.T) {
			writers, perWriter := hotAccountLoad()
			result := runHotAccount(t, batched, writers, perWriter)
			t.Logf("hot account batched=%t: %d writers × %d postings in %v = %.0f postings/s; latency p50 %v p99 %v max %v",
				batched, writers, perWriter, result.elapsed.Round(time.Millisecond), result.throughput(),
				result.percentile(50), result.percentile(99), result.percentile(100))
		})
	}
}

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

func runHotAccount(t *testing.T, batched bool, writers, perWriter int) loadResult {
	t.Helper()
	const unit = 10
	pool, err := postgres.Connect(t.Context(), server.URL(t)+"&pool_max_conns="+strconv.Itoa(writers+4))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	f := &fixture{t: t, pool: pool, ledger: ledger.New(), clock: newClock()}
	cash := f.cash()
	platform := f.account(ledger.AccountSpec{Code: "platform", Normal: ledger.CreditNormal, Batched: batched})
	wallets := make([]ledger.Account, writers)
	for i := range wallets {
		wallets[i] = f.wallet()
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
						ledger.Debit(wallets[w].ID, brl(t, unit)), ledger.Credit(platform.ID, brl(t, unit)),
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
	if got, want := f.fields(platform), [4]int64{0, unit * int64(writers*perWriter), 0, 0}; got != want {
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
