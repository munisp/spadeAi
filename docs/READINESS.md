# Production readiness

**Overall: about 35/100.** This is a working vertical slice: deploy, run, meter, bill, suspend and resume, done end to end on BYOVM. It is not a production platform yet.

## Verified locally (scripts/e2e.sh plus unit tests)
| Area | Evidence |
|---|---|
| Auth | Keycloak OIDC JWT verification; unauthenticated requests get 401 |
| Authz | Permify checks on every org/project/service route; fails closed when Permify is down |
| Idempotency | Replaying the same `Idempotency-Key` returns the same operation; reusing a key with a different body returns 409 |
| Audit | SHA-256 hash chain; DB trigger blocks UPDATE/DELETE; `/v1/audit-events/verify` |
| Deploy | Temporal `DeployService`, then a one-time enrolment token, then the Rust agent enrols over mTLS, then the sha256-checked compose bundle is applied, then the health probe runs |
| Billing | TigerBeetle pending hold used as a funds check and voided after deploy (no double charge); hourly metering; rating; `exceeds_credits` suspends the org and records arrears; a top-up settles arrears oldest first and resumes the org |
| Capabilities | `/v1/capabilities` reports every integration; anything unimplemented or unconfigured shows as `unavailable` |
| Template CI | Lint rules plus a real docker smoke test before a version can be deployed |

## Unavailable or not implemented (reported honestly as `unavailable`)
| Area | State |
|---|---|
| Cloud drivers (Hetzner, AWS, Proxmox, OpenStack) | Not implemented. BYOVM only |
| Nebula / WireGuard overlay controllers | Schema and API choice exist; no controller yet |
| Backups (Borg/restic orchestration) | Not implemented |
| Paystack / Flutterwave | Code and tests done; **no credentials configured**, so these report unavailable. Not yet verified against the live sandbox |
| AI DevOps | Router and policy enforcement are unit-tested; no LLM backend is configured, so `/healthz` returns 503 |
| Mobile app | Type-checks only; not run on a device or emulator |
| PWA | Builds and serves; OIDC flow not yet exercised in a browser |
| HA / DR | Single-node dev stack only. Lagos/Abuja topology is spec only |
| APISIX, Kafka | In the spec; not wired in |
| Security hardening | Dev credentials in compose; the API's internal gateway health probe skips TLS verification; no rate limiting; no secrets manager; no pen test |
| Observability | Structured logs only; no metrics or tracing |
| Tests | Go unit tests are thin; there are no Go integration tests in CI (e2e needs the full stack) |
