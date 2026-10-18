//! spade-agent: runs on each managed VM, connects outbound-only to the SpadeAi gateway over mTLS,
//! and executes Docker Compose lifecycle commands. It never opens a listening port.
use anyhow::{anyhow, bail, Context, Result};
use clap::{Parser, Subcommand};
use serde::Deserialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use std::net::{IpAddr, SocketAddr, ToSocketAddrs, UdpSocket};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::OnceLock;
use std::time::Duration;
use tokio::io::AsyncWriteExt;
use tokio::process::{Child, Command};
use tokio::sync::Mutex;

mod k8s;

const VERSION: &str = env!("CARGO_PKG_VERSION");

#[derive(Parser)]
#[command(name = "spade-agent", version)]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// Exchange a single-use enrolment token for a client certificate.
    Enrol {
        #[arg(long)]
        gateway: String,
        #[arg(long)]
        ca: PathBuf,
        #[arg(long)]
        token: String,
        #[arg(long, default_value = "/etc/spade")]
        state_dir: PathBuf,
    },
    /// Long-poll the gateway and execute commands.
    Run {
        #[arg(long)]
        gateway: String,
        #[arg(long, default_value = "/etc/spade")]
        state_dir: PathBuf,
        #[arg(long, default_value = "/var/lib/spade/stacks")]
        stacks_dir: PathBuf,
    },
    /// Run inside a Kubernetes cluster (installed by the Helm chart); manages one namespace per service.
    RunK8s {
        #[arg(long)]
        gateway: String,
        /// SHA-256 of the gateway CA certificate (DER), shown when the cluster was added.
        #[arg(long, env = "SPADE_CA_SHA256")]
        ca_sha256: String,
        #[arg(long, default_value = "/var/lib/spade")]
        state_dir: PathBuf,
        #[arg(long, default_value = "spade-agent-identity")]
        secret: String,
        #[arg(long, env = "POD_NAMESPACE", default_value = "spade-system")]
        namespace: String,
    },
}

#[derive(Deserialize)]
struct Command_ {
    id: String,
    verb: String,
    payload: Value,
}

#[tokio::main]
async fn main() -> Result<()> {
    match Cli::parse().cmd {
        Cmd::Enrol {
            gateway,
            ca,
            token,
            state_dir,
        } => enrol(&gateway, &ca, &token, &state_dir).await,
        Cmd::Run {
            gateway,
            state_dir,
            stacks_dir,
        } => run(&gateway, &state_dir, Some(&stacks_dir)).await,
        Cmd::RunK8s {
            gateway,
            ca_sha256,
            state_dir,
            secret,
            namespace,
        } => {
            k8s::bootstrap(&gateway, &ca_sha256, &state_dir, &secret, &namespace).await?;
            run(&gateway, &state_dir, None).await
        }
    }
}

fn ca_only_client(ca_pem: &[u8]) -> Result<reqwest::Client> {
    Ok(reqwest::Client::builder()
        .use_rustls_tls()
        .tls_built_in_root_certs(false)
        .add_root_certificate(reqwest::Certificate::from_pem(ca_pem)?)
        .timeout(Duration::from_secs(40))
        .build()?)
}

async fn enrol(gateway: &str, ca: &Path, token: &str, state: &Path) -> Result<()> {
    let ca_pem = std::fs::read(ca).context("read CA")?;
    let key = rcgen::KeyPair::generate()?;
    let params = rcgen::CertificateParams::new(vec!["spade-agent".into()])?;
    let csr = params.serialize_request(&key)?.pem()?;
    let resp = ca_only_client(&ca_pem)?
        .post(format!("{gateway}/agent/v1/enrol"))
        .json(&json!({"token": token, "csr_pem": csr}))
        .send()
        .await?;
    if !resp.status().is_success() {
        bail!(
            "enrolment rejected: {} {}",
            resp.status(),
            resp.text().await.unwrap_or_default()
        );
    }
    let body: Value = resp.json().await?;
    std::fs::create_dir_all(state)?;
    write_private(&state.join("agent.key"), key.serialize_pem().as_bytes())?;
    std::fs::write(
        state.join("agent.crt"),
        body["cert_pem"].as_str().ok_or(anyhow!("no cert"))?,
    )?;
    // Keep the CA that was pinned out of band; never adopt a different CA offered by the gateway.
    let offered = body["ca_pem"].as_str().ok_or(anyhow!("no ca"))?;
    if offered.trim() != String::from_utf8_lossy(&ca_pem).trim() {
        bail!("gateway offered a CA that differs from the pinned CA; refusing enrolment");
    }
    std::fs::write(state.join("ca.pem"), &ca_pem)?;
    println!(
        "enrolled as agent {}",
        body["agent_id"].as_str().unwrap_or("?")
    );
    Ok(())
}

