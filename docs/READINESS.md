# Production readiness

**Overall: about 49/100.** This is a working vertical slice: deploy, run, meter, bill, suspend and resume, done end to end on BYOVM (Hetzner, DigitalOcean, Vultr and Akamai/Linode drivers added on a shared cloud adapter, none yet live-tested), plus per-project private networks (WireGuard and Nebula) between hosts and encrypted off-host backups with restore. It is not a production platform yet.

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
| Backups | `scripts/e2e-backup.sh`: restic snapshot of the service's named volumes and stack dir (quiesced by default) to an off-host REST repository, encrypted with a per-service password sealed by the KEK; restore empties the volumes, restores and restarts, and the data matches the snapshot exactly. The gateway injects repository credentials at delivery, so they are never stored in `agent_command` (checked). Daily schedule per running service; retention via `restic forget --keep-daily` |
| Private networks | `scripts/e2e-overlay.sh`: two docker-in-docker hosts per kind. WireGuard full mesh and Nebula (lighthouse = lowest IP, per-project CA sealed with the KEK) both reach each other over the overlay; deleting a service removes it and the survivor's mesh shrinks. Host private keys never enter the command queue (checked) |

## Unavailable or not implemented (reported honestly as `unavailable`)
| Area | State |
|---|---|
| Hetzner Cloud driver | Implemented: servers labelled `spade-service`/`spade-attempt` so retries never double-create, cloud-init installs the agent, delete removes every server for the service and a failed delete leaves the service `stopped` (not `deleted`). Capability is `unavailable` without `SPADE_HETZNER_TOKEN`. First-boot cloud-init installs Docker + Compose, WireGuard tools, restic and pinned Nebula 1.9.5 (sha256-checked), and runs the agent installer embedded in user data (no `curl --insecure` script fetch; the CA pin is checked before anything else is downloaded). Every attempt deletes servers from other attempts; a failed deploy deprovisions; delete and failed deploys revoke the agent certificate. Unit-tested against a fake Hetzner API only; **not yet run against a live Hetzner account**. No Nigerian region, so residency-locked orgs are refused |
| Cloud adapter: DigitalOcean, Vultr, Akamai/Linode | Implemented on `internal/providers/cloud` with Hetzner: tag/label-based idempotent create per attempt, reconciliation, delete-all, shared cloud-init. Each driver passes the same contract test (`cloudtest.Contract`) against a fake of its API, including pagination and bad/missing tokens. **Not verified against any live account**; capability is `unavailable` without its token. Residency-locked orgs are refused (no Nigerian region). Vultr OS id and Linode Metadata regions are from public docs and need confirming on first live run |
| Cloud drivers (AWS, Proxmox, OpenStack) | Not implemented; capability reports `unavailable` |
| Paystack / Flutterwave | Code and tests done; **no credentials configured**, so these report unavailable. Not yet verified against the live sandbox |
| AI DevOps | Router and policy enforcement are unit-tested; no LLM backend is configured, so `/healthz` returns 503 |
| Mobile app | Type-checks only; not run on a device or emulator |
| PWA | Builds and serves; OIDC flow not yet exercised in a browser |
| HA / DR | Single-node dev stack only. Lagos/Abuja topology is spec only |
| APISIX, Kafka | In the spec; not wired in |
| Security hardening | Dev credentials in compose; the local KEK is a file auto-created under `.spade/` (production must mount it from a KMS/Vault and set `SPADE_KEK_CREATE=false`); no rate limiting; no pen test |
| Observability | Structured logs only; no metrics or tracing |
| Tests | Go unit tests are thin; there are no Go integration tests in CI (e2e needs the full stack) |
| Overlay at scale | Each apply restarts Nebula only when its config changes; WireGuard uses `wg syncconf` (no tunnel drop). Not yet tested beyond two hosts, across NAT, or with IPv6 endpoints; hosts that are offline during a mesh change converge only on the next change |
| Backups hardening | Local REST server runs without auth (repositories are still encrypted client-side). Missing: per-tenant repo credentials, append-only/immutable storage, restore into a new service, backup billing, and tests with large volumes or long-running databases (quiesce means brief downtime) |

### Backup hardening (review of #9)
- The restic REST server now requires HTTP basic auth (`scripts/backup-repo-user.sh` creates the user and prints `SPADE_BACKUP_REPO`); without a user every request is 401 and the `backups` capability reports the rejected credentials. Still one shared credential for all tenants: per-tenant users with `--private-repos` and append-only storage remain open.
- Restore checks the snapshot is still in the service's repository and that every snapshot path is one of the service's volumes or its stack dir, restores into a staging dir with `--include` for those paths only, and only then stops the app and swaps data. Failures before the swap report `data unchanged` and keep the service state; failures during the swap mark the service `failed` instead of hiding it.
- Backing up a stopped service no longer starts it (quiesce only touches running containers).
- Backups pruned by retention are marked `pruned` and can no longer be selected for restore.

