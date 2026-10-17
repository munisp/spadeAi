// Package workflows holds the Temporal workflows for deploy, day-2 actions and billing suspension.
package workflows

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const TaskQueue = "spade-core"

// Starter implements api.Workflows on a Temporal client.
type Starter struct{ C client.Client }

func (s Starter) StartDeploy(ctx context.Context, opID, serviceID string) (string, error) {
	run, err := s.C.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "deploy-" + serviceID, TaskQueue: TaskQueue}, DeployService, opID, serviceID)
	if err != nil {
		return "", err
	}
	return run.GetID(), nil
}

func (s Starter) StartAction(ctx context.Context, opID, serviceID, action string) (string, error) {
	run, err := s.C.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "action-" + opID, TaskQueue: TaskQueue}, ServiceAction, opID, serviceID, action)
	if err != nil {
		return "", err
	}
	return run.GetID(), nil
}

func (s Starter) StartBackup(ctx context.Context, opID, serviceID string) (string, error) {
	run, err := s.C.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "backup-" + opID, TaskQueue: TaskQueue}, BackupService, opID, serviceID)
	if err != nil {
		return "", err
	}
	return run.GetID(), nil
}

func (s Starter) StartRestore(ctx context.Context, opID, serviceID, backupID string) (string, error) {
	run, err := s.C.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "restore-" + opID, TaskQueue: TaskQueue}, RestoreService, opID, serviceID, backupID)
	if err != nil {
		return "", err
	}
	return run.GetID(), nil
}

func short(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 5, NonRetryableErrorTypes: []string{"NonRetryable"}}})
}

// slow covers provider calls that wait on the infrastructure: Proxmox clones and runs first boot inside
// Create, and stops the VM before deleting it. Retries are safe because drivers reconcile by tag.
func slow(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 25 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 3, NonRetryableErrorTypes: []string{"NonRetryable"}}})
}

func waiting(ctx workflow.Context, d time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: d, HeartbeatTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}})
}

// DeployService: hold (funds check) -> provision -> agent enrolment -> apply bundle -> health -> void hold -> running.
func DeployService(ctx workflow.Context, opID, serviceID string) error {
	var a *Activities
	info := workflow.GetInfo(ctx)
	sctx := short(ctx)
	if err := workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "running", info.WorkflowExecution.ID, "", nil).Get(ctx, nil); err != nil {
		return err
	}
	fail := func(stage string, cause error) error {
		dctx, _ := workflow.NewDisconnectedContext(ctx)
		dctx = short(dctx)
		_ = workflow.ExecuteActivity(dctx, a.VoidHold, serviceID).Get(dctx, nil)
		reason := fmt.Sprintf("%s: %v", stage, cause)
		// A failed deploy must not leave a provider server running (and billing the customer's account).
		if err := workflow.ExecuteActivity(slow(dctx), a.Deprovision, serviceID).Get(dctx, nil); err != nil {
			reason += "; cleanup failed, provider server may still exist: " + err.Error()
		}
		_ = workflow.ExecuteActivity(dctx, a.SetState, serviceID, "failed", reason).Get(dctx, nil)
		_ = workflow.ExecuteActivity(dctx, a.MarkOperation, opID, "failed", "", reason, nil).Get(dctx, nil)
		return cause
	}
	if err := workflow.ExecuteActivity(sctx, a.PlaceHold, serviceID).Get(ctx, nil); err != nil {
		return fail("funds_check", err)
	}
	if err := workflow.ExecuteActivity(sctx, a.SetState, serviceID, "provisioning_vm", "").Get(ctx, nil); err != nil {
		return fail("state", err)
	}
	var prov ProvisionResult
	if err := workflow.ExecuteActivity(slow(ctx), a.Provision, serviceID).Get(ctx, &prov); err != nil {
		return fail("provision", err)
	}
	_ = workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "running", info.WorkflowExecution.ID, "", map[string]any{"bootstrap": prov.Bootstrap, "enrol_expires_at": prov.EnrolExpiresAt}).Get(ctx, nil)
	if err := workflow.ExecuteActivity(sctx, a.SetState, serviceID, "bootstrapping_agent", "waiting for agent enrolment").Get(ctx, nil); err != nil {
		return fail("state", err)
	}
	if err := workflow.ExecuteActivity(waiting(ctx, AgentWait), a.WaitForAgent, serviceID).Get(ctx, nil); err != nil {
		return fail("agent_enrolment", err)
	}
	if err := joinOverlay(ctx, a, serviceID); err != nil {
		return fail("overlay", err)
	}
	if err := workflow.ExecuteActivity(sctx, a.SetState, serviceID, "installing_app", "").Get(ctx, nil); err != nil {
		return fail("state", err)
	}
	if err := runCommand(ctx, a, serviceID, "apply_bundle"); err != nil {
		return fail("install", err)
	}
	if err := workflow.ExecuteActivity(sctx, a.SetState, serviceID, "configuring_tls", "").Get(ctx, nil); err != nil {
		return fail("state", err)
	}
	if err := runCommand(ctx, a, serviceID, "health_probe"); err != nil {
		return fail("health_probe", err)
	}
	if err := workflow.ExecuteActivity(sctx, a.VoidHold, serviceID).Get(ctx, nil); err != nil {
		return fail("void_hold", err)
	}
	if err := workflow.ExecuteActivity(sctx, a.SetState, serviceID, "running", "").Get(ctx, nil); err != nil {
		return fail("state", err)
	}
	return workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "succeeded", "", "", map[string]any{"service_id": serviceID, "bootstrap": prov.Bootstrap}).Get(ctx, nil)
}

