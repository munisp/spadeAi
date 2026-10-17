import { useEffect, useState, useCallback } from "react";
import { auth, call, setToken, naira, PAYMENTS } from "./api";

type Cap = { component: string; status: string; reason: string };
type Org = { id: string; name: string; billing_mode: string; ai_policy: string };
type Wallet = { arrears_minor: number; balance: { available_minor: number; pending_debits_minor: number; posted_balance_minor: number } };
type Project = { id: string; name: string; org_id: string; overlay_kind?: string };
type Template = { slug: string; name: string; version: string; description: string };
type Service = { id: string; display_name: string; template: string; state: string; state_reason: string | null };
type OverlayPeer = { service_id: string; display_name: string; overlay_ip: string; endpoint: string; last_apply_status: string | null; applied_generation: number | null };
type Backup = { id: string; kind: string; status: string; started_at: string; bytes_added: number | null; error: string | null };
type Size = { id: string; provider_id: string; code: string; vcpu: number; ram_mb: number; disk_gb: number };
type Overlay = { kind: string; cidr: string; generation: number; peers: OverlayPeer[] };

const s = {
  page: { fontFamily: "system-ui, sans-serif", maxWidth: 960, margin: "0 auto", padding: 16 } as const,
  card: { border: "1px solid #e5e7eb", borderRadius: 8, padding: 12, marginBottom: 12 } as const,
  btn: { background: "#0f766e", color: "#fff", border: 0, borderRadius: 6, padding: "6px 12px", cursor: "pointer", marginRight: 6 } as const,
  pill: (ok: boolean) => ({ padding: "2px 8px", borderRadius: 999, fontSize: 12, background: ok ? "#dcfce7" : "#fee2e2", color: ok ? "#166534" : "#991b1b" }),
};

