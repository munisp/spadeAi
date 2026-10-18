// Package api serves the SpadeAi public REST API (/v1) and the internal payment-credit hook.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/munisp/spadeai/internal/kube"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/munisp/spadeai/internal/audit"
	"github.com/munisp/spadeai/internal/auth"
	"github.com/munisp/spadeai/internal/authz"
	"github.com/munisp/spadeai/internal/capability"
	"github.com/munisp/spadeai/internal/ids"
	"github.com/munisp/spadeai/internal/ledger"
)

// Workflows starts durable Temporal workflows. Implemented in internal/workflows.
type Workflows interface {
	StartDeploy(ctx context.Context, operationID, serviceID string) (string, error)
	StartAction(ctx context.Context, operationID, serviceID, action string) (string, error)
	StartBackup(ctx context.Context, operationID, serviceID string) (string, error)
	StartRestore(ctx context.Context, operationID, serviceID, backupID string) (string, error)
}

type Server struct {
	Pool           *pgxpool.Pool
	Auth           *auth.Verifier
	Authz          *authz.Client
	Ledger         *ledger.Ledger
	Caps           *capability.Registry
	WF             Workflows
	InternalSecret string // HMAC key shared with the payments service; empty disables the hook
	// GatewayURL / CAPin go into the Helm command shown when a Kubernetes cluster is added.
	GatewayURL string
	CAPin      string
	Log        *slog.Logger
}

type ctxKey struct{}

type User struct{ ID, Subject, Email string }

func userFrom(ctx context.Context) User { return ctx.Value(ctxKey{}).(User) }

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(cors)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	r.Get("/v1/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"capabilities": s.Caps.Snapshot()})
	})
	r.Get("/v1/templates", s.listTemplates)
	r.Get("/v1/sizes", s.listSizes)
	r.Group(func(r chi.Router) {
		r.Use(s.authn)
		r.Get("/v1/me", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, userFrom(r.Context())) })
		r.Post("/v1/orgs", s.createOrg)
		r.Get("/v1/orgs", s.listOrgs)
		r.Get("/v1/orgs/{org}/wallet", s.wallet)
		r.Get("/v1/orgs/{org}/usage", s.usage)
		r.Post("/v1/orgs/{org}/projects", s.createProject)
		r.Get("/v1/projects", s.listProjects)
		r.Post("/v1/projects/{project}/members", s.addMember)
		r.Get("/v1/projects/{project}/services", s.listServices)
		r.Get("/v1/projects/{project}/overlay", s.getOverlay)
		r.Post("/v1/projects/{project}/services", s.createService)
		r.Post("/v1/projects/{project}/clusters", s.createCluster)
		r.Get("/v1/projects/{project}/clusters", s.listClusters)
		r.Post("/v1/clusters/{cluster}/refresh", s.refreshCluster)
		r.Get("/v1/clusters/{cluster}/options", s.clusterOptions)
		r.Get("/v1/services/{service}", s.getService)
		r.Post("/v1/services/{service}/actions/{action}", s.serviceAction)
		r.Get("/v1/services/{service}/backups", s.listBackups)
		r.Post("/v1/services/{service}/backups", s.createBackup)
		r.Post("/v1/services/{service}/backups/{backup}/restore", s.restoreBackup)
		r.Delete("/v1/services/{service}", func(w http.ResponseWriter, r *http.Request) {
			rctx := chi.RouteContext(r.Context())
			rctx.URLParams.Add("action", "delete")
			s.serviceAction(w, r)
		})
		r.Get("/v1/operations/{op}", s.getOperation)
		r.Get("/v1/audit-events", s.listAudit)
		r.Get("/v1/audit-events/verify", s.verifyAudit)
	})
	return r
}

// InternalRouter serves service-to-service hooks. Bind it to a private interface only (SPADE_INTERNAL_LISTEN).
func (s *Server) InternalRouter() http.Handler {
	r := chi.NewRouter()
	r.Post("/internal/payments/verified", s.paymentVerified)
	return r
}

// errAuthz marks a transaction aborted because the Permify relationship write failed.
type errAuthz struct{ error }

// allowedOrigin restricts browser access to the configured client origins (SPADE_ALLOWED_ORIGINS, comma separated).
func allowedOrigin(o string) bool {
	list := os.Getenv("SPADE_ALLOWED_ORIGINS")
	if list == "" {
		list = "http://localhost:5173"
	}
	for _, a := range strings.Split(list, ",") {
		if strings.TrimSpace(a) == o {
			return true
		}
	}
	return false
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && allowedOrigin(o) {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := s.Auth.Verify(r.Context(), r)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				writeErr(w, 401, "unauthenticated", "valid bearer token required")
			} else {
				writeErr(w, 503, "identity_unavailable", err.Error())
			}
			return
		}
		var id string
		err = s.Pool.QueryRow(r.Context(), `INSERT INTO app_user(subject,email) VALUES ($1,$2)
			ON CONFLICT (subject) DO UPDATE SET email=EXCLUDED.email RETURNING id::text`, c.Subject, c.Email).Scan(&id)
		if err != nil {
			writeErr(w, 503, "db_unavailable", err.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, User{ID: id, Subject: c.Subject, Email: c.Email})))
	})
}