// AgentWait slightly exceeds the 15-minute single-use enrolment token lifetime.
var AgentWait = 16 * time.Minute

// joinOverlay: host generates its key -> control plane allocates IP (and signs a Nebula cert) -> mesh resync.
func joinOverlay(ctx workflow.Context, a *Activities, serviceID string) error {
	var keyCmd string
	if err := workflow.ExecuteActivity(short(ctx), a.QueueCommand, serviceID, "overlay_keygen").Get(ctx, &keyCmd); err != nil {
		return err
	}
	if err := workflow.ExecuteActivity(waiting(ctx, 15*time.Minute), a.WaitCommand, keyCmd).Get(ctx, nil); err != nil {
		return err
	}
	var projectID string
	if err := workflow.ExecuteActivity(short(ctx), a.OverlayRegister, serviceID, keyCmd).Get(ctx, &projectID); err != nil {
		return err
	}
	return syncOverlay(ctx, a, projectID, serviceID)
}

// syncOverlay pushes the current mesh to every member. Only `must`'s own apply is awaited; other members
// apply asynchronously and report through GET /v1/projects/{id}/overlay (an offline host converges on its next sync).
func syncOverlay(ctx workflow.Context, a *Activities, projectID, must string) error {
	var cmds map[string]string
	if err := workflow.ExecuteActivity(short(ctx), a.OverlaySync, projectID).Get(ctx, &cmds); err != nil {
		return err
	}
	id, ok := cmds[must]
	if must == "" {
		return nil
	}
	if !ok {
		return fmt.Errorf("service %s has no enrolled agent for overlay apply", must)
	}
	return workflow.ExecuteActivity(waiting(ctx, 15*time.Minute), a.WaitCommand, id).Get(ctx, nil)
}

func runCommand(ctx workflow.Context, a *Activities, serviceID, verb string) error {
	var cmdID string
	if err := workflow.ExecuteActivity(short(ctx), a.QueueCommand, serviceID, verb).Get(ctx, &cmdID); err != nil {
		return err
	}
	return workflow.ExecuteActivity(waiting(ctx, 15*time.Minute), a.WaitCommand, cmdID).Get(ctx, nil)
}

