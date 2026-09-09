//! DevAgent Control (FR-UI, PRD §20.4, issue #181): a thin Tauri 2 client of
//! the FR-CTRL daemon. This core owns ONLY transport + OS integration:
//!   - FR-UI-01  tray icon aggregate state (state.rs over GET /status poll)
//!   - FR-UI-04  native notifications on approval-needed / failure / completion
//!   - FR-UI-05  autostart + single-instance plugins
//!   - transport bridge to the webview (no business logic — FR-UI-07)
//! All feature logic (roster, dispatch sheet, approval inbox, DAG view) is in
//! the webview UI; losing this app degrades to CLI-only operation.

#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod daemon;
mod pipeline;
mod state;

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::Duration;

use parking_lot::Mutex;
use tauri::{
    AppHandle, Emitter, Manager, State,
    tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent},
};
use tauri_plugin_notification::NotificationExt;

/// Shared app runtime: the daemon config (re-read on settings change), the
/// last SSE event id (for reconnect replay), per-run pipeline timelines
/// (FR-UI-08, reduced here so the webview only renders), and the derived
/// aggregate state.
struct Runtime {
    cfg: Mutex<daemon::DaemonConfig>,
    last_event_id: AtomicU64,
    timelines: Mutex<HashMap<String, pipeline::RunTimeline>>,
    aggregate: Mutex<state::AggregateState>,
    sse_shutdown: AtomicBool,
}

impl Runtime {
    fn new() -> Self {
        Self {
            cfg: Mutex::new(daemon::DaemonConfig::load()),
            last_event_id: AtomicU64::new(0),
            timelines: Mutex::new(HashMap::new()),
            aggregate: Mutex::new(state::AggregateState::Offline),
            sse_shutdown: AtomicBool::new(false),
        }
    }
}

type Rt<'a> = State<'a, Runtime>;

// ---------------------------------------------------------------------------
// Commands (webview → core). The webview never does HTTP itself; it asks this
// core, keeping Origin/Host handling in one place.

#[tauri::command]
fn get_status(rt: Rt) -> Result<(u16, serde_json::Value), String> {
    let cfg = rt.cfg.lock().clone();
    daemon::get_json(&cfg, "/status")
}

#[tauri::command]
fn get_agents(rt: Rt) -> Result<(u16, serde_json::Value), String> {
    let cfg = rt.cfg.lock().clone();
    daemon::get_json(&cfg, "/agents")
}

#[tauri::command]
fn get_history(
    rt: Rt,
    limit: Option<u32>,
    task_id: Option<String>,
) -> Result<(u16, serde_json::Value), String> {
    let cfg = rt.cfg.lock().clone();
    let mut path = format!("/history?limit={}", limit.unwrap_or(100));
    if let Some(id) = task_id {
        if !id.is_empty() {
            path.push_str(&format!("&taskId={}", urlencode(&id)));
        }
    }
    daemon::get_json(&cfg, &path)
}

/// FR-UI-02: dispatch sheet submit → POST /dispatch.
/// Body mirrors parseDispatch (internal/daemon/endpoints.go): prompt required;
/// role/worker/repoPath optional; autoPr only on explicit true; budget object.
#[tauri::command]
fn dispatch(
    rt: Rt,
    prompt: String,
    role: Option<String>,
    worker: Option<String>,
    repo_path: Option<String>,
    auto_pr: Option<bool>,
    max_loops: Option<f64>,
    timeout_minutes: Option<f64>,
) -> Result<(u16, serde_json::Value), String> {
    let cfg = rt.cfg.lock().clone();
    let mut body = serde_json::json!({ "prompt": prompt });
    if let Some(v) = role {
        if !v.is_empty() {
            body["role"] = serde_json::json!(v);
        }
    }
    if let Some(v) = worker {
        if !v.is_empty() {
            body["worker"] = serde_json::json!(v);
        }
    }
    if let Some(v) = repo_path {
        if !v.is_empty() {
            body["repoPath"] = serde_json::json!(v);
        }
    }
    if auto_pr.unwrap_or(false) {
        body["autoPr"] = serde_json::json!(true);
    }
    if max_loops.is_some() || timeout_minutes.is_some() {
        let mut budget = serde_json::Map::new();
        if let Some(v) = max_loops {
            budget.insert("maxLoops".into(), serde_json::json!(v));
        }
        if let Some(v) = timeout_minutes {
            budget.insert("timeoutMinutes".into(), serde_json::json!(v));
        }
        body["budget"] = serde_json::Value::Object(budget);
    }
    daemon::post_json(&cfg, "/dispatch", &body)
}

