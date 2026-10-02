package main

import (
	"strings"
	"testing"
)

func TestThePublicRouteNeedsTLSAwayFromLoopback(t *testing.T) {
	env := func(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }
	for _, ok := range []map[string]string{
		{},
		{"JUPITER_VAULT_PUBLIC_ADDR": "127.0.0.1:9000"},
		{"JUPITER_VAULT_PUBLIC_ADDR": "[::1]:9000"},
		{"JUPITER_VAULT_PUBLIC_ADDR": "localhost:9000"},
		{"JUPITER_VAULT_PUBLIC_ADDR": ":8083", "JUPITER_VAULT_PUBLIC_BEHIND_EDGE": "true"},
	} {
		if p, err := publicListener(env(ok)); err != nil || p.tls != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, refused := range []map[string]string{
		{"JUPITER_VAULT_PUBLIC_ADDR": ":8083"},
		{"JUPITER_VAULT_PUBLIC_ADDR": "0.0.0.0:8083"},
		{"JUPITER_VAULT_PUBLIC_ADDR": "10.0.0.5:8083", "JUPITER_VAULT_PUBLIC_BEHIND_EDGE": "yes"},
		{"JUPITER_VAULT_PUBLIC_TLS_CERT": "/nonexistent.pem", "JUPITER_VAULT_PUBLIC_TLS_KEY": "/nonexistent.key"},
	} {
		if _, err := publicListener(env(refused)); err == nil {
			t.Errorf("%v was accepted", refused)
		}
	}
	if _, err := publicLimits(env(map[string]string{"JUPITER_VAULT_PUBLIC_RATE": "0"})); err == nil || !strings.Contains(err.Error(), "RATE") {
		t.Errorf("a zero rate: %v", err)
	}
	if cfg, err := publicLimits(env(map[string]string{"JUPITER_VAULT_PUBLIC_RATE": "5", "JUPITER_VAULT_PUBLIC_BURST": "50", "JUPITER_TRUSTED_PROXIES": "10.0.0.0/8"})); err != nil ||
		cfg.PublicRate != 5 || cfg.PublicBurst != 50 || len(cfg.Clients.Trusted) != 1 {
		t.Errorf("limits: %+v, %v", cfg, err)
	}
}
