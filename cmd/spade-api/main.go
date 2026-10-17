// spade-api serves the public /v1 REST API.
package main

import (
	"context"
	"fmt"
	"github.com/munisp/spadeai/internal/providers/registry"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/munisp/spadeai/db"
	"github.com/munisp/spadeai/internal/api"
	"github.com/munisp/spadeai/internal/auth"
	"github.com/munisp/spadeai/internal/authz"
	"github.com/munisp/spadeai/internal/capability"
	"github.com/munisp/spadeai/internal/config"
	"github.com/munisp/spadeai/internal/ledger"
	"github.com/munisp/spadeai/internal/workflows"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	pool, err := db.Open(ctx, config.DatabaseURL())
	if err != nil {
		return err
	}
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	// The residency check reads provider.in_nigeria; keep it in step with each driver's configuration so
	// an operator declaration (or its removal) takes effect on restart.
	for _, d := range registry.FromEnv() {
		if _, err := pool.Exec(ctx, `UPDATE provider SET in_nigeria = $2 WHERE id = $1`, d.Name(), d.InNigeria()); err != nil {
			return fmt.Errorf("provider %s residency: %w", d.Name(), err)
		}
	}
	tbc, err := ledger.Connect(0, config.TBAddresses())
	if err != nil {
		return fmt.Errorf("tigerbeetle: %w", err)
	}
	tc, err := client.NewLazyClient(client.Options{HostPort: config.Temporal()})
	if err != nil {
		return err
	}
	az := authz.New(config.Permify())
	go func() {
		for i := 0; i < 60; i++ {
			if err := az.WriteSchema(ctx); err == nil {
				log.Info("permify schema written")
				return
			} else {
				log.Warn("permify schema write failed; retrying", "err", err)
			}
			time.Sleep(2 * time.Second)
		}
	}()
	verifier := auth.NewVerifier(config.OIDCIssuer(), "spade-api")
	led := &ledger.Ledger{TB: tbc, Pool: pool}
	caps := capability.New(pool)
	registerProbes(caps, pool.Ping, led, tc, verifier, az)
	go caps.Loop(ctx, 15*time.Second)
	secret := os.Getenv("SPADE_INTERNAL_SECRET")
	if secret != "" && len(secret) < minInternalSecret {
		log.Error("SPADE_INTERNAL_SECRET is too short; payment credit hook disabled", "min_len", minInternalSecret)
		secret = ""
	}
	s := &api.Server{Pool: pool, Auth: verifier, Authz: az, Ledger: led, Caps: caps, WF: workflows.Starter{C: tc},
		InternalSecret: secret, Log: log}
	// Bind both listeners before serving: if either port is unavailable the process fails instead of
	// running a public API whose payment-credit hook is unreachable.
	internal := &http.Server{Addr: config.Get("SPADE_INTERNAL_LISTEN", "127.0.0.1:8081"), Handler: s.InternalRouter(), ReadHeaderTimeout: 10 * time.Second}
	internalLn, err := net.Listen("tcp", internal.Addr)
	if err != nil {
		return fmt.Errorf("internal listener %s: %w", internal.Addr, err)
	}
	srv := &http.Server{Addr: config.Get("LISTEN", ":8080"), Handler: s.Router(), ReadHeaderTimeout: 10 * time.Second}
	publicLn, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		internalLn.Close()
		return fmt.Errorf("public listener %s: %w", srv.Addr, err)
	}
	errc := make(chan error, 2)
	go func() { errc <- internal.Serve(internalLn) }()
	go func() { errc <- srv.Serve(publicLn) }()
	go func() { <-ctx.Done(); srv.Shutdown(context.Background()); internal.Shutdown(context.Background()) }()
	log.Info("spade-api listening", "addr", srv.Addr, "internal_addr", internal.Addr)
	// Either listener stopping takes the whole process down so the two are never out of step.
	err = <-errc
	srv.Shutdown(context.Background())
	internal.Shutdown(context.Background())
	if err != http.ErrServerClosed {
		return err
	}
	return nil
}

// minInternalSecret is 32 bytes of hex (openssl rand -hex 16) at minimum; shorter, guessable secrets are refused.
const minInternalSecret = 32

