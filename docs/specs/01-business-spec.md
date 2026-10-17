# 01 — Business Specification

**Product name:** *SpadeAi*, a sovereign managed open-source platform.
**Reference product:** Elestio, reverse-engineered clean-room. Evidence IDs (E-n, W-n) point to `00-evidence-ledger.md`.

## 1. Problem and thesis
Organisations want to run open-source software (Postgres, Keycloak, n8n, Nextcloud, GitLab, Metabase, Moodle and so on) in production. Doing that needs scarce DevOps skills: provisioning, TLS, backups, patching, monitoring, incident response. Elestio sells exactly that layer. It packages more than 400 templates (E28) onto a dedicated VM on any cloud or the customer's own VM (E10, E26), runs day-2 operations (E12–E16), and bills hourly from prepaid credits (E8).

**Our thesis:** the same "managed open source on any infrastructure" model, built **sovereign and Nigeria-first**:
- The control plane and data stay in-country.
- Pricing is in NGN, through local payment rails.
- Customers can bring their own account on Nigerian clouds and data centres, mirroring Elestio's T Cloud model (E27).
- Procurement fits government and regulated enterprises (NDPA 2023, NITDA, CBN, NCC).

## 2. Target segments
| Segment | Pain | What we sell |
|---|---|---|
| MDAs and public sector (federal and state) | Data sovereignty mandates, no DevOps staff, slow procurement | BYO-account inside Galaxy Backbone or other in-country tenancies; customer-held keys; auditable operator IAM (E27 pattern) |
| Banks, fintechs, PSPs (CBN-regulated) | Residency, audit trail, uptime SLAs | Managed DB clusters (E18), L3 support with P1 < 1h (E7), immutable audit log |
| SMEs and startups | High USD cloud bills, FX risk | NGN pricing, local DCs, the same 400-app catalogue |
| Agencies and system integrators | Run many client stacks | Multi-project tenancy (E23), white-label, partner referral (E3 `partner`) |
| Developers | Want a PaaS without lock-in | CI/CD from Git (E17), CLI/Terraform/agent skill (E24) |
| Universities and health | Moodle, OpenEMR, DHIS2 hosting | Curated templates and sector bundles |

## 3. Value proposition (parity, then differentiation)
**Parity (must-have, matches Elestio):**
- One-click deploy of catalogue apps on a dedicated VM in about 3 minutes.
- Multi-cloud plus BYOVM.
- Automatic TLS, firewall, rate limiter and cache.
- 3-2-1 backups.
- OS and app auto-updates inside a maintenance window.
- Monitoring and alerts.
- Browser IDE, terminal and file explorer.
- Clusters and load balancers.
- CI/CD.
- Object storage.
- Domains.
- API, CLI, Terraform and an AI-agent skill.
- AI DevOps.

**Differentiators (fixes for W1–W6, plus local fit):**
1. **Sovereign by default:** an in-country control plane, NDPA-aligned DPA, and customer-held KMS keys on BYO-account.
2. **Zero inbound management ports:** an outbound mTLS agent replaces SSH push (W3). This is easier to clear with security teams.
3. **Local money rails:** NGN pricing; Paystack, Flutterwave, Remita, bank transfer/NIP and USSD top-ups; postpaid invoicing against PO or escrow for government (W5). The ledger is TigerBeetle (double-entry, overdraft-proof prepaid wallets).
4. **Honest operations:** every capability reports *available / degraded / unavailable*, with no fake success. Every action is idempotent and audited.
5. **Offline-tolerant ops:** the PWA and native mobile app give on-call engineers push alerts, approvals and runbooks on poor connectivity.
6. **Compliance evidence packs:** per-service exports covering backups, patches, access and incidents, for NDPA audits and CBN/NCC inspections.

## 4. Pricing and packaging (benchmarked to E7–E10, E21)
All prices are held in a **versioned price book** in Postgres (fixes W4) and charged hourly in minor units (kobo / cents). They display as monthly (= 730 × hourly), as Elestio does.

| Line | Model | Reference (Elestio) | Our default (to be validated) |
|---|---|---|---|
| Managed VM | provider cost × (1 + margin) + management fee | from $11–14/mo | ₦ equivalent at a daily FX lock; margin 25–40% |
| BYOVM / BYO-account mgmt fee | per vCPU + per GB RAM + per GB disk | $5 + $2.5 + $0.025/GB | same structure; 1 free small VM |
| Support L1 / L2 / L3 | per hour, per service | free / $50 / $200 per month | free / ₦ tiered / ₦ tiered; custom SLA for MDAs |
| Backups | retention tied to plan; S3 external free | 7 / 14 / 30 days | same + "sovereign vault" add-on |
| Bandwidth overage | per GB | $0.05–0.15 | per provider, from price book |
| Object storage | GB-month + per-1k ops, free egress | $0.02/GB | local S3 (MinIO/Ceph RGW) pricing |
| AI DevOps | credits per plan + paid extra session | 60/120/300; $1 extra | same structure; self-hosted LLM option |
| Domains | registrar resale | — | .ng via NiRA-accredited registrar partner |

