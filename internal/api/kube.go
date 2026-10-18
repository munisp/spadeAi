package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/munisp/spadeai/internal/audit"
	"github.com/munisp/spadeai/internal/kube"
)

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// clusterProject authorizes a cluster request through the project that owns the cluster.
func (s *Server) clusterProject(w http.ResponseWriter, r *http.Request, perm string) (string, bool) {
	var p string
	if err := s.Pool.QueryRow(r.Context(), `SELECT project_id::text FROM k8s_cluster WHERE id::text=$1 AND deleted_at IS NULL`, chi.URLParam(r, "cluster")).Scan(&p); err != nil {
		writeErr(w, 404, "not_found", "cluster not found")
		return "", false
	}
	return p, s.allow(w, r, "project", p, perm)
}

// latestInventory is the most recent k8s_inventory result the cluster agent reported.
func latestInventory(ctx context.Context, q queryRower, clusterID string) (kube.Inventory, time.Time, error) {
	var inv kube.Inventory
	var raw []byte
	var at time.Time
	err := q.QueryRow(ctx, `SELECT c.result, c.updated_at FROM agent_command c JOIN agent a ON a.id=c.agent_id
		WHERE a.cluster_id::text=$1 AND c.verb='k8s_inventory' AND c.status='succeeded' ORDER BY c.updated_at DESC LIMIT 1`, clusterID).Scan(&raw, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return inv, at, fmt.Errorf("cluster has not reported its inventory yet; refresh it once the agent is online")
	}
	if err != nil {
		return inv, at, err
	}
	return inv, at, json.Unmarshal(raw, &inv)
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request) {
	p := chi.URLParam(r, "project")
	if !s.allow(w, r, "project", p, "create_service") {
		return
	}
	var in struct {
		Name         string `json:"name"`
		Distribution string `json:"distribution"`
		InNigeria    bool   `json:"in_nigeria"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		writeErr(w, 400, "bad_json", err.Error())
		return
	}
	if in.Distribution == "" {
		in.Distribution = "kubernetes"
	}
	if s.GatewayURL == "" || s.CAPin == "" {
		writeErr(w, 503, "gateway_unconfigured", "agent gateway URL/CA pin not configured on the API")
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeErr(w, 500, "rng", err.Error())
		return
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	exp := time.Now().UTC().Add(time.Hour)
	var id string
	err := s.tx(r.Context(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `INSERT INTO k8s_cluster(project_id,name,distribution,in_nigeria) VALUES ($1,$2,$3,$4) RETURNING id::text`,
			p, in.Name, in.Distribution, in.InNigeria).Scan(&id); err != nil {
			return fmt.Errorf("cluster name must be a unique DNS label (max 40) and distribution one of kubernetes, k3s, rke2, openshift, eks, aks, gke: %w", err)
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO agent(cluster_id, enrol_token_sha256, enrol_expires_at) VALUES ($1,$2,$3)`, id, hex.EncodeToString(sum[:]), exp); err != nil {
			return err
		}
		return audit.Append(r.Context(), tx, audit.Event{Actor: userFrom(r.Context()).ID, Verb: "cluster.create", TargetKind: "k8s_cluster", TargetID: id,
			Detail: map[string]any{"project_id": p, "name": in.Name, "distribution": in.Distribution, "in_nigeria": in.InNigeria}})
	})
	if err != nil {
		writeErr(w, 422, "invalid_request", err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "name": in.Name, "enrol_expires_at": exp,
		"helm": fmt.Sprintf("helm upgrade --install spade-agent deploy/helm/spade-agent -n spade-system --create-namespace --set gateway=%s --set caSha256=%s --set enrolToken=%s", s.GatewayURL, s.CAPin, token)})
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) {
	p := chi.URLParam(r, "project")
	if !s.allow(w, r, "project", p, "view") {
		return
	}
	s.queryJSON(w, r, `SELECT c.id, c.name, c.distribution, c.in_nigeria, c.created_at, a.cert_fingerprint IS NOT NULL AS enrolled,
		coalesce(a.last_seen_at > now() - interval '2 minutes', false) AS online, a.last_seen_at, a.agent_version,
		(SELECT count(*) FROM service s WHERE s.k8s_cluster_id=c.id AND s.deleted_at IS NULL) AS services
		FROM k8s_cluster c JOIN agent a ON a.cluster_id=c.id WHERE c.project_id=$1 AND c.deleted_at IS NULL ORDER BY c.created_at`, p)
}

func (s *Server) refreshCluster(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.clusterProject(w, r, "create_service"); !ok {
		return
	}
	var cmd string
	err := s.Pool.QueryRow(r.Context(), `INSERT INTO agent_command(agent_id, verb, payload)
		SELECT id, 'k8s_inventory', '{"project":"inventory"}' FROM agent WHERE cluster_id::text=$1 AND cert_fingerprint IS NOT NULL RETURNING id::text`,
		chi.URLParam(r, "cluster")).Scan(&cmd)
	if err != nil {
		writeErr(w, 409, "agent_not_enrolled", "the cluster agent has not enrolled yet")
		return
	}
	writeJSON(w, 202, map[string]string{"command_id": cmd})
}

type field struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Type     string   `json:"type"`
	Required bool     `json:"required,omitempty"`
	Options  []string `json:"options,omitempty"`
	Default  any      `json:"default,omitempty"`
	Min      int      `json:"min,omitempty"`
	Max      int      `json:"max,omitempty"`
	Help     string   `json:"help,omitempty"`
}

