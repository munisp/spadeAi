package main

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const drillQueue = "spade-hadrill"

// DrillWorkflow sleeps across the outage on a durable timer, then runs one activity. Completing proves
// Temporal kept its workflow state through the database failover.
func DrillWorkflow(ctx workflow.Context, wait time.Duration) (string, error) {
	if err := workflow.Sleep(ctx, wait); err != nil {
		return "", err
	}
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumInterval: 2 * time.Second},
	})
	var out string
	err := workflow.ExecuteActivity(ctx, DrillActivity).Get(ctx, &out)
	return out, err
}

func DrillActivity(context.Context) (string, error) { return "survived", nil }

// temporalDrill starts DrillWorkflow and a worker for it; the returned func waits for its result.
func temporalDrill(ctx context.Context, hostPort string, wait time.Duration) (string, func(time.Duration) (string, error), error) {
	c, err := client.Dial(client.Options{HostPort: hostPort})
	if err != nil {
		return "", nil, err
	}
	w := worker.New(c, drillQueue, worker.Options{})
	w.RegisterWorkflow(DrillWorkflow)
	w.RegisterActivity(DrillActivity)
	if err := w.Start(); err != nil {
		c.Close()
		return "", nil, err
	}
	id := fmt.Sprintf("ha-drill-%d", time.Now().UnixNano())
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: drillQueue}, DrillWorkflow, wait)
	if err != nil {
		w.Stop()
		c.Close()
		return "", nil, err
	}
	return id, func(timeout time.Duration) (string, error) {
		defer c.Close()
		defer w.Stop()
		wctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var out string
		err := run.Get(wctx, &out)
		return out, err
	}, nil
}
