package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/munisp/spadeai/internal/config"
)

// gatewayTransport verifies the agent gateway's certificate against the private agent CA.
func gatewayTransport() (http.RoundTripper, error) {
	pem, err := os.ReadFile(filepath.Join(config.CADir(), "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("agent CA not readable: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("agent CA is not valid PEM")
	}
	return &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}, nil
}
