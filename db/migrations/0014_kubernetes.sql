-- Kubernetes target: customers connect their own cluster (on-premises, data centre or cloud) by installing
-- the SpadeAi agent with Helm. One outbound-only agent per cluster serves every service deployed to it;
-- each service lives in its own namespace. Sizes are per replica; billing multiplies by replicas.
ALTER TABLE provider DROP CONSTRAINT provider_kind_check;
ALTER TABLE provider ADD CONSTRAINT provider_kind_check CHECK (kind IN ('api','byo_account','byovm','kubernetes'));
-- Residency comes from the cluster's declared location, not the provider row.
INSERT INTO provider (id, kind, name, in_nigeria) VALUES ('kubernetes','kubernetes','Your Kubernetes cluster', false) ON CONFLICT DO NOTHING;
INSERT INTO size (id, provider_id, code, vcpu, ram_mb, disk_gb) VALUES
  ('kubernetes:SMALL-1C-2G','kubernetes','SMALL-1C-2G',1,2048,10),
  ('kubernetes:MEDIUM-2C-4G','kubernetes','MEDIUM-2C-4G',2,4096,20),
  ('kubernetes:LARGE-4C-8G','kubernetes','LARGE-4C-8G',4,8192,40)
ON CONFLICT DO NOTHING;

CREATE TABLE k8s_cluster (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id    uuid NOT NULL REFERENCES project(id),
  name          text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
  distribution  text NOT NULL DEFAULT 'kubernetes' CHECK (distribution IN ('kubernetes','k3s','rke2','openshift','eks','aks','gke')),
  in_nigeria    boolean NOT NULL DEFAULT false,  -- customer's declaration of where the cluster runs
  created_at    timestamptz NOT NULL DEFAULT now(),
  deleted_at    timestamptz
);
CREATE UNIQUE INDEX k8s_cluster_name_uq ON k8s_cluster(project_id, name) WHERE deleted_at IS NULL;

ALTER TABLE agent ALTER COLUMN service_id DROP NOT NULL;
ALTER TABLE agent ADD COLUMN cluster_id uuid UNIQUE REFERENCES k8s_cluster(id);
ALTER TABLE agent ADD CONSTRAINT agent_owner_xor CHECK ((service_id IS NULL) <> (cluster_id IS NULL));

ALTER TABLE service ADD COLUMN replicas int NOT NULL DEFAULT 1 CHECK (replicas BETWEEN 1 AND 20);
ALTER TABLE service ADD COLUMN k8s_cluster_id uuid REFERENCES k8s_cluster(id);
ALTER TABLE service ADD COLUMN k8s_config jsonb;
ALTER TABLE service ADD CONSTRAINT service_k8s_shape CHECK ((provider_id = 'kubernetes') = (k8s_cluster_id IS NOT NULL AND k8s_config ? 'namespace'));
CREATE UNIQUE INDEX service_k8s_namespace_uq ON service(k8s_cluster_id, (k8s_config->>'namespace')) WHERE deleted_at IS NULL AND k8s_cluster_id IS NOT NULL;

-- Metering bills each interval at the replica count it ran with; scaling closes and reopens the interval.
ALTER TABLE service_state_interval ADD COLUMN replicas int NOT NULL DEFAULT 1 CHECK (replicas >= 1);

ALTER TABLE agent_command DROP CONSTRAINT agent_command_verb_check;
ALTER TABLE agent_command ADD CONSTRAINT agent_command_verb_check CHECK (verb IN
  ('apply_bundle','compose_up','compose_down','compose_restart','health_probe','collect_metrics','backup_local',
   'overlay_keygen','overlay_apply','overlay_down','backup_run','backup_restore','k8s_inventory','k8s_teardown'));
