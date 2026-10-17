-- Encrypted, off-host backups with restic. The per-service repository password is sealed with the
-- platform KEK and only injected into the command at delivery time; it is never stored in agent_command.
ALTER TABLE agent_command DROP CONSTRAINT agent_command_verb_check;
ALTER TABLE agent_command ADD CONSTRAINT agent_command_verb_check CHECK (verb IN
  ('apply_bundle','compose_up','compose_down','compose_restart','health_probe','collect_metrics','backup_local',
   'overlay_keygen','overlay_apply','overlay_down','backup_run','backup_restore'));

CREATE TABLE service_backup_config (
  service_id      uuid PRIMARY KEY REFERENCES service(id),
  password_sealed bytea NOT NULL,
  keep_daily      int NOT NULL DEFAULT 7 CHECK (keep_daily BETWEEN 1 AND 365),
  enabled         boolean NOT NULL DEFAULT true,
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE backup_snapshot (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  service_id         uuid NOT NULL REFERENCES service(id),
  operation_id       uuid REFERENCES operation(id),
  kind               text NOT NULL CHECK (kind IN ('backup','restore')),
  status             text NOT NULL DEFAULT 'running' CHECK (status IN ('running','succeeded','failed')),
  restic_snapshot_id text,
  restored_from      uuid REFERENCES backup_snapshot(id),
  bytes_added        bigint,
  error              text,
  started_at         timestamptz NOT NULL DEFAULT now(),
  finished_at        timestamptz
);
CREATE INDEX backup_snapshot_service ON backup_snapshot(service_id, started_at DESC);
