// Package loadgen drives Jupiter's public API the way traffic does: steady payments with
// good cards, or a card-testing burst, fraudsters trying many card numbers with small
// amounts from many addresses. It speaks only HTTP, as any client would.
package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

type Card struct {
	Number   string
	ExpMonth int
	ExpYear  int
}

// Result is how one payment ended.
type Result struct {
	Status       string
	DeclineCode  string
	RiskDecision string
}

func (c Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c Client) post(ctx context.Context, path string, body any) (int, map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("loadgen: %s answered %d with %q", path, resp.StatusCode, out)
	}
	return resp.StatusCode, decoded, nil
}

// Pay saves the card and pays amount with it from ip.
func (c Client) Pay(ctx context.Context, card Card, amount int64, ip string) (Result, error) {
	status, pm, err := c.post(ctx, "/v1/payment_methods", map[string]any{"type": "card", "card": map[string]any{
		"number": card.Number, "exp_month": card.ExpMonth, "exp_year": card.ExpYear,
	}})
	if err != nil {
		return Result{}, err
	}
	if status != http.StatusOK {
		return Result{Status: "card_refused", DeclineCode: code(pm)}, nil
	}
	methodID, _ := pm["id"].(string)
	status, body, err := c.post(ctx, "/v1/payment_intents", map[string]any{
		"amount": amount, "currency": "brl", "payment_method": methodID, "confirm": true, "customer_ip": ip,
	})
	if err != nil {
		return Result{}, err
	}
	intent := body
	if status == http.StatusPaymentRequired {
		e, _ := body["error"].(map[string]any)
		intent, _ = e["payment_intent"].(map[string]any)
	} else if status != http.StatusOK {
		return Result{}, fmt.Errorf("loadgen: paying answered %d: %v", status, body)
	}
	r := Result{DeclineCode: code(body)}
	r.Status, _ = intent["status"].(string)
	if rd, ok := intent["risk_decision"].(map[string]any); ok {
		r.RiskDecision, _ = rd["action"].(string)
	}
	return r, nil
}

func code(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	if c, ok := e["decline_code"].(string); ok {
		return c
	}
	c, _ := e["code"].(string)
	return c
}

// Report counts how a run's payments ended.
type Report struct {
	mu        sync.Mutex
	Attempts  int
	Succeeded int
	Declined  int
	Blocked   int
	Other     int
	Errors    []error
}

func (r *Report) add(res Result, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Attempts++
	switch {
	case err != nil:
		r.Errors = append(r.Errors, err)
	case res.RiskDecision == "block":
		r.Blocked++
	case res.Status == "succeeded", res.Status == "requires_capture":
		r.Succeeded++
	case res.Status == "requires_payment_method":
		r.Declined++
	default:
		r.Other++
	}
}

func (r *Report) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fmt.Sprintf("%d attempts: %d succeeded, %d declined, %d blocked, %d other, %d errors",
		r.Attempts, r.Succeeded, r.Declined, r.Blocked, r.Other, len(r.Errors))
}

// run makes n payments from workers goroutines; next draws the i-th payment.
func run(ctx context.Context, c Client, n, workers int, next func(i int) (Card, int64, string)) *Report {
	report := &Report{}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range max(1, workers) {
		wg.Go(func() {
			for i := range jobs {
				card, amount, ip := next(i)
				report.add(c.Pay(ctx, card, amount, ip))
			}
		})
	}
	for i := range n {
		select {
		case jobs <- i:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	return report
}

// CardTesting tries n random card numbers, each valid by its check digit, for small
// amounts, from a different address each time.
func CardTesting(ctx context.Context, c Client, n, workers int, seed uint64) *Report {
	var mu sync.Mutex
	rng := mrand.New(mrand.NewPCG(seed, seed+1)) //nolint:gosec // reproducible traffic, not security
	return run(ctx, c, n, workers, func(int) (Card, int64, string) {
		mu.Lock()
		defer mu.Unlock()
		ip := fmt.Sprintf("100.%d.%d.%d", 64+rng.IntN(64), rng.IntN(256), 1+rng.IntN(254))
		return Card{Number: RandomCardNumber(rng, "4"), ExpMonth: 1 + rng.IntN(12), ExpYear: 2027 + rng.IntN(5)}, int64(100 + rng.IntN(400)), ip
	})
}

// Steady pays with cards, in turn, for ordinary amounts from a few addresses.
func Steady(ctx context.Context, c Client, cards []Card, n, workers int) *Report {
	return run(ctx, c, n, workers, func(i int) (Card, int64, string) {
		// Whole reais: the test rail gives amounts ending in 91 to 93 centavos a meaning.
		return cards[i%len(cards)], int64(50+137*i%200) * 100, fmt.Sprintf("198.51.100.%d", 1+i%20)
	})
}

// RandomCardNumber is sixteen digits beginning with prefix, with a valid check digit.
func RandomCardNumber(rng *mrand.Rand, prefix string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for b.Len() < 15 {
		b.WriteString(strconv.Itoa(rng.IntN(10)))
	}
	body := b.String()
	sum := 0
	for i := range len(body) {
		d := int(body[len(body)-1-i] - '0')
		if i%2 == 0 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return body + strconv.Itoa((10-sum%10)%10)
}
