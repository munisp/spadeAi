import { useEffect, useState } from "react";
import { Alert, Button, FlatList, SafeAreaView, Text, View, StyleSheet } from "react-native";
import * as AuthSession from "expo-auth-session";
import * as SecureStore from "expo-secure-store";
import * as WebBrowser from "expo-web-browser";
import Constants from "expo-constants";

WebBrowser.maybeCompleteAuthSession();
const extra = (Constants.expoConfig?.extra ?? {}) as { api: string; issuer: string };
const API = extra.api;

type Org = { id: string; name: string; billing_mode: string };
type Wallet = { arrears_minor: number; balance: { available_minor: number; pending_debits_minor: number } };
type Service = { id: string; display_name: string; template: string; state: string; overlay_ip?: string };
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
  };
  const act = (s: Service, a: string) => call(`/v1/services/${s.id}/actions/${a}`, { method: "POST", idempotent: true }).then(() => open(org!)).catch(fail);

  if (!token) return <SafeAreaView style={st.c}><Text style={st.h}>SpadeAi</Text><Button title="Sign in" disabled={!req} onPress={() => prompt()} /></SafeAreaView>;
  if (!org) return <SafeAreaView style={st.c}><Text style={st.h}>Organizations</Text>
    <FlatList data={orgs} keyExtractor={o => o.id} renderItem={({ item }) => <Button title={item.name} onPress={() => open(item).catch(fail)} />} /></SafeAreaView>;
  return <SafeAreaView style={st.c}>
    <Button title="← Organizations" onPress={() => setOrg(null)} />
    <Text style={st.h}>{org.name}</Text>
    {wallet && <Text>Available {naira(wallet.balance.available_minor)} · Reserved {naira(wallet.balance.pending_debits_minor)} · Arrears {naira(wallet.arrears_minor)}</Text>}
    <FlatList data={services} keyExtractor={s => s.id} renderItem={({ item }) => <View style={st.card}>
      <Text style={{ fontWeight: "600" }}>{item.display_name} · {item.project}</Text><Text>{item.template} — {item.state}{item.overlay_ip ? ` · private IP ${item.overlay_ip}` : ""}</Text>
      <View style={{ flexDirection: "row" }}>{["start", "stop", "restart"].map(a => <Button key={a} title={a} onPress={() => act(item, a)} />)}</View>
    </View>} />
  </SafeAreaView>;
}

const st = StyleSheet.create({ c: { flex: 1, padding: 16 }, h: { fontSize: 22, fontWeight: "700", color: "#0f766e", marginVertical: 12 }, card: { borderWidth: 1, borderColor: "#e5e7eb", borderRadius: 8, padding: 10, marginBottom: 8 } });