// ServiceAction runs a day-2 action (start/stop/restart/delete) through the agent.
func ServiceAction(ctx workflow.Context, opID, serviceID, action string) error {
	var a *Activities
	sctx := short(ctx)
	_ = workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "running", workflow.GetInfo(ctx).WorkflowExecution.ID, "", nil).Get(ctx, nil)
	verb, final := map[string][2]string{
		"start": {"compose_up", "running"}, "stop": {"compose_down", "stopped"},
		"restart": {"compose_restart", "running"}, "delete": {"compose_down", "deleted"},
	}[action][0], map[string]string{"start": "running", "stop": "stopped", "restart": "running", "delete": "deleted"}[action]
	var prev string
	if action == "delete" {
		if err := workflow.ExecuteActivity(sctx, a.GetState, serviceID).Get(ctx, &prev); err != nil {
			_ = workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "failed", "", err.Error(), nil).Get(ctx, nil)
			return err
		}
		_ = workflow.ExecuteActivity(sctx, a.SetState, serviceID, "deleting", "").Get(ctx, nil)
	}
	if err := runCommand(ctx, a, serviceID, verb); err != nil {
		if action == "delete" {
			// Containers may still be running: restore the previous (possibly billable) state.
			_ = workflow.ExecuteActivity(sctx, a.SetState, serviceID, prev, "delete failed: "+err.Error()).Get(ctx, nil)
		}
		_ = workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "failed", "", err.Error(), nil).Get(ctx, nil)
		return err
	}
	if action == "delete" {
		// Best effort: the host may already be gone. Remaining members are resynced without this peer.
		_ = runCommand(ctx, a, serviceID, "overlay_down")
		var projectID string
		if err := workflow.ExecuteActivity(sctx, a.OverlayRemove, serviceID).Get(ctx, &projectID); err == nil && projectID != "" {
			_ = syncOverlay(ctx, a, projectID, "")
		}
		// Containers are down; a server we failed to delete would keep costing the customer, so the
		// service stays visible as stopped with the error instead of reporting deleted.
		if err := workflow.ExecuteActivity(slow(ctx), a.Deprovision, serviceID).Get(ctx, nil); err != nil {
			_ = workflow.ExecuteActivity(sctx, a.SetState, serviceID, "stopped", "deprovision failed: "+err.Error()).Get(ctx, nil)
			_ = workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "failed", "", err.Error(), nil).Get(ctx, nil)
			return err
		}
	}
	if err := workflow.ExecuteActivity(sctx, a.SetState, serviceID, final, "").Get(ctx, nil); err != nil {
		return err
	}
	return workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "succeeded", "", "", map[string]any{"state": final}).Get(ctx, nil)
}

// SuspendOrg stops every running service of an org whose prepaid debit was rejected.
func SuspendOrg(ctx workflow.Context, orgID string) error {
	var a *Activities
	sctx := short(ctx)
	var services []string
	if err := workflow.ExecuteActivity(sctx, a.SuspendOrgStart, orgID).Get(ctx, &services); err != nil {
		return err
	}
	for _, s := range services {
		if err := runCommand(ctx, a, s, "compose_down"); err != nil {
			workflow.GetLogger(ctx).Warn("stop during suspension failed; service stays suspended (not billed)", "service", s, "err", err)
		}
	}
	return nil
}

// ResumeOrg restarts services suspended for non-payment once arrears are cleared.
func ResumeOrg(ctx workflow.Context, orgID string) error {
	var a *Activities
	sctx := short(ctx)
	var services []string
	if err := workflow.ExecuteActivity(sctx, a.ResumeOrgStart, orgID).Get(ctx, &services); err != nil {
		return err
	}
	for _, s := range services {
		if err := runCommand(ctx, a, s, "compose_up"); err != nil {
			_ = workflow.ExecuteActivity(sctx, a.SetState, s, "failed", "resume failed: "+err.Error()).Get(ctx, nil)
			continue
		}
		_ = workflow.ExecuteActivity(sctx, a.SetState, s, "running", "").Get(ctx, nil)
	}
	return nil
}