/// FR-UI-04: approval inbox decision → POST /approve (taskId + answer; the
/// kill sentinel `__kill__` is intentionally NOT offered from the app —
/// operator kills stay in the TUI/CLI surfaces).
#[tauri::command]
fn approve(
    rt: Rt,
    repo_path: Option<String>,
    task_id: String,
    answer: String,
) -> Result<(u16, serde_json::Value), String> {
    let cfg = rt.cfg.lock().clone();
    let mut body = serde_json::json!({ "taskId": task_id, "answer": answer });
    if let Some(v) = repo_path {
        if !v.is_empty() {
            body["repoPath"] = serde_json::json!(v);
        }
    }
    daemon::post_json(&cfg, "/approve", &body)
}

/// Settings update: point the client at a different daemon URL / token.
#[tauri::command]
fn set_daemon_config(rt: Rt, base_url: String, token: Option<String>) -> Result<(), String> {
    let mut cfg = rt.cfg.lock();
    cfg.base_url = base_url.trim_end_matches('/').to_string();
    if let Some(t) = token {
        cfg.token = t;
    }
    Ok(())
}

#[tauri::command]
fn get_daemon_config(rt: Rt) -> Result<(String, String), String> {
    let cfg = rt.cfg.lock();
    Ok((cfg.base_url.clone(), cfg.token.clone()))
}

/// FR-UI-08 static fallback: rebuild a run timeline from /history rows.
#[tauri::command]
fn get_pipeline_from_history(rt: Rt, limit: Option<u32>) -> Result<pipeline::RunTimeline, String> {
    let cfg = rt.cfg.lock().clone();
    let (code, body) = daemon::get_json(&cfg, &format!("/history?limit={}", limit.unwrap_or(300)))?;
    if code >= 400 {
        return Err(format!("history fetch failed: HTTP {}", code));
    }
    let rows = match body.get("records").and_then(|r| r.as_array()) {
        Some(a) => a.clone(),
        None => Vec::new(),
    };
    Ok(pipeline::timeline_from_rows("history", &rows))
}

/// FR-UI-08 live view: the timeline reduced from the SSE stream so far.
#[tauri::command]
fn get_pipeline(rt: Rt, run_id: String) -> Result<Option<pipeline::RunTimeline>, String> {
    let map = rt.timelines.lock();
    Ok(map.get(&run_id).cloned())
}

/// Run ids the core has seen on the SSE stream (newest last).
#[tauri::command]
fn list_runs(rt: Rt) -> Vec<String> {
    let map = rt.timelines.lock();
    map.keys().cloned().collect()
}

fn urlencode(s: &str) -> String {
    let mut out = String::new();
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{:02X}", b)),
        }
    }
    out
}

// ---------------------------------------------------------------------------
// Background loops (core-owned): /status poll → tray state, and the /events
// SSE reader → webview fan-out + notification triggers.

