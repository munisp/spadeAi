// Package auth validates Keycloak-issued bearer tokens via OIDC discovery and JWKS.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
)

type Claims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
}

type Verifier struct {
	issuer, audience string
	mu               sync.Mutex
	v                *oidc.IDTokenVerifier
}

func NewVerifier(issuer, audience string) *Verifier {
	return &Verifier{issuer: issuer, audience: audience}
}

// verifier lazily discovers the issuer so the API can start before Keycloak is ready; until then auth fails closed.
func (a *Verifier) verifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v != nil {
		return a.v, nil
	}
	p, err := oidc.NewProvider(ctx, a.issuer)
	if err != nil {
		return nil, err
	}
	a.v = p.Verifier(&oidc.Config{ClientID: a.audience})
	return a.v, nil
}

func (a *Verifier) Ready(ctx context.Context) error {
	_, err := a.verifier(ctx)
	return err
}

var ErrUnauthenticated = errors.New("unauthenticated")

func (a *Verifier) Verify(ctx context.Context, r *http.Request) (*Claims, error) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil, ErrUnauthenticated
	}
	v, err := a.verifier(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := v.Verify(ctx, strings.TrimPrefix(h, "Bearer "))
	if err != nil {
		return nil, ErrUnauthenticated
	}
	var c Claims
	if err := tok.Claims(&c); err != nil || c.Subject == "" {
		return nil, ErrUnauthenticated
	}
	return &c, nil
}