fn write_private(p: &Path, data: &[u8]) -> Result<()> {
    std::fs::write(p, data)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(p, std::fs::Permissions::from_mode(0o600))?;
    }
    Ok(())
}

fn mtls_client(state: &Path) -> Result<reqwest::Client> {
    let ca = std::fs::read(state.join("ca.pem"))?;
    let mut ident = std::fs::read(state.join("agent.crt"))?;
    ident.extend(std::fs::read(state.join("agent.key"))?);
    Ok(reqwest::Client::builder()
        .use_rustls_tls()
        .tls_built_in_root_certs(false)
        .add_root_certificate(reqwest::Certificate::from_pem(&ca)?)
        .identity(reqwest::Identity::from_pem(&ident)?)
        .timeout(Duration::from_secs(40))
        .build()?)
}

/// `stacks` is the Compose stacks directory on a VM, or None in Kubernetes mode.
async fn run(gateway: &str, state: &Path, stacks: Option<&Path>) -> Result<()> {
    let client = mtls_client(state)?;
    if let Some(stacks) = stacks {
        std::fs::create_dir_all(stacks)?;
        if let Err(e) = restore_overlays(state).await {
            eprintln!("overlay restore: {e:#}");
        }
    }
    let env = Env { state, gateway };
    let hb_client = client.clone();
    let hb_gw = gateway.to_string();
    tokio::spawn(async move {
        loop {
            let _ = hb_client
                .post(format!("{hb_gw}/agent/v1/heartbeat"))
                .json(&json!({"version": VERSION}))
                .send()
                .await;
            tokio::time::sleep(Duration::from_secs(30)).await;
        }
    });
    let mut backoff = 1u64;
    loop {
        match client
            .get(format!("{gateway}/agent/v1/commands/next"))
            .send()
            .await
        {
            Ok(r) if r.status() == 204 => backoff = 1,
            Ok(r) if r.status().is_success() => {
                backoff = 1;
                let cmd: Command_ = r.json().await?;
                let res = match stacks {
                    Some(stacks) => execute(&cmd, stacks, &env).await,
                    None => k8s::execute(&cmd.verb, &cmd.payload).await,
                };
                let (status, result) = match res {
                    Ok(v) => ("succeeded", v),
                    Err(e) => ("failed", json!({"error": format!("{e:#}")})),
                };
                eprintln!("command {} {} -> {}", cmd.id, cmd.verb, status);
                report(
                    &client,
                    gateway,
                    &cmd.id,
                    &json!({"status": status, "result": result}),
                )
                .await;
            }
            Ok(r) => {
                eprintln!("gateway returned {}", r.status());
                if r.status() == 403 {
                    bail!("certificate revoked; re-enrol required");
                }
                tokio::time::sleep(Duration::from_secs(backoff)).await;
                backoff = (backoff * 2).min(60);
            }
            Err(e) => {
                eprintln!("gateway unreachable: {e}");
                tokio::time::sleep(Duration::from_secs(backoff)).await;
                backoff = (backoff * 2).min(60);
            }
        }
    }
}

/// Delivers a command result, retrying transient failures so a completed command is not lost.
/// Gives up on 4xx (e.g. 409: the command already expired or was recorded).
async fn report(client: &reqwest::Client, gateway: &str, id: &str, body: &Value) {
    let mut wait = 1u64;
    for attempt in 1..=30 {
        match client
            .post(format!("{gateway}/agent/v1/commands/{id}/result"))
            .json(body)
            .send()
            .await
        {
            Ok(r) if r.status().is_success() => return,
            Ok(r) if r.status().is_client_error() => {
                eprintln!("result for {id} rejected: {}", r.status());
                return;
            }
            Ok(r) => eprintln!("result for {id} attempt {attempt}: gateway {}", r.status()),
            Err(e) => eprintln!("result for {id} attempt {attempt}: {e}"),
        }
        tokio::time::sleep(Duration::from_secs(wait)).await;
        wait = (wait * 2).min(30);
    }
    eprintln!("giving up delivering result for {id}");
}

/// Validates a compose project name so it can never escape the stacks directory.
fn project_name(payload: &Value) -> Result<String> {
    let p = payload["project"]
        .as_str()
        .ok_or(anyhow!("payload.project missing"))?;
    if p.is_empty()
        || p.len() > 63
        || !p
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')
    {
        bail!("invalid project name");
    }
    Ok(p.to_string())
}

