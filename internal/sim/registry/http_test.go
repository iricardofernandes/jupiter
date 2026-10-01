package registry_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/sim/registry"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

const (
	accreditor = "11222333000181"
	financier  = "33000167000101"
	holder     = "11444777000161"
)

type client struct {
	t     *testing.T
	url   string
	token string
}

func (c client) do(method, path string, body, out any) int {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequestWithContext(c.t.Context(), method, c.url+path, &buf)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestTheAPI(t *testing.T) {
	now := time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC) // Monday
	sim := registry.New(registry.Config{Now: func() time.Time { return now }, Participants: []registry.Participant{
		{Token: "acc", TaxID: accreditor, Role: registry.Accreditor},
		{Token: "fin", TaxID: financier, Role: registry.Financier},
	}})
	srv := httptest.NewServer(sim.Handler())
	defer srv.Close()
	acc, fin, nobody := client{t, srv.URL, "acc"}, client{t, srv.URL, "fin"}, client{t, srv.URL, "?"}

	if status := nobody.do(http.MethodGet, "/v1/units", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("an unknown token: %d", status)
	}
	unit := registryapi.Unit{Holder: holder, Arrangement: "VCC", SettlementDate: "2026-11-03", Value: 9651, ConstitutedOn: "2026-10-01"}
	if status := fin.do(http.MethodPut, "/v1/units", registryapi.UnitsRequest{Units: []registryapi.Unit{unit}}, nil); status != http.StatusForbidden {
		t.Fatalf("a financier registering units: %d", status)
	}
	var results registryapi.UnitsResponse
	bad := unit
	bad.SettlementDate = "soon"
	if status := acc.do(http.MethodPut, "/v1/units", registryapi.UnitsRequest{Units: []registryapi.Unit{unit, bad}}, &results); status != http.StatusOK ||
		results.Results[0].Error != "" || results.Results[1].Error == "" {
		t.Fatalf("registering: %d %+v", status, results)
	}
	// A sale on Thursday 1 October is due by Friday: Monday is late.
	var compliance registryapi.ComplianceResponse
	if acc.do(http.MethodGet, "/v1/compliance", nil, &compliance); len(compliance.Late) != 1 || compliance.Late[0].Due != "2026-10-02" {
		t.Fatalf("compliance: %+v", compliance)
	}
	if status := fin.do(http.MethodGet, "/v1/units?holder="+holder, nil, nil); status != http.StatusForbidden {
		t.Fatalf("a financier without an opt-in: %d", status)
	}
	acc.do(http.MethodPost, "/v1/opt-ins", registryapi.OptIn{Holder: holder, Financier: financier}, nil)
	var positions registryapi.PositionsResponse
	if status := fin.do(http.MethodGet, "/v1/units?holder="+holder, nil, &positions); status != http.StatusOK || len(positions.Units) != 1 {
		t.Fatalf("a financier with an opt-in: %d %+v", status, positions)
	}
	contract := registryapi.Contract{ID: "c1", Holder: holder, Effect: "ownership_transfer", Rule: "percentage", BasisPoints: 5000}
	if status := fin.do(http.MethodPost, "/v1/contracts", contract, nil); status != http.StatusCreated {
		t.Fatalf("a contract: %d", status)
	}
	var holders registryapi.HoldersResponse
	if acc.do(http.MethodGet, "/v1/holders", nil, &holders); len(holders.Holders) != 1 || holders.Holders[0] != holder {
		t.Fatalf("holders: %+v", holders)
	}
	var instructions registryapi.InstructionsResponse
	acc.do(http.MethodGet, "/v1/instructions?holder="+holder+"&arrangement=VCC&settlement_date=2026-11-03", nil, &instructions)
	if len(instructions.Payments) != 2 || instructions.Payments[0].Amount != 4825 || instructions.Payments[1].Amount != 4826 {
		t.Fatalf("instructions: %+v", instructions)
	}
	n := registryapi.Settlement{Holder: holder, Arrangement: "VCC", SettlementDate: "2026-11-03", Amount: 9651, SettledOn: "2026-11-03"}
	var settled registryapi.SettlementResponse
	if status := acc.do(http.MethodPost, "/v1/settlements", n, &settled); status != http.StatusOK || len(settled.Payments) != 2 {
		t.Fatalf("settling: %d %+v", status, settled)
	}
	if status := acc.do(http.MethodPost, "/v1/settlements", n, nil); status != http.StatusOK {
		t.Fatalf("the same notice again: %d", status)
	}
	n.Amount = 1
	if status := acc.do(http.MethodPost, "/v1/settlements", n, nil); status != http.StatusConflict {
		t.Fatalf("a different notice for a settled unit: %d", status)
	}
	if status := fin.do(http.MethodPost, "/v1/contracts/c1/end", nil, nil); status != http.StatusNoContent {
		t.Fatalf("ending the contract: %d", status)
	}
}
