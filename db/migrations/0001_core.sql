-- SpadeAi core schema. Money lives in TigerBeetle; Postgres holds metadata and the transfer outbox.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE organization (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name          text NOT NULL,
  country       char(2) NOT NULL DEFAULT 'NG',
  billing_mode  text NOT NULL DEFAULT 'prepaid' CHECK (billing_mode IN ('prepaid','postpaid')),
  kyc_status    text NOT NULL DEFAULT 'pending' CHECK (kyc_status IN ('pending','verified','rejected')),
  ai_policy     text NOT NULL DEFAULT 'self_hosted' CHECK (ai_policy IN ('self_hosted','external','hybrid')),
  residency_locked boolean NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE app_user (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  subject     text NOT NULL UNIQUE,      -- OIDC sub
  email       text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE project (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id          uuid NOT NULL REFERENCES organization(id),
  name            text NOT NULL,
  technical_email text NOT NULL,
  overlay_kind    text NOT NULL DEFAULT 'wireguard' CHECK (overlay_kind IN ('nebula','wireguard')),
  overlay_cidr    cidr NOT NULL DEFAULT '10.42.0.0/16',
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  deleted_at      timestamptz,
  UNIQUE (org_id, name)
);

CREATE TABLE membership (
  user_id    uuid NOT NULL REFERENCES app_user(id),
  project_id uuid NOT NULL REFERENCES project(id),
  role       text NOT NULL CHECK (role IN ('owner','admin','operator','viewer','billing')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, project_id)
);

CREATE TABLE template (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug          text NOT NULL UNIQUE,
  name          text NOT NULL,
  category      text NOT NULL,
  licence_spdx  text NOT NULL,
  description   text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE template_version (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  template_id   uuid NOT NULL REFERENCES template(id),
  version       text NOT NULL,
  manifest      jsonb NOT NULL,          -- parsed spade.yml
  compose       text NOT NULL,
  compose_sha256 text NOT NULL,
  min_vcpu      int NOT NULL DEFAULT 1,
  min_ram_mb    int NOT NULL DEFAULT 1024,
  tested_at     timestamptz,             -- only tested versions are deployable
  test_report   jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (template_id, version)
);

CREATE TABLE provider (
  id         text PRIMARY KEY,           -- 'byovm', 'proxmox', 'hetzner'...
  kind       text NOT NULL CHECK (kind IN ('api','byo_account','byovm')),
  name       text NOT NULL
);

CREATE TABLE size (
  id            text PRIMARY KEY,        -- e.g. 'byovm:MEDIUM-2C-4G'
  provider_id   text NOT NULL REFERENCES provider(id),
  code          text NOT NULL,
  vcpu          int NOT NULL,
  ram_mb        int NOT NULL,
  disk_gb       int NOT NULL,
  UNIQUE (provider_id, code)
);

CREATE TABLE price_book (
  version        int PRIMARY KEY,
  status         text NOT NULL CHECK (status IN ('draft','approved','effective','retired')),
  effective_from timestamptz NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE price_item (
  price_book_version int NOT NULL REFERENCES price_book(version),
  sku           text NOT NULL,           -- 'mgmt.vcpu.hour', 'mgmt.ram_gb.hour', 'mgmt.disk_gb.hour', 'support.l2.hour'
  unit          text NOT NULL,
  price_minor   bigint NOT NULL CHECK (price_minor >= 0),   -- kobo per unit, scaled by 1/1000 (milli-kobo)
  currency      char(3) NOT NULL DEFAULT 'NGN',
  PRIMARY KEY (price_book_version, sku, currency)
);

CREATE TABLE service (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id          uuid NOT NULL REFERENCES project(id),
  template_version_id uuid NOT NULL REFERENCES template_version(id),
  display_name        text NOT NULL,
  kind                text NOT NULL DEFAULT 'app' CHECK (kind IN ('app','cluster_node','lb','cicd_target')),
  state               text NOT NULL DEFAULT 'requested' CHECK (state IN
    ('requested','provisioning_vm','bootstrapping_agent','installing_app','configuring_tls','running',
     'stopped','updating','resizing','restoring','maintenance','suspended','deleting','deleted','failed')),
  state_reason        text,
  provider_id         text NOT NULL REFERENCES provider(id),
  size_id             text NOT NULL REFERENCES size(id),
  provider_ref        text,
  ipv4                inet,
  overlay_ip          inet,
  support_plan        text NOT NULL DEFAULT 'L1' CHECK (support_plan IN ('L1','L2','L3')),
  maintenance_window  text NOT NULL DEFAULT 'SUN 02:00-04:00 UTC',
  termination_protected boolean NOT NULL DEFAULT false,
  env                 jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  deleted_at          timestamptz
);
CREATE INDEX service_project_idx ON service(project_id) WHERE deleted_at IS NULL;

-- State intervals drive metering: one open row per service while billable.
CREATE TABLE service_state_interval (
  id          bigserial PRIMARY KEY,
  service_id  uuid NOT NULL REFERENCES service(id),
  state       text NOT NULL,
  started_at  timestamptz NOT NULL DEFAULT now(),
  ended_at    timestamptz,
  CHECK (ended_at IS NULL OR ended_at >= started_at)
);
CREATE UNIQUE INDEX service_state_interval_open_uq ON service_state_interval(service_id) WHERE ended_at IS NULL;

CREATE TABLE agent (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  service_id      uuid NOT NULL UNIQUE REFERENCES service(id),
  enrol_token_sha256 text,               -- single-use; nulled after enrolment
  enrol_expires_at timestamptz,
  cert_fingerprint text,
  last_seen_at    timestamptz,
  agent_version   text,
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE agent_command (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  agent_id    uuid NOT NULL REFERENCES agent(id),
  verb        text NOT NULL CHECK (verb IN ('apply_bundle','compose_up','compose_down','compose_restart','health_probe','collect_metrics','backup_local')),
  payload     jsonb NOT NULL DEFAULT '{}'::jsonb,
  status      text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','delivered','succeeded','failed','expired')),
  result      jsonb,
  expires_at  timestamptz NOT NULL DEFAULT now() + interval '10 minutes',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX agent_command_queue_idx ON agent_command(agent_id, created_at) WHERE status = 'queued';

CREATE TABLE operation (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  idempotency_key text NOT NULL,
  actor_user_id   uuid NOT NULL REFERENCES app_user(id),
  request_hash    text NOT NULL,
  verb            text NOT NULL,
  target_kind     text NOT NULL,
  target_id       uuid,
  status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','succeeded','failed')),
  error           text,
  workflow_id     text,
  response        jsonb,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (actor_user_id, idempotency_key)
);

-- Append-only, hash-chained audit log.
CREATE TABLE audit_event (
  seq          bigserial PRIMARY KEY,
  occurred_at  timestamptz NOT NULL DEFAULT now(),
  actor        text NOT NULL,
  verb         text NOT NULL,
  target_kind  text NOT NULL,
  target_id    text,
  detail       jsonb NOT NULL DEFAULT '{}'::jsonb,
  prev_hash    text NOT NULL,
  hash         text NOT NULL UNIQUE
);
CREATE OR REPLACE FUNCTION audit_event_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'audit_event is append-only'; END $$;
CREATE TRIGGER audit_event_no_update BEFORE UPDATE OR DELETE ON audit_event
  FOR EACH ROW EXECUTE FUNCTION audit_event_immutable();

-- Financial: TigerBeetle account map + transfer outbox.
CREATE TABLE ledger_account_map (
  owner_kind   text NOT NULL CHECK (owner_kind IN ('org_wallet','platform_revenue','vat_liability','payment_clearing','writeoff')),
  owner_id     uuid,                     -- org id for wallets; NULL for platform accounts
  currency     char(3) NOT NULL,
  tb_account_id numeric(39,0) NOT NULL UNIQUE,  -- u128
  tb_ledger    int NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ledger_account_map_owner_uq ON ledger_account_map(owner_kind, COALESCE(owner_id,'00000000-0000-0000-0000-000000000000'::uuid), currency);

CREATE TABLE meter_reading (
  service_id    uuid NOT NULL REFERENCES service(id),
  sku           text NOT NULL,
  period_start  timestamptz NOT NULL,
  quantity_milli bigint NOT NULL CHECK (quantity_milli >= 0),  -- thousandths of a unit
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_id, sku, period_start)
);

CREATE TABLE transfer_intent (
  tb_transfer_id numeric(39,0) PRIMARY KEY,   -- deterministic u128
  kind          text NOT NULL CHECK (kind IN ('usage','vat','topup','hold','hold_void','arrears_settlement','writeoff')),
  org_id        uuid NOT NULL REFERENCES organization(id),
  debit_account numeric(39,0) NOT NULL,
  credit_account numeric(39,0) NOT NULL,
  amount_minor  bigint NOT NULL CHECK (amount_minor > 0),
  currency      char(3) NOT NULL,
  reference     text NOT NULL,
  status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','committed','unpaid','failed','settled')),
  tb_result     text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX transfer_intent_pending_idx ON transfer_intent(created_at) WHERE status = 'pending';
CREATE INDEX transfer_intent_arrears_idx ON transfer_intent(org_id, created_at) WHERE status = 'unpaid';

CREATE TABLE payment (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id        uuid NOT NULL REFERENCES organization(id),
  provider      text NOT NULL CHECK (provider IN ('paystack','flutterwave','remita','stripe')),
  provider_ref  text NOT NULL,
  amount_minor  bigint NOT NULL CHECK (amount_minor > 0),
  currency      char(3) NOT NULL,
  status        text NOT NULL DEFAULT 'initiated' CHECK (status IN ('initiated','verified','failed')),
  verified_at   timestamptz,
  raw_event     jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, provider_ref)
);

CREATE TABLE capability_state (
  component   text PRIMARY KEY,
  status      text NOT NULL CHECK (status IN ('available','degraded','unavailable')),
  reason      text NOT NULL DEFAULT '',
  checked_at  timestamptz NOT NULL DEFAULT now()
);

-- Seed reference data.
INSERT INTO provider (id, kind, name) VALUES ('byovm','byovm','Bring your own VM');
INSERT INTO size (id, provider_id, code, vcpu, ram_mb, disk_gb) VALUES
  ('byovm:SMALL-1C-2G','byovm','SMALL-1C-2G',1,2048,40),
  ('byovm:MEDIUM-2C-4G','byovm','MEDIUM-2C-4G',2,4096,80),
  ('byovm:LARGE-4C-8G','byovm','LARGE-4C-8G',4,8192,160);
-- Draft price book: values are placeholders pending commercial approval (milli-kobo per unit-hour).
INSERT INTO price_book (version, status, effective_from) VALUES (1, 'effective', '2026-01-01T00:00:00Z');
INSERT INTO price_item (price_book_version, sku, unit, price_minor, currency) VALUES
  (1,'mgmt.vcpu.hour','vcpu-hour', 1000000,'NGN'),
  (1,'mgmt.ram_gb.hour','gb-hour',  500000,'NGN'),
  (1,'mgmt.disk_gb.hour','gb-hour',   5000,'NGN'),
  (1,'support.L2.hour','service-hour', 9000000,'NGN'),
  (1,'support.L3.hour','service-hour',36000000,'NGN');
