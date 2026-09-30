//go:build integration

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/payments"
)

func TestRiskRulesListsAndTheDecisionLog(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa, "confirm": true, "customer_ip": "10.0.0.1"}, http.StatusOK)
	if it.RiskDecision == nil || it.RiskDecision.Action != "allow" {
		t.Fatalf("risk_decision = %+v", it.RiskDecision)
	}
	var d openapi.RiskDecision
	h.expect(call{method: "GET", path: "/v1/risk/decisions/" + it.RiskDecision.Id}, http.StatusOK).decode(t, &d)
	if d.PaymentIntent != it.Id || d.Features["ip"] != "10.0.0.1" || d.Features["amount"] != float64(1000) {
		t.Fatalf("decision = %+v", d)
	}
	conforms(t, "RiskDecision", h.expect(call{method: "GET", path: "/v1/risk/decisions/" + d.Id}, http.StatusOK).body)

	var rules openapi.RiskRulePage
	h.expect(call{method: "GET", path: "/v1/risk/rules"}, http.StatusOK).decode(t, &rules)
	if len(rules.Data) == 0 || !rules.Data[0].Platform {
		t.Fatalf("rules = %+v", rules)
	}
	var rule openapi.RiskRule
	h.expect(call{method: "POST", path: "/v1/risk/rules", body: map[string]any{
		"action": "request_3ds", "expression": "amount >= 50000", "description": "Authenticate large payments",
	}}, http.StatusOK).decode(t, &rule)
	bad := h.do(call{method: "POST", path: "/v1/risk/rules", body: map[string]any{"action": "block", "expression": "amount >"}})
	if bad.status != http.StatusBadRequest || !strings.Contains(string(bad.body), "does not compile") {
		t.Fatalf("a rule that does not compile: %d %s", bad.status, bad.body)
	}

	large := h.createIntent(map[string]any{"amount": 60000, "payment_method": payments.TestCardVisa, "confirm": true}, http.StatusOK)
	wantStatus(t, large, "requires_action")
	if large.RiskDecision.Action != "request_3ds" || large.NextAction == nil || large.NextAction.Type != "use_test_authentication" {
		t.Fatalf("a payment the merchant's rule sends to authentication: %+v %+v", large.RiskDecision, large.NextAction)
	}
	wantStatus(t, h.post("/v1/test_helpers/payment_intents/"+large.Id+"/complete_action", map[string]any{"outcome": "succeeded"}, http.StatusOK), "succeeded")
	h.expect(call{method: "DELETE", path: "/v1/risk/rules/" + rule.Id}, http.StatusOK)
	h.expect(call{method: "DELETE", path: "/v1/risk/rules/" + rule.Id}, http.StatusNotFound)

	pm := h.createMethod(cardBody("4242424242424242"), nil)
	var item openapi.RiskListItem
	h.expect(call{method: "POST", path: "/v1/risk/list_items", body: map[string]any{
		"list": "block", "kind": "card_fingerprint", "value": pm.Card.Fingerprint,
	}}, http.StatusOK).decode(t, &item)
	blocked := h.createIntent(map[string]any{"amount": 1000, "payment_method": pm.Id, "confirm": true}, http.StatusPaymentRequired)
	if blocked.RiskDecision == nil || blocked.RiskDecision.Action != "block" || *blocked.LastPaymentError.DeclineCode != "blocked_by_risk" {
		t.Fatalf("a blocked card: %+v %+v", blocked.RiskDecision, blocked.LastPaymentError)
	}
	var log openapi.RiskDecisionList
	h.expect(call{method: "GET", path: "/v1/risk/decisions?action=block"}, http.StatusOK).decode(t, &log)
	if len(log.Data) != 1 || !strings.Contains(log.Data[0].Rules[0].Description, "block list") {
		t.Fatalf("the block in the log: %+v", log.Data)
	}
	var items openapi.RiskListItemPage
	h.expect(call{method: "GET", path: "/v1/risk/list_items"}, http.StatusOK).decode(t, &items)
	if len(items.Data) != 1 {
		t.Fatalf("list items = %+v", items)
	}
	h.expect(call{method: "DELETE", path: "/v1/risk/list_items/" + item.Id}, http.StatusOK)
	wantStatus(t, h.createIntent(map[string]any{"amount": 1000, "payment_method": pm.Id, "confirm": true}, http.StatusOK), "succeeded")

	var key openapi.ApiKey
	h.expect(call{method: "POST", path: "/v1/api_keys", body: map[string]any{"scopes": []string{"payment_intents:read"}}}, http.StatusOK).decode(t, &key)
	h.expect(call{method: "GET", path: "/v1/risk/decisions", key: *key.Value}, http.StatusForbidden)
	h.consistent()
}

func TestRequestingThreeDSecureInTestMode(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa, "confirm": true, "request_three_d_secure": "any"}, http.StatusOK)
	wantStatus(t, it, "requires_action")
	if it.RequestThreeDSecure != "any" {
		t.Fatalf("request_three_d_secure = %s", it.RequestThreeDSecure)
	}
	failed := h.post("/v1/test_helpers/payment_intents/"+it.Id+"/complete_action", map[string]any{"outcome": "failed"}, http.StatusPaymentRequired)
	wantStatus(t, failed, "requires_payment_method")
	h.expect(call{method: "POST", path: "/v1/payment_intents", body: map[string]any{
		"amount": 1000, "currency": "brl", "request_three_d_secure": "always",
	}}, http.StatusBadRequest)
}