// clusterOptions is the Kubernetes configuration menu, populated from what the cluster itself reported.
func (s *Server) clusterOptions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.clusterProject(w, r, "view"); !ok {
		return
	}
	id := chi.URLParam(r, "cluster")
	inv, at, err := latestInventory(r.Context(), s.Pool, id)
	if err != nil {
		writeErr(w, 409, "inventory_missing", err.Error())
		return
	}
	sizes, err := s.rows(r.Context(), `SELECT id, code, vcpu, ram_mb, disk_gb FROM size WHERE provider_id='kubernetes' ORDER BY vcpu`)
	if err != nil {
		writeErr(w, 503, "db", err.Error())
		return
	}
	names := func(cs []kube.Class) (out []string, def string) {
		for _, c := range cs {
			out = append(out, c.Name)
			if c.Default {
				def = c.Name
			}
		}
		return
	}
	var sizeIDs []string
	for _, z := range sizes {
		sizeIDs = append(sizeIDs, fmt.Sprint(z["id"]))
	}
	sc, scDef := names(inv.StorageClasses)
	ic, icDef := names(inv.IngressClasses)
	fields := []field{
		{Name: "size", Label: "Size per replica", Type: "select", Required: true, Options: sizeIDs, Default: "kubernetes:SMALL-1C-2G"},
		{Name: "replicas", Label: "Replicas", Type: "int", Required: true, Min: 1, Max: kube.MaxReplicas, Default: 1, Help: "templates with persistent volumes run 1 replica"},
		{Name: "namespace", Label: "Namespace", Type: "text", Required: true, Help: "DNS label; not default, spade-system, kube-* or openshift*"},
		{Name: "storage_class", Label: "Storage class", Type: "select", Options: sc, Default: scDef},
		{Name: "storage_gb", Label: "Storage (GiB)", Type: "int", Min: 1, Max: 1000, Help: "defaults to the size's disk"},
		{Name: "host", Label: "Domain (Ingress host)", Type: "text", Help: "leave empty for cluster-internal only"},
		{Name: "ingress_class", Label: "Ingress class", Type: "select", Options: ic, Default: icDef},
		{Name: "tls_issuer", Label: "TLS certificate issuer (cert-manager)", Type: "select", Options: inv.TLSIssuers},
		{Name: "network_policy", Label: "Network policy", Type: "select", Options: []string{"isolated", "open"}, Default: "isolated",
			Help: "isolated: only pods in the namespace (and the ingress controller) can connect"},
		{Name: "ingress_namespace", Label: "Ingress controller namespace", Type: "text", Default: "kube-system"},
		{Name: "env", Label: "Environment variables", Type: "map"},
	}
	writeJSON(w, 200, map[string]any{"cluster_id": id, "inventory_at": at, "kubernetes_version": inv.Version, "nodes": inv.Nodes,
		"allocatable_cpu_milli": inv.CPUMilli, "allocatable_memory_mb": inv.MemoryMB, "sizes": sizes, "fields": fields})
}

// KubeSpec is the "kubernetes" block of a deploy request.
type KubeSpec struct {
	ClusterID string `json:"cluster_id"`
	Replicas  *int   `json:"replicas,omitempty"`
	kube.Config
}

// prepareKube validates a Kubernetes deploy inside the create transaction and returns the stored config.
func prepareKube(ctx context.Context, tx pgx.Tx, project, tvID, size string, k *KubeSpec, env map[string]string) (inNigeria bool, cfg []byte, err error) {
	if k == nil || k.ClusterID == "" {
		return false, nil, fmt.Errorf("kubernetes deploys need kubernetes.cluster_id and kubernetes.namespace")
	}
	if k.Replicas == nil {
		one := 1
		k.Replicas = &one
	}
	if err := tx.QueryRow(ctx, `SELECT in_nigeria FROM k8s_cluster WHERE id::text=$1 AND project_id=$2 AND deleted_at IS NULL`, k.ClusterID, project).Scan(&inNigeria); err != nil {
		return false, nil, fmt.Errorf("cluster %s is not registered in this project", k.ClusterID)
	}
	inv, _, err := latestInventory(ctx, tx, k.ClusterID)
	if err != nil {
		return false, nil, err
	}
	var vcpu, ram, disk int
	var compose string
	var manifest []byte
	if err := tx.QueryRow(ctx, `SELECT z.vcpu, z.ram_mb, z.disk_gb, tv.compose, tv.manifest FROM size z, template_version tv WHERE z.id=$1 AND tv.id=$2`, size, tvID).
		Scan(&vcpu, &ram, &disk, &compose, &manifest); err != nil {
		return false, nil, err
	}
	if err := kube.Validate(&k.Config, inv, *k.Replicas, disk); err != nil {
		return false, nil, err
	}
	var m struct {
		Health kube.Health `json:"health"`
	}
	_ = json.Unmarshal(manifest, &m)
	if _, err := kube.Render(compose, m.Health, k.Config, env, *k.Replicas, vcpu, ram, "check"); err != nil {
		return false, nil, fmt.Errorf("template cannot run on Kubernetes: %w", err)
	}
	cfg, err = json.Marshal(k.Config)
	return inNigeria, cfg, err
}
