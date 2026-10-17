// spade-gateway terminates agent mTLS. Agents only ever connect outbound to it.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/munisp/spadeai/db"
	"github.com/munisp/spadeai/internal/config"
	"github.com/munisp/spadeai/internal/gateway"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	pool, err := db.Open(ctx, config.DatabaseURL())
	if err != nil {
		log.Error("db", "err", err)
		os.Exit(1)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	ca, err := gateway.LoadOrCreateCA(config.CADir())
	if err != nil {
		log.Error("ca", "err", err)
		os.Exit(1)
	}
	tlsCfg, err := ca.ServerTLS(config.List("GATEWAY_HOSTS", "localhost,127.0.0.1"))
	if err != nil {
		log.Error("tls", "err", err)
		os.Exit(1)
	}
	s := &gateway.Server{Pool: pool, CA: ca, DistDir: config.Get("SPADE_AGENT_DIST", "./dist")}
	srv := &http.Server{Addr: config.Get("LISTEN", ":8443"), Handler: s.Router(), TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second}
	log.Info("spade-gateway listening", "addr", srv.Addr, "ca_pin", ca.Pin())
	if err := srv.ListenAndServeTLS("", ""); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