fn poll_loop(app: AppHandle) {
    let rt = app.state::<Runtime>();
    loop {
        if rt.sse_shutdown.load(Ordering::Relaxed) {
            return;
        }
        let cfg = rt.cfg.lock().clone();
        // Transport failure → Offline (auth failures arrive as HTTP 401 and
        // map inside state::aggregate; unreachable daemons never do).
        let next_agg = match daemon::get_json(&cfg, "/status") {
            Ok((code, body)) => state::aggregate(code, &body),
            Err(_) => state::aggregate(0, &serde_json::Value::Null),
        };
        let prev = *rt.aggregate.lock();
        if next_agg != prev {
            *rt.aggregate.lock() = next_agg;
            set_tray_icon(&app, next_agg);
        }
        // FR-UI-04: native notification on failure and recovery transitions.
        match (prev, next_agg) {
            (
                state::AggregateState::Running | state::AggregateState::Idle,
                state::AggregateState::Failed,
            ) => {
                app.notification()
                    .builder()
                    .title("DevAgent: run failed")
                    .body("A run failed — open the dashboard for details.")
                    .show()
                    .ok();
                let _ = app.emit("daemon://aggregate", next_agg.label());
            }
            (
                state::AggregateState::Failed,
                state::AggregateState::Running | state::AggregateState::Idle,
            ) => {
                app.notification()
                    .builder()
                    .title("DevAgent: run recovered")
                    .body("The loop is healthy again.")
                    .show()
                    .ok();
                let _ = app.emit("daemon://aggregate", next_agg.label());
            }
            (_, next) => {
                let _ = app.emit("daemon://aggregate", next.label());
            }
        }
        std::thread::sleep(Duration::from_secs(5));
    }
}

fn sse_loop(app: AppHandle) {
    let rt = app.state::<Runtime>();
    loop {
        if rt.sse_shutdown.load(Ordering::Relaxed) {
            return;
        }
        let cfg = rt.cfg.lock().clone();
        let id = rt.last_event_id.load(Ordering::Relaxed);
        let last_id = if id == 0 { None } else { Some(id) };
        let app2 = app.clone();
        let result = daemon::stream_events(&cfg, last_id, &mut |ev| {
            if let Some(eid) = ev.id {
                app2
                    .state::<Runtime>()
                    .last_event_id
                    .store(eid, Ordering::Relaxed);
            }
            // Forward every run-log row to the dashboard (FR-UI-03 tail)
            // and reduce it into the per-run pipeline timeline (FR-UI-08);
            // the webview only renders.
            if let Ok(v) = serde_json::from_str::<serde_json::Value>(&ev.data) {
                apply_to_timeline(&app2, &v);
                notify_on_row(&app2, &v);
            }
            let _ = app2.emit("daemon://event", &ev.data);
            !app2.state::<Runtime>().sse_shutdown.load(Ordering::Relaxed)
        });
        if rt.sse_shutdown.load(Ordering::Relaxed) {
            return;
        }
        let _ = result; // reconnect unconditionally; replay rides Last-Event-ID
        std::thread::sleep(Duration::from_secs(3)); // the daemon's own retry hint
    }
}

/// FR-UI-08: folds one SSE row into the per-run timeline and publishes the
/// run id. Run key: runId when the log row carries one, else the loopdriver
/// loop number, else a single "session" stream.
fn apply_to_timeline(app: &AppHandle, row: &serde_json::Value) {
    let rt = app.state::<Runtime>();
    let run_id = row
        .get("runId")
        .and_then(|s| s.as_str())
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string())
        .or_else(|| {
            let event = row.get("event").and_then(|s| s.as_str()).unwrap_or("");
            if event == "loop-phase" || event == "loop-result" {
                row.get("loop")
                    .and_then(|n| n.as_i64())
                    .map(|n| format!("loop-{}", n))
            } else {
                None
            }
        })
        .unwrap_or_else(|| "session".to_string());
    let ts = pipeline::parse_ts_ms(row);
    {
        let mut map = rt.timelines.lock();
        let timeline = map.entry(run_id.clone()).or_insert_with(|| pipeline::RunTimeline::new(&run_id));
        timeline.apply(row, ts);
    }
    let _ = app.emit("daemon://run", run_id);
}

