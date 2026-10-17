// spade-api serves the public /v1 REST API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	s := &api.Server{Pool: pool, Auth: verifier, Authz: az, Ledger: led, Caps: caps, WF: workflows.Starter{C: tc},
		InternalSecret: os.Getenv("SPADE_INTERNAL_SECRET"), Log: log}
	srv := &http.Server{Addr: config.Get("LISTEN", ":8080"), Handler: s.Router(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Shutdown(context.Background()) }()
	log.Info("spade-api listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

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
	for _, ext := range []struct{ name, env, path string }{
		{"payments", "PAYMENTS_URL", "/capabilities"},
		{"ai_devops", "AI_DEVOPS_URL", "/healthz"},
		{"agent_gateway", "GATEWAY_HEALTH_URL", "/healthz"},
	} {
		ext := ext
		c.Register(ext.name, func(ctx context.Context) (string, string) {
			base := os.Getenv(ext.env)
			if base == "" {
				return "unavailable", ext.env + " not configured"
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+ext.path, nil)
			cl := &http.Client{Timeout: 3 * time.Second, Transport: insecureLocal()}
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
	for _, p := range []string{"hetzner", "aws", "proxmox", "openstack"} {
		p := p
		c.Register("provider."+p, func(context.Context) (string, string) { return "unavailable", "driver not implemented" })
	}
	c.Register("provider.byovm", func(context.Context) (string, string) { return "available", "" })
	c.Register("overlay.wireguard", func(context.Context) (string, string) { return "unavailable", "overlay controller not implemented" })
	c.Register("overlay.nebula", func(context.Context) (string, string) { return "unavailable", "overlay controller not implemented" })
	c.Register("backups", func(context.Context) (string, string) { return "unavailable", "backup orchestration not implemented" })
}
