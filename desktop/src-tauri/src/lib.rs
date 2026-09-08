//! DevAgent desktop control app — Tauri 2 Rust shell (issue #181, FR-UI).
//!
//! The shell is deliberately thin (FR-UI-07): it owns only the things a
//! webview cannot do — the tray (FR-UI-01), single-instance + autostart
//! (FR-UI-05), native notifications (FR-UI-04), and the updater probe
//! (FR-UI-06). All business behavior lives in the webview UI talking to the
//! FR-CTRL daemon over loopback HTTP.

use serde::Serialize;
use std::fs;
use std::path::PathBuf;
use tauri::{
    menu::{Menu, MenuItem},
    tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent},
    AppHandle, Manager,
};

/// What the UI needs to reach the daemon: base URL + bearer token.
#[derive(Serialize)]
pub struct DaemonConfig {
    url: String,
    token: String,
    uds_path: Option<String>,
}

fn devagent_home() -> PathBuf {
    match std::env::var("DEVAGENT_HOME") {
        Ok(h) if !h.is_empty() => PathBuf::from(h),
        _ => dirs_home().join(".devagent"),
    }
}

/// HOME with a Windows fallback (USERPROFILE); an unset home degrades to ".".
fn dirs_home() -> PathBuf {
    #[cfg(windows)]
    let home = std::env::var("USERPROFILE");
    #[cfg(not(windows))]
    let home = std::env::var("HOME");
    home.map(PathBuf::from).unwrap_or_else(|_| PathBuf::from("."))
}

/// Read the per-boot 0600 daemon token the `devagent daemon` process wrote.
/// DEVAGENT_DAEMON_TOKEN (the TUI's own override) wins when set.
fn read_daemon_token() -> Option<String> {
    if let Ok(t) = std::env::var("DEVAGENT_DAEMON_TOKEN") {
        if !t.trim().is_empty() {
            return Some(t);
        }
    }
    let path = devagent_home().join("daemon-token");
    fs::read_to_string(path)
        .ok()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
}

/// Resolve daemon connection params for the webview: default TCP
/// 127.0.0.1:7788 (the `devagent daemon` default), with DEVAGENT_DAEMON_URL /
/// DEVAGENT_DAEMON_UDS overrides mirroring the TUI contract.
#[tauri::command]
fn daemon_config() -> Result<DaemonConfig, String> {
    let token = read_daemon_token().ok_or_else(|| {
        "no daemon token: start `devagent daemon` first (DEVAGENT_HOME/daemon-token)".to_string()
    })?;
    if let Ok(uds) = std::env::var("DEVAGENT_DAEMON_UDS") {
        if !uds.is_empty() {
            return Ok(DaemonConfig {
                url: String::new(),
                token,
                uds_path: Some(uds),
            });
        }
    }
    let url =
        std::env::var("DEVAGENT_DAEMON_URL").unwrap_or_else(|_| "http://127.0.0.1:7788".to_string());
    Ok(DaemonConfig {
        url,
        token,
        uds_path: None,
    })
}

/// Launch-at-login state (FR-UI-05): tauri-plugin-autostart owns the per-OS
/// registration (macOS LaunchAgent, Windows registry Run key, Linux
/// ~/.config/autostart).
#[tauri::command]
fn autostart_get(app: AppHandle) -> Result<bool, String> {
    use tauri_plugin_autostart::ManagerExt;
    app.autolaunch().is_enabled().map_err(|e| e.to_string())
}

#[tauri::command]
fn autostart_set(app: AppHandle, enabled: bool) -> Result<bool, String> {
    use tauri_plugin_autostart::ManagerExt;
    let m = app.autolaunch();
    if enabled {
        m.enable().map_err(|e| e.to_string())?;
    } else {
        m.disable().map_err(|e| e.to_string())?;
    }
    m.is_enabled().map_err(|e| e.to_string())
}

/// Release-channel probe (FR-UI-06). Returns the new version when an update
/// is available; a plain string error when the updater is not configured
/// (dev builds carry no signing keys) — the UI surfaces that as a hint.
#[tauri::command]
async fn check_updates(app: AppHandle) -> Result<Option<String>, String> {
    use tauri_plugin_updater::UpdaterExt;
    let updater = app.updater().map_err(|e| e.to_string())?;
    let update = updater.check().await.map_err(|e| e.to_string())?;
    Ok(update.map(|u| u.version))
}

