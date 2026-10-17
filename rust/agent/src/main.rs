//! spade-agent: runs on each managed VM, connects outbound-only to the SpadeAi gateway over mTLS,
//! and executes Docker Compose lifecycle commands. It never opens a listening port.
use anyhow::{anyhow, bail, Context, Result};
use clap::{Parser, Subcommand};
use serde::Deserialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::path::{Path, PathBuf};
use std::time::Duration;
use tokio::process::Command;

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
        Cmd::Enrol { gateway, ca, token, state_dir } => enrol(&gateway, &ca, &token, &state_dir).await,
        Cmd::Run { gateway, state_dir, stacks_dir } => run(&gateway, &state_dir, &stacks_dir).await,
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
        bail!("enrolment rejected: {} {}", resp.status(), resp.text().await.unwrap_or_default());
    }
    let body: Value = resp.json().await?;
    std::fs::create_dir_all(state)?;
    write_private(&state.join("agent.key"), key.serialize_pem().as_bytes())?;
    std::fs::write(state.join("agent.crt"), body["cert_pem"].as_str().ok_or(anyhow!("no cert"))?)?;
    // Keep the CA that was pinned out of band; never adopt a different CA offered by the gateway.
    let offered = body["ca_pem"].as_str().ok_or(anyhow!("no ca"))?;
    if offered.trim() != String::from_utf8_lossy(&ca_pem).trim() {
        bail!("gateway offered a CA that differs from the pinned CA; refusing enrolment");
    }
    std::fs::write(state.join("ca.pem"), &ca_pem)?;
    println!("enrolled as agent {}", body["agent_id"].as_str().unwrap_or("?"));
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

async fn run(gateway: &str, state: &Path, stacks: &Path) -> Result<()> {
    let client = mtls_client(state)?;
    std::fs::create_dir_all(stacks)?;
    let hb_client = client.clone();
    let hb_gw = gateway.to_string();
    tokio::spawn(async move {
        loop {
            let _ = hb_client.post(format!("{hb_gw}/agent/v1/heartbeat")).json(&json!({"version": VERSION})).send().await;
            tokio::time::sleep(Duration::from_secs(30)).await;
        }
    });
    let mut backoff = 1u64;
    loop {
        match client.get(format!("{gateway}/agent/v1/commands/next")).send().await {
            Ok(r) if r.status() == 204 => backoff = 1,
            Ok(r) if r.status().is_success() => {
                backoff = 1;
                let cmd: Command_ = r.json().await?;
                let (status, result) = match execute(&cmd, stacks).await {
                    Ok(v) => ("succeeded", v),
                    Err(e) => ("failed", json!({"error": format!("{e:#}")})),
                };
                eprintln!("command {} {} -> {}", cmd.id, cmd.verb, status);
                report(&client, gateway, &cmd.id, &json!({"status": status, "result": result})).await;
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
        match client.post(format!("{gateway}/agent/v1/commands/{id}/result")).json(body).send().await {
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
    let p = payload["project"].as_str().ok_or(anyhow!("payload.project missing"))?;
    if p.is_empty() || p.len() > 63 || !p.chars().all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-') {
        bail!("invalid project name");
    }
    Ok(p.to_string())
}

async fn compose(dir: &Path, project: &str, args: &[&str]) -> Result<Value> {
    // Service env is passed as process environment, never through a .env file, so values are not
    // re-interpolated by Compose (e.g. a value containing ${HOME} stays literal).
    let env: std::collections::BTreeMap<String, String> = match std::fs::read(dir.join("env.json")) {
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

async fn execute(cmd: &Command_, stacks: &Path) -> Result<Value> {
    let project = project_name(&cmd.payload)?;
    let dir = stacks.join(&project);
    match cmd.verb.as_str() {
        "apply_bundle" => {
            let compose_text = cmd.payload["compose"].as_str().ok_or(anyhow!("compose missing"))?;
            let want = cmd.payload["compose_sha256"].as_str().ok_or(anyhow!("compose_sha256 missing"))?;
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
        other => bail!("unsupported verb {other}"),
    }
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
    let expect: Vec<u64> = h["expect_status"].as_array().map(|a| a.iter().filter_map(|v| v.as_u64()).collect()).unwrap_or(vec![200]);
    let url = format!("http://127.0.0.1:{port}{path}");
    let c = reqwest::Client::builder().redirect(reqwest::redirect::Policy::none()).timeout(Duration::from_secs(5)).build()?;
    let mut last = String::new();
    for _ in 0..60 {
        match c.get(&url).send().await {
            Ok(r) if expect.contains(&(r.status().as_u16() as u64)) => return Ok(json!({"url": url, "status": r.status().as_u16()})),
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
        let cmd = Command_ { id: "x".into(), verb: "apply_bundle".into(),
            payload: json!({"project": "p1", "compose": "services: {}", "compose_sha256": "deadbeef"}) };
        let err = execute(&cmd, Path::new("/tmp/spade-test-stacks")).await.unwrap_err();
        assert!(err.to_string().contains("integrity"));
    }
}
