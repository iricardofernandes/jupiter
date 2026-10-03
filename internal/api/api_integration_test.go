//go:build integration

package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/platform/ratelimit"
	"github.com/iricardofernandes/jupiter/pkg/webhook"
)

func TestTheOpenAPIDocumentIsValid(t *testing.T) {
	loadSpec(t)
}

func TestAuthentication(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)

	resp := h.expect(call{method: "GET", path: "/v1/webhook_endpoints", key: "-"}, http.StatusUnauthorized)
	conforms(t, "ErrorResponse", resp.body)

	wrong := "sk_test_" + strings.Repeat("Z", 43)
	resp = h.expect(call{method: "GET", path: "/v1/webhook_endpoints", key: wrong}, http.StatusUnauthorized)
	var e openapi.ErrorResponse
	resp.decode(t, &e)
	if strings.Contains(string(resp.body), wrong) || e.Error.Type != openapi.AuthenticationError {
		t.Fatalf("error = %s; the key must be redacted", resp.body)
	}
	if e.Error.RequestId != resp.header.Get("Request-Id") || !strings.HasPrefix(e.Error.RequestId, "req_") {
		t.Fatalf("request_id %q, header %q", e.Error.RequestId, resp.header.Get("Request-Id"))
	}
	if !strings.HasSuffix(e.Error.DocUrl, "#api_key_invalid") {
		t.Fatalf("doc_url = %s", e.Error.DocUrl)
	}

	resp = h.expect(call{method: "GET", path: "/v1/webhook_endpoints", key: h.keys["pk_test_"]}, http.StatusForbidden)
	resp.decode(t, &e)
	if e.Error.Type != openapi.PermissionError {
		t.Fatalf("publishable key error = %s", resp.body)
	}
}

func (h *harness) createEndpoint(url string) openapi.WebhookEndpoint {
	h.t.Helper()
	resp := h.expect(call{
		method: "POST", path: "/v1/webhook_endpoints",
		body: map[string]any{"url": url, "enabled_events": []string{"*"}, "description": "orders"},
	}, http.StatusOK)
	var e openapi.WebhookEndpoint
	resp.decode(h.t, &e)
	return e
}

func TestWebhookEndpointLifecycle(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)

	created := h.createEndpoint("https://example.com/hooks")
	if created.Secret == nil || !strings.HasPrefix(*created.Secret, "whsec_") || created.Status != "enabled" || created.Livemode {
		t.Fatalf("created = %+v", created)
	}

	resp := h.expect(call{method: "GET", path: "/v1/webhook_endpoints/" + created.Id}, http.StatusOK)
	conforms(t, "WebhookEndpoint", resp.body)
	if strings.Contains(string(resp.body), "whsec_") {
		t.Fatal("the secret is returned after creation")
	}

	resp = h.expect(call{method: "POST", path: "/v1/webhook_endpoints/" + created.Id, body: map[string]any{"status": "disabled"}}, http.StatusOK)
	var updated openapi.WebhookEndpoint
	resp.decode(t, &updated)
	if updated.Status != "disabled" || updated.Url != created.Url {
		t.Fatalf("updated = %+v", updated)
	}

	// Live keys see live data only.
	h.expect(call{method: "GET", path: "/v1/webhook_endpoints/" + created.Id, key: h.keys["sk_live_"]}, http.StatusNotFound)

	resp = h.expect(call{method: "DELETE", path: "/v1/webhook_endpoints/" + created.Id}, http.StatusOK)
	conforms(t, "DeletedObject", resp.body)
	h.expect(call{method: "GET", path: "/v1/webhook_endpoints/" + created.Id}, http.StatusNotFound)
	h.expect(call{method: "DELETE", path: "/v1/webhook_endpoints/" + created.Id}, http.StatusNotFound)
}

func TestInvalidRequestsNameTheirParameter(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	tests := []struct {
		body      any
		code      string
		param     string
		wantParam bool
	}{
		{map[string]any{"url": "https://example.com", "enabled_events": []string{"*"}, "colour": "blue"}, "parameter_unknown", "colour", true},
		{map[string]any{"url": 42, "enabled_events": []string{"*"}}, "parameter_invalid", "url", true},
		{map[string]any{"url": "https://example.com", "enabled_events": []string{"charge.dispute"}}, "parameter_invalid", "", false},
		{"not json", "body_invalid", "", false},
		{nil, "body_missing", "", false},
	}
	for _, tt := range tests {
		resp := h.expect(call{method: "POST", path: "/v1/webhook_endpoints", body: tt.body}, http.StatusBadRequest)
		conforms(t, "ErrorResponse", resp.body)
		var e openapi.ErrorResponse
		resp.decode(t, &e)
		if e.Error.Code != tt.code || (tt.wantParam && (e.Error.Param == nil || *e.Error.Param != tt.param)) {
			t.Errorf("body %v: error = %s, want code %s param %q", tt.body, resp.body, tt.code, tt.param)
		}
	}
	h.expect(call{method: "GET", path: "/v1/nothing_here"}, http.StatusNotFound)
	h.expect(call{method: "GET", path: "/v1/webhook_endpoints?limit=500"}, http.StatusBadRequest)
}