async fn compose(dir: &Path, project: &str, args: &[&str]) -> Result<Value> {
    // Service env is passed as process environment, never through a .env file, so values are not
    // re-interpolated by Compose (e.g. a value containing ${HOME} stays literal).
    let env: std::collections::BTreeMap<String, String> = match std::fs::read(dir.join("env.json"))
    {
        Ok(b) => serde_json::from_slice(&b).context("read env.json")?,
        Err(_) => Default::default(),
    };
    let out = Command::new("docker")
        .envs(&env)
        .arg("compose")
        .arg("-p")
        .arg(project)
        .arg("--project-directory")
        .arg(dir)
        .args(args)
        .output()
        .await
        .context("spawn docker compose")?;
    let tail = |b: &[u8]| String::from_utf8_lossy(&b[b.len().saturating_sub(4000)..]).to_string();
    if !out.status.success() {
        bail!("docker compose {:?} failed: {}", args, tail(&out.stderr));
    }
    Ok(json!({"stdout": tail(&out.stdout), "stderr": tail(&out.stderr)}))
}

struct Env<'a> {
    state: &'a Path,
    gateway: &'a str,
}

async fn execute(cmd: &Command_, stacks: &Path, env: &Env<'_>) -> Result<Value> {
    let project = project_name(&cmd.payload)?;
    let dir = stacks.join(&project);
    match cmd.verb.as_str() {
        "apply_bundle" => {
            let compose_text = cmd.payload["compose"]
                .as_str()
                .ok_or(anyhow!("compose missing"))?;
            let want = cmd.payload["compose_sha256"]
                .as_str()
                .ok_or(anyhow!("compose_sha256 missing"))?;
            let got = hex::encode(Sha256::digest(compose_text.as_bytes()));
            if got != want {
                bail!("bundle integrity check failed");
            }
            std::fs::create_dir_all(&dir)?;
            std::fs::write(dir.join("docker-compose.yml"), compose_text)?;
            let env = service_env(&cmd.payload["env"])?;
            let _ = std::fs::remove_file(dir.join(".env"));
            write_private(&dir.join("env.json"), &serde_json::to_vec(&env)?)?;
            compose(&dir, &project, &["up", "-d", "--remove-orphans"]).await
        }
        "compose_up" => compose(&dir, &project, &["up", "-d"]).await,
        "compose_down" => compose(&dir, &project, &["stop"]).await,
        "compose_restart" => compose(&dir, &project, &["restart"]).await,
        "health_probe" => health(&cmd.payload["health"]).await,
        "collect_metrics" => compose(&dir, &project, &["ps", "--format", "json"]).await,
        "backup_run" => backup_run(&cmd.payload, &dir, &project).await,
        "backup_restore" => backup_restore(&cmd.payload, &dir, &project).await,
        "overlay_keygen" => overlay_keygen(&cmd.payload, env).await,
        "overlay_apply" => overlay_apply(&cmd.payload, env.state).await,
        "overlay_down" => overlay_down(&cmd.payload, env.state).await,
        other => bail!("unsupported verb {other}"),
    }
}

// ---- backups (restic) ----
// Repository URL and password arrive only in the delivered command (injected by the gateway) and are
// passed to restic via its environment; they are never written to disk.

fn restic_env(p: &Value) -> Result<Vec<(String, String)>> {
    let r = &p["restic"];
    let repo = r["repository"].as_str().ok_or(anyhow!(
        "restic credentials missing (gateway did not inject them)"
    ))?;
    let pw = r["password"]
        .as_str()
        .ok_or(anyhow!("restic password missing"))?;
    Ok(vec![
        ("RESTIC_REPOSITORY".into(), repo.into()),
        ("RESTIC_PASSWORD".into(), pw.into()),
    ])
}

async fn restic(env: &[(String, String)], args: &[&str]) -> Result<String> {
    let out = Command::new("restic")
        .envs(env.iter().map(|(k, v)| (k.as_str(), v.as_str())))
        .args(args)
        .output()
        .await
        .context("spawn restic (is it installed?)")?;
    if !out.status.success() {
        let e = String::from_utf8_lossy(&out.stderr);
        bail!(
            "restic {:?} failed: {}",
            args.first(),
            &e[e.len().saturating_sub(1500)..]
        );
    }
    Ok(String::from_utf8_lossy(&out.stdout).to_string())
}