/// Native notification push (FR-UI-04). The webview decides *whether* to
/// notify (desktop/ui/src/lib/notify.ts); the shell only performs the OS call.
#[tauri::command]
fn notify(app: AppHandle, title: String, body: String) -> Result<(), String> {
    use tauri_plugin_notification::NotificationExt;
    app.notification()
        .builder()
        .title(title)
        .body(body)
        .show()
        .map_err(|e| e.to_string())
}

/// Swap the tray icon with the aggregate-state glyph (FR-UI-01).
#[tauri::command]
fn set_tray_state(app: AppHandle, state: String) -> Result<(), String> {
    let Some(tray) = app.tray_by_id("main") else {
        return Err("tray not initialized".to_string());
    };
    let (bytes, tooltip): (&[u8], &str) = match state.as_str() {
        "running" => (
            include_bytes!("../icons/tray-running.png"),
            "DevAgent — running",
        ),
        "failed" => (
            include_bytes!("../icons/tray-failed.png"),
            "DevAgent — failed (circuit open)",
        ),
        _ => (
            include_bytes!("../icons/tray-idle.png"),
            "DevAgent — idle",
        ),
    };
    let img = tauri::image::Image::from_bytes(bytes).map_err(|e| e.to_string())?;
    tray.set_icon(Some(img)).map_err(|e| e.to_string())?;
    tray.set_tooltip(Some(tooltip))
        .map_err(|e| e.to_string())
}

/// Show-or-focus the main window (tray click, notification deep-link).
#[tauri::command]
fn show_main(app: AppHandle) -> Result<(), String> {
    if let Some(win) = app.get_webview_window("main") {
        let _ = win.show();
        let _ = win.unminimize();
        let _ = win.set_focus();
        Ok(())
    } else {
        Err("main window missing".to_string())
    }
}

pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _argv, _cwd| {
            // Second launch: surface the existing window instead (FR-UI-05).
            let _ = show_main(app.clone());
        }))
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            None,
        ))
        .plugin(tauri_plugin_updater::Builder::new().build())
        .setup(|app| {
            // Tray menu (FR-UI-01): open the dashboard, or quit. Dispatch and
            // approvals live in the dashboard window itself.
            let open = MenuItem::with_id(app, "open", "Open Dashboard", true, None::<&str>)?;
            let quit = MenuItem::with_id(app, "quit", "Quit DevAgent", true, None::<&str>)?;
            let menu = Menu::with_items(app, &[&open, &quit])?;
            let icon = tauri::image::Image::from_bytes(include_bytes!("../icons/tray-idle.png"))?;
            TrayIconBuilder::with_id("main")
                .icon(icon)
                .menu(&menu)
                .tooltip("DevAgent — idle")
                .on_menu_event(|app, event| {
                    if event.id.as_ref() == "quit" {
                        app.exit(0);
                    } else if event.id.as_ref() == "open" {
                        let _ = show_main(app.clone());
                    }
                })
                .on_tray_icon_event(|tray, event| {
                    // Left-click opens the dashboard on every OS.
                    if let TrayIconEvent::Click {
                        button: MouseButton::Left,
                        button_state: MouseButtonState::Up,
                        ..
                    } = event
                    {
                        let _ = show_main(tray.app_handle().clone());
                    }
                })
                .build(app)?;
            // Tray-first, but show the dashboard on first launch so the user
            // finds it; closing then hides to the tray (never quits).
            if let Some(win) = app.get_webview_window("main") {
                let _ = win.show();
            }
            Ok(())
        })
        .on_window_event(|window, event| {
            // Hide-to-tray: the app keeps running when the window closes.
            if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                api.prevent_close();
                let _ = window.hide();
            }
        })
        .invoke_handler(tauri::generate_handler![
            daemon_config,
            autostart_get,
            autostart_set,
            check_updates,
            notify,
            set_tray_state,
            show_main
        ])
        .build(tauri::generate_context!())
        .expect("error while building DevAgent desktop app")
        .run(|app, event| {
            // Deep-link surface (FR-UI-04): macOS re-activates the app when a
            // notification is clicked or the dock icon is pressed — surface
            // the dashboard instead of leaving it hidden in the tray.
            if let tauri::RunEvent::Reopen { .. } = event {
                let _ = show_main(app.clone());
            }
        });
}