// The same idempotent request sent concurrently has exactly one
// effect, and every caller gets the identical response.
func TestConcurrentIdempotentRequestsHaveOneEffectAndOneResponse(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	const callers = 20
	body := map[string]any{"url": "https://example.com/hooks", "enabled_events": []string{"*"}}
	headers := map[string]string{"Idempotency-Key": "create-endpoint-1"}

	responses := make([]response, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			<-start
			responses[i], errs[i] = h.send(t.Context(), call{method: "POST", path: "/v1/webhook_endpoints", body: body, headers: headers})
		})
	}
	close(start)
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if responses[i].status != http.StatusOK || string(responses[i].body) != string(responses[0].body) {
			t.Fatalf("caller %d got %d %s, caller 0 got %d %s", i, responses[i].status, responses[i].body, responses[0].status, responses[0].body)
		}
	}
	var endpoints, created int
	if err := h.pool.QueryRow(t.Context(), "SELECT count(*) FROM events.endpoints").Scan(&endpoints); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(t.Context(), "SELECT count(*) FROM events.events WHERE type = 'webhook_endpoint.created'").Scan(&created); err != nil {
		t.Fatal(err)
	}
	if endpoints != 1 || created != 1 {
		t.Fatalf("%d endpoints and %d created events, want exactly one of each", endpoints, created)
	}
}

func TestReusingAKeyForADifferentRequestIsRefused(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	headers := map[string]string{"Idempotency-Key": "k1"}
	first := h.expect(call{
		method: "POST", path: "/v1/webhook_endpoints", headers: headers,
		body: `{"url": "https://example.com/a", "enabled_events": ["*"]}`,
	}, http.StatusOK)
	// Key order and whitespace do not make a different request.
	replay := h.expect(call{
		method: "POST", path: "/v1/webhook_endpoints", headers: headers,
		body: `{"enabled_events":["*"],"url":"https://example.com/a"}`,
	}, http.StatusOK)
	if string(first.body) != string(replay.body) {
		t.Fatalf("replay = %s, want %s", replay.body, first.body)
	}
	resp := h.expect(call{
		method: "POST", path: "/v1/webhook_endpoints", headers: headers,
		body: map[string]any{"url": "https://example.com/b", "enabled_events": []string{"*"}},
	}, http.StatusUnprocessableEntity)
	var e openapi.ErrorResponse
	resp.decode(t, &e)
	if e.Error.Code != "idempotency_key_mismatch" {
		t.Fatalf("error = %s", resp.body)
	}
	// Keys are per mode: the same key in live mode is a new request.
	h.expect(call{
		method: "POST", path: "/v1/webhook_endpoints", headers: headers, key: h.keys["sk_live_"],
		body: map[string]any{"url": "https://example.com/b", "enabled_events": []string{"*"}},
	}, http.StatusOK)
}

func TestAClientErrorIsStoredAndReplayed(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	headers := map[string]string{"Idempotency-Key": "bad"}
	body := map[string]any{"url": "ftp://example.com", "enabled_events": []string{"*"}}
	first := h.expect(call{method: "POST", path: "/v1/webhook_endpoints", headers: headers, body: body}, http.StatusBadRequest)
	again := h.expect(call{method: "POST", path: "/v1/webhook_endpoints", headers: headers, body: body}, http.StatusBadRequest)
	if string(first.body) != string(again.body) {
		t.Fatalf("replayed error differs:\n%s\n%s", first.body, again.body)
	}
}

