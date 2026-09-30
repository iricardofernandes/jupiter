package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"time"
)

// PKI is a certificate authority for development and tests. Production certificates
// come from the platform's own CA.
type PKI struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

// Issued is a certificate with its key, in PEM and ready to serve.
type Issued struct {
	TLS     tls.Certificate
	CertPEM []byte
	KeyPEM  []byte
}

const validity = 365 * 24 * time.Hour

func NewPKI(name string) (*PKI, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mtls: CA key: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(validity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("mtls: CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &PKI{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

func (p *PKI) CAPEM() []byte { return p.pem }

func (p *PKI) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(p.cert)
	return pool
}

// Server issues a certificate for the given host names and IP addresses.
func (p *PKI) Server(hosts ...string) (Issued, error) {
	template := &x509.Certificate{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}
	if len(hosts) > 0 {
		template.Subject.CommonName = hosts[0]
	}
	return p.issue(template)
}

// Client issues a certificate whose URI SAN is identity, such as spiffe://jupiter/api.
func (p *PKI) Client(identity string) (Issued, error) {
	uri, err := url.Parse(identity)
	if err != nil {
		return Issued{}, fmt.Errorf("mtls: identity %q: %w", identity, err)
	}
	return p.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: identity},
		URIs:        []*url.URL{uri},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
}

func (p *PKI) issue(template *x509.Certificate) (Issued, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Issued{}, fmt.Errorf("mtls: key: %w", err)
	}
	now := time.Now()
	template.SerialNumber = serial()
	template.NotBefore = now.Add(-time.Minute)
	template.NotAfter = now.Add(validity)
	template.KeyUsage = x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, template, p.cert, &key.PublicKey, p.key)
	if err != nil {
		return Issued{}, fmt.Errorf("mtls: certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Issued{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Issued{}, err
	}
	return Issued{TLS: pair, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return n
}