/// FR-UI-04: approval-needed notification. A paused 'ask' task surfaces in
/// the run log as an audit row with verdict "ask" (same signal the TUI's
/// pickPausedTask falls back to).
fn notify_on_row(app: &AppHandle, row: &serde_json::Value) {
    let verdict = row.get("verdict").and_then(|s| s.as_str()).unwrap_or("");
    let task_id = row.get("taskId").and_then(|s| s.as_str()).unwrap_or("");
    if verdict == "ask" && !task_id.is_empty() {
        app.notification()
            .builder()
            .title("DevAgent: approval needed")
            .body(format!(
                "Task {} paused for input — open the approval inbox.",
                task_id
            ))
            .show()
            .ok();
        let _ = app.emit("daemon://approval-needed", task_id);
    }
}

fn tray_icon_bytes(agg: state::AggregateState) -> &'static [u8] {
    match agg {
        state::AggregateState::Running => include_bytes!("../icons/tray-running.png"),
        state::AggregateState::Failed => include_bytes!("../icons/tray-failed.png"),
        state::AggregateState::Offline | state::AggregateState::AuthFailed => {
            include_bytes!("../icons/tray-offline.png")
        }
        state::AggregateState::Idle => include_bytes!("../icons/tray-idle.png"),
    }
}

fn set_tray_icon(app: &AppHandle, agg: state::AggregateState) {
    let Some(tray) = app.tray_by_id("main-tray") else {
        return;
    };
    if let Ok(img) = tauri::image::Image::from_bytes(tray_icon_bytes(agg)) {
        let _ = tray.set_icon(Some(img));
        let _ = tray.set_tooltip(Some(format!("DevAgent: {}", agg.label())));
    }
}

// ---------------------------------------------------------------------------
// Setup

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _argv, _cwd| {
            // FR-UI-05: second launch deep-links into the running dashboard.
            if let Some(w) = app.get_webview_window("main") {
                let _ = w.show();
                let _ = w.set_focus();
            }
        }))
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            None,
        ))
        .setup(|app| {
            app.manage(Runtime::new());
            build_tray(app.handle().clone())?;
            {
                let h = app.handle().clone();
                std::thread::spawn(move || poll_loop(h));
            }
            {
                let h = app.handle().clone();
                std::thread::spawn(move || sse_loop(h));
            }
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            get_status,
            get_agents,
            get_history,
            get_pipeline,
            get_pipeline_from_history,
            list_runs,
            dispatch,
            approve,
            set_daemon_config,
            get_daemon_config,
        ])
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}

fn build_tray(app: AppHandle) -> tauri::Result<()> {
    let aggregate = *app.state::<Runtime>().aggregate.lock();
    let tray = TrayIconBuilder::with_id("main-tray")
        .icon(
            tauri::image::Image::from_bytes(tray_icon_bytes(aggregate))
                .expect("bundled tray icon must decode"),
        )
        .tooltip(format!("DevAgent: {}", aggregate.label()))
        // Left click = deep-link into the dashboard; right click = this menu.
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| match event.id().as_ref() {
            "open" => {
                if let Some(w) = app.get_webview_window("main") {
                    let _ = w.show();
                    let _ = w.set_focus();
                }
            }
            "quit" => {
                app.state::<Runtime>().sse_shutdown.store(true, Ordering::Relaxed);
                app.exit(0);
            }
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            if let TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                ..
            } = event
            {
                if let Some(w) = tray.app_handle().get_webview_window("main") {
                    let _ = w.show();
                    let _ = w.set_focus();
                }
            }
        })
        .build(&app)?;

    use tauri::menu::{MenuBuilder, MenuItemBuilder};
    let status_item = MenuItemBuilder::with_id("aggregate", format!("Status: {}", aggregate.label()))
        .enabled(false)
        .build(&app)?;
    let open = MenuItemBuilder::with_id("open", "Open Dashboard").build(&app)?;
    let quit = MenuItemBuilder::with_id("quit", "Quit").build(&app)?;
    let menu = MenuBuilder::new(&app)
        .item(&status_item)
        .separator()
        .item(&open)
        .separator()
        .item(&quit)
        .build()?;
    tray.set_menu(Some(menu))?;
    Ok(())
}