// allow checks a Permify permission and writes 403/503 itself when denied.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, entity, id, perm string) bool {
	ok, err := s.Authz.Check(r.Context(), entity, id, perm, userFrom(r.Context()).ID)
	if err != nil {
		writeErr(w, 503, "authz_unavailable", err.Error())
		return false
	}
	if !ok {
		writeErr(w, 403, "forbidden", fmt.Sprintf("missing %s on %s", perm, entity))
		return false
	}
	return true
}

func (s *Server) createOrg(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name            string `json:"name"`
		BillingMode     string `json:"billing_mode"`
		AIPolicy        string `json:"ai_policy"`
		ResidencyLocked bool   `json:"residency_locked"`
	}
	if !decode(w, r, &in) || !require(w, in.Name != "", "name is required") {
		return
	}
	if in.BillingMode == "" {
		in.BillingMode = "prepaid"
	}
	if in.AIPolicy == "" {
		in.AIPolicy = "self_hosted"
	}
	if in.ResidencyLocked && in.AIPolicy != "self_hosted" {
		writeErr(w, 422, "policy_conflict", "residency-locked organizations must use ai_policy=self_hosted")
		return
	}
	u := userFrom(r.Context())
	var orgID string
	var granted []authz.Tuple
	err := s.tx(r.Context(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `INSERT INTO organization(name,billing_mode,ai_policy,residency_locked) VALUES ($1,$2,$3,$4) RETURNING id::text`,
			in.Name, in.BillingMode, in.AIPolicy, in.ResidencyLocked).Scan(&orgID); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO org_member(user_id,org_id,role) VALUES ($1,$2,'owner')`, u.ID, orgID); err != nil {
			return err
		}
		if err := audit.Append(r.Context(), tx, audit.Event{Actor: u.ID, Verb: "org.create", TargetKind: "organization", TargetID: orgID, Detail: map[string]any{"name": in.Name, "billing_mode": in.BillingMode, "ai_policy": in.AIPolicy}}); err != nil {
			return err
		}
		// Last step before commit: if Permify is down the org is rolled back, so a retry starts clean.
		t := authz.T("organization", orgID, "owner", "user", u.ID)
		if err := s.Authz.Write(r.Context(), t); err != nil {
			return errAuthz{err}
		}
		granted = append(granted, t)
		return nil
	})
	if err != nil {
		s.revokeIfAbsent("organization", orgID, granted)
	}
	if writeTxErr(w, err, "create_failed") {
		return
	}
	if _, err := s.Ledger.EnsureAccount(r.Context(), "org_wallet", orgID, "NGN", in.BillingMode == "prepaid"); err != nil {
		writeErr(w, 503, "ledger_unavailable", err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": orgID, "name": in.Name, "billing_mode": in.BillingMode, "ai_policy": in.AIPolicy})
}

func (s *Server) listOrgs(w http.ResponseWriter, r *http.Request) {
	s.queryJSON(w, r, `SELECT o.id, o.name, o.billing_mode, o.ai_policy, o.residency_locked, m.role FROM organization o
		JOIN org_member m ON m.org_id=o.id WHERE m.user_id=$1 ORDER BY o.created_at`, userFrom(r.Context()).ID)
}

func (s *Server) wallet(w http.ResponseWriter, r *http.Request) {
	org := chi.URLParam(r, "org")
	if !s.allow(w, r, "organization", org, "view") {
		return
	}
	if err := s.ensureWallet(r.Context(), org, "NGN"); err != nil {
		writeErr(w, 503, "ledger_unavailable", err.Error())
		return
	}
	b, err := s.Ledger.WalletBalance(org, "NGN")
	if err != nil {
		writeErr(w, 503, "ledger_unavailable", err.Error())
		return
	}
	var arrears int64
	_ = s.Pool.QueryRow(r.Context(), `SELECT coalesce(sum(amount_minor),0) FROM transfer_intent WHERE org_id=$1 AND status='unpaid'`, org).Scan(&arrears)
	writeJSON(w, 200, map[string]any{"balance": b, "arrears_minor": arrears})
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	org := chi.URLParam(r, "org")
	if !s.allow(w, r, "organization", org, "view") {
		return
	}
	s.queryJSON(w, r, `SELECT tb_transfer_id::text AS id, kind, amount_minor, currency, reference, status, tb_result, created_at
		FROM transfer_intent WHERE org_id=$1 ORDER BY created_at DESC LIMIT 200`, org)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	org := chi.URLParam(r, "org")
	if !s.allow(w, r, "organization", org, "manage") {
		return
	}
	var in struct {
		Name           string `json:"name"`
		TechnicalEmail string `json:"technical_email"`
		Overlay        string `json:"overlay"`
	}
	if !decode(w, r, &in) || !require(w, in.Name != "" && in.TechnicalEmail != "", "name and technical_email are required") {
		return
	}
	if in.Overlay == "" {
		in.Overlay = "wireguard"
	}
	if !require(w, in.Overlay == "wireguard" || in.Overlay == "nebula", "overlay must be wireguard or nebula") {
		return
	}
	u := userFrom(r.Context())
	var id string
	var granted []authz.Tuple
	err := s.tx(r.Context(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `INSERT INTO project(org_id,name,technical_email,overlay_kind) VALUES ($1,$2,$3,$4) RETURNING id::text`,
			org, in.Name, in.TechnicalEmail, in.Overlay).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO membership(user_id,project_id,role) VALUES ($1,$2,'owner')`, u.ID, id); err != nil {
			return err
		}
		if err := audit.Append(r.Context(), tx, audit.Event{Actor: u.ID, Verb: "project.create", TargetKind: "project", TargetID: id, Detail: map[string]any{"org_id": org, "overlay": in.Overlay}}); err != nil {
			return err
		}
		ts := []authz.Tuple{authz.T("project", id, "org", "organization", org), authz.T("project", id, "owner", "user", u.ID)}
		if err := s.Authz.Write(r.Context(), ts...); err != nil {
			return errAuthz{err}
		}
		granted = ts
		return nil
	})
	if err != nil {
		s.revokeIfAbsent("project", id, granted)
	}
	if writeTxErr(w, err, "create_failed") {
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "org_id": org, "name": in.Name, "overlay": in.Overlay})
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	s.queryJSON(w, r, `SELECT p.id, p.org_id, p.name, p.technical_email, p.overlay_kind AS overlay, m.role FROM project p
		JOIN membership m ON m.project_id=p.id WHERE m.user_id=$1 AND p.deleted_at IS NULL ORDER BY p.created_at`, userFrom(r.Context()).ID)
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	p := chi.URLParam(r, "project")
	if !s.allow(w, r, "project", p, "manage") {
		return
	}
	var in struct{ Email, Role string }
	if !decode(w, r, &in) {
		return
	}
	valid := map[string]bool{"admin": true, "operator": true, "viewer": true, "billing": true}
	if !require(w, valid[in.Role], "role must be admin, operator, viewer or billing") {
		return
	}
	var uid string
	if err := s.Pool.QueryRow(r.Context(), `SELECT id::text FROM app_user WHERE lower(email)=lower($1)`, in.Email).Scan(&uid); err != nil {
		writeErr(w, 404, "user_not_found", "user must sign in once before being added")
		return
	}
	var prev string
	var changed bool
	err := s.tx(r.Context(), func(tx pgx.Tx) error {
		// Every Permify change for this (project, user) happens under this lock, so concurrent member
		// updates and failure reconciliation cannot interleave.
		if err := lockMember(r.Context(), tx, p, uid); err != nil {
			return err
		}
		if err := tx.QueryRow(r.Context(), `SELECT role FROM membership WHERE user_id=$1 AND project_id=$2 FOR UPDATE`, uid, p).Scan(&prev); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if prev == "owner" {
			return fmt.Errorf("the project owner's role cannot be changed here")
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO membership(user_id,project_id,role) VALUES ($1,$2,$3) ON CONFLICT (user_id,project_id) DO UPDATE SET role=EXCLUDED.role`, uid, p, in.Role); err != nil {
			return err
		}
		if err := audit.Append(r.Context(), tx, audit.Event{Actor: userFrom(r.Context()).ID, Verb: "project.member.add", TargetKind: "project", TargetID: p, Detail: map[string]any{"user_id": uid, "role": in.Role, "previous_role": prev}}); err != nil {
			return err
		}
		// Exactly one role tuple per member. One atomic delete drops every role the user held, so a
		// failure part-way can only leave less access (fail closed), never the old privileges plus the new.
		if err := s.Authz.DeleteSubject(r.Context(), "project", p, "user", uid); err != nil {
			return errAuthz{err}
		}
		changed = true
		if err := s.Authz.Write(r.Context(), authz.T("project", p, in.Role, "user", uid)); err != nil {
			return errAuthz{err}
		}
		return nil
	})
	if err != nil && changed {
		s.reconcileMember(context.WithoutCancel(r.Context()), p, uid)
	}
	if writeTxErr(w, err, "failed") {
		return
	}
	writeJSON(w, 200, map[string]string{"user_id": uid, "role": in.Role})
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	s.queryJSON(w, r, `SELECT t.slug, t.name, t.category, t.licence_spdx, t.description, v.version, v.min_vcpu, v.min_ram_mb, v.tested_at
		FROM template t JOIN LATERAL (SELECT * FROM template_version tv WHERE tv.template_id=t.id AND tv.tested_at IS NOT NULL ORDER BY tv.created_at DESC LIMIT 1) v ON true
		ORDER BY t.name`)
}

func (s *Server) listSizes(w http.ResponseWriter, r *http.Request) {
	s.queryJSON(w, r, `SELECT id, provider_id, code, vcpu, ram_mb, disk_gb FROM size ORDER BY provider_id, vcpu`)
}

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	p := chi.URLParam(r, "project")
	if !s.allow(w, r, "project", p, "view") {
		return
	}
	s.queryJSON(w, r, `SELECT s.id, s.display_name, s.state, s.state_reason, s.provider_id, s.size_id, host(s.ipv4) AS ipv4, t.slug AS template, tv.version
		FROM service s JOIN template_version tv ON tv.id=s.template_version_id JOIN template t ON t.id=tv.template_id
		WHERE s.project_id=$1 AND s.deleted_at IS NULL ORDER BY s.created_at`, p)
}

// getOverlay reports the project's private network: members, overlay IPs and each host's latest apply.
func (s *Server) getOverlay(w http.ResponseWriter, r *http.Request) {
	p := chi.URLParam(r, "project")
	if !s.allow(w, r, "project", p, "view") {
		return
	}
	proj, err := s.one(r.Context(), `SELECT id, overlay_kind AS kind, overlay_cidr::text AS cidr, overlay_generation AS generation FROM project WHERE id=$1`, p)
	if err != nil {
		writeErr(w, 404, "not_found", "project not found")
		return
	}
	peers, err := s.rows(r.Context(), `SELECT o.service_id, s.display_name, host(o.overlay_ip) AS overlay_ip, o.endpoint, o.updated_at AS joined_at,
		c.status AS last_apply_status, (c.payload->>'generation')::bigint AS applied_generation, c.updated_at AS last_apply_at
		FROM overlay_peer o JOIN service s ON s.id=o.service_id LEFT JOIN agent a ON a.service_id=o.service_id
		LEFT JOIN LATERAL (SELECT status, payload, updated_at FROM agent_command WHERE agent_id=a.id AND verb='overlay_apply' ORDER BY created_at DESC LIMIT 1) c ON true
		WHERE o.project_id=$1 ORDER BY o.overlay_ip`, p)
	if err != nil {
		writeErr(w, 500, "internal", err.Error())
		return
	}
	proj["peers"] = peers
	writeJSON(w, 200, proj)
}

func (s *Server) getService(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "service")
	if !s.allow(w, r, "service", id, "view") {
		return
	}
	s.queryOneJSON(w, r, `SELECT s.id, s.project_id, s.display_name, s.state, s.state_reason, s.provider_id, s.size_id, host(s.ipv4) AS ipv4,
		host(s.overlay_ip) AS overlay_ip, s.support_plan, s.maintenance_window, t.slug AS template, tv.version, a.last_seen_at AS agent_last_seen
		FROM service s JOIN template_version tv ON tv.id=s.template_version_id JOIN template t ON t.id=tv.template_id
		LEFT JOIN agent a ON a.service_id=s.id WHERE s.id=$1`, id)
}

// startOperation implements Idempotency-Key semantics: same key + same body returns the original operation.
func (s *Server) startOperation(ctx context.Context, tx pgx.Tx, key, verb, kind string, body []byte) (id string, existing bool, err error) {
	u := userFrom(ctx)
	sum := sha256.Sum256(append([]byte(verb+"|"), body...))
	hash := hex.EncodeToString(sum[:])
	err = tx.QueryRow(ctx, `INSERT INTO operation(idempotency_key, actor_user_id, request_hash, verb, target_kind) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (actor_user_id, idempotency_key) DO NOTHING RETURNING id::text`, key, u.ID, hash, verb, kind).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	var prevHash string
	if err := tx.QueryRow(ctx, `SELECT id::text, request_hash FROM operation WHERE actor_user_id=$1 AND idempotency_key=$2`, u.ID, key).Scan(&id, &prevHash); err != nil {
		return "", false, err
	}
	if prevHash != hash {
		return "", false, errIdempotencyConflict
	}
	return id, true, nil
}

var errIdempotencyConflict = errors.New("Idempotency-Key reused with a different request")

func (s *Server) createService(w http.ResponseWriter, r *http.Request) {
	p := chi.URLParam(r, "project")
	key := r.Header.Get("Idempotency-Key")
	if !require(w, key != "", "Idempotency-Key header is required") || !s.allow(w, r, "project", p, "create_service") {
		return
	}
	if s.Caps.Status("temporal") != "available" {
		writeErr(w, 503, "workflows_unavailable", "workflow engine is unavailable; refusing to accept a deployment that cannot run")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var in struct {
		Template    string            `json:"template"`
		Version     string            `json:"version"`
		DisplayName string            `json:"display_name"`
		Provider    string            `json:"provider"`
		Size        string            `json:"size"`
		SupportPlan string            `json:"support_plan"`
		Env         map[string]string `json:"env"`
		Kubernetes  *KubeSpec         `json:"kubernetes"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, 400, "bad_json", err.Error())
		return
	}
	if !require(w, in.Template != "" && in.DisplayName != "" && in.Size != "", "template, display_name and size are required") {
		return
	}
	if in.Provider == "" {
		// Size IDs are "<provider>:<code>", so the size alone selects the provider.
		in.Provider, _, _ = strings.Cut(in.Size, ":")
	}
	if in.SupportPlan == "" {
		in.SupportPlan = "L1"
	}
	envJSON, _ := json.Marshal(in.Env)
	if in.Env == nil {
		envJSON = []byte("{}")
	}
	var opID, svcID string
	var existing bool
	err := s.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		opID, existing, err = s.startOperation(r.Context(), tx, key, "service.create", "service", append([]byte(chi.URLParam(r, "project")+"|"), body...))
		if err != nil || existing {
			return err
		}
		var tvID string
		q := `SELECT tv.id::text FROM template_version tv JOIN template t ON t.id=tv.template_id WHERE t.slug=$1 AND tv.tested_at IS NOT NULL AND ($2='' OR tv.version=$2) ORDER BY tv.created_at DESC LIMIT 1`
		if err := tx.QueryRow(r.Context(), q, in.Template, in.Version).Scan(&tvID); err != nil {
			return fmt.Errorf("no tested version of template %q", in.Template)
		}
		var sizeOK, locked, inNigeria bool
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM size WHERE id=$1 AND provider_id=$2), o.residency_locked,
			COALESCE((SELECT in_nigeria FROM provider WHERE id=$2), false)
			FROM project pr JOIN organization o ON o.id=pr.org_id WHERE pr.id=$3`, in.Size, in.Provider, p).Scan(&sizeOK, &locked, &inNigeria); err != nil {
			return err
		}
		if !sizeOK {
			return fmt.Errorf("size %q is not offered by provider %q", in.Size, in.Provider)
		}
		var kubeCluster, kubeCfg any
		replicas := 1
		if in.Provider == "kubernetes" {
			var cfg []byte
			var err error
			if inNigeria, cfg, err = prepareKube(r.Context(), tx, p, tvID, in.Size, in.Kubernetes, in.Env); err != nil {
				return err
			}
			kubeCluster, kubeCfg, replicas = in.Kubernetes.ClusterID, cfg, *in.Kubernetes.Replicas
		} else if in.Kubernetes != nil {
			return fmt.Errorf("the kubernetes block needs a kubernetes:* size")
		}
		if locked && !inNigeria {
			return fmt.Errorf("residency-locked organizations cannot deploy to %s (no Nigerian region)", in.Provider)
		}
		if err := tx.QueryRow(r.Context(), `INSERT INTO service(project_id, template_version_id, display_name, provider_id, size_id, support_plan, env, replicas, k8s_cluster_id, k8s_config)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::uuid,$10) RETURNING id::text`, p, tvID, in.DisplayName, in.Provider, in.Size, in.SupportPlan, envJSON, replicas, kubeCluster, kubeCfg).Scan(&svcID); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE operation SET target_id=$2 WHERE id=$1`, opID, svcID); err != nil {
			return err
		}
		return audit.Append(r.Context(), tx, audit.Event{Actor: userFrom(r.Context()).ID, Verb: "service.create", TargetKind: "service", TargetID: svcID,
			Detail: map[string]any{"project_id": p, "template": in.Template, "size": in.Size, "operation_id": opID}})
	})
	switch {
	case errors.Is(err, errIdempotencyConflict):
		writeErr(w, 409, "idempotency_conflict", err.Error())
		return
	case err != nil:
		writeErr(w, 422, "invalid_request", err.Error())
		return
	case existing:
		s.writeOperation(w, r, opID, 200)
		return
	}
	if err := s.Authz.Write(r.Context(), authz.T("service", svcID, "project", "project", p)); err != nil {
		s.failOperation(r.Context(), opID, "authz write failed: "+err.Error())
		writeErr(w, 503, "authz_unavailable", err.Error())
		return
	}
	if _, err := s.WF.StartDeploy(r.Context(), opID, svcID); err != nil {
		s.failOperation(r.Context(), opID, "workflow start failed: "+err.Error())
		_, _ = s.Pool.Exec(r.Context(), `UPDATE service SET state='failed', state_reason=$2 WHERE id=$1`, svcID, "workflow start failed")
		writeErr(w, 503, "workflows_unavailable", err.Error())
		return
	}
	s.writeOperation(w, r, opID, 202)
}

