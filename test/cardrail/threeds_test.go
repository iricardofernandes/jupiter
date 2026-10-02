//go:build integration

package cardrail_test

import (
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	threedssim "github.com/iricardofernandes/jupiter/internal/sim/3ds"
)

func TestFrictionlessThreeDSecure(t *testing.T) {
	h := newHarness(t)
	_, it := h.pay(map[string]any{"amount": 60000, "payment_method": h.saveCard("4242424242424242"), "request_three_d_secure": "any"})
	if str(it, "status") != "succeeded" {
		t.Fatalf("%v", it)
	}
	var eci, status, value string
	var shift bool
	if err := h.pool.QueryRow(t.Context(), "SELECT eci, three_ds_status, authentication_value, liability_shift FROM payments.attempts WHERE id = $1",
		str(it, "latest_attempt")).Scan(&eci, &status, &value, &shift); err != nil {
		t.Fatal(err)
	}
	if eci != "05" || status != "Y" || value == "" || !shift {
		t.Fatalf("authentication: eci %s, status %s, value %q, liability shift %t", eci, status, value, shift)
	}
	h.consistent()
}

func TestWhatTheIssuerAnswers(t *testing.T) {
	h := newHarness(t)
	for number, want := range map[string]struct {
		status, code string
		shift        bool
	}{
		"4000000000003238": {"requires_payment_method", "payment_intent_authentication_failure", false},
		"4000000000003246": {"requires_payment_method", "payment_intent_authentication_failure", false},
		"4000000000003253": {"succeeded", "", false},
		"4000000000003261": {"succeeded", "", true},
	} {
		_, it := h.pay(map[string]any{"amount": 1000, "payment_method": h.saveCard(number), "request_three_d_secure": "any"})
		code := str(obj(it, "last_payment_error"), "code")
		var shift bool
		_ = h.pool.QueryRow(t.Context(), "SELECT liability_shift FROM payments.attempts WHERE id = $1", str(it, "latest_attempt")).Scan(&shift)
		if str(it, "status") != want.status || code != want.code || shift != want.shift {
			t.Errorf("%s: %s %s, liability shift %t; want %+v", number[12:], str(it, "status"), code, shift, want)
		}
	}
	h.consistent()
}

// A payment completes a 3-D Secure challenge, the customer's
// browser going to the issuer and back, and is then authorized over ISO 8583.
func TestAChallengeCompletes(t *testing.T) {
	h := newHarness(t)
	_, it := h.pay(map[string]any{
		"amount": 25000, "payment_method": h.saveCard("4000000000003220"), "request_three_d_secure": "any",
		"return_url": "https://loja.example/pedido/42",
	})
	next := obj(obj(it, "next_action"), "redirect_to_url")
	if str(it, "status") != "requires_action" || str(next, "url") == "" {
		t.Fatalf("%v", it)
	}
	final := h.browseChallenge(str(next, "url"), threedssim.ChallengeCode)
	if !strings.HasPrefix(final, "https://loja.example/pedido/42?payment_intent="+str(it, "id")) {
		t.Fatalf("the customer was sent to %q", final)
	}
	intent := h.waitFor(str(it, "id"), "succeeded")
	if str(intent, "status") != "succeeded" {
		t.Fatalf("after the challenge: %v", intent)
	}
	h.consistent()
}

func TestAFailedChallenge(t *testing.T) {
	h := newHarness(t)
	_, it := h.pay(map[string]any{"amount": 25000, "payment_method": h.saveCard("4000000000003220"), "request_three_d_secure": "any"})
	h.browseChallenge(str(obj(obj(it, "next_action"), "redirect_to_url"), "url"), "000000")
	intent := h.waitFor(str(it, "id"), "requires_payment_method")
	if str(obj(intent, "last_payment_error"), "code") != "payment_intent_authentication_failure" {
		t.Fatalf("after a failed challenge: %v", intent)
	}
	if holds := h.network.Holds(); len(holds) != 0 {
		t.Fatalf("a failed authentication reached the issuer: %+v", holds)
	}
}