func registerProbes(c *capability.Registry, pgPing func(context.Context) error, led *ledger.Ledger, tc client.Client, v *auth.Verifier, az *authz.Client) {
	c.Register("postgres", func(ctx context.Context) (string, string) { return capability.FromErr(pgPing(ctx)) })
	c.Register("tigerbeetle", func(ctx context.Context) (string, string) {
		done := make(chan error, 1)
		go func() { _, err := led.TB.LookupAccounts(nil); done <- err }()
		select {
		case err := <-done:
			return capability.FromErr(err)
		case <-ctx.Done():
			return "unavailable", "tigerbeetle did not answer in time"
		}
	})
	c.Register("temporal", func(ctx context.Context) (string, string) {
		_, err := tc.CheckHealth(ctx, &client.CheckHealthRequest{})
		return capability.FromErr(err)
	})
	c.Register("keycloak", func(ctx context.Context) (string, string) { return capability.FromErr(v.Ready(ctx)) })
	c.Register("permify", func(ctx context.Context) (string, string) { return capability.FromErr(az.Healthy(ctx)) })
	for _, ext := range []struct {
		name, env, path string
		privateCA       bool
	}{
		{"payments", "PAYMENTS_URL", "/capabilities", false},
		{"ai_devops", "AI_DEVOPS_URL", "/healthz", false},
		{"agent_gateway", "GATEWAY_HEALTH_URL", "/healthz", true},
	} {
		ext := ext
		c.Register(ext.name, func(ctx context.Context) (string, string) {
			base := os.Getenv(ext.env)
			if base == "" {
				return "unavailable", ext.env + " not configured"
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+ext.path, nil)
			// Public integrations are verified against system roots; the gateway against the agent CA. Never skip verification.
			cl := &http.Client{Timeout: 3 * time.Second}
			if ext.privateCA {
				t, err := gatewayTransport()
				if err != nil {
					return "unavailable", err.Error()
				}
				cl.Transport = t
			}
			resp, err := cl.Do(req)
			if err != nil {
				return "unavailable", err.Error()
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				return "degraded", fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
			return "available", ""
		})
	}
	for _, d := range registry.FromEnv() {
		d := d
		c.Register("provider."+d.Name(), func(ctx context.Context) (string, string) {
			if !d.Configured() {
				return "unavailable", d.TokenEnv() + " not configured"
			}
			if err := d.Ping(ctx); err != nil {
				return "degraded", err.Error()
			}
			if d.InNigeria() {
				return "available", "operator-declared Nigerian hosting; residency-locked orgs may use it"
			}
			return "available", "servers are created outside Nigeria (no NG region); residency-locked orgs cannot use it"
		})
	}
	c.Register("provider.byovm", func(context.Context) (string, string) { return "available", "" })
	c.Register("overlay.wireguard", func(context.Context) (string, string) {
		return "available", "hosts need the wireguard kernel module and wireguard-tools"
	})
	c.Register("overlay.nebula", func(context.Context) (string, string) {
		if _, err := exec.LookPath(config.Get("NEBULA_CERT", "nebula-cert")); err != nil {
			return "unavailable", "nebula-cert not installed on the control plane"
		}
		if _, err := os.Stat(config.KEKFile()); err != nil {
			return "unavailable", "KEK not present (SPADE_KEK_FILE); CA keys cannot be sealed"
		}
		return "available", "hosts need the nebula binary"
	})
	c.Register("backups", func(ctx context.Context) (string, string) {
		if _, err := os.Stat(config.KEKFile()); err != nil {
			return "unavailable", "KEK not present (SPADE_KEK_FILE); repository passwords cannot be sealed"
		}
		repo := os.Getenv("SPADE_BACKUP_REPO")
		if repo == "" {
			return "unavailable", "SPADE_BACKUP_REPO not configured"
		}
		u := strings.TrimPrefix(repo, "rest:")
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err != nil {
			return "unavailable", "backup repository unreachable: " + err.Error()
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "unavailable", "backup repository rejected the credentials in SPADE_BACKUP_REPO"
		}
		return "available", "restic REST repository; hosts need restic"
	})
}