// A response under an older pinned version keeps its old shape.
func TestAnOlderPinnedVersionKeepsItsShape(t *testing.T) {
	h := newHarness(t, "2026-09-01")
	created := h.do(call{
		method: "POST", path: "/v1/webhook_endpoints",
		body: map[string]any{"url": "https://example.com/hooks", "enabled_events": []string{"*"}},
	})
	var old map[string]any
	created.decode(t, &old)
	if _, has := old["status"]; has || old["disabled"] != false {
		t.Fatalf("pinned to 2026-09-01, created = %s", created.body)
	}
	if created.header.Get("Jupiter-Version") != "2026-09-01" {
		t.Fatalf("Jupiter-Version = %q", created.header.Get("Jupiter-Version"))
	}
	id, ok := old["id"].(string)
	if !ok {
		t.Fatalf("created = %s", created.body)
	}

	list := h.expect(call{method: "GET", path: "/v1/webhook_endpoints"}, http.StatusOK)
	if !strings.Contains(string(list.body), `"disabled":false`) || strings.Contains(string(list.body), `"status"`) {
		t.Fatalf("list under 2026-09-01 = %s", list.body)
	}

	current := h.expect(call{
		method: "GET", path: "/v1/webhook_endpoints/" + id,
		headers: map[string]string{"Jupiter-Version": api.CurrentVersion},
	}, http.StatusOK)
	conforms(t, "WebhookEndpoint", current.body)

	h.expect(call{
		method: "GET", path: "/v1/webhook_endpoints/" + id,
		headers: map[string]string{"Jupiter-Version": "2019-01-01"},
	}, http.StatusBadRequest)
}

func TestPaginationAndInclude(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	var ids []string
	for i := range 3 {
		ids = append(ids, h.createEndpoint(fmt.Sprintf("https://example.com/%d", i)).Id)
	}

	first := h.expect(call{method: "GET", path: "/v1/webhook_endpoints?limit=2"}, http.StatusOK)
	conforms(t, "WebhookEndpointList", first.body)
	var page openapi.WebhookEndpointList
	first.decode(t, &page)
	if len(page.Data) != 2 || !page.HasMore || page.Data[0].Id != ids[2] {
		t.Fatalf("first page = %s", first.body)
	}
	second := h.expect(call{method: "GET", path: "/v1/webhook_endpoints?limit=2&starting_after=" + page.Data[1].Id}, http.StatusOK)
	second.decode(t, &page)
	if len(page.Data) != 1 || page.HasMore || page.Data[0].Id != ids[0] {
		t.Fatalf("second page = %s", second.body)
	}

	events := h.expect(call{method: "GET", path: "/v1/events?type=webhook_endpoint.created&limit=1"}, http.StatusOK)
	conforms(t, "EventList", events.body)
	var list openapi.EventList
	events.decode(t, &list)
	if len(list.Data) != 1 || list.Data[0].RelatedObject.Object != nil {
		t.Fatalf("events = %s", events.body)
	}
	eventID := list.Data[0].Id
	with := h.expect(call{method: "GET", path: "/v1/events/" + eventID + "?include[]=related_object"}, http.StatusOK)
	conforms(t, "Event", with.body)
	var ev openapi.Event
	with.decode(t, &ev)
	if ev.RelatedObject.Object == nil || (*ev.RelatedObject.Object)["id"] != ids[2] || ev.RelatedObject.Url != "/v1/webhook_endpoints/"+ids[2] {
		t.Fatalf("included event = %s", with.body)
	}

	h.expect(call{method: "GET", path: "/v1/events/" + eventID + "?include[]=everything"}, http.StatusBadRequest)
	h.expect(call{method: "DELETE", path: "/v1/webhook_endpoints/" + ids[2]}, http.StatusOK)
	gone := h.expect(call{method: "GET", path: "/v1/events/" + eventID + "?include[]=related_object"}, http.StatusOK)
	var afterDelete openapi.Event
	gone.decode(t, &afterDelete)
	if afterDelete.RelatedObject.Object != nil {
		t.Fatalf("a deleted object was included: %s", gone.body)
	}
}

func TestRestrictedKeys(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	resp := h.expect(call{method: "POST", path: "/v1/api_keys", body: map[string]any{"name": "reporting", "scopes": []string{"events:read"}}}, http.StatusOK)
	conforms(t, "ApiKey", resp.body)
	var k openapi.ApiKey
	resp.decode(t, &k)
	if k.Value == nil || !strings.HasPrefix(*k.Value, "rk_test_") {
		t.Fatalf("created key = %s", resp.body)
	}

	h.expect(call{method: "GET", path: "/v1/events", key: *k.Value}, http.StatusOK)
	h.expect(call{method: "GET", path: "/v1/webhook_endpoints", key: *k.Value}, http.StatusForbidden)
	h.expect(call{method: "POST", path: "/v1/api_keys", key: *k.Value, body: map[string]any{"scopes": []string{"events:read"}}}, http.StatusForbidden)

	revoked := h.expect(call{method: "POST", path: "/v1/api_keys/" + k.Id + "/revoke"}, http.StatusOK)
	conforms(t, "ApiKey", revoked.body)
	h.expect(call{method: "GET", path: "/v1/events", key: *k.Value}, http.StatusUnauthorized)

	listed := h.expect(call{method: "GET", path: "/v1/api_keys"}, http.StatusOK)
	conforms(t, "ApiKeyList", listed.body)
	if strings.Contains(string(listed.body), `"value"`) {
		t.Fatal("a listed key includes its value")
	}
}