**Billing modes:** prepaid credits with auto-refill (Elestio default, E8), or **postpaid enterprise/government** (monthly invoice, credit limit, PO number, VAT 7.5%, WHT handling).

## 5. Revenue model and unit economics (illustrative; validate)
- **Gross margin drivers:** VM resale margin, management fee, support plans, storage, AI credits.
- **Example service:** a managed Postgres on a 2 vCPU / 4 GB VM at cost ₦X/h. We sell at 1.35X + L2 plan. Contribution margin is roughly 40–55% after backup storage and monitoring overhead.
- **Cost centres:** catalogue engineering (template upkeep is the moat: 400+ templates × version churn), 24/7 support, compliance audits, in-country DC capacity.
- **KPIs:** active services, ARPA, net revenue retention, template coverage and update latency (CVE → patched hours), MTTR, backup restore success rate (target 100% in monthly drills), churn.

## 6. Go-to-market
1. **Phase A (0–6 months):** developer/SME self-serve on 2–3 in-country/regional providers plus BYOVM. Top 50 templates.
2. **Phase B:** enterprise and fintech (clusters, L3, compliance packs). SI/agency partner programme.
3. **Phase C:** government BYO-account (sovereign tenancy), procurement frameworks, state-level shared services.

**Channels:** cloud and DC partnerships (co-sell), SI partners, developer community, a public catalogue SEO surface (Elestio runs a 7-page SEO funnel per app, E28; replicate it).

## 7. Regulatory and compliance requirements
- **NDPA 2023 / NDPC:**
  - lawful basis
  - DPA template
  - DPIA for AI DevOps
  - breach notification within 72h
  - data-subject rights
  - cross-border transfer controls (keep the control plane in-country; LLM calls opt-in or self-hosted)
- **NITDA** cloud and hosting guidelines (local hosting for government data).
- **CBN** risk-based cybersecurity framework, for fintech customers.
- **Tax:** VAT 7.5% on invoices; FIRS e-invoicing readiness.
- **Certifications roadmap:** ISO 27001, then SOC 2 Type 2, then PCI DSS (scope reduction via the payment processor). Elestio claims SOC 2/ISO/HIPAA/GDPR (E30).
- **Free-trial abuse controls:** KYC (BVN/NIN-backed for NG; E8 analogue), device/card fingerprinting, Turnstile-equivalent bot checks (E2).

## 8. Key user journeys (acceptance level)
1. **Sign up and verify:** email or OIDC sign-up → MFA (TOTP default) → KYC when the trial is risk-flagged → wallet top-up in NGN.
2. **Deploy an app:** choose template → provider/region (or BYOVM/BYO-account) → size → support plan + maintenance window → deploy. Live in ≤ 5 min, with credentials delivered once over a secure channel.
3. **Day-2 operations:** restart, power, resize, version change, firewall, domain + TLS, backup/restore, open IDE/terminal, view metrics/alerts, AI DevOps session.
4. **Cluster lifecycle:** create a primary/replica cluster → add a node → failover drill → restore.
5. **CI/CD:** connect Git → `spade.yml` (compatible superset of `elestio.yml`, E17) → push → build → zero-downtime deploy → rollback.
6. **Finance:** hourly metering → wallet debit → low-balance alert → suspend → grace → delete, with every state notified. Postpaid customers get monthly invoices instead.
7. **Government:** connect tenancy (IAM role / service account) → operator actions visible in the customer's own audit trail → evidence pack export.

## 9. Success criteria (first 12 months)
- 99.9% control-plane availability.
- 100% of services with a verified restorable backup in the last 24h.
- Critical CVE patched across the fleet in ≤ 72h.
- 150 production-grade templates with automated install, upgrade and restore tests.
- Reconciliation: Σ(TigerBeetle wallet debits) = Σ(metered usage × price book), zero-variance daily.

## 10. Risks
| Risk | Mitigation |
|---|---|
| Template maintenance cost explodes | Automated template CI (install, upgrade, backup, restore) per version; community contributions; prioritise by demand |
| FX volatility vs USD-priced upstream clouds | Daily FX lock in the price book; prefer NGN-billed local DCs; hedge margin |
| Local DC capacity and APIs immature | BYOVM and on-prem (Proxmox/OpenStack) drivers first; API drivers where available |
| Licence changes upstream (BSL/SSPL apps) | Licence field per template; legal review gate; prefer OSI-approved |
| Customer data exposure through support or AI | Just-in-time, customer-approved, recorded operator sessions; self-hosted LLM option |
| Elestio IP | Clean-room: behaviour and public facts only; no copied code, assets or text; own template sources |
