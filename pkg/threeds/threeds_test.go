package threeds_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/threeds"
)

func TestFormRoundTrip(t *testing.T) {
	in := threeds.CReq{MessageType: threeds.TypeCReq, MessageVersion: threeds.Version, ThreeDSServerTransID: "a", ACSTransID: "b", ChallengeWindowSize: "05"}
	encoded, err := threeds.EncodeForm(in)
	if err != nil || strings.ContainsAny(encoded, "+/=") {
		t.Fatalf("EncodeForm = %q, %v", encoded, err)
	}
	var out threeds.CReq
	if err := threeds.DecodeForm(encoded, &out); err != nil || out != in {
		t.Fatalf("DecodeForm = %+v, %v", out, err)
	}
	if err := threeds.DecodeForm("not base64!", &out); err == nil {
		t.Fatal("decoded garbage")
	}
}

func TestSignatures(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"messageType":"RReq"}`)
	sig := threeds.Sign(body, "secret", now)
	if err := threeds.Verify(body, sig, "secret", time.Minute, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]error{
		"another secret": threeds.Verify(body, sig, "other", time.Minute, now),
		"another body":   threeds.Verify([]byte(`{}`), sig, "secret", time.Minute, now),
		"too old":        threeds.Verify(body, sig, "secret", time.Minute, now.Add(2*time.Minute)),
		"malformed":      threeds.Verify(body, "v1=abc", "secret", time.Minute, now),
	} {
		if !errors.Is(check, threeds.ErrSignature) {
			t.Errorf("%s: %v", name, check)
		}
	}
}

func TestAnAReqPrintsTheCardMasked(t *testing.T) {
	a := threeds.AReq{AcctNumber: "4000000000003220", ThreeDSServerTransID: "x"}
	if out := fmt.Sprintf("%v %+v %#v", a, a, a); strings.Contains(out, "4000000000003220") || !strings.Contains(out, "****3220") {
		t.Fatalf("printed %s", out)
	}
}

func TestAuthenticationValues(t *testing.T) {
	key := []byte("issuer key")
	a := threeds.AuthenticationValue(key, "4242424242424242", 1000, "ds-1")
	if len(a) != 28 || a != threeds.AuthenticationValue(key, "4242424242424242", 1000, "ds-1") {
		t.Fatalf("value %q", a)
	}
	if a == threeds.AuthenticationValue(key, "4242424242424242", 1001, "ds-1") || a == threeds.AuthenticationValue([]byte("x"), "4242424242424242", 1000, "ds-1") {
		t.Fatal("the value does not depend on the amount or the key")
	}
}
