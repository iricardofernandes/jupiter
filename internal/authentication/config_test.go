package authentication

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/vault"
)

type noCards struct{}

func (noCards) ReadCard(context.Context, string, string) (vault.CardData, error) {
	return vault.CardData{}, nil
}

// The directory server is sent card numbers, so it is reached over HTTPS, or over HTTP
// only on this machine; so is the URL results come back to, signed with a long secret.
func TestTheServerRefusesUnsafeSettings(t *testing.T) {
	secret := strings.Repeat("s", minResultsSecret)
	valid := Config{Pool: &pgxpool.Pool{}, Cards: noCards{}, DirectoryURL: "https://ds.example.com/areq", PublicURL: "https://api.example.com", ResultsSecret: secret}
	if _, err := New(valid); err != nil {
		t.Fatal(err)
	}
	for what, change := range map[string]func(*Config){
		"a directory over plain HTTP":  func(c *Config) { c.DirectoryURL = "http://ds.example.com/areq" },
		"a public URL over plain HTTP": func(c *Config) { c.PublicURL = "http://api.example.com" },
		"a relative directory URL":     func(c *Config) { c.DirectoryURL = "/ds/areq" },
		"a short results secret":       func(c *Config) { c.ResultsSecret = "short" },
	} {
		cfg := valid
		change(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s was accepted", what)
		}
	}
	local := valid
	local.DirectoryURL, local.PublicURL = "http://127.0.0.1:8585/ds/areq", "http://localhost:8080"
	if _, err := New(local); err != nil {
		t.Fatalf("simulators on this machine: %v", err)
	}
}

func TestPlainHTTPChallengesStayOnThisMachine(t *testing.T) {
	s := &Server{cfg: Config{AllowHTTP: true}}
	if !s.acceptableACS("http://127.0.0.1:8585/acs") || s.acceptableACS("http://acs.example.com/") || !s.acceptableACS("https://acs.example.com/") {
		t.Fatal("an ACS over plain HTTP is accepted only on this machine")
	}
	strict := &Server{}
	if strict.acceptableACS("http://127.0.0.1:8585/acs") {
		t.Fatal("plain HTTP was accepted without AllowHTTP")
	}
}
