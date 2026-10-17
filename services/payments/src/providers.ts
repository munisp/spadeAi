// Payment provider adapters. Every adapter verifies charges server-side; nothing is ever assumed paid.
import { createHmac, timingSafeEqual } from "node:crypto";

export interface VerifiedCharge {
  provider: "paystack" | "flutterwave";
  providerRef: string;
  orgId: string;
  amountMinor: number; // kobo
  currency: string;
  raw: unknown;
}

export interface InitResult { authorizationUrl: string; reference: string }

export interface Provider {
  name: "paystack" | "flutterwave";
  configured(): boolean;
  initialize(orgId: string, email: string, amountMinor: number, reference: string, callbackUrl: string): Promise<InitResult>;
  verifyWebhook(rawBody: string, headers: Record<string, string | undefined>): boolean;
  /** Re-fetches the transaction from the provider API; the webhook body alone is never trusted. */
  verify(reference: string): Promise<VerifiedCharge | null>;
}

type Fetch = typeof fetch;

function safeEq(a: string, b: string): boolean {
  const x = Buffer.from(a), y = Buffer.from(b);
  return x.length === y.length && timingSafeEqual(x, y);
}

export class Paystack implements Provider {
  name = "paystack" as const;
  constructor(private secret = process.env.PAYSTACK_SECRET_KEY ?? "", private f: Fetch = fetch, private base = "https://api.paystack.co") {}
  configured() { return this.secret !== ""; }
  private async call(path: string, init?: RequestInit) {
    if (!this.configured()) throw new Error("paystack not configured");
    const r = await this.f(this.base + path, { ...init, headers: { Authorization: `Bearer ${this.secret}`, "Content-Type": "application/json" } });
    const body = await r.json() as any;
    if (!r.ok || body.status !== true) throw new Error(`paystack ${path}: HTTP ${r.status} ${body?.message ?? ""}`);
    return body.data;
  }
  async initialize(orgId: string, email: string, amountMinor: number, reference: string, callbackUrl: string) {
    const d = await this.call("/transaction/initialize", { method: "POST", body: JSON.stringify({ email, amount: amountMinor, currency: "NGN", reference, callback_url: callbackUrl, metadata: { org_id: orgId } }) });
    return { authorizationUrl: d.authorization_url, reference: d.reference };
  }
  verifyWebhook(rawBody: string, headers: Record<string, string | undefined>) {
    const sig = headers["x-paystack-signature"];
    if (!this.configured() || !sig) return false;
    return safeEq(createHmac("sha512", this.secret).update(rawBody).digest("hex"), sig);
  }
  async verify(reference: string): Promise<VerifiedCharge | null> {
    const d = await this.call(`/transaction/verify/${encodeURIComponent(reference)}`);
    if (d.status !== "success" || d.currency !== "NGN" || !d.metadata?.org_id) return null;
    return { provider: "paystack", providerRef: String(d.reference), orgId: String(d.metadata.org_id), amountMinor: Number(d.amount), currency: d.currency, raw: d };
  }
}

export class Flutterwave implements Provider {
  name = "flutterwave" as const;
  constructor(private secret = process.env.FLW_SECRET_KEY ?? "", private webhookHash = process.env.FLW_WEBHOOK_HASH ?? "", private f: Fetch = fetch, private base = "https://api.flutterwave.com/v3") {}
  configured() { return this.secret !== "" && this.webhookHash !== ""; }
  private async call(path: string, init?: RequestInit) {
    if (!this.configured()) throw new Error("flutterwave not configured");
    const r = await this.f(this.base + path, { ...init, headers: { Authorization: `Bearer ${this.secret}`, "Content-Type": "application/json" } });
    const body = await r.json() as any;
    if (!r.ok || body.status !== "success") throw new Error(`flutterwave ${path}: HTTP ${r.status} ${body?.message ?? ""}`);
    return body.data;
  }
  async initialize(orgId: string, email: string, amountMinor: number, reference: string, callbackUrl: string) {
    const d = await this.call("/payments", { method: "POST", body: JSON.stringify({ tx_ref: reference, amount: amountMinor / 100, currency: "NGN", redirect_url: callbackUrl, customer: { email }, meta: { org_id: orgId } }) });
    return { authorizationUrl: d.link, reference };
  }
  verifyWebhook(_raw: string, headers: Record<string, string | undefined>) {
    const h = headers["verif-hash"];
    return this.configured() && !!h && safeEq(h, this.webhookHash);
  }
  async verify(reference: string): Promise<VerifiedCharge | null> {
    const d = await this.call(`/transactions/verify_by_reference?tx_ref=${encodeURIComponent(reference)}`);
    if (d.status !== "successful" || d.currency !== "NGN" || !d.meta?.org_id) return null;
    // Flutterwave reports major units; convert to kobo without float drift.
    return { provider: "flutterwave", providerRef: String(d.tx_ref), orgId: String(d.meta.org_id), amountMinor: Math.round(Number(d.amount) * 100), currency: d.currency, raw: d };
  }
}

/** Signs and forwards a verified charge to the core API, which posts it to TigerBeetle. */
export async function creditCore(c: VerifiedCharge, coreUrl: string, secret: string, f: Fetch = fetch) {
  if (!secret) throw new Error("SPADE_INTERNAL_SECRET not set");
  const body = JSON.stringify({ org_id: c.orgId, provider: c.provider, provider_ref: c.providerRef, amount_minor: c.amountMinor, currency: c.currency, raw: c.raw });
  const sig = createHmac("sha256", secret).update(body).digest("hex");
  const r = await f(`${coreUrl}/internal/payments/verified`, { method: "POST", body, headers: { "Content-Type": "application/json", "X-Spade-Signature": sig } });
  if (!r.ok) throw new Error(`core credit failed: HTTP ${r.status} ${await r.text()}`);
  return r.json();
}
