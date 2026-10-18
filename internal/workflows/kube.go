package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/munisp/spadeai/internal/kube"
)

// agentOf joins a service (s) to the agent (a) that runs it: its own host agent, or its cluster's agent.
const agentOf = `(a.service_id=s.id OR (s.k8s_cluster_id IS NOT NULL AND a.cluster_id=s.k8s_cluster_id))`

// ServiceProvider lets workflows branch deterministically on the provider (e.g. no overlay on Kubernetes).
func (a *Activities) ServiceProvider(ctx context.Context, serviceID string) (string, error) {
	var p string
	err := a.Pool.QueryRow(ctx, `SELECT provider_id FROM service WHERE id=$1`, serviceID).Scan(&p)
	return p, err
}

// provisionKube creates no infrastructure: the customer's cluster already runs an enrolled agent.
func (a *Activities) provisionKube(ctx context.Context, serviceID string) (*ProvisionResult, error) {
	var name, ns string
	var enrolled bool
	err := a.Pool.QueryRow(ctx, `SELECT c.name, s.k8s_config->>'namespace', a.cert_fingerprint IS NOT NULL FROM service s
		JOIN k8s_cluster c ON c.id=s.k8s_cluster_id AND c.deleted_at IS NULL JOIN agent a ON a.cluster_id=c.id WHERE s.id=$1`, serviceID).Scan(&name, &ns, &enrolled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nonRetryable("service %s has no registered Kubernetes cluster", serviceID)
	}
	if err != nil {
		return nil, err
	}
	if !enrolled {
		return nil, nonRetryable("cluster %s has not enrolled its agent yet (run the Helm install shown when the cluster was added)", name)
	}
	return &ProvisionResult{Bootstrap: fmt.Sprintf("kubernetes cluster %s, namespace %s", name, ns)}, nil
}

// teardownKube deletes the service's namespace through the cluster agent and waits for it.
func (a *Activities) teardownKube(ctx context.Context, serviceID string) error {
	id, err := a.QueueCommand(ctx, serviceID, "k8s_teardown")
	if err != nil {
		return err
	}
	return a.WaitCommand(ctx, id)
}

type kubeRow struct {
	Cfg            kube.Config
	Replicas       int
	VCPU, RAM, Dsk int
}

// kubePayload builds the cluster-agent payload; apply_bundle carries the rendered, hashed manifests.
func (a *Activities) kubePayload(ctx context.Context, serviceID, verb, compose string, manifest, env []byte) (map[string]any, error) {
	var r kubeRow
	var cfg []byte
	if err := a.Pool.QueryRow(ctx, `SELECT s.k8s_config, s.replicas, z.vcpu, z.ram_mb, z.disk_gb FROM service s JOIN size z ON z.id=s.size_id WHERE s.id=$1`, serviceID).
		Scan(&cfg, &r.Replicas, &r.VCPU, &r.RAM, &r.Dsk); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(cfg, &r.Cfg); err != nil {
		return nil, err
	}
	var m struct {
		Health kube.Health `json:"health"`
	}
	_ = json.Unmarshal(manifest, &m)
	var e map[string]string
	_ = json.Unmarshal(env, &e)
	b, err := kube.Render(compose, m.Health, r.Cfg, e, r.Replicas, r.VCPU, r.RAM, serviceID)
	if err != nil {
		return nil, nonRetryable("render for Kubernetes: %v", err)
	}
	p := map[string]any{"project": "spade-" + serviceID[:8], "namespace": r.Cfg.Namespace, "service_id": serviceID, "replicas": r.Replicas}
	switch verb {
	case "apply_bundle":
		p["manifests"], p["manifests_sha256"] = b.Manifests, sha256hex(b.Manifests)
	case "health_probe":
		p["health"] = map[string]any{"url": b.HealthURL, "expect_status": m.Health.Expect}
	case "compose_up", "compose_down", "compose_restart", "collect_metrics", "k8s_teardown":
	default:
		return nil, nonRetryable("%s is not supported on Kubernetes services", verb)
	}
	return p, nil
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// SetReplicas stores a new replica count and, if the service is billing, closes the current interval and
// opens one at the new count so metering charges each period at the replicas it actually ran.
func (a *Activities) SetReplicas(ctx context.Context, serviceID string, replicas int) (int, error) {
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var prev int
	if err := tx.QueryRow(ctx, `SELECT replicas FROM service WHERE id=$1 AND provider_id='kubernetes' FOR UPDATE`, serviceID).Scan(&prev); err != nil {
		return 0, nonRetryable("service %s is not a Kubernetes service", serviceID)
	}
	if prev == replicas {
		return prev, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE service SET replicas=$2, updated_at=now() WHERE id=$1`, serviceID, replicas); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `WITH closed AS (UPDATE service_state_interval SET ended_at=now() WHERE service_id=$1 AND ended_at IS NULL RETURNING state)
		INSERT INTO service_state_interval(service_id, state, replicas) SELECT $1, state, $2 FROM closed`, serviceID, replicas); err != nil {
		return 0, err
	}
	return prev, tx.Commit(ctx)
}
