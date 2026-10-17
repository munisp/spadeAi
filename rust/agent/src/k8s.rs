//! Kubernetes mode: one agent per customer cluster, running in-cluster with a ServiceAccount.
//! It only ever connects outwards to the gateway. Each SpadeAi service owns one namespace labelled
//! `spade.ai/service=<id>`; the agent refuses to touch namespaces that are not labelled for that service.

use anyhow::{anyhow, bail, Context, Result};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::time::Duration;
use tokio::io::AsyncWriteExt;
use tokio::process::Command;

const ALLOWED_KINDS: &[&str] = &[
    "Namespace",
    "Secret",
    "PersistentVolumeClaim",
    "Deployment",
    "Service",
    "Ingress",
    "NetworkPolicy",
];

pub async fn kubectl(args: &[&str], stdin: Option<&[u8]>) -> Result<String> {
    let mut child = Command::new("kubectl")
        .args(args)
        .stdin(if stdin.is_some() {
            std::process::Stdio::piped()
        } else {
            std::process::Stdio::null()
        })
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .context("spawn kubectl")?;
    if let Some(b) = stdin {
        let mut i = child.stdin.take().unwrap();
        i.write_all(b).await?;
        drop(i);
    }
    let out = child.wait_with_output().await?;
    let tail = |b: &[u8]| String::from_utf8_lossy(&b[b.len().saturating_sub(4000)..]).to_string();
    if !out.status.success() {
        bail!(
            "kubectl {} failed: {}",
            args.first().unwrap_or(&""),
            tail(&out.stderr)
        );
    }
    Ok(String::from_utf8_lossy(&out.stdout).to_string())
}

pub fn namespace(p: &Value) -> Result<String> {
    let n = p["namespace"]
        .as_str()
        .ok_or(anyhow!("payload.namespace missing"))?;
    let ok = !n.is_empty()
        && n.len() <= 63
        && n.chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')
        && !n.starts_with('-')
        && !n.ends_with('-');
    if !ok
        || n == "default"
        || n == "spade-system"
        || n.starts_with("kube-")
        || n.starts_with("openshift")
    {
        bail!("invalid or reserved namespace {n:?}");
    }
    Ok(n.to_string())
}

fn service_id(p: &Value) -> Result<String> {
    let s = p["service_id"]
        .as_str()
        .ok_or(anyhow!("payload.service_id missing"))?;
    if s.is_empty() || s.len() > 64 || !s.chars().all(|c| c.is_ascii_hexdigit() || c == '-') {
        bail!("invalid service_id");
    }
    Ok(s.to_string())
}

/// Owner of an existing namespace: Some("") if it exists unlabelled, None if absent.
async fn ns_owner(ns: &str) -> Result<Option<String>> {
    match kubectl(
        &["get", "namespace", ns, "--ignore-not-found", "-o", "json"],
        None,
    )
    .await?
    {
        s if s.trim().is_empty() => Ok(None),
        s => {
            let v: Value = serde_json::from_str(&s)?;
            Ok(Some(
                v["metadata"]["labels"]["spade.ai/service"]
                    .as_str()
                    .unwrap_or("")
                    .to_string(),
            ))
        }
    }
}

