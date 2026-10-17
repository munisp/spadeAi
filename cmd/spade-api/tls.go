package main

import (
	"crypto/tls"
	"net/http"
)

// insecureLocal is used only for health probes of the internal gateway, whose cert is signed by the private agent CA.
func insecureLocal() http.RoundTripper {
	return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
}
