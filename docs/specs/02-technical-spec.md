# 02 — Technical Specification

Evidence IDs refer to `00-evidence-ledger.md`. **Stack constraints:**
- Languages: Go, Rust, Python, TypeScript.
- PostgreSQL is the system-of-record datastore for all non-financial state. **TigerBeetle** is the financial ledger: wallets, holds, postings.
- The PWA and the native mobile app (React Native/Expo) consume the same API.
- Preferred OSS: Keycloak, Permify, APISIX, Temporal, Kafka, OpenTelemetry.
- Fail-closed everywhere.

## 1. Domain model (Postgres, schema `core`)
| Entity | Key fields | Notes |
|---|---|---|
| `organization` | id, name, kyc_status, billing_mode (prepaid/postpaid), country, tax_id | Replaces Elestio's user-owned billing (E8) |
| `project` | id, org_id, name, technical_email, overlay_cidr | Billing and network boundary (E20, E23) |
| `membership` | user_id, project_id, role (owner/admin/operator/viewer/billing) | Permify relations |
| `template` | id, slug, category, licence_spdx, versions[], ports[], env_schema, lifecycle_hooks, health_probe, backup_spec, min_size | Catalogue (E28); superset of `elestio.yml` (E17) |
| `template_version` | template_id, version, image_digests, compose_hash, tested_at, test_report_id | Only tested versions are deployable |
| `provider` / `region` / `size` | provider kind (api/byo-account/byovm), region code, vcpu, ram_mb, disk_gb, upstream_cost_minor, currency | Size naming `SIZE-<C>C-<G>G` (E9) |
| `service` | id, project_id, template_version_id, kind (app/cluster_node/lb/cicd_target), state, provider_ref, ipv4/ipv6, overlay_ip, support_plan, maintenance_window, termination_protected | State machine §3 |
| `cluster` | id, topology (single/primary_replica/multi_master), node_count 1–15 | E18 |
| `load_balancer` | id, targets[], rules[], health_checks, sticky, proxy_protocol, header_ops | E19 |
| `domain_binding` | service_id, fqdn, cert_status, acme_account | E13, E22 |
| `firewall_rule` | service_id, direction, proto, port_range, cidr | Seeded from template (E13) |
| `backup_policy` / `backup_artifact` | type (local/remote/snapshot/external_s3), retention, schedule, encryption_key_ref, verified_at | 3-2-1 (E12) |
| `pipeline` / `pipeline_run` | repo, branch, runtime, build/run cmds, webhook_enabled, target_service_id | E17 |
| `bucket` / `bucket_key` | quota, lifecycle, cors, scopes (prefix/IP/expiry) | E21 |
| `operation` | id, idempotency_key, actor, service_id, verb, input, status, temporal_workflow_id | Every mutation |
| `audit_event` | hash-chained, actor, verb, target, before/after digest | Append-only (`getAudits` analogue, E5) |
| `price_book` / `price_item` | version, effective_from, sku, unit, price_minor, currency | Single source for pricing (W4) |
| `meter_reading` | service_id, sku, period_start, quantity | Hourly |
| `ledger_account_map` / `transfer_intent` | maps org/project wallets, revenue, VAT and provider-payable accounts to **TigerBeetle** account IDs (u128); `transfer_intent` is a Postgres outbox row carrying the TigerBeetle transfer ID | TigerBeetle enforces double-entry, balances and `debits_must_not_exceed_credits` on prepaid wallets; Postgres keeps metadata and invoices |
| `capability_state` | component, status (available/degraded/unavailable), reason, checked_at | Honest readiness |

**Conventions:**
- Money is stored as `bigint` minor units plus an ISO currency, never floats.
- Every table has `created_at`, `updated_at` and soft-delete where restore is required (E8: 14-day restore).

## 2. Public API (`/v1`, resource-oriented; fixes W2)
**Auth:**
- OIDC via Keycloak. The PWA uses a BFF with an HttpOnly cookie; mobile uses PKCE; API tokens are scoped PATs (Bearer).
- Never a JWT in the body (W1).
- Every mutating call requires an `Idempotency-Key` and returns an `Operation` (202 plus a polling or SSE URL).

