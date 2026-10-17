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
