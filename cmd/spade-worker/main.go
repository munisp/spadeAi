// spade-worker runs Temporal workflows/activities and the rating + ledger relay loop.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/munisp/spadeai/db"
	"github.com/munisp/spadeai/internal/config"
	"github.com/munisp/spadeai/internal/gateway"
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
	ca, err := gateway.LoadOrCreateCA(config.CADir())
	if err != nil {
		return err
	}
	tc, err := client.Dial(client.Options{HostPort: config.Temporal()})
	if err != nil {
		return fmt.Errorf("temporal: %w", err)
	}
	defer tc.Close()
	led := &ledger.Ledger{TB: tbc, Pool: pool}
	acts := &workflows.Activities{Pool: pool, Ledger: led, HoldHours: config.Int("SPADE_HOLD_HOURS", 24),
		GatewayURL: config.Get("GATEWAY_PUBLIC_URL", "https://localhost:8443"), CAPin: ca.Pin()}
	w := worker.New(tc, workflows.TaskQueue, worker.Options{})
	w.RegisterWorkflow(workflows.DeployService)
	w.RegisterWorkflow(workflows.ServiceAction)
	w.RegisterWorkflow(workflows.SuspendOrg)
	w.RegisterWorkflow(workflows.ResumeOrg)
	w.RegisterActivity(acts)
	rater := &workflows.Rater{A: acts, Temporal: tc, Log: log}
	go rater.Loop(ctx, time.Duration(config.Int("SPADE_RATING_INTERVAL_SEC", 60))*time.Second)
	log.Info("spade-worker started", "task_queue", workflows.TaskQueue)
	return w.Run(worker.InterruptCh())
}
