-- Private overlay networks (WireGuard or Nebula) per project. Host private keys never leave the host;
-- only public keys / Nebula host certs are stored here.
ALTER TABLE agent_command DROP CONSTRAINT agent_command_verb_check;
ALTER TABLE agent_command ADD CONSTRAINT agent_command_verb_check CHECK (verb IN
  ('apply_bundle','compose_up','compose_down','compose_restart','health_probe','collect_metrics','backup_local',
   'overlay_keygen','overlay_apply','overlay_down'));

ALTER TABLE project ADD COLUMN overlay_generation bigint NOT NULL DEFAULT 0;

CREATE TABLE overlay_ca (               -- Nebula CA per project; key sealed with the platform KEK
  project_id  uuid PRIMARY KEY REFERENCES project(id),
  ca_crt      text NOT NULL,
  ca_key_sealed bytea NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE overlay_peer (
  service_id   uuid PRIMARY KEY REFERENCES service(id),
  project_id   uuid NOT NULL REFERENCES project(id),
  kind         text NOT NULL CHECK (kind IN ('wireguard','nebula')),
  overlay_ip   inet NOT NULL,
  public_key   text NOT NULL,
  endpoint     text NOT NULL,          -- host:port reachable by other peers
  host_cert    text,                   -- Nebula only
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id, overlay_ip)
);
