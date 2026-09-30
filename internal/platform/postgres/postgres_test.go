package postgres_test

import (
	"strings"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

func TestConnectRejectsAnInvalidURLWithoutLeakingIt(t *testing.T) {
	_, err := postgres.Connect(t.Context(), "postgres://jupiter:secret@host:notaport/db")
	if err == nil {
		t.Fatal("Connect accepted an invalid URL")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaks the password: %v", err)
	}
}
