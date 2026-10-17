package workflows

import (
	"context"
	"log/slog"
	"time"

	"go.temporal.io/sdk/client"
)

// BackupScheduler starts a daily BackupService for every running, enrolled service without a successful
// (or in-progress) backup in the last 24 hours. Workflow IDs are per service per day.
type BackupScheduler struct {
	A        *Activities
	Temporal client.Client
	Log      *slog.Logger
}

func (b *BackupScheduler) Loop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := b.Once(ctx); err != nil {
			b.Log.Error("backup schedule pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (b *BackupScheduler) Once(ctx context.Context) error {
	if b.A.KEK == nil {
		return nil // backups unavailable; the capability probe reports why
	}
	rows, err := b.A.Pool.Query(ctx, `SELECT s.id::text FROM service s JOIN agent a ON a.service_id=s.id AND a.cert_fingerprint IS NOT NULL
		LEFT JOIN service_backup_config c ON c.service_id=s.id
		WHERE s.state='running' AND s.deleted_at IS NULL AND coalesce(c.enabled, true)
		AND NOT EXISTS (SELECT 1 FROM backup_snapshot k WHERE k.service_id=s.id AND k.kind='backup'
		  AND (k.status='running' OR (k.status='succeeded' AND k.started_at > now() - interval '24 hours')))`)
	if err != nil {
		return err
	}
	var due []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		due = append(due, id)
	}
	rows.Close()
	day := time.Now().UTC().Format("20060102")
	for _, svc := range due {
		if _, err := b.Temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "backup-sched-" + svc + "-" + day, TaskQueue: TaskQueue}, BackupService, "", svc); err != nil {
			b.Log.Warn("scheduled backup start", "service", svc, "err", err)
		}
	}
	return nil
}