// A merchant's integration, end to end: create an endpoint through the API, cause an
// event, and receive a signed webhook whose body is the API's event object.
func TestWebhooksThroughTheAPI(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	type delivery struct {
		body   []byte
		header string
	}
	received := make(chan delivery, 10)
	receiver := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- delivery{body: body, header: r.Header.Get(webhook.SignatureHeader)}
	}))
	defer receiver.Close()
	h.startWorker()

	endpoint := h.createEndpoint(receiver.URL)
	var d delivery
	select {
	case d = <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("no webhook arrived")
	}
	if err := webhook.Verify(d.body, d.header, *endpoint.Secret, webhook.DefaultTolerance, time.Now()); err != nil {
		t.Fatalf("webhook signature: %v", err)
	}
	conforms(t, "Event", d.body)
	var ev openapi.Event
	if err := json.Unmarshal(d.body, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != "webhook_endpoint.created" || ev.RelatedObject.Id != endpoint.Id {
		t.Fatalf("webhook = %s", d.body)
	}
	fetched := h.expect(call{method: "GET", path: "/v1/events/" + ev.Id}, http.StatusOK)
	if strings.TrimSpace(string(fetched.body)) != strings.TrimSpace(string(d.body)) {
		t.Fatalf("the webhook body and the API's event differ:\n%s\n%s", d.body, fetched.body)
	}
}

func TestConcurrentRevocationsPublishOneEvent(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	resp := h.expect(call{method: "POST", path: "/v1/api_keys", body: map[string]any{"scopes": []string{"events:read"}}}, http.StatusOK)
	var k openapi.ApiKey
	resp.decode(t, &k)

	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() {
			h.do(call{method: "POST", path: "/v1/api_keys/" + k.Id + "/revoke", headers: map[string]string{"Idempotency-Key": fmt.Sprint("revoke-", i)}})
		})
	}
	wg.Wait()
	var revoked int
	if err := h.pool.QueryRow(t.Context(), "SELECT count(*) FROM events.events WHERE type = 'api_key.revoked'").Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Fatalf("%d api_key.revoked events, want 1", revoked)
	}
}

func TestConcurrentPartialUpdatesKeepEachField(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	e := h.createEndpoint("https://example.com/a")
	for round := range 10 {
		var wg sync.WaitGroup
		description := fmt.Sprint("round ", round)
		url := fmt.Sprint("https://example.com/", round)
		wg.Go(func() {
			h.do(call{method: "POST", path: "/v1/webhook_endpoints/" + e.Id, body: map[string]any{"description": description}})
		})
		wg.Go(func() {
			h.do(call{method: "POST", path: "/v1/webhook_endpoints/" + e.Id, body: map[string]any{"url": url}})
		})
		wg.Wait()
		var got openapi.WebhookEndpoint
		h.expect(call{method: "GET", path: "/v1/webhook_endpoints/" + e.Id}, http.StatusOK).decode(t, &got)
		if got.Description != description || got.Url != url {
			t.Fatalf("round %d: endpoint = %+v; an update was lost", round, got)
		}
	}
}

func TestDeleteUnderAnOlderVersionIsNotRewritten(t *testing.T) {
	h := newHarness(t, "2026-09-01")
	var e map[string]any
	h.expect(call{
		method: "POST", path: "/v1/webhook_endpoints",
		body: map[string]any{"url": "https://example.com", "enabled_events": []string{"*"}},
	}, http.StatusOK).decode(t, &e)
	deleted := h.expect(call{method: "DELETE", path: fmt.Sprint("/v1/webhook_endpoints/", e["id"])}, http.StatusOK)
	if strings.Contains(string(deleted.body), "disabled") {
		t.Fatalf("delete response = %s", deleted.body)
	}
}

