import { UserManager, WebStorageStateStore, InMemoryWebStorage } from "oidc-client-ts";

export const API = import.meta.env.VITE_SPADE_API ?? "http://localhost:8080";
export const PAYMENTS = import.meta.env.VITE_SPADE_PAYMENTS ?? "http://localhost:8090";
const ISSUER = import.meta.env.VITE_OIDC_ISSUER ?? "http://localhost:8180/realms/spadeai";

// Authorization-code + PKCE; tokens are held in memory only (never localStorage).
export const auth = new UserManager({
  authority: ISSUER, client_id: "spade-pwa", redirect_uri: `${location.origin}/`, post_logout_redirect_uri: `${location.origin}/`,
  response_type: "code", scope: "openid profile email",
  userStore: new WebStorageStateStore({ store: new InMemoryWebStorage() }),
});

let token = "";
export const setToken = (t: string) => { token = t; };

export async function call<T = any>(path: string, init: RequestInit & { idempotent?: boolean; base?: string } = {}): Promise<T> {
  const headers: Record<string, string> = { "Content-Type": "application/json", Authorization: `Bearer ${token}` };
  if (init.idempotent) headers["Idempotency-Key"] = crypto.randomUUID();
  const r = await fetch((init.base ?? API) + path, { ...init, headers });
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body?.error?.message ?? body?.error ?? body?.message ?? `HTTP ${r.status}`);
  return body as T;
}

export const naira = (kobo: number) => `₦${(kobo / 100).toLocaleString("en-NG", { minimumFractionDigits: 2 })}`;
