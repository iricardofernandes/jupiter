package registry_test

import (
	"testing"

	"github.com/iricardofernandes/jupiter/internal/registry"
)

func TestTheURLKeepsTheTokenSafe(t *testing.T) {
	for url, ok := range map[string]bool{
		"https://registry.example": true, "http://127.0.0.1:8588": true, "http://localhost:8588": true,
		"http://registry.example": false, "ftp://127.0.0.1": false,
	} {
		if _, err := registry.New(registry.Config{BaseURL: url, Token: "t"}); (err == nil) != ok {
			t.Errorf("%s: %v", url, err)
		}
	}
	env := map[string]string{"JUPITER_REGISTRY_URL": "http://127.0.0.1:8588", "JUPITER_REGISTRY_TOKEN": "a", "JUPITER_REGISTRY_TEST_URL": "http://127.0.0.1:8588", "JUPITER_REGISTRY_TEST_TOKEN": "b"}
	if _, _, err := registry.Registries(func(k string) string { return env[k] }); err == nil {
		t.Error("both modes on one registry")
	}
	env["JUPITER_REGISTRY_TEST_URL"] = "http://127.0.0.1:8589"
	if live, test, err := registry.Registries(func(k string) string { return env[k] }); err != nil || live == nil || test == nil {
		t.Errorf("two registries: %v", err)
	}
}