/// Defence in depth: the control plane renders manifests, but the agent independently refuses anything
/// outside the service's namespace or that would escape the pod sandbox.
pub fn check_manifests(list: &Value, ns: &str, svc: &str) -> Result<()> {
    if list["kind"] != "List" {
        bail!("manifests must be a v1 List");
    }
    for it in list["items"]
        .as_array()
        .ok_or(anyhow!("manifests.items missing"))?
    {
        let kind = it["kind"].as_str().unwrap_or("");
        if !ALLOWED_KINDS.contains(&kind) {
            bail!("kind {kind:?} is not allowed");
        }
        let md = &it["metadata"];
        if kind == "Namespace" {
            if md["name"] != ns || md["labels"]["spade.ai/service"] != svc {
                bail!("namespace object does not match the service");
            }
        } else if md["namespace"] != ns {
            bail!("{kind} {} is outside namespace {ns}", md["name"]);
        }
        if kind == "Deployment" {
            let pod = &it["spec"]["template"]["spec"];
            for k in ["hostNetwork", "hostPID", "hostIPC"] {
                if pod[k].as_bool().unwrap_or(false) {
                    bail!("{k} is not allowed");
                }
            }
            if pod["serviceAccountName"].is_string() {
                bail!("custom service accounts are not allowed");
            }
            for v in pod["volumes"].as_array().into_iter().flatten() {
                if v.get("persistentVolumeClaim").is_none() {
                    bail!("only persistentVolumeClaim volumes are allowed");
                }
            }
            for c in pod["containers"].as_array().into_iter().flatten() {
                let sc = &c["securityContext"];
                if sc["privileged"].as_bool().unwrap_or(false)
                    || sc["capabilities"]["add"].is_array()
                {
                    bail!("privileged containers and added capabilities are not allowed");
                }
            }
            if pod["initContainers"].is_array() {
                bail!("initContainers are not allowed");
            }
        }
    }
    Ok(())
}

async fn wait_available(ns: &str) -> Result<String> {
    kubectl(
        &[
            "wait",
            "--for=condition=Available",
            "deployment",
            "--all",
            "-n",
            ns,
            "--timeout=300s",
        ],
        None,
    )
    .await
}

async fn owned(ns: &str, svc: &str) -> Result<()> {
    match ns_owner(ns).await? {
        Some(o) if o == svc => Ok(()),
        Some(_) => bail!("namespace {ns} exists and is not managed for this service"),
        None => bail!("namespace {ns} does not exist"),
    }
}

pub async fn execute(verb: &str, p: &Value) -> Result<Value> {
    if verb == "k8s_inventory" {
        return inventory().await;
    }
    let ns = namespace(p)?;
    let svc = service_id(p)?;
    match verb {
        "apply_bundle" => {
            let text = p["manifests"]
                .as_str()
                .ok_or(anyhow!("manifests missing"))?;
            let want = p["manifests_sha256"]
                .as_str()
                .ok_or(anyhow!("manifests_sha256 missing"))?;
            if hex::encode(Sha256::digest(text.as_bytes())) != want {
                bail!("bundle integrity check failed");
            }
            let list: Value = serde_json::from_str(text)?;
            check_manifests(&list, &ns, &svc)?;
            if let Some(o) = ns_owner(&ns).await? {
                if o != svc {
                    bail!("namespace {ns} already exists and belongs to something else");
                }
            }
            let out = kubectl(&["apply", "-f", "-"], Some(text.as_bytes())).await?;
            let ready = wait_available(&ns).await?;
            Ok(json!({"applied": out, "ready": ready}))
        }
        "compose_up" | "compose_down" => {
            owned(&ns, &svc).await?;
            let n = if verb == "compose_down" {
                0
            } else {
                p["replicas"].as_u64().unwrap_or(1).clamp(1, 20)
            };
            let out = kubectl(
                &[
                    "scale",
                    "deployment",
                    "--all",
                    "-n",
                    &ns,
                    &format!("--replicas={n}"),
                ],
                None,
            )
            .await?;
            if n > 0 {
                wait_available(&ns).await?;
            }
            Ok(json!({"scaled": out, "replicas": n}))
        }
        "compose_restart" => {
            owned(&ns, &svc).await?;
            let out = kubectl(&["rollout", "restart", "deployment", "-n", &ns], None).await?;
            for d in kubectl(&["get", "deployment", "-n", &ns, "-o", "name"], None)
                .await?
                .lines()
            {
                kubectl(&["rollout", "status", d, "-n", &ns, "--timeout=300s"], None).await?;
            }
            Ok(json!({"restarted": out}))
        }
        "health_probe" => health(&p["health"], &ns).await,
        "collect_metrics" => {
            owned(&ns, &svc).await?;
            Ok(json!({"pods": kubectl(&["get", "pods", "-n", &ns, "-o", "wide"], None).await?}))
        }
        "k8s_teardown" => match ns_owner(&ns).await? {
            None => Ok(json!({"deleted": false, "reason": "namespace absent"})),
            Some(o) if o == svc => {
                kubectl(
                    &["delete", "namespace", &ns, "--wait=true", "--timeout=300s"],
                    None,
                )
                .await?;
                Ok(json!({"deleted": true}))
            }
            Some(_) => bail!("refusing to delete namespace {ns}: not managed for this service"),
        },
        other => bail!("{other} is not supported on Kubernetes"),
    }
}

