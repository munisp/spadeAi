import { useEffect, useState, useCallback } from "react";
import { auth, call, setToken, naira, PAYMENTS } from "./api";

type Cap = { component: string; status: string; reason: string };
type Org = { id: string; name: string; billing_mode: string; ai_policy: string };
type Wallet = { arrears_minor: number; balance: { available_minor: number; pending_debits_minor: number; posted_balance_minor: number } };
type Project = { id: string; name: string; org_id: string; overlay_kind?: string };
type Template = { slug: string; name: string; version: string; description: string };
type Service = { id: string; display_name: string; template: string; state: string; state_reason: string | null; provider_id?: string };
type Cluster = { id: string; name: string; distribution: string; in_nigeria: boolean; enrolled: boolean; online: boolean; last_seen_at: string | null; agent_version: string | null; services: number };
type Field = { name: string; label: string; type: "select" | "int" | "text" | "map"; required?: boolean; options?: string[]; default?: string | number; min?: number; max?: number; help?: string };
type KubeMenu = { kubernetes_version: string; nodes: number; allocatable_cpu_milli: number; allocatable_memory_mb: number; inventory_at: string; fields: Field[] };
type KubeForm = { t: Template; name: string; cluster: Cluster; menu: KubeMenu; values: Record<string, string> };
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
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const [helmCmd, setHelmCmd] = useState("");
  const [kform, setKform] = useState<KubeForm | null>(null);

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
  useEffect(() => { if (err) window.scrollTo({ top: 0, behavior: "smooth" }); }, [err]);
  useEffect(() => {
    if (!project) return;
    const t = setInterval(() => {
      Promise.all([
        call<{ items: Service[] }>(`/v1/projects/${project.id}/services`).then(r => setServices(r.items ?? [])),
        call<{ items: Cluster[] }>(`/v1/projects/${project.id}/clusters`).then(r => setClusters(r.items ?? [])),
      ]).catch(() => undefined);
    }, 10000);
    return () => clearInterval(t);
  }, [project]); // eslint-disable-line react-hooks/exhaustive-deps

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
    setClusters((await call<{ items: Cluster[] }>(`/v1/projects/${p.id}/clusters`)).items ?? []);
  };
  const waitOp = async (id: string, onResponse?: (r: { bootstrap?: string }) => boolean) => {
    for (let i = 0; i < 300; i++) {
      const o = await call<{ status: string; response?: { bootstrap?: string }; error?: string }>(`/v1/operations/${id}`);
      if (o.response && onResponse?.(o.response)) return;
      if (o.status === "failed") throw new Error(o.error ?? "operation failed");
      if (o.status === "succeeded") return;
      await new Promise(r => setTimeout(r, 1000));
    }
  };
  const addCluster = async () => {
    const name = prompt("Cluster name (lowercase, e.g. lagos-dc1)"); if (!name) return;
    const distribution = prompt("Distribution: kubernetes | k3s | rke2 | openshift | eks | aks | gke", "kubernetes"); if (!distribution) return;
    const in_nigeria = confirm("Is this cluster physically located in Nigeria? (Residency-locked organisations can only deploy to Nigerian clusters.)");
    const r = await call<{ helm: string }>(`/v1/projects/${project!.id}/clusters`, { method: "POST", body: JSON.stringify({ name, distribution, in_nigeria }) });
    setHelmCmd(r.helm); await openProject(project!);
  };
  const refreshCluster = async (c: Cluster) => { await call(`/v1/clusters/${c.id}/refresh`, { method: "POST" }); setErr("Inventory requested; the menu updates once the agent reports (a few seconds)."); };
  const openKubeForm = async (t: Template, name: string, size: string) => {
    const online = clusters.filter(c => c.online);
    if (online.length === 0) throw new Error("no Kubernetes cluster is online in this project; add one and install the agent first");
    const pick = online.length === 1 ? online[0].name : prompt(`Cluster: ${online.map(c => c.name).join(" | ")}`, online[0].name);
    const cluster = online.find(c => c.name === pick); if (!cluster) return;
    const menu = await call<KubeMenu>(`/v1/clusters/${cluster.id}/options`);
    const values: Record<string, string> = { namespace: `${t.slug}-${name}`.toLowerCase().replace(/[^a-z0-9-]/g, "-").slice(0, 40) };
    for (const f of menu.fields) if (f.default !== undefined) values[f.name] = String(f.default);
    values.size = size;
    setKform({ t, name, cluster, menu, values });
  };
  const submitKube = async () => {
    const { t, name, cluster, values } = kform!;
    const env: Record<string, string> = {};
    for (const line of (values.env ?? "").split("\n").map(l => l.trim()).filter(Boolean)) {
      const i = line.indexOf("="); if (i < 1) throw new Error(`environment line "${line}" is not KEY=VALUE`);
      env[line.slice(0, i)] = line.slice(i + 1);
    }
    const num = (k: string) => (values[k] ? Number(values[k]) : undefined);
    const kubernetes = { cluster_id: cluster.id, namespace: values.namespace, replicas: num("replicas") ?? 1, storage_class: values.storage_class || undefined,
      storage_gb: num("storage_gb"), host: values.host || undefined, ingress_class: values.host ? values.ingress_class || undefined : undefined,
      tls_issuer: values.tls_issuer || undefined, network_policy: values.network_policy || undefined, ingress_namespace: values.ingress_namespace || undefined };
    const op = await call<{ id: string }>(`/v1/projects/${project!.id}/services`, { method: "POST", idempotent: true, body: JSON.stringify({ template: t.slug, display_name: name, size: values.size, env, kubernetes }) });
    setKform(null);
    await waitOp(op.id);
    await openProject(project!);
  };
  const scale = async (v: Service) => {
    const n = Number(prompt("Replicas (1-20)", "2")); if (!n) return;
    const op = await call<{ id: string }>(`/v1/services/${v.id}/actions/scale`, { method: "POST", idempotent: true, body: JSON.stringify({ replicas: n }) });
    await waitOp(op.id); await openProject(project!);
  };
  const deploy = async (t: Template) => {
    const name = prompt(`Name for ${t.name}`, t.slug); if (!name) return;
    const ready = (p: string) => p === "byovm" || (p === "kubernetes" && clusters.some(c => c.online)) || caps.find(c => c.component === `provider.${p}`)?.status === "available";
    const sizes = (await call<{ items: Size[] }>("/v1/sizes")).items ?? [];
    const offered = sizes.filter(z => ready(z.provider_id)).map(z => z.id);
    const missing = [...new Set(sizes.filter(z => !ready(z.provider_id)).map(z => z.provider_id))];
    const size = prompt(`Size: ${offered.join(" | ")}${missing.length ? ` (unavailable: ${missing.join(", ")})` : ""}`, "byovm:SMALL-1C-2G"); if (!size) return;
    if (!offered.includes(size)) throw new Error(`size ${size} is not offered by an available provider`);
    if (size.startsWith("kubernetes:")) return openKubeForm(t, name, size);
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
          {v.provider_id === "kubernetes" && <button style={{ ...s.btn, padding: "2px 8px" }} onClick={guard(() => scale(v))}>scale</button>}
          {v.provider_id !== "kubernetes" && <button style={{ ...s.btn, padding: "2px 8px" }} onClick={guard(() => backupNow(v))}>back up</button>}
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
        <h4>Kubernetes clusters</h4>
        {clusters.length === 0 && <p>No clusters. Add one to deploy into your own on-premises or cloud Kubernetes.</p>}
        {clusters.map(c => <div key={c.id}><b>{c.name}</b> <small>{c.distribution}{c.in_nigeria ? " · in Nigeria" : ""} · {c.services} services{c.agent_version ? ` · agent ${c.agent_version}` : ""}</small>{" "}
          <span style={s.pill(c.online)}>{c.online ? "online" : c.enrolled ? "offline" : "waiting for agent"}</span>
          {c.enrolled && <button style={{ ...s.btn, padding: "2px 8px", marginLeft: 6 }} onClick={guard(() => refreshCluster(c))}>refresh inventory</button>}</div>)}
        <button style={s.btn} onClick={guard(addCluster)}>Add Kubernetes cluster</button>
        {helmCmd && <div style={{ ...s.card, background: "#f0fdfa", marginTop: 8 }}><b>Install the agent in your cluster (outbound-only; the token is single-use and expires in 1 hour):</b>
          <pre style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>{helmCmd}</pre></div>}
        {kform && <div style={{ ...s.card, background: "#f8fafc", marginTop: 8 }}>
          <h4>Deploy {kform.t.name} to {kform.cluster.name}</h4>
          <small>Kubernetes {kform.menu.kubernetes_version} · {kform.menu.nodes} nodes · {(kform.menu.allocatable_cpu_milli / 1000).toFixed(1)} vCPU · {Math.round(kform.menu.allocatable_memory_mb / 1024)} GiB allocatable · inventory {new Date(kform.menu.inventory_at).toLocaleString()}</small>
          {kform.menu.fields.map(f => {
            const v = kform.values[f.name] ?? "";
            const set = (x: string) => setKform({ ...kform, values: { ...kform.values, [f.name]: x } });
            return <label key={f.name} style={{ display: "block", margin: "6px 0" }}>{f.label}{f.required ? " *" : ""}<br />
              {f.type === "select" ? <select value={v} onChange={e => set(e.target.value)}>{!f.required && <option value="">(none)</option>}{(f.options ?? []).map(o => <option key={o}>{o}</option>)}</select>
                : f.type === "map" ? <textarea rows={3} cols={50} placeholder="KEY=VALUE, one per line" value={v} onChange={e => set(e.target.value)} />
                : <input type={f.type === "int" ? "number" : "text"} min={f.min} max={f.max} value={v} onChange={e => set(e.target.value)} />}
              {f.help && <small> {f.help}</small>}</label>;
          })}
          <button style={s.btn} onClick={guard(submitKube)}>Deploy</button><button style={{ ...s.btn, background: "#6b7280" }} onClick={() => setKform(null)}>Cancel</button>
        </div>}
        {bootstrap && <div style={{ ...s.card, background: "#f0fdfa" }}><b>Run on your VM to attach it (expires in 15 min):</b><pre style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>{bootstrap}</pre></div>}
        <h4>Catalogue (tested versions only)</h4>
        {templates.map(t => <div key={t.slug}><b>{t.name}</b> {t.version} — {t.description} <button style={s.btn} onClick={guard(() => deploy(t))}>Deploy</button></div>)}
      </section>}
    </div>
  );
}
