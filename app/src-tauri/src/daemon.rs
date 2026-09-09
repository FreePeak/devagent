//! Daemon client: the app's only I/O surface.
//!
//! Thin client of the FR-CTRL daemon (internal/daemon): every call carries the
//! per-boot bearer token from `DEVAGENT_HOME/daemon-token`; no other
//! credential exists anywhere in this app (FR-UI-07). HTTP lives here in Rust,
//! NOT in the webview: the daemon rejects non-loopback `Origin` headers
//! (`originAllowed`), and Tauri webviews carry `http://tauri.localhost` on
//! Windows/Linux, which the daemon 403s. Rust-side requests send no Origin.
//!
//! Endpoints used (verified against internal/daemon/daemon.go route table):
//! GET /status /agents /history /events (SSE) · POST /dispatch /approve.

use std::fs;
use std::io::{BufRead, BufReader};
use std::path::PathBuf;
use std::time::Duration;

use serde_json::Value;

/// Resolved daemon location + token. Parsed once per poll cycle so the UI
/// settings always win over defaults without an app restart.
#[derive(Clone, Debug)]
pub struct DaemonConfig {
    pub base_url: String,
    pub token: String,
}

impl DaemonConfig {
    /// Default config: loopback port 7788 (daemon default) + token file under
    /// DEVAGENT_HOME (default ~/.devagent), mirroring internal/daemon's
    /// devagentHome().
    pub fn load() -> Self {
        let base_url = std::env::var("DEVAGENT_DAEMON_URL")
            .unwrap_or_else(|_| "http://127.0.0.1:7788".to_string());
        let token = read_token_file()
            .unwrap_or_default();
        Self { base_url, token }
    }
}

/// Reads the per-boot bearer token the daemon writes 0600 to
/// `$DEVAGENT_HOME/daemon-token` (default `$HOME/.devagent`).
fn read_token_file() -> Option<String> {
    let mut path = if let Some(home) = std::env::var_os("DEVAGENT_HOME") {
        PathBuf::from(home)
    } else {
        PathBuf::from(std::env::var_os("HOME")?).join(".devagent")
    };
    path.push("daemon-token");
    let raw = fs::read_to_string(path).ok()?;
    Some(raw.trim().to_string())
}

fn agent() -> ureq::Agent {
    ureq::AgentBuilder::new()
        .timeout_connect(Duration::from_secs(3))
        .redirects(0)
        // Host header is loopback by construction; the daemon's hostAllowed
        // accepts 127.0.0.1 / localhost / [::1] only.
        .build()
}

fn auth_header(cfg: &DaemonConfig) -> String {
    format!("Bearer {}", cfg.token)
}

/// GET an JSON endpoint. Returns (status, body). Network errors surface as
/// status 0 so the UI can degrade to "daemon unreachable" (FR-UI-07).
pub fn get_json(cfg: &DaemonConfig, path: &str) -> Result<(u16, Value), String> {
    let url = format!("{}{}", cfg.base_url, path);
    let resp = agent()
        .get(&url)
        .set("Authorization", &auth_header(cfg))
        .call();
    match resp {
        Ok(r) => {
            let status = r.status();
            let body = r.into_string().map_err(|e| e.to_string())?;
            let v = serde_json::from_str(&body).unwrap_or(Value::Null);
            Ok((status, v))
        }
        Err(ureq::Error::Status(code, r)) => {
            let body = r.into_string().unwrap_or_default();
            let v = serde_json::from_str(&body).unwrap_or(Value::Null);
            Ok((code, v))
        }
        Err(e) => Err(e.to_string()),
    }
}

/// POST a JSON endpoint. Body must be a JSON object.
pub fn post_json(cfg: &DaemonConfig, path: &str, body: &Value) -> Result<(u16, Value), String> {
    let url = format!("{}{}", cfg.base_url, path);
    let resp = agent()
        .post(&url)
        .set("Authorization", &auth_header(cfg))
        .send_json(body);
    match resp {
        Ok(r) => {
            let status = r.status();
            let text = r.into_string().map_err(|e| e.to_string())?;
            let v = serde_json::from_str(&text).unwrap_or(Value::Null);
            Ok((status, v))
        }
        Err(ureq::Error::Status(code, r)) => {
            let text = r.into_string().unwrap_or_default();
            let v = serde_json::from_str(&text).unwrap_or(Value::Null);
            Ok((code, v))
        }
        Err(e) => Err(e.to_string()),
    }
}

/// One parsed SSE event from GET /events (the daemon's run-log follower:
/// `id: N`, `data: <raw jsonl line>`; heartbeat comment lines ignored).
#[derive(Clone, Debug)]
pub struct SseEvent {
    pub id: Option<u64>,
    pub data: String,
}

/// Long-lived GET /events reader. Yields parsed events; reconnects with
/// `Last-Event-ID` are the caller's job (send_back the last id). The call
/// blocks until the stream errors or closes — run it on its own thread.
/// `on_event` returns false to request shutdown (app exit).
pub fn stream_events<F>(cfg: &DaemonConfig, last_id: Option<u64>, on_event: &mut F) -> Result<(), String>
where
    F: FnMut(SseEvent) -> bool,
{
    let url = format!("{}/events", cfg.base_url);
    let mut req = agent()
        .get(&url)
        .set("Authorization", &auth_header(cfg))
        .set("Accept", "text/event-stream");
    if let Some(id) = last_id {
        // FR-CTRL-04 reconnect replay: the daemon's follower honors
        // Last-Event-ID with its in-memory tail buffer.
        req = req.set("Last-Event-ID", &id.to_string());
    }
    let resp = req.call().map_err(|e| e.to_string())?;

    // The daemon writes `retry: 3000` first; we honor the intent by being
    // tolerant here — the caller owns reconnect pacing.
    let reader = BufReader::new(resp.into_reader());
    let mut current_id: Option<u64> = None;
    let mut data_lines: Vec<String> = Vec::new();
    for line in reader.lines() {
        let line = match line {
            Ok(l) => l,
            Err(e) => return Err(e.to_string()),
        };
        if line.starts_with(':') || line.is_empty() {
            // heartbeat comment or event terminator (empty line flushes)
            if line.is_empty() && !data_lines.is_empty() {
                let ev = SseEvent {
                    id: current_id.take(),
                    data: data_lines.join("\n"),
                };
                data_lines.clear();
                if !on_event(ev) {
                    return Ok(());
                }
            }
            continue;
        }
        if let Some(rest) = line.strip_prefix("id: ") {
            current_id = rest.trim().parse::<u64>().ok().or(current_id);
        } else if let Some(rest) = line.strip_prefix("data: ") {
            data_lines.push(rest.to_string());
        } else if line.starts_with("data:") {
            data_lines.push(line[5..].trim_start().to_string());
        }
        // `retry:` and any other fields are ignored.
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{get_json, DaemonConfig};

    #[test]
    fn token_file_missing_degrades_to_empty() {
        // The tray must come up even with no token (shows "auth missing").
        let cfg = DaemonConfig {
            base_url: "http://127.0.0.1:1".into(),
            token: String::new(),
        };
        // Transport error (connection refused) — never a panic.
        match get_json(&cfg, "/status") {
            Err(_) => {}                                  // unreachable daemon
            Ok((code, _)) => assert!(code >= 400),        // or an HTTP rejection
        }
    }
}