func (s *Server) serviceAction(w http.ResponseWriter, r *http.Request) {
	id, action := chi.URLParam(r, "service"), chi.URLParam(r, "action")
	perm := map[string]string{"start": "operate", "stop": "operate", "restart": "operate", "delete": "destroy", "scale": "operate"}[action]
	key := r.Header.Get("Idempotency-Key")
	var scale struct {
		Replicas int `json:"replicas"`
	}
	if action == "scale" {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&scale)
		if !require(w, scale.Replicas >= 1 && scale.Replicas <= kube.MaxReplicas, fmt.Sprintf("scale needs {\"replicas\": 1..%d}", kube.MaxReplicas)) {
			return
		}
	}
	reqID := id
	if action == "scale" {
		reqID = fmt.Sprintf("%s:%d", id, scale.Replicas)
	}
	if !require(w, perm != "", "action must be start, stop, restart, scale or delete") || !require(w, key != "", "Idempotency-Key header is required") || !s.allow(w, r, "service", id, perm) {
		return
	}
	var opID string
	var existing bool
	err := s.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if opID, existing, err = s.startOperation(r.Context(), tx, key, "service."+action, "service", []byte(reqID)); err != nil || existing {
			return err
		}
		var protected bool
		var provider, state string
		if err := tx.QueryRow(r.Context(), `SELECT termination_protected, provider_id, state FROM service WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&protected, &provider, &state); err != nil {
			return fmt.Errorf("service not found")
		}
		if action == "scale" && (provider != "kubernetes" || state != "running") {
			return fmt.Errorf("scale applies to running Kubernetes services; VM services change size instead")
		}
		if action == "delete" && protected {
			return fmt.Errorf("service is termination-protected")
		}
		if _, err := tx.Exec(r.Context(), `UPDATE operation SET target_id=$2 WHERE id=$1`, opID, id); err != nil {
			return err
		}
		return audit.Append(r.Context(), tx, audit.Event{Actor: userFrom(r.Context()).ID, Verb: "service." + action, TargetKind: "service", TargetID: id, Detail: map[string]any{"operation_id": opID}})
	})
	switch {
	case errors.Is(err, errIdempotencyConflict):
		writeErr(w, 409, "idempotency_conflict", err.Error())
		return
	case err != nil:
		writeErr(w, 422, "invalid_request", err.Error())
		return
	case existing:
		s.writeOperation(w, r, opID, 200)
		return
	}
	wfAction := action
	if action == "scale" {
		wfAction = fmt.Sprintf("scale:%d", scale.Replicas)
	}
	if _, err := s.WF.StartAction(r.Context(), opID, id, wfAction); err != nil {
		s.failOperation(r.Context(), opID, "workflow start failed: "+err.Error())
		writeErr(w, 503, "workflows_unavailable", err.Error())
		return
	}
	s.writeOperation(w, r, opID, 202)
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "service")
	if !s.allow(w, r, "service", id, "view") {
		return
	}
	s.queryJSON(w, r, `SELECT id, kind, status, restic_snapshot_id, restored_from, bytes_added, error, started_at, finished_at
		FROM backup_snapshot WHERE service_id=$1 ORDER BY started_at DESC LIMIT 100`, id)
}

func (s *Server) createBackup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "service")
	s.backupOp(w, r, id, "operate", "service.backup", []byte(id), func(opID string) error {
		_, err := s.WF.StartBackup(r.Context(), opID, id)
		return err
	})
}

// restoreBackup overwrites the service's current data, so it needs the destroy permission.
func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	id, b := chi.URLParam(r, "service"), chi.URLParam(r, "backup")
	s.backupOp(w, r, id, "destroy", "service.restore", []byte(id+"|"+b), func(opID string) error {
		_, err := s.WF.StartRestore(r.Context(), opID, id, b)
		return err
	})
}

func (s *Server) backupOp(w http.ResponseWriter, r *http.Request, id, perm, verb string, body []byte, start func(string) error) {
	key := r.Header.Get("Idempotency-Key")
	if !require(w, key != "", "Idempotency-Key header is required") || !s.allow(w, r, "service", id, perm) {
		return
	}
	var provider string
	if err := s.Pool.QueryRow(r.Context(), `SELECT provider_id FROM service WHERE id::text=$1`, id).Scan(&provider); err != nil {
		writeErr(w, 404, "not_found", "service not found")
		return
	}
	if !require(w, provider != "kubernetes", "backups are not yet available for Kubernetes services (volume snapshots/Velero not implemented)") {
		return
	}
	var opID string
	var existing bool
	err := s.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if opID, existing, err = s.startOperation(r.Context(), tx, key, verb, "service", body); err != nil || existing {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE operation SET target_id=$2 WHERE id=$1`, opID, id); err != nil {
			return err
		}
		return audit.Append(r.Context(), tx, audit.Event{Actor: userFrom(r.Context()).ID, Verb: verb, TargetKind: "service", TargetID: id, Detail: map[string]any{"operation_id": opID}})
	})
	switch {
	case errors.Is(err, errIdempotencyConflict):
		writeErr(w, 409, "idempotency_conflict", err.Error())
		return
	case err != nil:
		writeErr(w, 422, "invalid_request", err.Error())
		return
	case existing:
		s.writeOperation(w, r, opID, 200)
		return
	}
	if err := start(opID); err != nil {
		s.failOperation(r.Context(), opID, "workflow start failed: "+err.Error())
		writeErr(w, 503, "workflows_unavailable", err.Error())
		return
	}
	s.writeOperation(w, r, opID, 202)
}

