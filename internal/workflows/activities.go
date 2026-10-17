package workflows

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/munisp/spadeai/internal/audit"
	"github.com/munisp/spadeai/internal/gateway"
	"github.com/munisp/spadeai/internal/ledger"
	"github.com/munisp/spadeai/internal/overlay"
	"github.com/munisp/spadeai/internal/providers/hetzner"
	"github.com/munisp/spadeai/internal/secrets"
)

type Activities struct {
	KEK       *secrets.KEK        // seals per-service backup passwords; nil disables backups (fail closed)
	Overlay   *overlay.Controller // nil disables private networks (deploys into projects then fail closed)
	Pool      *pgxpool.Pool
	Ledger    *ledger.Ledger
	HoldHours int64
	// GatewayURL / CAPin are embedded in the BYOVM bootstrap command shown to the user.
	GatewayURL string
	CAPin      string
	// Hetzner is nil/tokenless unless SPADE_HETZNER_TOKEN is set; hetzner deploys then fail closed.
	Hetzner         *hetzner.Client
	HetznerLocation string
}

func nonRetryable(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), "NonRetryable", nil)
}

func (a *Activities) MarkOperation(ctx context.Context, opID, status, workflowID, errMsg string, response map[string]any) error {
	if opID == "" {
		return nil // system-initiated workflow (e.g. scheduled backup) without a user operation
	}
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
	switch {
	case s.Provider == "hetzner" && (a.Hetzner == nil || a.Hetzner.Token == ""):
		return nil, nonRetryable("provider hetzner unavailable: SPADE_HETZNER_TOKEN not configured")
	case s.Provider != "byovm" && s.Provider != "hetzner":
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
	if s.Provider == "hetzner" {
		return a.provisionHetzner(ctx, serviceID, token, hex.EncodeToString(sum[:8]), exp)
	}
	return &ProvisionResult{Bootstrap: cmd, EnrolExpiresAt: exp}, nil
}

// Host dependencies on a stock ubuntu-24.04 image: Docker + Compose for stacks, WireGuard tools and
// pinned Nebula for overlays, restic for backups.
var (
	safeGatewayURL = regexp.MustCompile(`^https://[A-Za-z0-9.\-]+(:[0-9]{1,5})?(/[A-Za-z0-9._\-/]*)?$`)
	hexOnly        = regexp.MustCompile(`^[0-9a-f]+$`)
)

var hetznerPackages = []string{"docker.io", "docker-compose-v2", "wireguard-tools", "restic", "curl", "openssl"}

const nebulaInstall = `curl -fsSL -o /tmp/nebula.tgz https://github.com/slackhq/nebula/releases/download/v1.9.5/nebula-linux-amd64.tar.gz && ` +
	`echo "af57ded8f3370f0486bb24011942924b361d77fa34e3478995b196a5441dbf71  /tmp/nebula.tgz" | sha256sum -c - && ` +
	`tar -xzf /tmp/nebula.tgz -C /usr/local/bin nebula nebula-cert && rm /tmp/nebula.tgz`

// provisionHetzner creates the server whose cloud-init installs host dependencies and runs the embedded
// agent installer. The attempt label ties the server to this enrolment token.
func (a *Activities) provisionHetzner(ctx context.Context, serviceID, token, attempt string, exp time.Time) (*ProvisionResult, error) {
	var code string
	if err := a.Pool.QueryRow(ctx, `SELECT z.code FROM service s JOIN size z ON z.id=s.size_id WHERE s.id=$1`, serviceID).Scan(&code); err != nil {
		return nil, err
	}
	// Reconcile by label on every attempt, whether or not provider_ref was recorded.
	if err := a.Hetzner.DeleteOthers(ctx, serviceID, attempt); err != nil {
		return nil, err
	}
	// Values are interpolated into a root shell command, so only accept URL/hex characters.
	if !safeGatewayURL.MatchString(a.GatewayURL) || !hexOnly.MatchString(a.CAPin) || !hexOnly.MatchString(token) {
		return nil, nonRetryable("GATEWAY_PUBLIC_URL must be https://host[:port][/path] with no quotes or spaces, and the CA pin hex")
	}
	run := fmt.Sprintf("SPADE_GATEWAY='%s' SPADE_CA_SHA256='%s' SPADE_ENROL_TOKEN='%s' sh /opt/spade/install.sh", a.GatewayURL, a.CAPin, token)
	boot := hetzner.Boot{Packages: hetznerPackages, Files: map[string]string{"/opt/spade/install.sh": gateway.InstallScript()},
		Pre: []string{"systemctl enable --now docker", nebulaInstall}, Run: run}
	loc := a.HetznerLocation
	if loc == "" {
		loc = "fsn1"
	}
	srv, _, err := a.Hetzner.Ensure(ctx, hetzner.CreateOpts{ServiceID: serviceID, Attempt: attempt, ServerType: strings.ToLower(code),
		Location: loc, Image: "ubuntu-24.04", UserData: hetzner.CloudInit(boot)})
	if err != nil {
		return nil, err
	}
	if _, err := a.Pool.Exec(ctx, `UPDATE service SET provider_ref=$2 WHERE id=$1`, serviceID, strconv.FormatInt(srv.ID, 10)); err != nil {
		return nil, err
	}
	return &ProvisionResult{Bootstrap: fmt.Sprintf("hetzner server %s (%s) installs the agent via cloud-init", srv.Name, srv.IPv4), EnrolExpiresAt: exp}, nil
}

// Deprovision deletes provider infrastructure for API providers; BYOVM hosts belong to the customer.
func (a *Activities) Deprovision(ctx context.Context, serviceID string) error {
	s, err := a.info(ctx, serviceID)
	if err != nil {
		return err
	}
	if s.Provider == "hetzner" {
		if a.Hetzner == nil || a.Hetzner.Token == "" {
			return nonRetryable("hetzner server for %s not deleted: SPADE_HETZNER_TOKEN not configured", serviceID)
		}
		if _, err := a.Hetzner.Delete(ctx, serviceID); err != nil {
			return err
		}
	}
	// The host is gone or no longer managed: its agent certificate and any enrolment token stop working.
	_, err = a.Pool.Exec(ctx, `UPDATE agent SET cert_fingerprint=NULL, enrol_token_sha256=NULL, enrol_expires_at=NULL WHERE service_id=$1`, serviceID)
	return err
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
	case "overlay_keygen", "overlay_down":
		var projectID, kind string
		if err := a.Pool.QueryRow(ctx, `SELECT p.id::text, p.overlay_kind FROM service s JOIN project p ON p.id=s.project_id WHERE s.id=$1`, serviceID).Scan(&projectID, &kind); err != nil {
			return "", err
		}
		payload["kind"], payload["iface"], payload["listen_port"] = kind, overlay.Iface(kind, projectID), overlay.Port(kind)
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

// OverlayRegister reads the host's overlay_keygen result and records it in the project's overlay
// (stable IP allocation; Nebula host certificate signing). Returns the project ID.
func (a *Activities) OverlayRegister(ctx context.Context, serviceID, keygenCmd string) (string, error) {
	if a.Overlay == nil {
		return "", nonRetryable("overlay controller not configured")
	}
	var raw []byte
	if err := a.Pool.QueryRow(ctx, `SELECT result FROM agent_command WHERE id=$1 AND status='succeeded'`, keygenCmd).Scan(&raw); err != nil {
		return "", err
	}
	var r struct {
		PublicKey string `json:"public_key"`
		Endpoint  string `json:"endpoint"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.PublicKey == "" {
		return "", nonRetryable("overlay_keygen returned no public key")
	}
	pid, err := a.Overlay.Register(ctx, serviceID, r.PublicKey, r.Endpoint)
	if errors.Is(err, overlay.ErrNebulaUnavailable) {
		return "", nonRetryable("%v", err)
	}
	return pid, err
}

// OverlaySync queues overlay_apply for every enrolled member of the project; returns service -> command ID.
func (a *Activities) OverlaySync(ctx context.Context, projectID string) (map[string]string, error) {
	if a.Overlay == nil {
		return nil, nonRetryable("overlay controller not configured")
	}
	plans, err := a.Overlay.Plan(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for svc, p := range plans {
		b, _ := json.Marshal(p)
		var id string
		err := a.Pool.QueryRow(ctx, `INSERT INTO agent_command(agent_id, verb, payload) SELECT id, 'overlay_apply', $2 FROM agent
			WHERE service_id=$1 AND cert_fingerprint IS NOT NULL RETURNING id::text`, svc, b).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[svc] = id
	}
	return out, nil
}

// OverlayRemove drops the service from its overlay; returns the project ID ("" if it was not a member).
func (a *Activities) OverlayRemove(ctx context.Context, serviceID string) (string, error) {
	if a.Overlay == nil {
		return "", nil
	}
	return a.Overlay.Remove(ctx, serviceID)
}

// EnsureBackupConfig creates the service's restic password (random, sealed with the KEK) on first use.
func (a *Activities) EnsureBackupConfig(ctx context.Context, serviceID string) error {
	if a.KEK == nil {
		return nonRetryable("backups unavailable: KEK not configured")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	sealed, err := a.KEK.Seal([]byte(hex.EncodeToString(raw)), "backup:"+serviceID)
	if err != nil {
		return err
	}
	_, err = a.Pool.Exec(ctx, `INSERT INTO service_backup_config(service_id,password_sealed) VALUES ($1,$2) ON CONFLICT (service_id) DO NOTHING`, serviceID, sealed)
	return err
}

// QueueBackupCommand queues backup_run/backup_restore carrying only a secret_ref; the gateway injects the
// repository credentials at delivery.
func (a *Activities) QueueBackupCommand(ctx context.Context, serviceID, verb, snapshot string) (string, error) {
	var agentID string
	var keep int
	var manifest []byte
	err := a.Pool.QueryRow(ctx, `SELECT a.id::text, c.keep_daily, tv.manifest FROM service s JOIN template_version tv ON tv.id=s.template_version_id
		JOIN agent a ON a.service_id=s.id JOIN service_backup_config c ON c.service_id=s.id WHERE s.id=$1 AND a.cert_fingerprint IS NOT NULL`, serviceID).Scan(&agentID, &keep, &manifest)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nonRetryable("service %s has no enrolled agent or backup config", serviceID)
	}
	if err != nil {
		return "", err
	}
	quiesce := true
	var m map[string]any
	_ = json.Unmarshal(manifest, &m)
	if b, ok := m["backup"].(map[string]any); ok {
		if q, ok := b["quiesce"].(bool); ok {
			quiesce = q
		}
	}
	payload := map[string]any{"project": "spade-" + serviceID[:8], "secret_ref": map[string]string{"kind": "backup", "service_id": serviceID},
		"keep_daily": keep, "quiesce": quiesce}
	if verb == "backup_restore" {
		payload["snapshot"] = snapshot
	}
	b, _ := json.Marshal(payload)
	var id string
	err = a.Pool.QueryRow(ctx, `INSERT INTO agent_command(agent_id, verb, payload, expires_at) VALUES ($1,$2,$3, now() + interval '1 hour') RETURNING id::text`, agentID, verb, b).Scan(&id)
	return id, err
}

func (a *Activities) BackupRecord(ctx context.Context, serviceID, opID, kind, restoredFrom string) (string, error) {
	var id string
	err := a.Pool.QueryRow(ctx, `INSERT INTO backup_snapshot(service_id, operation_id, kind, restored_from) VALUES ($1, nullif($2,'')::uuid, $3, nullif($4,'')::uuid) RETURNING id::text`,
		serviceID, opID, kind, restoredFrom).Scan(&id)
	return id, err
}

// BackupFinish records the outcome; for backups the restic snapshot id and bytes come from the agent result.
func (a *Activities) BackupFinish(ctx context.Context, recordID, cmdID, errMsg string) error {
	if errMsg != "" {
		_, err := a.Pool.Exec(ctx, `UPDATE backup_snapshot SET status='failed', error=$2, finished_at=now() WHERE id=$1`, recordID, errMsg)
		return err
	}
	_, err := a.Pool.Exec(ctx, `UPDATE backup_snapshot b SET status='succeeded', finished_at=now(), restic_snapshot_id=c.result->>'snapshot_id',
		bytes_added=(c.result->>'bytes_added')::bigint FROM agent_command c WHERE b.id=$1 AND c.id=$2`, recordID, cmdID)
	if err != nil {
		return err
	}
	// Retire backups whose snapshot retention pruned, so they are never offered for restore.
	_, err = a.Pool.Exec(ctx, `UPDATE backup_snapshot o SET status='pruned'
		FROM backup_snapshot b JOIN agent_command c ON c.id=$2
		WHERE b.id=$1 AND b.kind='backup' AND o.service_id=b.service_id AND o.kind='backup' AND o.status='succeeded'
		  AND o.restic_snapshot_id IS NOT NULL AND jsonb_typeof(c.result->'kept')='array'
		  AND NOT jsonb_exists(c.result->'kept', o.restic_snapshot_id)`, recordID, cmdID)
	return err
}

// BackupSnapshotID resolves a succeeded backup of this service to its restic snapshot id.
func (a *Activities) BackupSnapshotID(ctx context.Context, serviceID, backupID string) (string, error) {
	var snap string
	err := a.Pool.QueryRow(ctx, `SELECT restic_snapshot_id FROM backup_snapshot WHERE id=$1 AND service_id=$2 AND kind='backup' AND status='succeeded' AND restic_snapshot_id IS NOT NULL`,
		backupID, serviceID).Scan(&snap)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nonRetryable("backup %s is not a completed backup of service %s", backupID, serviceID)
	}
	return snap, err
}
