// Package config reads environment configuration with local-dev defaults matching deploy/compose.
package config

import (
	"os"
	"strconv"
	"strings"
)

func Get(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func Int(k string, def int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(k), 10, 64); err == nil {
		return v
	}
	return def
}

func List(k, def string) []string { return strings.Split(Get(k, def), ",") }

var (
	DatabaseURL = func() string {
		return Get("SPADE_DATABASE_URL", "postgres://spade:spade-local-dev@localhost:5433/spadeai?sslmode=disable")
	}
	TBAddresses = func() []string { return List("TB_ADDRESSES", "3001") }
	Temporal    = func() string { return Get("TEMPORAL_HOSTPORT", "localhost:7233") }
	OIDCIssuer  = func() string { return Get("OIDC_ISSUER", "http://localhost:8180/realms/spadeai") }
	Permify     = func() string { return Get("PERMIFY_URL", "http://localhost:3476") }
	CADir       = func() string { return Get("SPADE_CA_DIR", "./.spade/ca") }
	// KEKFile holds the 32-byte key-encryption key. Local dev auto-creates it; production must mount it
	// from a KMS/Vault and set SPADE_KEK_CREATE=false so a missing key fails closed.
	KEKFile = func() string { return Get("SPADE_KEK_FILE", "./.spade/kek") }
)