func (s *Server) failOperation(ctx context.Context, id, msg string) {
	_, _ = s.Pool.Exec(ctx, `UPDATE operation SET status='failed', error=$2, updated_at=now() WHERE id=$1`, id, msg)
}

func (s *Server) writeOperation(w http.ResponseWriter, r *http.Request, id string, code int) {
	row, err := s.one(r.Context(), `SELECT id, verb, target_kind, target_id, status, error, workflow_id, response, created_at, updated_at FROM operation WHERE id=$1`, id)
	if err != nil {
		writeErr(w, 500, "internal", err.Error())
		return
	}
	writeJSON(w, code, row)
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "op")
	var actor string
	if err := s.Pool.QueryRow(r.Context(), `SELECT actor_user_id::text FROM operation WHERE id=$1`, id).Scan(&actor); err != nil || actor != userFrom(r.Context()).ID {
		writeErr(w, 404, "not_found", "operation not found")
		return
	}
	s.writeOperation(w, r, id, 200)
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target_id")
	kind := r.URL.Query().Get("target_kind")
	if !require(w, target != "" && (kind == "project" || kind == "service" || kind == "organization"), "target_kind and target_id are required") {
		return
	}
	if !s.allow(w, r, kind, target, "view") {
		return
	}
	s.queryJSON(w, r, `SELECT seq, occurred_at, actor, verb, target_kind, target_id, detail, hash FROM audit_event WHERE target_id=$1 ORDER BY seq DESC LIMIT 500`, target)
}