// A key may make so many requests a second, and an address presenting keys that do not
// exist is refused before each costs a lookup; both are told when to come back.
func TestRateLimits(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	limited := api.New(api.Deps{
		Pool: h.pool, Merchants: h.merchants, Events: h.events, Payments: h.payments, Vault: h.vault, Risk: h.risk, Now: h.clock.Now,
		Limits: api.Limits{Test: ratelimit.Rate{PerSecond: 2, Burst: 3}, Unknown: ratelimit.Rate{PerSecond: 1, Burst: 2}},
	})
	h.server = httptest.NewServer(limited.Handler())
	t.Cleanup(h.server.Close)

	list := call{method: "GET", path: "/v1/payment_intents"}
	for range 3 {
		h.expect(list, http.StatusOK)
	}
	resp := h.expect(list, http.StatusTooManyRequests)
	conforms(t, "ErrorResponse", resp.body)
	var e openapi.ErrorResponse
	resp.decode(t, &e)
	if e.Error.Type != openapi.RateLimitError || resp.header.Get("Retry-After") != "1" {
		t.Fatalf("over the limit: %s, Retry-After %q", resp.body, resp.header.Get("Retry-After"))
	}
	h.clock.Advance(time.Second)
	h.expect(list, http.StatusOK)

	wrong := call{method: "GET", path: "/v1/payment_intents", key: "sk_test_" + strings.Repeat("Z", 43)}
	h.expect(wrong, http.StatusUnauthorized)
	h.expect(wrong, http.StatusUnauthorized)
	h.expect(wrong, http.StatusTooManyRequests)
	// A key that authenticated lately still gets through from that address: a neighbour
	// presenting bad keys cannot lock it out.
	h.expect(list, http.StatusOK)
	// One never seen waits, from that address, until the unknown ones have.
	h.newMerchant(api.CurrentVersion)
	h.expect(list, http.StatusTooManyRequests)
	h.clock.Advance(time.Second)
	h.expect(list, http.StatusOK)
}

// A response carrying a secret is replayed only to the key that asked for it: another
// key of the account repeating the Idempotency-Key and the body gets a mismatch, not the
// new key. And what a keyed request stored of its body is sealed.
func TestASecretIsReplayedOnlyToItsKey(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	resp := h.expect(call{method: "POST", path: "/v1/api_keys", body: map[string]any{"name": "keys", "scopes": []string{"api_keys:write"}}}, http.StatusOK)
	var restricted openapi.ApiKey
	resp.decode(t, &restricted)

	body := map[string]any{"name": "everything", "scopes": []string{"events:read"}}
	first := h.expect(call{method: "POST", path: "/v1/api_keys", body: body, headers: map[string]string{"Idempotency-Key": "secret-1"}}, http.StatusOK)
	again := h.expect(call{method: "POST", path: "/v1/api_keys", body: body, headers: map[string]string{"Idempotency-Key": "secret-1"}}, http.StatusOK)
	if string(first.body) != string(again.body) {
		t.Fatal("the same key's retry got another answer")
	}
	other := h.expect(call{method: "POST", path: "/v1/api_keys", key: *restricted.Value, body: body, headers: map[string]string{"Idempotency-Key": "secret-1"}},
		http.StatusUnprocessableEntity)
	if strings.Contains(string(other.body), "sk_test_") || strings.Contains(string(other.body), "rk_test_") {
		t.Fatalf("another key was handed the secret: %s", other.body)
	}

	var stored []byte
	if err := h.pool.QueryRow(t.Context(), `SELECT request_body FROM api.idempotency_keys WHERE key = 'secret-1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "everything") {
		t.Fatal("the request body is stored in the clear")
	}
}

// Answers are never cached and never read as anything but JSON, and an amount is bounded.
func TestAnswersAreNotCachedAndAmountsAreBounded(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	resp := h.expect(call{method: "GET", path: "/v1/payment_intents"}, http.StatusOK)
	if resp.header.Get("Cache-Control") != "no-store" || resp.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers: %v", resp.header)
	}
	unauthorized := h.expect(call{method: "GET", path: "/v1/payment_intents", key: "-"}, http.StatusUnauthorized)
	if unauthorized.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("an error's headers: %v", unauthorized.header)
	}
	over := h.expect(call{method: "POST", path: "/v1/payment_intents", body: map[string]any{"amount": 10_000_000_01, "currency": "brl"}}, http.StatusBadRequest)
	var e openapi.ErrorResponse
	over.decode(t, &e)
	if e.Error.Code != "amount_too_large" {
		t.Fatalf("an amount past the bound: %s", over.body)
	}
	h.expect(call{method: "POST", path: "/v1/payment_intents", body: map[string]any{"amount": 10_000_000_00, "currency": "brl"}}, http.StatusOK)
}