export function App() {
  const [user, setUser] = useState<string>("");
  const [err, setErr] = useState("");
  const [caps, setCaps] = useState<Cap[]>([]);
  const [orgs, setOrgs] = useState<Org[]>([]);
  const [org, setOrg] = useState<Org | null>(null);
  const [wallet, setWallet] = useState<Wallet | null>(null);
  const [projects, setProjects] = useState<Project[]>([]);
  const [project, setProject] = useState<Project | null>(null);
  const [templates, setTemplates] = useState<Template[]>([]);
  const [services, setServices] = useState<Service[]>([]);
  const [bootstrap, setBootstrap] = useState("");
  const [overlay, setOverlay] = useState<Overlay | null>(null);
  const [backups, setBackups] = useState<{ svc: string; items: Backup[] } | null>(null);

  const guard = (f: () => Promise<void>) => () => { setErr(""); f().catch(e => setErr(String(e.message ?? e))); };

  useEffect(() => {
    (async () => {
      if (location.search.includes("code=")) { await auth.signinRedirectCallback(); history.replaceState({}, "", "/"); }
      const u = await auth.getUser();
      if (u && !u.expired) { setToken(u.access_token); setUser(u.profile.email ?? u.profile.sub); }
      setCaps((await call<{ capabilities: Cap[] }>("/v1/capabilities")).capabilities);
    })().catch(e => setErr(String(e.message ?? e)));
  }, []);

  const loadOrgs = useCallback(async () => setOrgs((await call<{ items: Org[] }>("/v1/orgs")).items ?? []), []);
  useEffect(() => { if (user) loadOrgs().catch(e => setErr(e.message)); }, [user, loadOrgs]);

  const openOrg = async (o: Org) => {
    setOrg(o); setProject(null);
    setWallet(await call(`/v1/orgs/${o.id}/wallet`));
    setProjects(((await call<{ items: Project[] }>("/v1/projects")).items ?? []).filter(p => p.org_id === o.id));
  };
  const openProject = async (p: Project) => {
    setProject(p); setBootstrap("");
    setTemplates((await call<{ items: Template[] }>("/v1/templates")).items ?? []);
    setServices((await call<{ items: Service[] }>(`/v1/projects/${p.id}/services`)).items ?? []);
    setOverlay(await call<Overlay>(`/v1/projects/${p.id}/overlay`));
  };
  const deploy = async (t: Template) => {
    const name = prompt(`Name for ${t.name}`, t.slug); if (!name) return;
    const ready = (p: string) => p === "byovm" || caps.find(c => c.component === `provider.${p}`)?.status === "available";
    const sizes = (await call<{ items: Size[] }>("/v1/sizes")).items ?? [];
    const offered = sizes.filter(z => ready(z.provider_id)).map(z => z.id);
    const missing = [...new Set(sizes.filter(z => !ready(z.provider_id)).map(z => z.provider_id))];
    const size = prompt(`Size: ${offered.join(" | ")}${missing.length ? ` (unavailable: ${missing.join(", ")})` : ""}`, "byovm:SMALL-1C-2G"); if (!size) return;
    if (!offered.includes(size)) throw new Error(`size ${size} is not offered by an available provider`);
    const op = await call<{ id: string }>(`/v1/projects/${project!.id}/services`, { method: "POST", idempotent: true, body: JSON.stringify({ template: t.slug, display_name: name, size }) });
    for (let i = 0; i < 30; i++) {
      const o = await call<{ status: string; response?: { bootstrap?: string }; error?: string }>(`/v1/operations/${op.id}`);
      if (o.response?.bootstrap) { setBootstrap(o.response.bootstrap); break; }
      if (o.status === "failed") throw new Error(o.error ?? "deployment failed");
      await new Promise(r => setTimeout(r, 1000));
    }
    await openProject(project!);
  };
  const act = async (svc: Service, a: string) => { await call(`/v1/services/${svc.id}/actions/${a}`, { method: "POST", idempotent: true }); await openProject(project!); };
  const showBackups = async (v: Service) => setBackups({ svc: v.id, items: (await call<{ items: Backup[] }>(`/v1/services/${v.id}/backups`)).items ?? [] });
  const backupNow = async (v: Service) => { await call(`/v1/services/${v.id}/backups`, { method: "POST", idempotent: true }); await showBackups(v); };
  const restore = async (svc: string, b: Backup) => {
    if (!confirm("Restore replaces the service's current data with this backup. Continue?")) return;
    await call(`/v1/services/${svc}/backups/${b.id}/restore`, { method: "POST", idempotent: true });
  };
  const topup = async () => {
    const amt = Number(prompt("Top-up amount in naira", "5000")); if (!amt) return;
    const r = await call<{ authorizationUrl: string }>("/v1/topups", { base: PAYMENTS, method: "POST", body: JSON.stringify({ org_id: org!.id, provider: "paystack", amount_minor: Math.round(amt * 100), email: user }) });
    location.href = r.authorizationUrl;
  };
  const createOrg = async () => {
    const name = prompt("Organization name"); if (!name) return;
    await call("/v1/orgs", { method: "POST", body: JSON.stringify({ name, billing_mode: "prepaid" }) }); await loadOrgs();
  };
  const createProject = async () => {
    const name = prompt("Project name"); if (!name) return;
    const overlay = prompt("Private network: wireguard or nebula", "wireguard") ?? "wireguard";
    await call(`/v1/orgs/${org!.id}/projects`, { method: "POST", body: JSON.stringify({ name, technical_email: user, overlay }) }); await openOrg(org!);
  };

  return (
    <div style={s.page}>
      <header style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <h1 style={{ color: "#0f766e" }}>SpadeAi</h1>
        {user ? <span>{user} <button style={s.btn} onClick={() => auth.signoutRedirect()}>Sign out</button></span>
              : <button style={s.btn} onClick={() => auth.signinRedirect()}>Sign in</button>}
      </header>
      {err && <div role="alert" style={{ ...s.card, background: "#fee2e2" }}>{err}</div>}

      {!user && <section style={s.card}><h3>Platform capabilities</h3>
        {caps.map(c => <div key={c.component}><span style={s.pill(c.status === "available")}>{c.status}</span> {c.component} <small>{c.reason}</small></div>)}
      </section>}

      {user && !org && <section style={s.card}><h3>Organizations</h3>
        {orgs.map(o => <div key={o.id}><a href="#" onClick={e => { e.preventDefault(); guard(() => openOrg(o))(); }}>{o.name}</a> <small>{o.billing_mode} · AI {o.ai_policy}</small></div>)}
        <button style={s.btn} onClick={guard(createOrg)}>New organization</button>
      </section>}

      {org && <section style={s.card}>
        <a href="#" onClick={e => { e.preventDefault(); setOrg(null); }}>← organizations</a>
        <h3>{org.name}</h3>
        {wallet && <p>Available {naira(wallet.balance.available_minor)} · Reserved {naira(wallet.balance.pending_debits_minor)} · Arrears {naira(wallet.arrears_minor)}</p>}
        <button style={s.btn} onClick={guard(topup)}>Top up (Paystack)</button>
        <button style={s.btn} onClick={guard(createProject)}>New project</button>
        <ul>{projects.map(p => <li key={p.id}><a href="#" onClick={e => { e.preventDefault(); guard(() => openProject(p))(); }}>{p.name}</a></li>)}</ul>
      </section>}

      {project && <section style={s.card}><h3>Project {project.name}</h3>
        <h4>Services</h4>
        {services.length === 0 && <p>No services yet.</p>}
        {services.map(v => <div key={v.id} style={{ marginBottom: 6 }}>
          <b>{v.display_name}</b> ({v.template}) <span style={s.pill(v.state === "running")}>{v.state}</span> {v.state_reason && <small>{v.state_reason}</small>}
          {["start", "stop", "restart", "delete"].map(a => <button key={a} style={{ ...s.btn, padding: "2px 8px" }} onClick={guard(() => act(v, a))}>{a}</button>)}
          <button style={{ ...s.btn, padding: "2px 8px" }} onClick={guard(() => backupNow(v))}>back up</button>
          <button style={{ ...s.btn, padding: "2px 8px" }} onClick={guard(() => showBackups(v))}>backups</button>
          {backups?.svc === v.id && <div style={{ marginLeft: 16 }}>
            {backups.items.length === 0 && <small>No backups yet.</small>}
            {backups.items.map(b => <div key={b.id}><small>{b.kind} {new Date(b.started_at).toLocaleString()} </small>
              <span style={s.pill(b.status === "succeeded")}>{b.status}</span> {b.error && <small>{b.error}</small>}
              {b.kind === "backup" && b.status === "succeeded" && <button style={{ ...s.btn, padding: "2px 8px" }} onClick={guard(() => restore(v.id, b))}>restore</button>}
            </div>)}
          </div>}
        </div>)}
        {overlay && <div style={{ marginTop: 8 }}>
          <h4>Private network ({overlay.kind}, {overlay.cidr})</h4>
          {overlay.peers.length === 0 && <p>No hosts have joined yet.</p>}
          {(overlay.peers ?? []).map(p => {
            const inSync = p.last_apply_status === "succeeded" && p.applied_generation === overlay.generation;
            return <div key={p.service_id}><b>{p.display_name}</b> {p.overlay_ip} <span style={s.pill(inSync)}>{inSync ? "in sync" : (p.last_apply_status ?? "pending")}</span></div>;
          })}
        </div>}
        {bootstrap && <div style={{ ...s.card, background: "#f0fdfa" }}><b>Run on your VM to attach it (expires in 15 min):</b><pre style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>{bootstrap}</pre></div>}
        <h4>Catalogue (tested versions only)</h4>
        {templates.map(t => <div key={t.slug}><b>{t.name}</b> {t.version} — {t.description} <button style={s.btn} onClick={guard(() => deploy(t))}>Deploy</button></div>)}
      </section>}
    </div>
  );
}
