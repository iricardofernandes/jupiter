package threedssim_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	threedssim "github.com/iricardofernandes/jupiter/internal/sim/3ds"
	"github.com/iricardofernandes/jupiter/pkg/threeds"
)

func areq(pan string) threeds.AReq {
	return threeds.AReq{
		MessageType: threeds.TypeAReq, MessageVersion: threeds.Version, ThreeDSServerTransID: "tx-1",
		ThreeDSServerURL: "http://jupiter/results", NotificationURL: "http://jupiter/notify", AcctNumber: pan,
		PurchaseAmount: "1000", PurchaseCurrency: "986", PurchaseExponent: "2",
	}
}

func post(t *testing.T, srv *httptest.Server, a threeds.AReq) map[string]string {
	t.Helper()
	body, _ := json.Marshal(a)
	resp, err := http.Post(srv.URL+"/ds/areq", "application/json", bytes.NewReader(body)) //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheDirectoryServer(t *testing.T) {
	sim := threedssim.New(threedssim.Config{PublicURL: "http://acs", AuthenticationKey: []byte("k"), ResultsSecret: "s"})
	srv := httptest.NewServer(sim.Handler())
	defer srv.Close()
	for pan, want := range map[string]string{
		"4242424242424242": "Y", "5555555555554444": "Y", "4000000000003220": "C", "4000000000003238": "N",
		"4000000000003246": "R", "4000000000003253": "U", "4000000000003261": "A",
	} {
		res := post(t, srv, areq(pan))
		if res["messageType"] != threeds.TypeARes || res["transStatus"] != want {
			t.Errorf("%s: %v, want %s", pan, res, want)
		}
	}
	if res := post(t, srv, areq("5555555555554444")); res["eci"] != "02" || res["authenticationValue"] != threeds.AuthenticationValue([]byte("k"), "5555555555554444", 1000, res["dsTransID"]) {
		t.Errorf("a Mastercard authentication: %v", res)
	}
	if res := post(t, srv, areq("4000000000003220")); res["acsURL"] != "http://acs/acs/challenge" {
		t.Errorf("a challenge: %v", res)
	}
	bad := areq("4242424242424242")
	bad.PurchaseCurrency = "840"
	if res := post(t, srv, bad); res["messageType"] != threeds.TypeError {
		t.Errorf("an AReq in dollars: %v", res)
	}
	bad = areq("4242424242424242")
	bad.NotificationURL = ""
	if res := post(t, srv, bad); res["messageType"] != threeds.TypeError {
		t.Errorf("an AReq without a notification URL: %v", res)
	}
}
