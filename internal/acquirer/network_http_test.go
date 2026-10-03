package acquirer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

// The network's clearing file decides which captures are cleared: it is taken only with
// the network's signature, and every call carries Jupiter's token.
func TestTheClearingFileIsTakenOnlySigned(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	file := []byte("HEADER\nRECORD\n")
	secret := ""
	var authorized []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorized = append(authorized, r.Header.Get("Authorization"))
		if secret != "" {
			w.Header().Set(cardnet.EventSignatureHeader, cardnet.SignEvent(file, secret, now))
		}
		_, _ = w.Write(file)
	}))
	defer srv.Close()
	c := &Connector{cfg: Config{
		NetworkURL: srv.URL, HTTPClient: srv.Client(), AcquirerID: "1234", NetworkToken: "tok",
		ClearingSecret: "clearing", Now: func() time.Time { return now },
	}}
	if _, err := c.fetchClearing(t.Context(), now); err == nil {
		t.Fatal("an unsigned clearing file was taken")
	}
	secret = "another"
	if _, err := c.fetchClearing(t.Context(), now); err == nil {
		t.Fatal("a clearing file signed with another secret was taken")
	}
	secret = "clearing"
	if got, err := c.fetchClearing(t.Context(), now); err != nil || string(got) != string(file) {
		t.Fatalf("a signed clearing file: %q, %v", got, err)
	}
	for _, a := range authorized {
		if a != "Bearer tok" {
			t.Fatalf("a call carried %q", a)
		}
	}
}

func TestATokenReferenceStaysInItsPath(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	c := &Connector{cfg: Config{NetworkURL: srv.URL, HTTPClient: srv.Client()}}
	for _, ref := range []string{"../admin/close-day", "ref?x=1", "a/b", "", strings.Repeat("r", 65)} {
		if _, err := c.cryptogram(t.Context(), ref, 100); err == nil {
			t.Errorf("reference %q was sent", ref)
		}
	}
	if called {
		t.Fatal("the network was called with a bad reference")
	}
}