func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request) {
	broken, err := audit.Verify(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, 500, "internal", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"intact": broken == 0, "first_broken_seq": broken})
}

// paymentVerified credits a wallet after the payments service has verified a provider charge server-side.
// The body must be HMAC-SHA256 signed with InternalSecret (header X-Spade-Signature).
func (s *Server) paymentVerified(w http.ResponseWriter, r *http.Request) {
	if s.InternalSecret == "" {
		writeErr(w, 503, "payments_unconfigured", "internal payment hook disabled: SPADE_INTERNAL_SECRET not set")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	mac := hmac.New(sha256.New, []byte(s.InternalSecret))
	mac.Write(body)
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(r.Header.Get("X-Spade-Signature"))) {
		writeErr(w, 401, "bad_signature", "signature mismatch")
		return
	}
	var in struct {
		OrgID       string          `json:"org_id"`
		Provider    string          `json:"provider"`
		ProviderRef string          `json:"provider_ref"`
		AmountMinor int64           `json:"amount_minor"`
		Currency    string          `json:"currency"`
		Raw         json.RawMessage `json:"raw"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.OrgID == "" || in.ProviderRef == "" || in.AmountMinor <= 0 {
		writeErr(w, 400, "bad_request", "org_id, provider_ref and positive amount_minor are required")
		return
	}
	if in.Currency == "" {
		in.Currency = "NGN"
	}
	if len(in.Raw) == 0 {
		in.Raw = json.RawMessage("{}")
	}
	_, _, clearing, _, err := s.Ledger.PlatformAccounts(r.Context(), in.Currency)
	if err != nil {
		writeErr(w, 503, "ledger_unavailable", err.Error())
		return
	}
	if err := s.ensureWallet(r.Context(), in.OrgID, in.Currency); err != nil {
		writeErr(w, 503, "ledger_unavailable", err.Error())
		return
	}
	ref := in.Provider + ":" + in.ProviderRef
	err = s.tx(r.Context(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `INSERT INTO payment(org_id,provider,provider_ref,amount_minor,currency,status,verified_at,raw_event)
			VALUES ($1,$2,$3,$4,$5,'verified',now(),$6) ON CONFLICT (provider,provider_ref) DO NOTHING`, in.OrgID, in.Provider, in.ProviderRef, in.AmountMinor, in.Currency, []byte(in.Raw)); err != nil {
			return err
		}
		if err := ledger.Enqueue(r.Context(), tx, ledger.Intent{ID: ids.U128("topup", ref), Kind: "topup", OrgID: in.OrgID, Debit: clearing,
			Credit: ledger.WalletID(in.OrgID, in.Currency), Amount: in.AmountMinor, Currency: in.Currency, Reference: ref}); err != nil {
			return err
		}
		return audit.Append(r.Context(), tx, audit.Event{Actor: "system:payments", Verb: "wallet.topup", TargetKind: "organization", TargetID: in.OrgID, Detail: map[string]any{"ref": ref, "amount_minor": in.AmountMinor}})
	})
	if err != nil {
		writeErr(w, 400, "failed", err.Error())
		return
	}
	res, err := s.Ledger.Relay(r.Context(), 8000)
	if err != nil {
		writeErr(w, 503, "ledger_unavailable", "credit queued but not yet posted: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"queued": true, "committed": res.Committed})
}

// ---- helpers ----

// ensureWallet idempotently (re)creates an org's TigerBeetle wallet, repairing orgs whose wallet
// creation failed after the org row committed.
func (s *Server) ensureWallet(ctx context.Context, org, currency string) error {
	var mode string
	if err := s.Pool.QueryRow(ctx, `SELECT billing_mode FROM organization WHERE id=$1`, org).Scan(&mode); err != nil {
		return fmt.Errorf("organization %s: %w", org, err)
	}
	_, err := s.Ledger.EnsureAccount(ctx, "org_wallet", org, currency, mode == "prepaid")
	return err
}

func lockMember(ctx context.Context, tx pgx.Tx, project, user string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7426003, hashtext($1 || ':' || $2))`, project, user)
	return err
}

