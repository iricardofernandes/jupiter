package mtls_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
)

const apiIdentity = "spiffe://jupiter/api"

func newServer(t *testing.T, pki *mtls.PKI) *httptest.Server {
	t.Helper()
	cert, err := pki.Server("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = mtls.ServerConfig(cert.TLS, pki.Pool(), apiIdentity)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func get(srv *httptest.Server, cfg *tls.Config) error {
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func TestServerAdmitsOnlyNamedClientsOfItsCA(t *testing.T) {
	pki := must(mtls.NewPKI("jupiter test"))
	other := must(mtls.NewPKI("someone else"))
	srv := newServer(t, pki)

	api := must(pki.Client(apiIdentity))
	stranger := must(pki.Client("spiffe://jupiter/worker-of-another-team"))
	forged := must(other.Client(apiIdentity))

	if err := get(srv, mtls.ClientConfig(api.TLS, pki.Pool())); err != nil {
		t.Fatalf("the API's certificate was refused: %v", err)
	}
	refused := map[string]*tls.Config{
		"no certificate":           {MinVersion: tls.VersionTLS13, RootCAs: pki.Pool()},
		"another identity":         mtls.ClientConfig(stranger.TLS, pki.Pool()),
		"the identity, another CA": mtls.ClientConfig(forged.TLS, pki.Pool()),
	}
	for name, cfg := range refused {
		t.Run(name, func(t *testing.T) {
			if err := get(srv, cfg); err == nil {
				t.Fatal("the server accepted the connection")
			}
		})
	}
}

func TestLoadFromFiles(t *testing.T) {
	pki := must(mtls.NewPKI("jupiter test"))
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	server := must(pki.Server("127.0.0.1"))
	client := must(pki.Client(apiIdentity))
	ca := write("ca.pem", pki.CAPEM())

	serverCfg, err := mtls.LoadServerConfig(write("s.pem", server.CertPEM), write("s-key.pem", server.KeyPEM), ca, apiIdentity)
	if err != nil {
		t.Fatal(err)
	}
	clientCfg, err := mtls.LoadClientConfig(write("c.pem", client.CertPEM), write("c-key.pem", client.KeyPEM), ca)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = serverCfg
	srv.StartTLS()
	defer srv.Close()
	if err := get(srv, clientCfg); err != nil {
		t.Fatal(err)
	}

	if _, err := mtls.LoadClientConfig(ca, ca, ca); err == nil {
		t.Fatal("a CA certificate loaded as a key pair")
	}
	if _, err := mtls.LoadClientConfig(write("c2.pem", client.CertPEM), write("c2-key.pem", client.KeyPEM), write("empty.pem", nil)); err == nil {
		t.Fatal("an empty CA file loaded")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
