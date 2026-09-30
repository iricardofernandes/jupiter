// Package mtls builds TLS configurations in which both sides present certificates from
// a private CA, and the server admits only the client identities it names. An identity
// is a URI SAN such as spiffe://jupiter/api.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"slices"
)

var ErrIdentity = errors.New("mtls: client identity not allowed")

// ServerConfig requires every client to present a certificate issued by clientCAs whose
// URI SAN is one of allowed.
func ServerConfig(cert tls.Certificate, clientCAs *x509.CertPool, allowed ...string) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return ErrIdentity
			}
			for _, uri := range cs.PeerCertificates[0].URIs {
				if slices.Contains(allowed, uri.String()) {
					return nil
				}
			}
			return ErrIdentity
		},
	}
}

func ClientConfig(cert tls.Certificate, rootCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      rootCAs,
	}
}

func LoadServerConfig(certFile, keyFile, caFile string, allowed ...string) (*tls.Config, error) {
	cert, pool, err := load(certFile, keyFile, caFile)
	if err != nil {
		return nil, err
	}
	return ServerConfig(cert, pool, allowed...), nil
}

func LoadClientConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, pool, err := load(certFile, keyFile, caFile)
	if err != nil {
		return nil, err
	}
	return ClientConfig(cert, pool), nil
}

func load(certFile, keyFile, caFile string) (tls.Certificate, *x509.CertPool, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("mtls: loading key pair: %w", err)
	}
	caPEM, err := os.ReadFile(caFile) //nolint:gosec // the path comes from the operator's configuration
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("mtls: reading CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, fmt.Errorf("mtls: %s holds no PEM certificate", caFile)
	}
	return cert, pool, nil
}