// reconcileMember makes Permify match the member's *committed* role, read under the member lock, so a
// failed update can never restore a role that a concurrent request has since changed.
func (s *Server) reconcileMember(ctx context.Context, project, user string) {
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := lockMember(ctx, tx, project, user); err != nil {
			return err
		}
		var role string
		if err := tx.QueryRow(ctx, `SELECT role FROM membership WHERE user_id=$1 AND project_id=$2`, user, project).Scan(&role); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := s.Authz.DeleteSubject(ctx, "project", project, "user", user); err != nil {
			return err
		}
		if role == "" {
			return nil
		}
		return s.Authz.Write(ctx, authz.T("project", project, role, "user", user))
	})
	if err != nil {
		s.Log.Error("authz reconcile after failed member change; access may be reduced until the next change", "project", project, "user", user, "err", err)
	}
}

// revokeIfAbsent undoes grants only when the row provably does not exist: a commit whose reply was
// lost may still have succeeded, and revoking then would orphan the creator's own resource.
func (s *Server) revokeIfAbsent(table, id string, ts []authz.Tuple) {
	if len(ts) == 0 {
		return
	}
	var exists bool
	if err := s.Pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id=$1)`, id).Scan(&exists); err != nil || exists {
		if err != nil {
			s.Log.Warn("cannot confirm rollback; keeping grants", "table", table, "id", id, "err", err)
		}
		return
	}
	s.revoke(ts)
}

// revoke best-effort removes grants written inside a transaction that then failed to commit.
func (s *Server) revoke(ts []authz.Tuple) {
	for _, t := range ts {
		if err := s.Authz.Delete(context.Background(), t); err != nil {
			s.Log.Error("authz revoke after failed commit", "entity", t.EntityType, "id", t.EntityID, "relation", t.Relation, "err", err)
		}
	}
}

func writeTxErr(w http.ResponseWriter, err error, kind string) bool {
	var az errAuthz
	switch {
	case err == nil:
		return false
	case errors.As(err, &az):
		writeErr(w, 503, "authz_unavailable", az.Error())
	default:
		writeErr(w, 400, kind, err.Error())
	}
	return true
}

func (s *Server) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) rows(ctx context.Context, q string, args ...any) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToMap)
	if out == nil {
		out = []map[string]any{}
	}
	for _, m := range out {
		for k, v := range m {
			if b, ok := v.([16]byte); ok {
				m[k] = fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
			}
		}
	}
	return out, err
}

func (s *Server) one(ctx context.Context, q string, args ...any) (map[string]any, error) {
	out, err := s.rows(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, pgx.ErrNoRows
	}
	return out[0], nil
}

func (s *Server) queryJSON(w http.ResponseWriter, r *http.Request, q string, args ...any) {
	out, err := s.rows(r.Context(), q, args...)
	if err != nil {
		writeErr(w, 503, "db_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) queryOneJSON(w http.ResponseWriter, r *http.Request, q string, args ...any) {
	out, err := s.one(r.Context(), q, args...)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, 404, "not_found", "not found")
		return
	}
	if err != nil {
		writeErr(w, 503, "db_error", err.Error())
		return
	}
	writeJSON(w, 200, out)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeErr(w, 400, "bad_json", err.Error())
		return false
	}
	return true
}

func require(w http.ResponseWriter, ok bool, msg string) bool {
	if !ok {
		writeErr(w, 422, "invalid_request", msg)
	}
	return ok
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"code": kind, "message": msg}, "at": time.Now().UTC()})
}