/// Host paths of the compose project's named volumes.
async fn project_volumes(project: &str) -> Result<Vec<String>> {
    let names = run_tool(
        "docker",
        &[
            "volume",
            "ls",
            "-q",
            "--filter",
            &format!("label=com.docker.compose.project={project}"),
        ],
        None,
    )
    .await?;
    let mut paths = vec![];
    for n in names.lines().filter(|l| !l.is_empty()) {
        paths.push(
            run_tool(
                "docker",
                &["volume", "inspect", "-f", "{{.Mountpoint}}", n],
                None,
            )
            .await?
            .trim()
            .to_string(),
        );
    }
    Ok(paths)
}

async fn backup_run(p: &Value, dir: &Path, project: &str) -> Result<Value> {
    let env = restic_env(p)?;
    if restic(&env, &["cat", "config"]).await.is_err() {
        restic(&env, &["init"]).await?;
    }
    let mut paths = project_volumes(project).await?;
    paths.push(path_str(dir)?.to_string());
    // Quiesce only the containers running now and restart exactly those, so a backup never changes
    // which containers run (stopped or partially stopped services stay that way).
    let ids: Vec<String> = run_tool(
        "docker",
        &["compose", "-p", project, "ps", "-q", "--status", "running"],
        None,
    )
    .await?
    .split_whitespace()
    .map(String::from)
    .collect();
    let quiesce = p["quiesce"].as_bool().unwrap_or(true) && !ids.is_empty();
    let with_ids = |verb: &'static str| {
        let mut a = vec![verb];
        a.extend(ids.iter().map(String::as_str));
        a
    };
    if quiesce {
        if let Err(e) = run_tool("docker", &with_ids("stop"), None).await {
            let _ = run_tool("docker", &with_ids("start"), None).await;
            return Err(e);
        }
    }
    let mut args = vec!["backup", "--json", "--tag", "spade", "--tag", project];
    args.extend(paths.iter().map(String::as_str));
    let res = restic(&env, &args).await;
    if quiesce {
        // Always restart the app, even when the backup failed.
        run_tool("docker", &with_ids("start"), None).await?;
    }
    let out = res?;
    let summary: Value = out
        .lines()
        .rev()
        .filter_map(|l| serde_json::from_str::<Value>(l).ok())
        .find(|v| v["message_type"] == "summary")
        .ok_or(anyhow!("restic produced no summary"))?;
    // Retention runs from the control plane: this host's login is append-only and cannot delete.
    Ok(
        json!({"snapshot_id": summary["snapshot_id"], "bytes_added": summary["data_added"], "paths": paths, "quiesced": quiesce}),
    )
}

async fn backup_restore(p: &Value, dir: &Path, project: &str) -> Result<Value> {
    let env = restic_env(p)?;
    let snap = p["snapshot"].as_str().unwrap_or_default();
    if snap.len() < 8 || snap.len() > 64 || !snap.chars().all(|c| c.is_ascii_hexdigit()) {
        bail!("data unchanged: invalid snapshot id");
    }
    // Every step before the swap leaves live data untouched; errors say so, so the control plane
    // can keep the service's state instead of marking it failed.
    let unchanged = |e: anyhow::Error| anyhow!("data unchanged: {e:#}");
    let listed: Vec<Value> = serde_json::from_str(
        &restic(&env, &["snapshots", "--json", "--tag", project, snap])
            .await
            .map_err(unchanged)?,
    )
    .map_err(|e| unchanged(e.into()))?;
    let Some(meta) = listed.first() else {
        bail!("data unchanged: snapshot {snap} is not in this service's repository (pruned?)");
    };
    let stack = path_str(dir)?.to_string();
    let mut allowed = project_volumes(project).await.map_err(unchanged)?;
    allowed.push(stack.clone());
    let paths: Vec<String> = meta["paths"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|v| v.as_str().map(String::from))
                .collect()
        })
        .unwrap_or_default();
    if paths.is_empty() || paths.iter().any(|x| !allowed.contains(x)) {
        bail!("data unchanged: snapshot paths {paths:?} are outside this service's volumes and stack dir");
    }
    // Restore into a staging dir limited to those paths, never directly onto the host root.
    let stage = format!("{stack}.restore");
    let _ = std::fs::remove_dir_all(&stage);
    let mut args = vec!["restore", snap, "--target", stage.as_str()];
    for x in &paths {
        args.extend(["--include", x.as_str()]);
    }
    if let Err(e) = restic(&env, &args).await {
        let _ = std::fs::remove_dir_all(&stage);
        return Err(unchanged(e));
    }
    // A failed stop may have stopped some containers, so it is not reported as "data unchanged".
    if let Err(e) = compose(dir, project, &["stop"]).await {
        let _ = std::fs::remove_dir_all(&stage);
        bail!("restore aborted while stopping the app (volumes untouched, containers may be stopped): {e:#}");
    }
    let swap = async {
        for x in &paths {
            let staged = format!("{stage}{x}");
            if *x == stack {
                // Replace the stack dir wholesale (files added since the snapshot must not survive),
                // keeping the live copy until the staged one is fully in place.
                if Path::new(&staged).exists() {
                    let prev = format!("{stack}.prev");
                    let _ = std::fs::remove_dir_all(&prev);
                    std::fs::rename(&stack, &prev)?;
                    if let Err(e) = run_tool("cp", &["-a", &staged, &stack], None).await {
                        let _ = std::fs::remove_dir_all(&stack);
                        std::fs::rename(&prev, &stack)?;
                        return Err(e);
                    }
                    std::fs::remove_dir_all(&prev)?;
                }
                continue;
            }
            {
                // Empty volumes first so files created after the snapshot do not survive.
                for e in std::fs::read_dir(x)?.flatten() {
                    let path = e.path();
                    if path.is_dir() {
                        std::fs::remove_dir_all(&path)?;
                    } else {
                        std::fs::remove_file(&path)?;
                    }
                }
            }
            if Path::new(&staged).exists() {
                run_tool("cp", &["-a", &format!("{staged}/."), x], None).await?;
            }
        }
        anyhow::Ok(())
    }
    .await;
    let up = compose(dir, project, &["up", "-d"]).await;
    let _ = std::fs::remove_dir_all(&stage);
    swap?;
    up?;
    Ok(json!({"restored": snap, "paths": paths}))
}

