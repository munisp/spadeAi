import { createServer, IncomingMessage, ServerResponse } from "node:http";
import { randomUUID } from "node:crypto";
import { Paystack, Flutterwave, Provider, creditCore } from "./providers.js";
import { corsHeaders } from "./cors.js";

const CORE = process.env.CORE_API_URL ?? "http://localhost:8080";
// Private listener of spade-api that accepts signed payment credits (SPADE_INTERNAL_LISTEN).
const CORE_INTERNAL = process.env.CORE_INTERNAL_URL ?? "http://localhost:8081";
const SECRET = process.env.SPADE_INTERNAL_SECRET ?? "";
const CALLBACK = process.env.PAYMENT_CALLBACK_URL ?? "http://localhost:5173/billing";
const providers: Record<string, Provider> = { paystack: new Paystack(), flutterwave: new Flutterwave() };

function send(res: ServerResponse, code: number, body: unknown) {
  res.writeHead(code, { "Content-Type": "application/json" });
  res.end(JSON.stringify(body));
}

async function readBody(req: IncomingMessage): Promise<string> {
  const chunks: Buffer[] = [];
  let n = 0;
  for await (const c of req) { n += (c as Buffer).length; if (n > 1 << 20) throw new Error("body too large"); chunks.push(c as Buffer); }
  return Buffer.concat(chunks).toString("utf8");
}

export function capabilities() {
  const list = Object.values(providers).map(p => ({ provider: p.name, status: p.configured() ? "available" : "unavailable", reason: p.configured() ? "" : "credentials not configured" }));
  const any = list.some(p => p.status === "available") && SECRET !== "";
  return { code: any ? 200 : 503, body: { providers: list, core_credit: SECRET ? "configured" : "SPADE_INTERNAL_SECRET not set" } };
}

const server = createServer(async (req, res) => {
  try {
    const url = new URL(req.url ?? "/", "http://x");
    for (const [k, v] of Object.entries(corsHeaders(req.headers.origin))) res.setHeader(k, v);
    if (req.method === "OPTIONS") return send(res, 204, {});
    if (req.method === "GET" && url.pathname === "/capabilities") { const c = capabilities(); return send(res, c.code, c.body); }
    if (req.method === "POST" && url.pathname === "/v1/topups") {
      const { org_id, provider, amount_minor, email } = JSON.parse(await readBody(req));
      const p = providers[provider];
      if (!p) return send(res, 422, { error: "provider must be paystack or flutterwave" });
      if (!p.configured()) return send(res, 503, { error: `${provider} unavailable: credentials not configured` });
      if (!Number.isInteger(amount_minor) || amount_minor < 10000) return send(res, 422, { error: "amount_minor must be an integer >= 10000 (₦100)" });
      // Authorise against the core API with the caller's own token: only org members can top up.
      const check = await fetch(`${CORE}/v1/orgs/${encodeURIComponent(org_id)}/wallet`, { headers: { Authorization: req.headers.authorization ?? "" } });
      if (!check.ok) return send(res, check.status === 401 ? 401 : 403, { error: "not authorised for this organization" });
      const r = await p.initialize(org_id, email, amount_minor, `spade-${randomUUID()}`, CALLBACK);
      return send(res, 201, r);
    }
    const m = url.pathname.match(/^\/webhooks\/(paystack|flutterwave)$/);
    if (req.method === "POST" && m) {
      const p = providers[m[1]];
      const raw = await readBody(req);
      if (!p.verifyWebhook(raw, req.headers as Record<string, string>)) return send(res, 401, { error: "bad signature" });
      const evt = JSON.parse(raw);
      const ref = m[1] === "paystack" ? evt?.data?.reference : evt?.data?.tx_ref;
      if (!ref) return send(res, 200, { ignored: true });
      const charge = await p.verify(String(ref));
      if (!charge) return send(res, 200, { ignored: "not a successful NGN charge" });
      return send(res, 200, await creditCore(charge, CORE_INTERNAL, SECRET));
    }
    send(res, 404, { error: "not found" });
  } catch (e) {
    send(res, 502, { error: String((e as Error).message) });
  }
});

if (process.env.NODE_ENV !== "test") {
  const port = Number(process.env.PORT ?? 8090);
  server.listen(port, () => console.log(JSON.stringify({ msg: "payments listening", port, ...capabilities().body })));
}
