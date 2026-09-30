package api

import (
	"bytes"
	"testing"
)

func TestFingerprintKeepsLargeIntegersApart(t *testing.T) {
	a, err := fingerprint("op", "", []byte(`{"amount": 9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := fingerprint("op", "", []byte(`{"amount": 9007199254740992}`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("amounts differing above 2^53 fingerprint alike")
	}
}

func TestFingerprintIgnoresKeyOrderAndWhitespace(t *testing.T) {
	a, _ := fingerprint("op", "we_1", []byte(`{"a":1,"b":[1,2]}`))
	b, _ := fingerprint("op", "we_1", []byte("{ \"b\": [1, 2],\n \"a\": 1 }"))
	c, _ := fingerprint("op", "we_2", []byte(`{"a":1,"b":[1,2]}`))
	if !bytes.Equal(a, b) || bytes.Equal(a, c) {
		t.Fatal("fingerprint depends on formatting, or ignores the path id")
	}
}

func TestDeletedObjectsAreNotDowngraded(t *testing.T) {
	raw, err := render(map[string]any{"id": "we_1", "object": "webhook_endpoint", "deleted": true}, Versions[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("disabled")) {
		t.Fatalf("render = %s", raw)
	}
}