// ---- private overlay (WireGuard / Nebula) ----
// Private keys are generated here and never leave the host; only public keys are reported.

fn nebula_procs() -> &'static Mutex<HashMap<String, Child>> {
    static P: OnceLock<Mutex<HashMap<String, Child>>> = OnceLock::new();
    P.get_or_init(|| Mutex::new(HashMap::new()))
}

/// Interface names are spw-/spn- + 8 hex chars, so they are safe as paths and command arguments.
fn overlay_iface(p: &Value) -> Result<String> {
    let i = p["iface"].as_str().ok_or(anyhow!("iface missing"))?;
    if i.len() != 12
        || !(i.starts_with("spw-") || i.starts_with("spn-"))
        || !i[4..].chars().all(|c| c.is_ascii_hexdigit())
    {
        bail!("invalid overlay interface {i}");
    }
    Ok(i.to_string())
}

fn check_cidr(s: &str) -> Result<()> {
    let (ip, bits) = s.split_once('/').ok_or(anyhow!("invalid cidr {s}"))?;
    let ip: IpAddr = ip.parse().map_err(|_| anyhow!("invalid cidr {s}"))?;
    let bits: u8 = bits.parse().map_err(|_| anyhow!("invalid cidr {s}"))?;
    if bits > if ip.is_ipv4() { 32 } else { 128 } {
        bail!("invalid cidr {s}");
    }
    Ok(())
}

fn check_wg_key(k: &str) -> Result<()> {
    if k.len() != 44
        || !k.ends_with('=')
        || !k
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || "+/=".contains(c))
    {
        bail!("invalid wireguard public key");
    }
    Ok(())
}