// A directory server that does not answer does not let the payment skip 3-D Secure:
// the attempt waits for the resolver, which authenticates it once the directory server is
// back, or fails it after GiveUpAfter.
func TestAnUnreachableDirectoryServer(t *testing.T) {
	h := newHarness(t)
	h.directoryDown.Store(true)
	_, it := h.pay(map[string]any{"amount": 7000, "payment_method": h.saveCard("4242424242424242"), "request_three_d_secure": "any"})
	if str(it, "status") != "processing" || len(h.network.Holds()) != 0 {
		t.Fatalf("with the directory server down: %v", it)
	}
	h.resolve()
	if a := h.attempt(str(it, "id")); a.Status != "authenticating" {
		t.Fatalf("the attempt is %s", a.Status)
	}
	h.directoryDown.Store(false)
	for range 3 { // authenticate, authorize, capture
		h.resolve()
	}
	if intent := h.waitFor(str(it, "id"), "succeeded"); str(intent, "status") != "succeeded" {
		t.Fatalf("once the directory server was back: %v", intent)
	}

	h.directoryDown.Store(true)
	_, it = h.pay(map[string]any{"amount": 8000, "payment_method": h.saveCard("4242424242424242"), "request_three_d_secure": "any"})
	h.clock.Advance(20 * time.Minute)
	h.resolve()
	intent := h.waitFor(str(it, "id"), "requires_payment_method")
	var declineCode string
	_ = h.pool.QueryRow(t.Context(), "SELECT decline_code FROM payments.attempts WHERE id = $1", str(it, "latest_attempt")).Scan(&declineCode)
	if str(obj(intent, "last_payment_error"), "code") != "payment_intent_authentication_failure" || declineCode != "authentication_unavailable" {
		t.Fatalf("after giving up: %v (%s)", intent, declineCode)
	}
	h.consistent()
}

// browseChallenge plays the customer's browser: Jupiter's page posts the CReq to the
// ACS, the customer types the code, the ACS posts the CRes back to Jupiter, which
// redirects to the merchant. It returns where the customer ends up.
func (h *harness) browseChallenge(start, code string) string {
	h.t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	page := h.get(client, start)
	page = h.submit(client, page, nil)
	page = h.submit(client, page, url.Values{"otp": {code}})
	resp := h.submitRaw(client, page, nil)
	defer func() { _ = resp.Body.Close() }()
	return resp.Header.Get("Location")
}

var (
	formAction = regexp.MustCompile(`<form method="post" action="([^"]+)"`)
	formInput  = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)"`)
)

type page struct {
	base string
	body string
}

func (h *harness) get(client *http.Client, u string) page {
	h.t.Helper()
	req, _ := http.NewRequestWithContext(h.t.Context(), http.MethodGet, u, nil)
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s = %d %s", u, resp.StatusCode, body)
	}
	return page{base: u, body: string(body)}
}

func (h *harness) submitRaw(client *http.Client, p page, extra url.Values) *http.Response {
	h.t.Helper()
	action := formAction.FindStringSubmatch(p.body)
	if action == nil {
		h.t.Fatalf("no form on the page:\n%s", p.body)
	}
	target, err := url.Parse(p.base)
	if err != nil {
		h.t.Fatal(err)
	}
	if target, err = target.Parse(html.UnescapeString(action[1])); err != nil {
		h.t.Fatal(err)
	}
	form := url.Values{}
	for _, m := range formInput.FindAllStringSubmatch(p.body, -1) {
		form.Set(m[1], html.UnescapeString(m[2]))
	}
	for k, v := range extra {
		form[k] = v
	}
	req, _ := http.NewRequestWithContext(h.t.Context(), http.MethodPost, target.String(), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func (h *harness) submit(client *http.Client, p page, extra url.Values) page {
	h.t.Helper()
	resp := h.submitRaw(client, p, extra)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("POST = %d %s", resp.StatusCode, body)
	}
	return page{base: resp.Request.URL.String(), body: string(body)}
}

// waitFor reads the intent until it reaches status: a payment resumes on its own after
// the directory server reports the challenge.
func (h *harness) waitFor(intentID, status string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(h.t.Context(), http.MethodGet, h.api.URL+"/v1/payment_intents/"+intentID, nil)
		req.Header.Set("Authorization", "Bearer "+h.liveKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			h.t.Fatal(err)
		}
		var it map[string]any
		_ = decodeJSON(resp.Body, &it)
		_ = resp.Body.Close()
		if str(it, "status") == status || time.Now().After(deadline) {
			return it
		}
		time.Sleep(20 * time.Millisecond)
	}
}
