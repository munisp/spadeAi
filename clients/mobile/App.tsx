import { useEffect, useState } from "react";
import { Alert, Button, FlatList, SafeAreaView, ScrollView, Text, TextInput, View, StyleSheet } from "react-native";
import * as AuthSession from "expo-auth-session";
import * as SecureStore from "expo-secure-store";
import * as WebBrowser from "expo-web-browser";
import Constants from "expo-constants";

WebBrowser.maybeCompleteAuthSession();
const extra = (Constants.expoConfig?.extra ?? {}) as { api: string; issuer: string };
const API = extra.api;

type Org = { id: string; name: string; billing_mode: string };
type Wallet = { arrears_minor: number; balance: { available_minor: number; pending_debits_minor: number } };
type Service = { id: string; display_name: string; template: string; state: string; overlay_ip?: string; provider_id?: string };
type Cluster = { id: string; name: string; distribution: string; in_nigeria: boolean; enrolled: boolean; online: boolean; services: number; project: string; projectId: string };
type Field = { name: string; label: string; type: "select" | "int" | "text" | "map"; required?: boolean; options?: string[]; default?: string | number; min?: number; max?: number; help?: string };
type KubeMenu = { kubernetes_version: string; nodes: number; fields: Field[] };
type Template = { slug: string; name: string };
type Overlay = { kind: string; peers: { service_id: string; overlay_ip: string }[] };
type Project = { id: string; name: string; org_id: string };

const naira = (k: number) => `₦${(k / 100).toLocaleString("en-NG", { minimumFractionDigits: 2 })}`;
const uuid = () => Array.from({ length: 32 }, () => Math.floor(Math.random() * 16).toString(16)).join("");

