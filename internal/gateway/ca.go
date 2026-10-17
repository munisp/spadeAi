// Package gateway is the outbound-only agent endpoint: enrolment, mTLS command long-poll and heartbeats.
package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// CA is the internal agent CA. Keys live on disk (mount from a secret store/HSM in production).
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
}

func LoadOrCreateCA(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if cp, err := os.ReadFile(certPath); err == nil {
		kp, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, err
		}
		cb, _ := pem.Decode(cp)
		kb, _ := pem.Decode(kp)
		if cb == nil || kb == nil {
			return nil, errors.New("invalid CA PEM")
		}
		cert, err := x509.ParseCertificate(cb.Bytes)
		if err != nil {
			return nil, err
		}
		key, err := x509.ParseECPrivateKey(kb.Bytes)
		if err != nil {
			return nil, err
		}
		return &CA{Cert: cert, Key: key, CertPEM: cp}, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "SpadeAi Agent CA", Organization: []string{"SpadeAi"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	cp := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, cp, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	return &CA{Cert: cert, Key: key, CertPEM: cp}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// Pin is the SHA-256 of the CA certificate DER, given to agents to pin trust at enrolment.
func (c *CA) Pin() string {
	s := sha256.Sum256(c.Cert.Raw)
	return hex.EncodeToString(s[:])
}

func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.Cert)
	return p
}

// SignAgent signs a CSR for an agent, binding CN to the agent id.
func (c *CA) SignAgent(csrPEM []byte, agentID string, validity time.Duration) ([]byte, string, error) {
	b, _ := pem.Decode(csrPEM)
	if b == nil {
		return nil, "", errors.New("csr is not PEM")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, "", err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", err
	}
	tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: agentID, Organization: []string{"SpadeAi agent"}},
		NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: time.Now().Add(validity),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, csr.PublicKey, c.Key)
	if err != nil {
		return nil, "", err
	}
	fp := sha256.Sum256(der)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), hex.EncodeToString(fp[:]), nil
}

// ServerTLS issues the gateway's own serving certificate from the CA.
func (c *CA) ServerTLS(hosts []string) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "spade-gateway"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.Key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der, c.Cert.Raw}, PrivateKey: key}},
		ClientCAs:    c.Pool(),
		ClientAuth:   tls.VerifyClientCertIfGiven,
	}, nil
}