/// Probes only the service's own in-cluster DNS name, never arbitrary URLs.
pub fn health_url(h: &Value, ns: &str) -> Result<String> {
    let url = h["url"].as_str().ok_or(anyhow!("health.url missing"))?;
    let u = reqwest::Url::parse(url)?;
    let host = u.host_str().unwrap_or("");
    if u.scheme() != "http"
        || !host.ends_with(&format!(".{ns}.svc.cluster.local"))
        || host.matches('.').count() != 4
    {
        bail!("health url must be http://<service>.{ns}.svc.cluster.local");
    }
    Ok(url.to_string())
}

async fn health(h: &Value, ns: &str) -> Result<Value> {
    let url = health_url(h, ns)?;
    let expect: Vec<u64> = h["expect_status"]
        .as_array()
        .map(|a| a.iter().filter_map(|v| v.as_u64()).collect())
        .unwrap_or(vec![200]);
    let c = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .timeout(Duration::from_secs(5))
        .build()?;
    let mut last = String::new();
    for _ in 0..60 {
        match c.get(&url).send().await {
            Ok(r) if expect.contains(&(r.status().as_u16() as u64)) => {
                return Ok(json!({"url": url, "status": r.status().as_u16()}))
            }
            Ok(r) => last = r.status().to_string(),
            Err(e) => last = e.to_string(),
        }
        tokio::time::sleep(Duration::from_secs(2)).await;
    }
    bail!("health probe {url} failed: {last}")
}

fn cpu_milli(q: &str) -> i64 {
    q.strip_suffix('m')
        .map(|v| v.parse().unwrap_or(0))
        .unwrap_or_else(|| q.parse::<i64>().unwrap_or(0) * 1000)
}

fn mem_mb(q: &str) -> i64 {
    for (suf, mul) in [
        ("Ki", 1024f64),
        ("Mi", 1048576.0),
        ("Gi", 1073741824.0),
        ("Ti", 1099511627776.0),
        ("k", 1e3),
        ("M", 1e6),
        ("G", 1e9),
    ] {
        if let Some(v) = q.strip_suffix(suf) {
            return (v.parse::<f64>().unwrap_or(0.0) * mul / 1048576.0) as i64;
        }
    }
    (q.parse::<f64>().unwrap_or(0.0) / 1048576.0) as i64
}

fn classes(v: &Value, annotation: &str) -> Vec<Value> {
    v["items"]
        .as_array()
        .into_iter()
        .flatten()
        .map(|i| json!({"name": i["metadata"]["name"], "default": i["metadata"]["annotations"][annotation] == "true"}))
        .collect()
}