export default function App() {
  const discovery = AuthSession.useAutoDiscovery(extra.issuer);
  const redirectUri = AuthSession.makeRedirectUri({ scheme: "spadeai", path: "auth/callback" });
  const [req, res, prompt] = AuthSession.useAuthRequest({ clientId: "spade-mobile", redirectUri, scopes: ["openid", "profile", "email"], usePKCE: true }, discovery);
  const [token, setToken] = useState<string | null>(null);
  const [orgs, setOrgs] = useState<Org[]>([]);
  const [org, setOrg] = useState<Org | null>(null);
  const [wallet, setWallet] = useState<Wallet | null>(null);
  const [services, setServices] = useState<(Service & { project: string })[]>([]);
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const [scaleFor, setScaleFor] = useState<{ id: string; n: string } | null>(null);
  const [kform, setKform] = useState<{ cluster: Cluster; menu: KubeMenu; values: Record<string, string>; templates: Template[] } | null>(null);

  const call = async <T,>(path: string, init: RequestInit & { idempotent?: boolean } = {}): Promise<T> => {
    const headers: Record<string, string> = { Authorization: `Bearer ${token}`, "Content-Type": "application/json" };
    if (init.idempotent) headers["Idempotency-Key"] = uuid();
    const r = await fetch(API + path, { ...init, headers });
    const b = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(b?.error?.message ?? `HTTP ${r.status}`);
    return b as T;
  };
  const fail = (e: unknown) => Alert.alert("SpadeAi", String((e as Error).message ?? e));

  useEffect(() => { SecureStore.getItemAsync("spade_token").then(t => t && setToken(t)); }, []);
  useEffect(() => {
    if (res?.type !== "success" || !discovery) return;
    AuthSession.exchangeCodeAsync({ clientId: "spade-mobile", code: res.params.code, redirectUri, extraParams: { code_verifier: req?.codeVerifier ?? "" } }, discovery)
      .then(async t => { await SecureStore.setItemAsync("spade_token", t.accessToken); setToken(t.accessToken); }).catch(fail);
  }, [res]);
  useEffect(() => { if (token) call<{ items: Org[] }>("/v1/orgs").then(r => setOrgs(r.items ?? [])).catch(async e => { await SecureStore.deleteItemAsync("spade_token"); setToken(null); fail(e); }); }, [token]);

  const open = async (o: Org) => {
    setOrg(o);
    setWallet(await call(`/v1/orgs/${o.id}/wallet`));
    const ps = ((await call<{ items: Project[] }>("/v1/projects")).items ?? []).filter(p => p.org_id === o.id);
    const all = await Promise.all(ps.map(async p => {
      const [svcs, ov] = await Promise.all([call<{ items: Service[] }>(`/v1/projects/${p.id}/services`), call<Overlay>(`/v1/projects/${p.id}/overlay`)]);
      const ips = new Map((ov.peers ?? []).map(x => [x.service_id, x.overlay_ip]));
      return (svcs.items ?? []).map(s => ({ ...s, project: `${p.name} · ${ov.kind}`, overlay_ip: ips.get(s.id) }));
    }));
    setServices(all.flat());
    const cs = await Promise.all(ps.map(async p => ((await call<{ items: Cluster[] }>(`/v1/projects/${p.id}/clusters`)).items ?? []).map(c => ({ ...c, project: p.name, projectId: p.id }))));
    setClusters(cs.flat());
  };
  const configure = async (c: Cluster) => {
    const [menu, t] = await Promise.all([call<KubeMenu>(`/v1/clusters/${c.id}/options`), call<{ items: Template[] }>("/v1/templates")]);
    const values: Record<string, string> = { template: t.items?.[0]?.slug ?? "", name: "" };
    for (const f of menu.fields) if (f.default !== undefined) values[f.name] = String(f.default);
    setKform({ cluster: c, menu, values, templates: t.items ?? [] });
  };
  const deployKube = async () => {
    const { cluster, values } = kform!;
    const env: Record<string, string> = {};
    for (const l of (values.env ?? "").split("\n").map(x => x.trim()).filter(Boolean)) {
      const i = l.indexOf("="); if (i < 1) throw new Error(`"${l}" is not KEY=VALUE`); env[l.slice(0, i)] = l.slice(i + 1);
    }
    const num = (k: string) => (values[k] ? Number(values[k]) : undefined);
    await call(`/v1/projects/${cluster.projectId}/services`, { method: "POST", idempotent: true, body: JSON.stringify({ template: values.template, display_name: values.name || values.template, size: values.size, env,
      kubernetes: { cluster_id: cluster.id, namespace: values.namespace, replicas: num("replicas") ?? 1, storage_class: values.storage_class || undefined, storage_gb: num("storage_gb"),
        host: values.host || undefined, ingress_class: values.host ? values.ingress_class || undefined : undefined, tls_issuer: values.tls_issuer || undefined,
        network_policy: values.network_policy || undefined, ingress_namespace: values.ingress_namespace || undefined } }) });
    setKform(null); Alert.alert("SpadeAi", "Deployment started"); await open(org!);
  };
  const scale = (s: Service, n: string) => call(`/v1/services/${s.id}/actions/scale`, { method: "POST", idempotent: true, body: JSON.stringify({ replicas: Number(n) }) })
    .then(() => { setScaleFor(null); Alert.alert("SpadeAi", `Scaling to ${n} replicas`); return open(org!); }).catch(fail);
  const act = (s: Service, a: string) => call(`/v1/services/${s.id}/actions/${a}`, { method: "POST", idempotent: true }).then(() => open(org!)).catch(fail);

  if (!token) return <SafeAreaView style={st.c}><Text style={st.h}>SpadeAi</Text><Button title="Sign in" disabled={!req} onPress={() => prompt()} /></SafeAreaView>;
  if (!org) return <SafeAreaView style={st.c}><Text style={st.h}>Organizations</Text>
    <FlatList data={orgs} keyExtractor={o => o.id} renderItem={({ item }) => <Button title={item.name} onPress={() => open(item).catch(fail)} />} /></SafeAreaView>;
  if (kform) {
    const v = kform.values; const set = (k: string, x: string) => setKform({ ...kform, values: { ...v, [k]: x } });
    const choose = (k: string, opts: string[]) => <View style={{ flexDirection: "row", flexWrap: "wrap" }}>{opts.map(o => <Button key={o} title={(v[k] === o ? "● " : "") + o} onPress={() => set(k, o)} />)}</View>;
    return <SafeAreaView style={st.c}><ScrollView>
      <Text style={st.h}>Deploy to {kform.cluster.name}</Text><Text>Kubernetes {kform.menu.kubernetes_version} · {kform.menu.nodes} nodes</Text>
      <Text style={st.l}>Template</Text>{choose("template", kform.templates.map(t => t.slug))}
      <Text style={st.l}>Name</Text><TextInput style={st.i} value={v.name} onChangeText={x => set("name", x)} />
      {kform.menu.fields.map(f => <View key={f.name}><Text style={st.l}>{f.label}{f.required ? " *" : ""}</Text>
        {f.type === "select" ? choose(f.name, [...(f.required ? [] : [""]), ...(f.options ?? [])])
          : f.type === "map" ? <TextInput style={st.i} multiline placeholder="KEY=VALUE per line" value={v[f.name] ?? ""} onChangeText={x => set(f.name, x)} />
          : <TextInput style={st.i} keyboardType={f.type === "int" ? "number-pad" : "default"} autoCapitalize="none" value={v[f.name] ?? ""} onChangeText={x => set(f.name, x)} />}
        {f.help && <Text style={{ color: "#6b7280" }}>{f.help}</Text>}</View>)}
      <Button title="Deploy" onPress={() => deployKube().catch(fail)} /><Button title="Cancel" onPress={() => setKform(null)} />
    </ScrollView></SafeAreaView>;
  }
  return <SafeAreaView style={st.c}>
    <Button title="← Organizations" onPress={() => setOrg(null)} />
    <Text style={st.h}>{org.name}</Text>
    {wallet && <Text>Available {naira(wallet.balance.available_minor)} · Reserved {naira(wallet.balance.pending_debits_minor)} · Arrears {naira(wallet.arrears_minor)}</Text>}
    {clusters.map(c => <View key={c.id} style={st.card}><Text style={{ fontWeight: "600" }}>☸ {c.name} · {c.project}</Text>
      <Text>{c.distribution}{c.in_nigeria ? " · in Nigeria" : ""} · {c.online ? "online" : c.enrolled ? "offline" : "waiting for agent (run the Helm command from the web app or CLI)"} · {c.services} services</Text>
      {c.enrolled && <View style={{ flexDirection: "row" }}><Button title="refresh inventory" onPress={() => call(`/v1/clusters/${c.id}/refresh`, { method: "POST" }).then(() => Alert.alert("SpadeAi", "Inventory requested")).catch(fail)} />
        {c.online && <Button title="configure & deploy" onPress={() => configure(c).catch(fail)} />}</View>}</View>)}
    <FlatList data={services} keyExtractor={s => s.id} renderItem={({ item }) => <View style={st.card}>
      <Text style={{ fontWeight: "600" }}>{item.display_name} · {item.project}</Text><Text>{item.template} — {item.state}{item.overlay_ip ? ` · private IP ${item.overlay_ip}` : ""}</Text>
      <View style={{ flexDirection: "row" }}>{["start", "stop", "restart"].map(a => <Button key={a} title={a} onPress={() => act(item, a)} />)}
        {item.provider_id === "kubernetes" && <Button title="scale" onPress={() => setScaleFor({ id: item.id, n: "2" })} />}
        {item.provider_id !== "kubernetes" && <Button title="back up" onPress={() => call(`/v1/services/${item.id}/backups`, { method: "POST", idempotent: true }).then(() => Alert.alert("SpadeAi", "Backup started")).catch(fail)} />}</View>
      {scaleFor?.id === item.id && <View style={{ flexDirection: "row", alignItems: "center" }}><Text>Replicas (1-20) </Text>
        <TextInput style={[st.i, { width: 60 }]} keyboardType="number-pad" value={scaleFor.n} onChangeText={n => setScaleFor({ id: item.id, n })} />
        <Button title="apply" onPress={() => scale(item, scaleFor.n)} /><Button title="cancel" onPress={() => setScaleFor(null)} /></View>}
    </View>} />
  </SafeAreaView>;
}

const st = StyleSheet.create({ c: { flex: 1, padding: 16 }, h: { fontSize: 22, fontWeight: "700", color: "#0f766e", marginVertical: 12 }, l: { marginTop: 8, fontWeight: "600" }, i: { borderWidth: 1, borderColor: "#d1d5db", borderRadius: 6, padding: 6 }, card: { borderWidth: 1, borderColor: "#e5e7eb", borderRadius: 8, padding: 10, marginBottom: 8 } });
