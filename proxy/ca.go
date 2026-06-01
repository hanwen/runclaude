package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LoadOrCreateCA returns a long-lived self-signed CA persisted in dir. If
// ca.crt/ca.key are already present they are loaded; otherwise a fresh 3072-bit
// RSA key + 10-year cert is generated and written with 0600/0644 perms.
func LoadOrCreateCA(dir string) (*tls.Certificate, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	crtPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if _, err := os.Stat(crtPath); err == nil {
		cert, err := tls.LoadX509KeyPair(crtPath, keyPath)
		if err != nil {
			return nil, err
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, err
		}
		cert.Leaf = leaf
		return &cert, nil
	}
	priv, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "runclaude CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}
	crtPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	if err := os.WriteFile(crtPath, crtPEM, 0644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}, nil
}

// WriteBundle writes a CA bundle to dst that concatenates the system trust
// store (first one of the common Linux locations that exists) with the
// caller-supplied PEM. Useful for handing a single trust file to a sandboxed
// process whose libraries each expect their own bundle path.
func WriteBundle(caCertPEM []byte, dst string) error {
	var sys []byte
	for _, p := range []string{
		"/etc/ssl/certs/ca-bundle.crt",
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/cert.pem",
	} {
		if b, err := os.ReadFile(p); err == nil {
			sys = b
			break
		}
	}
	out := append(append([]byte(nil), sys...), '\n')
	out = append(out, caCertPEM...)
	return os.WriteFile(dst, out, 0644)
}

// LeafCache mints and memoizes per-hostname leaf certificates signed by a
// long-lived CA. The cache is concurrency-safe.
type LeafCache struct {
	mu sync.Mutex
	m  map[string]*tls.Certificate
	ca *tls.Certificate
}

// NewLeafCache returns an empty LeafCache that signs leaves with ca.
func NewLeafCache(ca *tls.Certificate) *LeafCache {
	return &LeafCache{m: map[string]*tls.Certificate{}, ca: ca}
}

// Get returns a cached or freshly minted leaf certificate for host. The leaf
// chain includes the CA cert so a TLS server can present it directly.
func (c *LeafCache) Get(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cert, ok := c.m[host]; ok {
		return cert, nil
	}
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial := big.NewInt(mathrand.Int63())
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, c.ca.Leaf, &priv.PublicKey, c.ca.PrivateKey)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{
		Certificate: [][]byte{der, c.ca.Certificate[0]},
		PrivateKey:  priv,
	}
	c.m[host] = cert
	return cert, nil
}
