# SpadeAi

Sovereign, Nigeria-first managed open-source platform. It does one-click deployment and full day-2 operations for 400+ OSS apps, on any cloud, data centre, on-prem or bring-your-own VM/account.

## Specifications
- [00 — Evidence ledger](docs/specs/00-evidence-ledger.md): clean-room reverse engineering of the public Elestio surface, done with [REA](https://github.com/morluto/rea)
- [01 — Business spec](docs/specs/01-business-spec.md)
- [02 — Technical spec](docs/specs/02-technical-spec.md)
- [03 — Architecture spec](docs/specs/03-architecture-spec.md)
- Raw public evidence: `docs/evidence/elestio-re-evidence.zip` (docs/site text captures, sitemaps, REA bundle analysis)

## Stack
- Languages: Go, Rust, Python, TypeScript
- Data: PostgreSQL for system-of-record state; TigerBeetle for the financial ledger
- Platform components: Keycloak, Permify, APISIX, Temporal, Kafka
- Private networking: Nebula + WireGuard
- Clients: PWA + native mobile (React Native/Expo)

## Repository layout
| Path | Language | What it is |
|---|---|---|
| `cmd/spade-api` | Go | Public API: OIDC auth (Keycloak), Permify authz, idempotent operations, hash-chained audit, capabilities, TigerBeetle wallets |
| `cmd/spade-worker` | Go | Temporal workers (deploy, day-2 actions, suspend/resume) + rating loop (meter readings to TigerBeetle transfers, arrears) |
| `cmd/spade-gateway` | Go | Agent gateway: one-time enrolment tokens, internal CA, mTLS long-poll command channel |
| `cmd/spade` | Go | CLI |
| `rust/agent` | Rust | `spade-agent`: outbound-only, runs Docker Compose bundles, checks bundle sha256, health probes |
| `rust/metering` | Rust | `spade-metering`: running intervals become hourly `meter_reading` rows (idempotent) |
| `services/payments` | TypeScript | Paystack + Flutterwave adapters: webhook signature check, server-side re-verification, HMAC-signed credit to core |
| `python/template_ci` | Python | Template linting (pinned images, no privileged/public binds, licence policy), smoke test, publish |
| `python/ai_devops` | Python | AI DevOps LLM router: `self_hosted` / `external` / `hybrid`, residency lock, audited token usage |
| `clients/pwa` | TypeScript | React PWA (OIDC PKCE, in-memory tokens) |
| `clients/mobile` | TypeScript | Expo / React Native app (PKCE, SecureStore) |
| `db/migrations` | SQL | PostgreSQL schema |
| `templates/` | YAML | Catalogue templates (`spade.yml` + compose) |
| `deploy/compose` | YAML | Local stack: Postgres, TigerBeetle, Temporal, Keycloak, Permify |

## Running locally
```bash
docker compose -f deploy/compose/docker-compose.yml up -d
go build -o bin/ ./cmd/... && (cd rust && cargo build --release)
python3 -m venv .venv && .venv/bin/pip install -e 'python[dev]'
export SPADE_INTERNAL_SECRET=$(openssl rand -hex 32)   # >= 32 chars or the payment hook stays disabled
./bin/spade-gateway &                      # :8443 (mTLS)
./bin/spade-api &                          # :8080 public, 127.0.0.1:8081 internal payment hook
./bin/spade-worker &
./scripts/e2e.sh
```
`scripts/e2e-overlay.sh` builds two privileged docker-in-docker "VMs" per overlay kind and checks WireGuard and Nebula end to end. It needs `wireguard-tools`, `nebula`/`nebula-cert` (1.9.x), the `x86_64-unknown-linux-musl` Rust target, and the gateway started with `GATEWAY_HOSTS=localhost,127.0.0.1,172.31.250.1`.

`scripts/e2e-backup.sh` deploys `templates/static-site` into a dind VM, backs it up to the compose `backups` restic REST server, overwrites the data, restores, and checks the data matches the snapshot. Start api, worker and gateway with the values printed by `scripts/backup-repo-user.sh`. The test also checks that another tenant's login is refused, that deletes are refused (append-only), that the admin endpoint is unreachable from hosts, and that deleting the service revokes its login.

`scripts/e2e.sh` runs a real deploy through the Rust agent, then checks the funds-check hold is voided, that a rejected debit suspends the org and records arrears, that a top-up settles arrears and resumes the org, and that the audit chain is intact.

The signed payment-credit hook (`/internal/payments/verified`) is served only on `SPADE_INTERNAL_LISTEN` (default `127.0.0.1:8081`), never on the public listener. Point the payments service at it with `CORE_INTERNAL_URL`.

All services read `SPADE_DATABASE_URL`, not the generic `DATABASE_URL`, so they never pick up another project's database by accident. The local defaults in `internal/config` are for development only.

See [docs/READINESS.md](docs/READINESS.md) for an honest production-readiness assessment.

Backups are isolated per service. The tenant restic REST server (compose `backups`) runs `--private-repos --append-only`: the worker gives each service its own login (random, sealed with the KEK, written as bcrypt to `SPADE_BACKUP_HTPASSWD`), so a host can only reach `/s-<service id>` and can never delete or rewrite snapshots. Retention runs from the worker through a second endpoint on the same storage (compose `backups-admin`, loopback only). Run `scripts/backup-repo-user.sh` after `docker compose up`: it creates the admin login and prints `SPADE_BACKUP_REPO` (credential-free; one value for the gateway as hosts see it, one for the worker and API), `SPADE_BACKUP_HTPASSWD` and `SPADE_BACKUP_ADMIN_REPO`. The worker needs restic 0.14 or newer (repository format v2; set `RESTIC` to its path). The local servers are plain HTTP, so the gateway refuses to send credentials unless `SPADE_BACKUP_ALLOW_PLAINTEXT=1` (local dev only); production repositories must be `rest:https://`. Repositories made before this change live at `/<service id>` and must be moved to `/s-<service id>` on the backup server.

### Cloud providers (Hetzner, DigitalOcean, Vultr, Akamai/Linode, AWS, OpenStack, Proxmox)
All API providers share one adapter (`internal/providers/cloud`): servers are tagged with the service and enrolment attempt, so a retried deploy never creates a second server, stale attempts are removed, and deleting a service removes every server it owns. First boot (cloud-init, Ubuntu 24.04) locks the root password, installs Docker + Compose, WireGuard tools, restic and a pinned Nebula, then runs the embedded agent installer, which checks the gateway CA pin before downloading anything.

| Provider | Token (set on `spade-api` and `spade-worker`) | Region variable (default) | Sizes |
|---|---|---|---|
| Hetzner Cloud | `SPADE_HETZNER_TOKEN` | `SPADE_HETZNER_LOCATION` (`fsn1`) | `hetzner:CX22`, `CX32`, `CX42` |
| DigitalOcean | `SPADE_DIGITALOCEAN_TOKEN` | `SPADE_DIGITALOCEAN_REGION` (`lon1`) | `digitalocean:s-1vcpu-2gb`, `s-2vcpu-4gb`, `s-4vcpu-8gb` |
| Vultr | `SPADE_VULTR_TOKEN` | `SPADE_VULTR_REGION` (`jnb`, Johannesburg) | `vultr:vc2-1c-2gb`, `vc2-2c-4gb`, `vc2-4c-8gb` |
| Akamai Cloud (Linode) | `SPADE_LINODE_TOKEN` | `SPADE_LINODE_REGION` (`gb-lon`, must support Metadata) | `linode:g6-standard-1`, `g6-standard-2`, `g6-standard-4` |

| AWS EC2 | `SPADE_AWS_ACCESS_KEY_ID` + `SPADE_AWS_SECRET_ACCESS_KEY`, and `SPADE_AWS_AMI` (Ubuntu 24.04 AMI for the region) | `SPADE_AWS_REGION` (`af-south-1`, Cape Town) | `aws:t3.small`, `t3.medium`, `t3.large` |
| OpenStack (Nova) | `SPADE_OPENSTACK_AUTH_URL` (Keystone v3), `SPADE_OPENSTACK_APP_CRED_ID` + `SPADE_OPENSTACK_APP_CRED_SECRET`, `SPADE_OPENSTACK_IMAGE` | `SPADE_OPENSTACK_REGION` | `openstack:m1.small`, `m1.medium`, `m1.large` (flavor names; must exist on the cloud) |
| Proxmox VE | `SPADE_PROXMOX_API` (`https://host:8006/api2/json`), `SPADE_PROXMOX_TOKEN` (`user@realm!id=secret`), `SPADE_PROXMOX_NODE`, `SPADE_PROXMOX_TEMPLATE` (vmid) | n/a | `proxmox:SMALL-1C-2G`, `MEDIUM-2C-4G`, `LARGE-4C-8G` |

AWS uses only the `SPADE_AWS_*` keys (never the ambient AWS credential chain); optional `SPADE_AWS_SUBNET_ID` and comma-separated `SPADE_AWS_SECURITY_GROUP_IDS`. Instances require IMDSv2 and use the attempt as the EC2 client token. OpenStack servers are tagged with Nova microversion 2.52; `SPADE_OPENSTACK_NETWORK` picks a network id (default: auto-allocate). Proxmox clones the template (linked clone; `SPADE_PROXMOX_FULL_CLONE=1` with optional `SPADE_PROXMOX_STORAGE`, plus `SPADE_PROXMOX_POOL`), so the template must be Ubuntu 24.04 with `qemu-guest-agent` installed and enabled. Proxmox cannot take cloud-init user data over the API, so the same first boot runs as a shell script through the guest agent; it must exit 0 within 15 minutes or the VM is destroyed, and only then is the VM tagged `spade-ready`. Use `SPADE_PROXMOX_CA_FILE` for a self-signed node certificate. Delete refuses any VM not tagged `spade-managed`.

OpenStack and Proxmox are often run in Nigerian data centres: set `SPADE_OPENSTACK_IN_NIGERIA=1` / `SPADE_PROXMOX_IN_NIGERIA=1` to declare that, and the API writes `provider.in_nigeria` on start so residency-locked organisations may use them. Unset, they are treated as outside Nigeria. AWS is never in Nigeria.

`SPADE_<PROVIDER>_API` overrides the API base URL; `SPADE_VULTR_OS_ID` overrides Vultr's Ubuntu 24.04 image id (2284). `GATEWAY_PUBLIC_URL` must be an internet-reachable `https://host[:port]` because the new server enrols itself. Without a token a provider reports `unavailable` in `/v1/capabilities` and its deploys fail closed. Hetzner, DigitalOcean, Vultr, Linode and AWS have no Nigerian region, so residency-locked organisations are refused (`provider.in_nigeria`).
