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

`scripts/e2e.sh` runs a real deploy through the Rust agent, then checks the funds-check hold is voided, that a rejected debit suspends the org and records arrears, that a top-up settles arrears and resumes the org, and that the audit chain is intact.

The signed payment-credit hook (`/internal/payments/verified`) is served only on `SPADE_INTERNAL_LISTEN` (default `127.0.0.1:8081`), never on the public listener. Point the payments service at it with `CORE_INTERNAL_URL`.

All services read `SPADE_DATABASE_URL`, not the generic `DATABASE_URL`, so they never pick up another project's database by accident. The local defaults in `internal/config` are for development only.

See [docs/READINESS.md](docs/READINESS.md) for an honest production-readiness assessment.