async fn run_tool(prog: &str, args: &[&str], stdin: Option<&[u8]>) -> Result<String> {
    let mut child = Command::new(prog)
        .args(args)
        .stdin(if stdin.is_some() {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .with_context(|| format!("spawn {prog} (is it installed?)"))?;
    if let (Some(data), Some(mut si)) = (stdin, child.stdin.take()) {
        si.write_all(data).await?;
    }
    let out = child.wait_with_output().await?;
    if !out.status.success() {
        bail!(
            "{prog} {args:?} failed: {}",
            String::from_utf8_lossy(&out.stderr).trim()
        );
    }
    Ok(String::from_utf8_lossy(&out.stdout).to_string())
}

/// Address other peers should use: SPADE_OVERLAY_ENDPOINT_IP, else the source IP of the route to the gateway.
fn endpoint_ip(gateway: &str) -> Result<IpAddr> {
    if let Ok(ip) = std::env::var("SPADE_OVERLAY_ENDPOINT_IP") {
        return Ok(ip.parse()?);
    }
    let url = reqwest::Url::parse(gateway)?;
    let host = url.host_str().ok_or(anyhow!("gateway has no host"))?;
    let addr = (host, url.port_or_known_default().unwrap_or(443))
        .to_socket_addrs()?
        .next()
        .ok_or(anyhow!("cannot resolve gateway"))?;
    let sock = UdpSocket::bind(if addr.is_ipv4() {
        "0.0.0.0:0"
    } else {
        "[::]:0"
    })?;
    sock.connect(addr)?;
    Ok(sock.local_addr()?.ip())
}

fn private_dir(p: &Path) -> Result<()> {
    std::fs::create_dir_all(p)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(p, std::fs::Permissions::from_mode(0o700))?;
    }
    Ok(())
}

async fn overlay_keygen(p: &Value, env: &Env<'_>) -> Result<Value> {
    let iface = overlay_iface(p)?;
    let port = p["listen_port"]
        .as_u64()
        .ok_or(anyhow!("listen_port missing"))? as u16;
    let dir = env.state.join("overlay").join(&iface);
    private_dir(&dir)?;
    let public_key = match p["kind"].as_str() {
        Some("wireguard") => {
            let k = dir.join("private.key");
            if !k.exists() {
                let key = run_tool("wg", &["genkey"], None).await?;
                write_private(&k, key.trim().as_bytes())?;
            }
            run_tool("wg", &["pubkey"], Some(&std::fs::read(&k)?))
                .await?
                .trim()
                .to_string()
        }
        Some("nebula") => {
            let (k, pubf) = (dir.join("host.key"), dir.join("host.pub"));
            if !k.exists() {
                run_tool(
                    "nebula-cert",
                    &[
                        "keygen",
                        "-out-key",
                        path_str(&k)?,
                        "-out-pub",
                        path_str(&pubf)?,
                    ],
                    None,
                )
                .await?;
                write_private(&k, &std::fs::read(&k)?)?;
            }
            std::fs::read_to_string(&pubf)?
        }
        other => bail!("unknown overlay kind {other:?}"),
    };
    let endpoint = SocketAddr::new(endpoint_ip(env.gateway)?, port);
    Ok(json!({"public_key": public_key, "endpoint": endpoint.to_string()}))
}

fn path_str(p: &Path) -> Result<&str> {
    p.to_str().ok_or(anyhow!("non-utf8 path"))
}

async fn overlay_apply(p: &Value, state: &Path) -> Result<Value> {
    let iface = overlay_iface(p)?;
    let dir = state.join("overlay").join(&iface);
    let port = p["listen_port"]
        .as_u64()
        .ok_or(anyhow!("listen_port missing"))?;
    let address = p["address"].as_str().ok_or(anyhow!("address missing"))?;
    check_cidr(address)?;
    match p["kind"].as_str() {
        Some("wireguard") => {
            let key = std::fs::read_to_string(dir.join("private.key"))
                .context("overlay_keygen has not run")?;
            let mut conf = format!(
                "[Interface]\nPrivateKey = {}\nListenPort = {port}\n",
                key.trim()
            );
            let peers = p["peers"].as_array().cloned().unwrap_or_default();
            for peer in &peers {
                let pk = peer["public_key"].as_str().unwrap_or_default();
                check_wg_key(pk)?;
                let ep: SocketAddr = peer["endpoint"]
                    .as_str()
                    .unwrap_or_default()
                    .parse()
                    .map_err(|_| anyhow!("invalid peer endpoint"))?;
                let allowed = peer["allowed_ips"].as_str().unwrap_or_default();
                check_cidr(allowed)?;
                conf.push_str(&format!("\n[Peer]\nPublicKey = {pk}\nEndpoint = {ep}\nAllowedIPs = {allowed}\nPersistentKeepalive = 25\n"));
            }
            write_private(&dir.join("wg.conf"), conf.as_bytes())?;
            std::fs::write(dir.join("address"), address)?;
            wg_up(&iface, &dir, address).await?;
            Ok(json!({"iface": iface, "peers": peers.len(), "generation": p["generation"]}))
        }
        Some("nebula") => {
            let ca = p["ca"].as_str().unwrap_or_default();
            let cert = p["cert"].as_str().unwrap_or_default();
            if !ca.contains("NEBULA CERTIFICATE") || !cert.contains("NEBULA CERTIFICATE") {
                bail!("nebula ca/cert missing");
            }
            let mut shm = serde_json::Map::new();
            for (ip, eps) in p["static_host_map"]
                .as_object()
                .cloned()
                .unwrap_or_default()
            {
                let _: IpAddr = ip
                    .parse()
                    .map_err(|_| anyhow!("invalid static_host_map ip"))?;
                let mut list = vec![];
                for e in eps.as_array().cloned().unwrap_or_default() {
                    let ep: SocketAddr = e
                        .as_str()
                        .unwrap_or_default()
                        .parse()
                        .map_err(|_| anyhow!("invalid static_host_map endpoint"))?;
                    list.push(json!(ep.to_string()));
                }
                shm.insert(ip, Value::Array(list));
            }
            let mut hosts = vec![];
            for h in p["lighthouses"].as_array().cloned().unwrap_or_default() {
                let ip: IpAddr = h
                    .as_str()
                    .unwrap_or_default()
                    .parse()
                    .map_err(|_| anyhow!("invalid lighthouse ip"))?;
                hosts.push(json!(ip.to_string()));
            }
            let unchanged_pki = std::fs::read_to_string(dir.join("ca.crt")).ok().as_deref()
                == Some(ca)
                && std::fs::read_to_string(dir.join("host.crt"))
                    .ok()
                    .as_deref()
                    == Some(cert);
            std::fs::write(dir.join("ca.crt"), ca)?;
            std::fs::write(dir.join("host.crt"), cert)?;
            let any = json!([{"port": "any", "proto": "any", "host": "any"}]);
            let cfg = json!({
                "pki": {"ca": dir.join("ca.crt"), "cert": dir.join("host.crt"), "key": dir.join("host.key")},
                "static_host_map": shm,
                "lighthouse": {"am_lighthouse": p["am_lighthouse"].as_bool().unwrap_or(false), "interval": 10, "hosts": hosts},
                "listen": {"host": "0.0.0.0", "port": port},
                "punchy": {"punch": true},
                "tun": {"dev": iface},
                "logging": {"level": "info"},
                "firewall": {"outbound": any, "inbound": any},
            });
            // JSON is valid YAML, so nebula reads it directly.
            let bytes = serde_json::to_vec_pretty(&cfg)?;
            let unchanged = unchanged_pki
                && std::fs::read(dir.join("config.yml")).ok().as_deref() == Some(&bytes[..]);
            // Restarting a lighthouse forgets every host it has learned, so only restart on a real change.
            if unchanged && nebula_running(&iface).await {
                return Ok(
                    json!({"iface": iface, "lighthouses": hosts, "generation": p["generation"], "restarted": false}),
                );
            }
            write_private(&dir.join("config.yml"), &bytes)?;
            nebula_start(&iface, &dir).await?;
            Ok(
                json!({"iface": iface, "lighthouses": hosts, "generation": p["generation"], "restarted": true}),
            )
        }
        other => bail!("unknown overlay kind {other:?}"),
    }
}

async fn wg_up(iface: &str, dir: &Path, address: &str) -> Result<()> {
    if run_tool("ip", &["link", "show", "dev", iface], None)
        .await
        .is_err()
    {
        run_tool(
            "ip",
            &["link", "add", "dev", iface, "type", "wireguard"],
            None,
        )
        .await?;
    }
    run_tool(
        "wg",
        &["syncconf", iface, path_str(&dir.join("wg.conf"))?],
        None,
    )
    .await?;
    run_tool("ip", &["address", "replace", address, "dev", iface], None).await?;
    run_tool("ip", &["link", "set", "up", "dev", iface], None).await?;
    Ok(())
}

async fn nebula_running(iface: &str) -> bool {
    match nebula_procs().lock().await.get_mut(iface) {
        Some(c) => matches!(c.try_wait(), Ok(None)),
        None => false,
    }
}

async fn nebula_start(iface: &str, dir: &Path) -> Result<()> {
    let mut procs = nebula_procs().lock().await;
    if let Some(mut old) = procs.remove(iface) {
        let _ = old.kill().await;
    }
    let log = std::fs::File::create(dir.join("nebula.log"))?;
    let mut child = Command::new("nebula")
        .arg("-config")
        .arg(dir.join("config.yml"))
        .stdout(log.try_clone()?)
        .stderr(log)
        .kill_on_drop(true)
        .spawn()
        .context("spawn nebula (is it installed?)")?;
    tokio::time::sleep(Duration::from_secs(2)).await;
    if let Some(st) = child.try_wait()? {
        let tail = std::fs::read_to_string(dir.join("nebula.log")).unwrap_or_default();
        bail!(
            "nebula exited ({st}): {}",
            &tail[tail.len().saturating_sub(1500)..]
        );
    }
    procs.insert(iface.to_string(), child);
    Ok(())
}

async fn overlay_down(p: &Value, state: &Path) -> Result<Value> {
    let iface = overlay_iface(p)?;
    if let Some(mut c) = nebula_procs().lock().await.remove(&iface) {
        let _ = c.kill().await;
    }
    if iface.starts_with("spw-") {
        let _ = run_tool("ip", &["link", "del", "dev", &iface], None).await;
    }
    let _ = std::fs::remove_dir_all(state.join("overlay").join(&iface));
    Ok(json!({"iface": iface, "removed": true}))
}

/// Re-establishes overlays after an agent or host restart (interfaces and nebula processes are not persistent).
async fn restore_overlays(state: &Path) -> Result<()> {
    let Ok(entries) = std::fs::read_dir(state.join("overlay")) else {
        return Ok(());
    };
    for e in entries.flatten() {
        let iface = e.file_name().to_string_lossy().to_string();
        if overlay_iface(&json!({"iface": iface})).is_err() {
            continue;
        }
        let dir = e.path();
        let r = if iface.starts_with("spw-") && dir.join("wg.conf").exists() {
            let addr = std::fs::read_to_string(dir.join("address")).unwrap_or_default();
            wg_up(&iface, &dir, addr.trim()).await
        } else if dir.join("config.yml").exists() {
            nebula_start(&iface, &dir).await
        } else {
            Ok(())
        };
        if let Err(err) = r {
            eprintln!("restore overlay {iface}: {err:#}");
        }
    }
    Ok(())
}

fn service_env(v: &Value) -> Result<std::collections::BTreeMap<String, String>> {
    let mut env = std::collections::BTreeMap::new();
    if let Some(m) = v.as_object() {
        for (k, v) in m {
            if k.is_empty() || !k.chars().all(|c| c.is_ascii_alphanumeric() || c == '_') {
                bail!("invalid env key {k}");
            }
            let val = v.as_str().unwrap_or_default();
            if val.contains('\0') {
                bail!("env value for {k} contains NUL");
            }
            env.insert(k.clone(), val.to_string());
        }
    }
    Ok(env)
}

async fn health(h: &Value) -> Result<Value> {
    let port = h["port"].as_u64().ok_or(anyhow!("health.port missing"))?;
    let path = h["path"].as_str().unwrap_or("/");
    let expect: Vec<u64> = h["expect_status"]
        .as_array()
        .map(|a| a.iter().filter_map(|v| v.as_u64()).collect())
        .unwrap_or(vec![200]);
    let url = format!("http://127.0.0.1:{port}{path}");
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

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn overlay_inputs_are_validated() {
        assert!(overlay_iface(&json!({"iface": "spw-1a2b3c4d"})).is_ok());
        assert!(overlay_iface(&json!({"iface": "spw-../etc/x"})).is_err());
        assert!(overlay_iface(&json!({"iface": "eth0"})).is_err());
        assert!(check_cidr("10.42.0.2/16").is_ok());
        assert!(check_cidr("10.42.0.2/33").is_err());
        assert!(check_cidr("10.42.0.2/16\n[Peer]").is_err());
        assert!(check_wg_key("YAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=").is_ok());
        assert!(check_wg_key("x\nEndpoint = 1.2.3.4:1").is_err());
    }
    #[test]
    fn env_values_are_kept_literal() {
        let e = service_env(&json!({"A": "${HOME}$PATH", "B": "x'y"})).unwrap();
        assert_eq!(e["A"], "${HOME}$PATH");
        assert!(service_env(&json!({"A=B": "x"})).is_err());
    }
    #[test]
    fn rejects_path_traversal_project() {
        assert!(project_name(&json!({"project": "../etc"})).is_err());
        assert!(project_name(&json!({"project": "spade-1a2b3c4d"})).is_ok());
    }
    #[tokio::test]
    async fn bundle_integrity_is_enforced() {
        let cmd = Command_ {
            id: "x".into(),
            verb: "apply_bundle".into(),
            payload: json!({"project": "p1", "compose": "services: {}", "compose_sha256": "deadbeef"}),
        };
        let err = execute(
            &cmd,
            Path::new("/tmp/spade-test-stacks"),
            &Env {
                state: Path::new("/tmp/spade-test-state"),
                gateway: "https://localhost:8443",
            },
        )
        .await
        .unwrap_err();
        assert!(err.to_string().contains("integrity"));
    }
}
