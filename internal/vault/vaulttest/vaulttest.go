// Package vaulttest runs a real vault in-process for tests: its own database, a local
// KMS, and its internal interface served over mTLS to a client holding the API's
// certificate.
package vaulttest

import (
	"crypto/rand"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/ratelimit"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/internal/vault/kms"
	"github.com/iricardofernandes/jupiter/internal/vault/server"
)

// Template names the postgrestest template migrated with the vault's schema:
// postgrestest.Templates(m, &srv, map[string][]postgrestest.MigrateFunc{vaulttest.Template: {vaulttest.Migrate}}).
const Template = "vault"

var Migrate postgrestest.MigrateFunc = server.Migrate

type Vault struct {
	Service   *server.Service
	KMS       *kms.Local
	Pool      *pgxpool.Pool
	PKI       *mtls.PKI
	Client    *vault.Client
	URL       string
	PublicURL string
}

type Options struct {
	Now     func() time.Time
	CVCTTL  time.Duration
	Logger  *slog.Logger
	Clients ratelimit.Clients
}

// Start serves a vault on pool, which must hold the vault's schema, until t ends.
func Start(t testing.TB, pool *pgxpool.Pool, opts Options) *Vault {
	t.Helper()
	local, err := kms.NewLocal(map[string][]byte{"k1": Key()}, "k1", Key())
	if err != nil {
		t.Fatal(err)
	}
	svc := server.New(server.Config{Pool: pool, KMS: local, Now: opts.Now, CVCTTL: opts.CVCTTL, Logger: opts.Logger, Clients: opts.Clients})
	pki, err := mtls.NewPKI("vaulttest")
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := pki.Server("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	internal := httptest.NewUnstartedServer(svc.InternalHandler())
	internal.TLS = mtls.ServerConfig(serverCert.TLS, pki.Pool(), vault.APIIdentity, vault.WorkerIdentity)
	internal.StartTLS()
	t.Cleanup(internal.Close)
	public := httptest.NewServer(svc.PublicHandler())
	t.Cleanup(public.Close)

	return &Vault{
		Service: svc, KMS: local, Pool: pool, PKI: pki,
		Client: ClientFor(t, pki, internal.URL, vault.APIIdentity),
		URL:    internal.URL, PublicURL: public.URL,
	}
}

// ClientFor returns a client presenting a certificate for identity from pki.
func ClientFor(t testing.TB, pki *mtls.PKI, url, identity string) *vault.Client {
	t.Helper()
	cert, err := pki.Client(identity)
	if err != nil {
		t.Fatal(err)
	}
	return vault.NewClient(url, mtls.ClientConfig(cert.TLS, pki.Pool()))
}

func Key() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}