/// What the configuration menu can offer: storage classes, ingress classes, cert-manager issuers, capacity.
pub async fn inventory() -> Result<Value> {
    let ver: Value = serde_json::from_str(&kubectl(&["version", "-o", "json"], None).await?)?;
    let sc: Value =
        serde_json::from_str(&kubectl(&["get", "storageclass", "-o", "json"], None).await?)?;
    let ic: Value =
        serde_json::from_str(&kubectl(&["get", "ingressclass", "-o", "json"], None).await?)?;
    let issuers: Vec<Value> = match kubectl(
        &["get", "clusterissuers.cert-manager.io", "-o", "json"],
        None,
    )
    .await
    {
        Ok(s) => serde_json::from_str::<Value>(&s)?["items"]
            .as_array()
            .into_iter()
            .flatten()
            .map(|i| i["metadata"]["name"].clone())
            .collect(),
        Err(_) => vec![], // cert-manager not installed
    };
    let nodes: Value =
        serde_json::from_str(&kubectl(&["get", "nodes", "-o", "json"], None).await?)?;
    let (mut cpu, mut mem, mut n) = (0i64, 0i64, 0);
    for node in nodes["items"].as_array().into_iter().flatten() {
        n += 1;
        cpu += cpu_milli(node["status"]["allocatable"]["cpu"].as_str().unwrap_or("0"));
        mem += mem_mb(
            node["status"]["allocatable"]["memory"]
                .as_str()
                .unwrap_or("0"),
        );
    }
    Ok(json!({
        "version": ver["serverVersion"]["gitVersion"],
        "nodes": n, "allocatable_cpu_milli": cpu, "allocatable_memory_mb": mem,
        "storage_classes": classes(&sc, "storageclass.kubernetes.io/is-default-class"),
        "ingress_classes": classes(&ic, "ingressclass.kubernetes.io/is-default-class"),
        "tls_issuers": issuers,
    }))
}

