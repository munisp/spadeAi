package workflows

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/munisp/spadeai/internal/audit"
	"github.com/munisp/spadeai/internal/ledger"
)

type Activities struct {
	Pool      *pgxpool.Pool
	Ledger    *ledger.Ledger
	HoldHours int64
	// GatewayURL / CAPin are embedded in the BYOVM bootstrap command shown to the user.
	GatewayURL string
	CAPin      string
}

func nonRetryable(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), "NonRetryable", nil)
}

func (a *Activities) MarkOperation(ctx context.Context, opID, status, workflowID, errMsg string, response map[string]any) error {
	var resp any
	if response != nil {
		b, _ := json.Marshal(response)
		resp = b
	}
	_, err := a.Pool.Exec(ctx, `UPDATE operation SET status=$2, workflow_id=coalesce(nullif($3,''),workflow_id), error=nullif($4,''),
		response=coalesce($5::jsonb,response), updated_at=now() WHERE id=$1`, opID, status, workflowID, errMsg, resp)
	return err
}

var billable = map[string]bool{"running": true, "stopped": true, "maintenance": true, "updating": true, "resizing": true, "restoring": true}

func (a *Activities) GetState(ctx context.Context, serviceID string) (string, error) {
	var st string
	err := a.Pool.QueryRow(ctx, `SELECT state FROM service WHERE id=$1`, serviceID).Scan(&st)
	return st, err
}