| Area | Endpoints (abridged) | Elestio analogue (E5) |
|---|---|---|
| Catalogue | `GET /templates`, `GET /templates/{slug}`, `GET /templates/{slug}/versions`, `GET /sizes?provider=&region=` | getTemplates, getServerSizes, getDockerTags |
| Projects | `CRUD /projects`, `CRUD /projects/{id}/members` | projects/* |
| Services | `POST /projects/{id}/services`, `GET /services/{id}`, `DELETE /services/{id}`, `POST /services/{id}:{action}` where action ∈ reboot, reset, shutdown, poweroff, poweron, restart-stack, resize, change-version, update-now, clone, resync, move, lock, unlock | createServer, DoActionOnServer, templateAction, moveService |
| Config | `GET/PUT /services/{id}/config` (env + compose, schema-validated), `GET /services/{id}/credentials` (one-time reveal, audited) | getAppCredentials |
| Security | `CRUD /services/{id}/firewall-rules`, `PUT /services/{id}/rate-limit`, `PUT /services/{id}/output-cache`, `CRUD /services/{id}/domains`, `CRUD /ssh-keys` | setRateLimiter, setOutputCache |
| Tools | `POST /services/{id}/sessions` with type ∈ terminal, ide, files, logs: short-lived signed URL through the gateway | startSSHClient, startVSCode, startFileExplorer, startLogTailView |
| Backups | `CRUD /services/{id}/backup-policies`, `POST /services/{id}/backups`, `POST /backups/{id}:restore`, `POST /backups/{id}:verify` | backups/* |
| Volumes | `CRUD /volumes`, `POST /volumes/{id}:attach/detach/resize` (grow-only) | volumes/* |
| Clusters / LB | `CRUD /clusters`, `POST /clusters/{id}/nodes`, `POST /clusters/{id}:failover`; `CRUD /load-balancers` | clusters UI, LB |
| CI/CD | `CRUD /pipelines`, `POST /pipelines/{id}/runs`, `GET /pipeline-runs/{id}/logs` (SSE), `CRUD /registries`, `POST /webhooks/git/{provider}` (HMAC-verified) | cicd/* |
| Storage | `CRUD /buckets`, `CRUD /buckets/{id}/keys`, lifecycle, cors, notifications | object storage |
| Monitoring | `GET /services/{id}/metrics?series=&from=&to=`, `CRUD /alert-rules`, `GET /services/{id}/events` | monitoring backend (E1) |
| AI DevOps (dual LLM backends: self-hosted and external, routed per org policy) | `POST /services/{id}/ai-sessions`, `POST /ai-sessions/{id}:approve-access`, SSE transcript | AI DevOps (E25) |
| Billing | `GET /billing/wallet`, `POST /billing/topups` (Paystack/Flutterwave/Remita/Stripe), `GET /billing/usage?project=`, `GET /invoices`, `PUT /billing/auto-refill` | billings/* |
| Audit | `GET /audit-events?target=` | getAudits |
| Health | `GET /capabilities` returns each integration's honest state | — |

**Documentation and SDKs:**
- An OpenAPI 3.1 spec is generated from source and is the contract.
- TS SDK for the PWA/mobile; Go SDK for the CLI and Terraform provider; Python SDK.

## 3. Service lifecycle (Temporal workflows)
```
requested → provisioning_vm → bootstrapping_agent → installing_app → configuring_tls → running
running ⇄ stopped | updating | resizing | restoring | maintenance
running → suspended (billing) → deleting → deleted (restorable 14d) → purged
any → failed (with compensations; never reported as running)
```
- **DeployService:** reserve funds (TigerBeetle pending transfer) → provider driver `CreateVM` → wait for agent enrolment (mTLS CSR signed by an internal CA; token is single-use and expires in 15 min) → agent pulls the signed template bundle → `compose up` → health probe → ACME → firewall → initial backup → post pending transfer, start metering. Each step is an activity with retry/timeout; failure triggers compensation (destroy VM, void pending transfer).
- **UpdateApp:** runs inside the maintenance window. Pre-update snapshot → `preUpdateCommand` → pull new digests → up → health check → `postUpdateCommand`. On failure, automatic rollback to the snapshot (E14).
- **Backup / Restore:** restic or Borg (E12) to (a) local, (b) remote in-country vault, (c) customer S3. Every restore runs `preRestore`/`postRestore` hooks. A weekly **restore-verification** job restores into a scratch VM and records `verified_at`.
- **Suspend / Delete:** the billing workflow enforces balance ≥ minimum to start (E8). At zero balance: notify, then suspend (stop the stack, keep the disk), then delete after N days with a final backup kept 14 days.
- **AI DevOps:**
  0. **LLM routing:** the `llm_router` picks a backend from the org's `ai_policy`: `self_hosted` (vLLM serving open-weight models in-country), `external` (OpenAI-compatible providers via an allow-list), or `hybrid` (self-hosted first, external as fallback or for complex cases).
     - Residency-bound tenants (gov, regulated) are locked to `self_hosted`.
     - The policy, model used and token usage are recorded per session for audit and billing.
     - If the selected backend is down, the router fails closed: it reports `unavailable` and never silently switches to a backend the policy does not allow.
  1. LLM triage, answer-only.
  2. If shell is needed, the user approves explicitly.
  3. A pre-change snapshot is taken.
  4. The agent opens a recorded PTY with an allow-list policy and a 1h time-box.
  5. A summary and diff are produced.
  6. Credits are debited (1/6, E25).

## 4. Node agent (`spade-agent`, Rust)
- **Overlay:** both supported per project: **Nebula** (lighthouse-based, certificate groups, UDP 4242) and **WireGuard** (kernel mesh; keys distributed by the control plane). Choose at project creation; the agent manages either.
- **Transport:** outbound-only gRPC over mTLS to the regional gateway. No inbound management ports (fixes W3). An optional SSH fallback exists for BYOVM bootstrap only.
- **Responsibilities:**
  - Docker/Compose lifecycle; signed bundle verification (cosign/minisign digest pinning).
  - Host Nginx or Caddy config render; ACME.
  - nftables firewall.
  - Rate-limit/cache config.
  - Metrics (node exporter-style) streamed every 10 s.
  - Log tail streaming.
  - Backup execution.
  - Overlay network (Nebula or WireGuard, per project, E20).
  - PTY multiplexing for the web terminal.
  - On-demand code-server / file browser behind gateway auth (E16).
- **Safety:** commands are typed (enum verbs), not arbitrary shell, except the recorded terminal session. All results are signed and reported. The agent self-updates via signed releases with staged rollout.

## 5. Provider drivers (Go, `drivers/<provider>`)
- Interface: `CreateVM, DeleteVM, Power(op), Resize, Snapshot, RestoreSnapshot, AttachVolume, ListSizes, ListRegions, Pricing`.
- Implementations, in order:
  - BYOVM (agent-only)
  - Proxmox and OpenStack (for local DCs and gov clouds)
  - Hetzner, DigitalOcean, AWS EC2, Azure
  - Further providers as partnerships land
- Upstream costs are synced daily into `price_book` (draft, then approved, then effective). A driver that is not configured reports `unavailable` and is hidden from the UI. No fake sizes.

## 6. Catalogue and templates
- The template repo (Git) holds per-app: `spade.yml` (runtime, ports, env schema with generators such as `random_password`, lifecycle hooks, webUI, backup paths, health probe, min size, licence), `docker-compose.yml`, and tests.
- **Template CI (Python):**
  - For every version bump: install → smoke → backup → restore → upgrade-from-previous → CVE scan (Trivy/Grype).
  - Only passing versions are published; digests are pinned.
- **Import compatibility:** accept `elestio.yml`-shaped files (E17) and translate them, to ease migration.

## 7. Billing and metering
- **Metering (Rust):** hourly aggregation from `service` state intervals plus agent counters (bandwidth, storage, requests) into `meter_reading`. Exactly-once is enforced by `(service_id, sku, period_start)` uniqueness.
- **Rating:** `meter_reading` × `price_item` (version pinned at period start) produces TigerBeetle transfers: debit customer wallet, credit revenue, plus a linked transfer crediting VAT liability.
- **Transfer IDs:** deterministic, as hash(service, sku, period). Retries are therefore idempotent, and TigerBeetle rejects duplicates with `exists`.
- **Deploy holds:** placed as TigerBeetle **pending transfers** with a timeout; post or void on completion.
- **Prepaid wallets:** use the `debits_must_not_exceed_credits` flag. Overdraft is impossible at the ledger level, and an `exceeds_credits` result triggers the suspend workflow.
- **Postpaid accounts:** use a credit-limit control account.
- **Outbox:** Postgres `transfer_intent` is the outbox. A Go relay submits batches to TigerBeetle (≤ 8,190 per batch) and records the result.
- **Payments:** Paystack, Flutterwave, Remita, Stripe adapters. Webhooks are verified by HMAC and are idempotent on provider reference. Top-up credits the wallet only on a verified webhook, never on client redirect.
- **Reconciliation:** a daily job compares provider settlements to journals and fails loudly on variance.
- **Postpaid:** monthly invoice, credit limit, dunning, PO reference.

## 8. Security requirements
- MFA is required for owners and admins (TOTP, WebAuthn), unlike Elestio's email-OTP default (E13).
- Authorisation: Permify relationship-based checks on every API call (project → service → action).
- Secrets: per-service credentials are encrypted with envelope encryption (KMS / Vault Transit; customer-managed keys for BYO-account). Reveal-once with audit.
- Operator access: JIT, customer-approved, session-recorded; break-glass with dual control.
- Supply chain: SBOMs and signatures for agent, templates and images; reproducible builds for the agent.
- Edge: APISIX gateway (rate limits, WAF via open-appsec), Turnstile-equivalent bot protection on signup (E2).
- Audit: a hash-chained `audit_event` table, with daily anchor export to WORM object storage.

## 9. Non-functional requirements
| NFR | Target |
|---|---|
| Deploy time (catalogue app) | p95 ≤ 5 min |
| Control-plane availability | 99.9% (multi-AZ in-country) |
| API latency | p95 < 300 ms for reads |
| Agent fleet scale | 50k agents per regional gateway cluster |
| Metrics freshness | ≤ 10 s; 1 s live mode (E15) |
| RPO / RTO (customer services, L2) | 24h / 1h; L3: 1h / 30 min |
| Backup verification | ≥ 1 verified restore per service per week |
| Accessibility | WCAG 2.1 AA for the PWA |

## 10. Clients
- **PWA (TypeScript/React):** full dashboard. Works offline for read views and queued approvals; web push for alerts.
- **Mobile (React Native/Expo):** alerts, power actions, approvals (AI DevOps access, JIT operator), wallet top-up, status. Biometric re-auth for destructive actions.
- **CLI (Go)** `spade`: parity with the Elestio CLI verbs (E24), JSON output.
- **Terraform provider (Go)** plus cluster modules.
- **Agent skill / MCP server** exposing safe, typed tools (deploy, status, backup, restart) with confirmation prompts (E24).

## 11. Testing and readiness gates
- Unit and contract tests per service; OpenAPI conformance tests.
- Temporal workflow replay tests.
- Ledger property tests against a real TigerBeetle replica: Σ debits = Σ credits, no wallet below zero, idempotent replays.
- End-to-end: deploy → backup → restore → update → delete on the BYOVM driver in CI (real VM via a nested-virt runner).
- Chaos: kill the agent mid-update, partition the gateway, provider API 5xx. The expected outcome is a correct compensation, never a phantom "running".
- **Readiness score:** the `capabilities` endpoint is surfaced in admin; no feature is marketed as available unless its integration test passed in the last 24h.