/// Restores the agent identity from its Secret, or enrols (CA verified against the pinned SHA-256 of
/// its DER) and stores the identity in the Secret so pod restarts don't need a new token. A different
/// enrolment token (the cluster was re-registered) replaces the stored identity.
pub async fn bootstrap(
    gateway: &str,
    ca_sha256: &str,
    state: &std::path::Path,
    secret: &str,
    ns: &str,
) -> Result<()> {
    use base64::Engine;
    let b64 = base64::engine::general_purpose::STANDARD;
    std::fs::create_dir_all(state)?;
    let token = std::env::var("SPADE_ENROL_TOKEN")
        .ok()
        .filter(|t| !t.is_empty());
    let token_hash = token
        .as_deref()
        .map(|t| hex::encode(Sha256::digest(t.as_bytes())));
    let got = kubectl(
        &[
            "get",
            "secret",
            secret,
            "-n",
            ns,
            "--ignore-not-found",
            "-o",
            "json",
        ],
        None,
    )
    .await?;
    if !got.trim().is_empty() {
        let v: Value = serde_json::from_str(&got)?;
        let data = v["data"].as_object().cloned().unwrap_or_default();
        let stored_hash = data
            .get("token.sha256")
            .and_then(|x| x.as_str())
            .and_then(|x| b64.decode(x).ok())
            .map(|x| String::from_utf8_lossy(&x).into_owned());
        let same_token = token_hash.is_none() || stored_hash == token_hash;
        let complete = ["agent.key", "agent.crt", "ca.pem"]
            .iter()
            .all(|k| data.contains_key(*k));
        if same_token && complete {
            for k in ["agent.key", "agent.crt", "ca.pem"] {
                let bytes = b64.decode(data[k].as_str().unwrap_or(""))?;
                crate::write_private(&state.join(k), &bytes)?;
            }
            eprintln!("restored agent identity from secret {ns}/{secret}");
            return Ok(());
        }
        if !same_token {
            eprintln!("new enrolment token supplied; replacing stored identity");
        }
    }
    let token =
        token.ok_or_else(|| anyhow!("no stored identity and SPADE_ENROL_TOKEN is not set"))?;
    let pem = reqwest::Client::builder()
        .use_rustls_tls()
        .danger_accept_invalid_certs(true) // trust comes from the pin check below, not from TLS
        .timeout(Duration::from_secs(20))
        .build()?
        .get(format!("{gateway}/agent/v1/ca.pem"))
        .send()
        .await?
        .error_for_status()?
        .text()
        .await?;
    let body: String = pem.lines().filter(|l| !l.starts_with("-----")).collect();
    let der = b64.decode(body.trim()).context("CA PEM")?;
    if !hex::encode(Sha256::digest(&der)).eq_ignore_ascii_case(ca_sha256.trim()) {
        bail!("gateway CA does not match the pinned caSha256; refusing to enrol");
    }
    let ca_path = state.join("pinned-ca.pem");
    std::fs::write(&ca_path, pem.as_bytes())?;
    crate::enrol(gateway, &ca_path, &token, state).await?;
    std::fs::write(state.join("token.sha256"), token_hash.unwrap_or_default())?;
    let dir = state.to_str().ok_or(anyhow!("state dir"))?;
    let files: Vec<String> = ["agent.key", "agent.crt", "ca.pem", "token.sha256"]
        .iter()
        .map(|f| format!("--from-file={dir}/{f}"))
        .collect();
    let mut args = vec![
        "create",
        "secret",
        "generic",
        secret,
        "-n",
        ns,
        "--dry-run=client",
        "-o",
        "json",
    ];
    args.extend(files.iter().map(|s| s.as_str()));
    let manifest = kubectl(&args, None).await?;
    kubectl(&["apply", "-f", "-"], Some(manifest.as_bytes())).await?;
    eprintln!("enrolled; identity stored in secret {ns}/{secret}");
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    fn list(items: Value) -> Value {
        json!({"kind": "List", "items": items})
    }
    #[test]
    fn manifests_are_confined() {
        let ns = json!({"kind": "Namespace", "metadata": {"name": "shop", "labels": {"spade.ai/service": "ab-12"}}});
        let dep = |pod: Value| json!({"kind": "Deployment", "metadata": {"name": "app", "namespace": "shop"}, "spec": {"template": {"spec": pod}}});
        assert!(check_manifests(
            &list(json!([ns, dep(json!({"containers": [{"name": "a"}]}))])),
            "shop",
            "ab-12"
        )
        .is_ok());
        assert!(check_manifests(&list(json!([ns])), "shop", "other").is_err());
        assert!(check_manifests(
            &list(json!([{"kind": "ClusterRoleBinding", "metadata": {"name": "x"}}])),
            "shop",
            "ab-12"
        )
        .is_err());
        assert!(check_manifests(
            &list(
                json!([{"kind": "Secret", "metadata": {"name": "x", "namespace": "kube-system"}}])
            ),
            "shop",
            "ab-12"
        )
        .is_err());
        assert!(check_manifests(
            &list(json!([dep(json!({"hostNetwork": true}))])),
            "shop",
            "ab-12"
        )
        .is_err());
        assert!(check_manifests(
            &list(json!([dep(
                json!({"volumes": [{"name": "h", "hostPath": {"path": "/"}}]})
            )])),
            "shop",
            "ab-12"
        )
        .is_err());
        assert!(check_manifests(
            &list(json!([dep(
                json!({"containers": [{"securityContext": {"privileged": true}}]})
            )])),
            "shop",
            "ab-12"
        )
        .is_err());
    }
    #[test]
    fn namespaces_and_health_urls_are_validated() {
        assert!(namespace(&json!({"namespace": "shop"})).is_ok());
        for bad in [
            "kube-system",
            "default",
            "spade-system",
            "Shop",
            "-x",
            "a/b",
        ] {
            assert!(namespace(&json!({"namespace": bad})).is_err(), "{bad}");
        }
        assert!(health_url(
            &json!({"url": "http://app.shop.svc.cluster.local:80/"}),
            "shop"
        )
        .is_ok());
        assert!(health_url(
            &json!({"url": "http://kubernetes.default.svc.cluster.local/"}),
            "shop"
        )
        .is_err());
        assert!(health_url(&json!({"url": "http://169.254.169.254/latest"}), "shop").is_err());
        assert!(health_url(
            &json!({"url": "http://a.b.shop.svc.cluster.local/"}),
            "shop"
        )
        .is_err());
    }
    #[test]
    fn quantities_parse() {
        assert_eq!(cpu_milli("4"), 4000);
        assert_eq!(cpu_milli("3900m"), 3900);
        assert_eq!(mem_mb("16Gi"), 16384);
        assert_eq!(mem_mb("2048Mi"), 2048);
        assert_eq!(mem_mb("8000000Ki"), 7812);
    }
}