// SetState moves a service to a new state and keeps service_state_interval (the metering source) consistent.
func (a *Activities) SetState(ctx context.Context, serviceID, state, reason string) error {
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := setStateTx(ctx, tx, serviceID, state, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func setStateTx(ctx context.Context, tx pgx.Tx, serviceID, state, reason string) error {
	var prev string
	if err := tx.QueryRow(ctx, `SELECT state FROM service WHERE id=$1 FOR UPDATE`, serviceID).Scan(&prev); err != nil {
		return err
	}
	if prev == state {
		_, err := tx.Exec(ctx, `UPDATE service SET state_reason=nullif($2,''), updated_at=now() WHERE id=$1`, serviceID, reason)
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE service_state_interval SET ended_at=now() WHERE service_id=$1 AND ended_at IS NULL`, serviceID); err != nil {
		return err
	}
	if billable[state] {
		if _, err := tx.Exec(ctx, `INSERT INTO service_state_interval(service_id,state) VALUES ($1,$2)`, serviceID, state); err != nil {
			return err
		}
	}
	del := ""
	if state == "deleted" {
		del = ", deleted_at=now()"
	}
	if _, err := tx.Exec(ctx, `UPDATE service SET state=$2, state_reason=nullif($3,''), updated_at=now()`+del+` WHERE id=$1`, serviceID, state, reason); err != nil {
		return err
	}
	return audit.Append(ctx, tx, audit.Event{Actor: "system:workflow", Verb: "service.state", TargetKind: "service", TargetID: serviceID, Detail: map[string]any{"from": prev, "to": state, "reason": reason}})
}

type svcInfo struct {
	OrgID, Currency, BillingMode, Provider string
	HourlyMinor                            int64
}

func (a *Activities) info(ctx context.Context, serviceID string) (*svcInfo, error) {
	var s svcInfo
	err := a.Pool.QueryRow(ctx, `
		SELECT o.id::text, 'NGN', o.billing_mode, s.provider_id,
		  ((z.vcpu*coalesce(pv.price_minor,0) + (z.ram_mb/1024.0)*coalesce(pr.price_minor,0) + z.disk_gb*coalesce(pd.price_minor,0) + coalesce(ps.price_minor,0)) / 1000)::bigint
		FROM service s JOIN project p ON p.id=s.project_id JOIN organization o ON o.id=p.org_id JOIN size z ON z.id=s.size_id
		JOIN price_book b ON b.status='effective'
		LEFT JOIN price_item pv ON pv.price_book_version=b.version AND pv.sku='mgmt.vcpu.hour'
		LEFT JOIN price_item pr ON pr.price_book_version=b.version AND pr.sku='mgmt.ram_gb.hour'
		LEFT JOIN price_item pd ON pd.price_book_version=b.version AND pd.sku='mgmt.disk_gb.hour'
		LEFT JOIN price_item ps ON ps.price_book_version=b.version AND ps.sku='support.'||s.support_plan||'.hour'
		WHERE s.id=$1 ORDER BY b.version DESC LIMIT 1`, serviceID).Scan(&s.OrgID, &s.Currency, &s.BillingMode, &s.Provider, &s.HourlyMinor)
	return &s, err
}

// PlaceHold reserves HoldHours of estimated cost as a pending TigerBeetle transfer (funds check only, always voided).
func (a *Activities) PlaceHold(ctx context.Context, serviceID string) error {
	s, err := a.info(ctx, serviceID)
	if err != nil {
		return err
	}
	if _, _, _, _, err := a.Ledger.PlatformAccounts(ctx, s.Currency); err != nil {
		return err
	}
	amount := s.HourlyMinor * a.HoldHours
	if amount <= 0 {
		return nonRetryable("price book yields zero cost for service %s; refusing unpriced deployment", serviceID)
	}
	_, err = a.Ledger.Hold(s.OrgID, s.Currency, "deploy:"+serviceID, amount, uint32(2*time.Hour/time.Second))
	if errors.Is(err, ledger.ErrInsufficientFunds) {
		return nonRetryable("insufficient prepaid balance: need %d kobo available for a %dh funds check", amount, a.HoldHours)
	}
	return err
}

func (a *Activities) VoidHold(ctx context.Context, serviceID string) error {
	return a.Ledger.VoidHold("NGN", "deploy:"+serviceID)
}

type ProvisionResult struct {
	Bootstrap      string    `json:"bootstrap"`
	EnrolExpiresAt time.Time `json:"enrol_expires_at"`
}

// Provision dispatches to the provider driver. Only BYOVM is implemented; API providers fail closed.
func (a *Activities) Provision(ctx context.Context, serviceID string) (*ProvisionResult, error) {
	s, err := a.info(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	if s.Provider != "byovm" {
		return nil, nonRetryable("provider %q has no driver configured", s.Provider)
	}
	if a.GatewayURL == "" || a.CAPin == "" {
		return nil, nonRetryable("agent gateway URL/CA pin not configured")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	exp := time.Now().UTC().Add(15 * time.Minute)
	_, err = a.Pool.Exec(ctx, `INSERT INTO agent(service_id, enrol_token_sha256, enrol_expires_at) VALUES ($1,$2,$3)
		ON CONFLICT (service_id) DO UPDATE SET enrol_token_sha256=EXCLUDED.enrol_token_sha256, enrol_expires_at=EXCLUDED.enrol_expires_at`,
		serviceID, hex.EncodeToString(sum[:]), exp)
	if err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf("curl -fsSL --insecure %s/agent/v1/install.sh | sudo SPADE_GATEWAY=%s SPADE_CA_SHA256=%s SPADE_ENROL_TOKEN=%s sh", a.GatewayURL, a.GatewayURL, a.CAPin, token)
	return &ProvisionResult{Bootstrap: cmd, EnrolExpiresAt: exp}, nil
}

func (a *Activities) WaitForAgent(ctx context.Context, serviceID string) error {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		var ok bool
		err := a.Pool.QueryRow(ctx, `SELECT cert_fingerprint IS NOT NULL AND last_seen_at > now() - interval '2 minutes' FROM agent WHERE service_id=$1`, serviceID).Scan(&ok)
		if err == nil && ok {
			return nil
		}
		activity.RecordHeartbeat(ctx)
		select {
		case <-ctx.Done():
			return fmt.Errorf("agent did not enrol in time")
		case <-t.C:
		}
	}
}

func (a *Activities) QueueCommand(ctx context.Context, serviceID, verb string) (string, error) {
	var agentID, compose, sha, display string
	var manifest, env []byte
	err := a.Pool.QueryRow(ctx, `SELECT a.id::text, tv.compose, tv.compose_sha256, tv.manifest, s.env, s.display_name FROM service s
		JOIN template_version tv ON tv.id=s.template_version_id JOIN agent a ON a.service_id=s.id WHERE s.id=$1 AND a.cert_fingerprint IS NOT NULL`, serviceID).
		Scan(&agentID, &compose, &sha, &manifest, &env, &display)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nonRetryable("service %s has no enrolled agent", serviceID)
	}
	if err != nil {
		return "", err
	}
	payload := map[string]any{"project": "spade-" + serviceID[:8]}
	var m map[string]any
	_ = json.Unmarshal(manifest, &m)
	switch verb {
	case "apply_bundle":
		var e map[string]string
		_ = json.Unmarshal(env, &e)
		payload["compose"], payload["compose_sha256"], payload["env"] = compose, sha, e
	case "health_probe":
		payload["health"] = m["health"]
	}
	b, _ := json.Marshal(payload)
	var id string
	err = a.Pool.QueryRow(ctx, `INSERT INTO agent_command(agent_id, verb, payload) VALUES ($1,$2,$3) RETURNING id::text`, agentID, verb, b).Scan(&id)
	return id, err
}

func (a *Activities) WaitCommand(ctx context.Context, cmdID string) error {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		_, _ = a.Pool.Exec(ctx, `UPDATE agent_command SET status='expired', updated_at=now() WHERE id=$1 AND status IN ('queued','delivered') AND expires_at < now()`, cmdID)
		var status string
		var result []byte
		if err := a.Pool.QueryRow(ctx, `SELECT status, coalesce(result,'{}'::jsonb) FROM agent_command WHERE id=$1`, cmdID).Scan(&status, &result); err != nil {
			return err
		}
		switch status {
		case "succeeded":
			return nil
		case "failed", "expired":
			return fmt.Errorf("agent command %s %s: %s", cmdID, status, string(result))
		}
		activity.RecordHeartbeat(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// SuspendOrgStart marks the org suspended and moves its billable services to 'suspended' (stops metering).
func (a *Activities) SuspendOrgStart(ctx context.Context, orgID string) ([]string, error) {
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE organization SET suspended_at=coalesce(suspended_at,now()) WHERE id=$1`, orgID); err != nil {
		return nil, err
	}
	ids, err := collect(ctx, tx, `SELECT s.id::text FROM service s JOIN project p ON p.id=s.project_id WHERE p.org_id=$1 AND s.state IN ('running','stopped','maintenance','updating')`, orgID)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := setStateTx(ctx, tx, id, "suspended", "prepaid debit rejected (arrears)"); err != nil {
			return nil, err
		}
	}
	if err := audit.Append(ctx, tx, audit.Event{Actor: "system:billing", Verb: "org.suspend", TargetKind: "organization", TargetID: orgID, Detail: map[string]any{"services": ids}}); err != nil {
		return nil, err
	}
	return ids, tx.Commit(ctx)
}

func (a *Activities) ResumeOrgStart(ctx context.Context, orgID string) ([]string, error) {
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var unpaid int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM transfer_intent WHERE org_id=$1 AND status='unpaid'`, orgID).Scan(&unpaid); err != nil {
		return nil, err
	}
	if unpaid > 0 {
		return nil, nonRetryable("org %s still has %d unpaid charges", orgID, unpaid)
	}
	if _, err := tx.Exec(ctx, `UPDATE organization SET suspended_at=NULL WHERE id=$1`, orgID); err != nil {
		return nil, err
	}
	ids, err := collect(ctx, tx, `SELECT s.id::text FROM service s JOIN project p ON p.id=s.project_id WHERE p.org_id=$1 AND s.state='suspended'`, orgID)
	if err != nil {
		return nil, err
	}
	if err := audit.Append(ctx, tx, audit.Event{Actor: "system:billing", Verb: "org.resume", TargetKind: "organization", TargetID: orgID, Detail: map[string]any{"services": ids}}); err != nil {
		return nil, err
	}
	return ids, tx.Commit(ctx)
}

func collect(ctx context.Context, tx pgx.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if out == nil {
		out = []string{}
	}
	return out, err
}