// BackupService takes an encrypted restic snapshot of the service's volumes and stack directory.
func BackupService(ctx workflow.Context, opID, serviceID string) error {
	var a *Activities
	sctx := short(ctx)
	_ = workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "running", workflow.GetInfo(ctx).WorkflowExecution.ID, "", nil).Get(ctx, nil)
	failOp := func(err error) error {
		_ = workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "failed", "", err.Error(), nil).Get(ctx, nil)
		return err
	}
	if err := workflow.ExecuteActivity(sctx, a.EnsureBackupConfig, serviceID).Get(ctx, nil); err != nil {
		return failOp(err)
	}
	var rec, cmd string
	if err := workflow.ExecuteActivity(sctx, a.BackupRecord, serviceID, opID, "backup", "").Get(ctx, &rec); err != nil {
		return failOp(err)
	}
	if err := workflow.ExecuteActivity(sctx, a.QueueBackupCommand, serviceID, "backup_run", "").Get(ctx, &cmd); err != nil {
		_ = workflow.ExecuteActivity(sctx, a.BackupFinish, rec, "", err.Error()).Get(ctx, nil)
		return failOp(err)
	}
	werr := workflow.ExecuteActivity(waiting(ctx, 70*time.Minute), a.WaitCommand, cmd).Get(ctx, nil)
	msg := ""
	if werr != nil {
		msg = werr.Error()
	}
	if err := workflow.ExecuteActivity(sctx, a.BackupFinish, rec, cmd, msg).Get(ctx, nil); err != nil {
		return failOp(err)
	}
	if werr != nil {
		return failOp(werr)
	}
	// The snapshot is safe even if retention fails; report that instead of hiding it.
	res := map[string]any{"backup_id": rec}
	if err := workflow.ExecuteActivity(slow(ctx), a.PruneBackups, serviceID).Get(ctx, nil); err != nil {
		res["prune_error"] = err.Error()
	}
	return workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "succeeded", "", "", res).Get(ctx, nil)
}

// RestoreService restores a completed backup in place: stop, empty volumes, restore, start.
func RestoreService(ctx workflow.Context, opID, serviceID, backupID string) error {
	var a *Activities
	sctx := short(ctx)
	_ = workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "running", workflow.GetInfo(ctx).WorkflowExecution.ID, "", nil).Get(ctx, nil)
	failOp := func(err error) error {
		_ = workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "failed", "", err.Error(), nil).Get(ctx, nil)
		return err
	}
	var snap, prev, rec, cmd string
	if err := workflow.ExecuteActivity(sctx, a.BackupSnapshotID, serviceID, backupID).Get(ctx, &snap); err != nil {
		return failOp(err)
	}
	if err := workflow.ExecuteActivity(sctx, a.GetState, serviceID).Get(ctx, &prev); err != nil {
		return failOp(err)
	}
	if err := workflow.ExecuteActivity(sctx, a.BackupRecord, serviceID, opID, "restore", backupID).Get(ctx, &rec); err != nil {
		return failOp(err)
	}
	_ = workflow.ExecuteActivity(sctx, a.SetState, serviceID, "restoring", "restoring backup "+backupID).Get(ctx, nil)
	werr := workflow.ExecuteActivity(sctx, a.QueueBackupCommand, serviceID, "backup_restore", snap).Get(ctx, &cmd)
	if werr == nil {
		werr = workflow.ExecuteActivity(waiting(ctx, 70*time.Minute), a.WaitCommand, cmd).Get(ctx, nil)
	}
	msg := ""
	if werr != nil {
		msg = werr.Error()
	}
	_ = workflow.ExecuteActivity(sctx, a.BackupFinish, rec, cmd, msg).Get(ctx, nil)
	final := prev
	if werr != nil && !strings.Contains(msg, "data unchanged") {
		// Containers may be stopped or data partly swapped: surface it rather than report the old state.
		final = "failed"
	}
	_ = workflow.ExecuteActivity(sctx, a.SetState, serviceID, final, msg).Get(ctx, nil)
	if werr != nil {
		return failOp(werr)
	}
	return workflow.ExecuteActivity(sctx, a.MarkOperation, opID, "succeeded", "", "", map[string]any{"restore_id": rec, "backup_id": backupID}).Get(ctx, nil)
}
