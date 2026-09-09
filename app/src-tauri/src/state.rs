//! Tray state derivation (FR-UI-01): aggregate running / idle / failed from
//! GET /status, with circuit-breaker "open" promoted to failed. Offline
//! (transport error) and unauthorized are distinct states so the menu can
//! tell the operator what to fix.

use serde_json::Value;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AggregateState {
    Offline,
    AuthFailed,
    Failed,
    Running,
    Idle,
}

impl AggregateState {
    #[allow(dead_code)] // exercised in unit tests; main.rs uses tray_icon_bytes
    pub fn icon_name(self) -> &'static str {
        match self {
            AggregateState::Offline => "tray-offline",
            AggregateState::AuthFailed => "tray-offline",
            AggregateState::Failed => "tray-failed",
            AggregateState::Running => "tray-running",
            AggregateState::Idle => "tray-idle",
        }
    }

    pub fn label(self) -> &'static str {
        match self {
            AggregateState::Offline => "offline",
            AggregateState::AuthFailed => "auth failed",
            AggregateState::Failed => "failed",
            AggregateState::Running => "running",
            AggregateState::Idle => "idle",
        }
    }
}

/// Derives the aggregate from a /status body. Precedence mirrors the PRD:
/// failed (failed_recent > 0 or circuit open) beats running (active > 0)
/// beats idle. Auth/reachability are the caller's signal (status code).
pub fn aggregate(status_code: u16, body: &Value) -> AggregateState {
    match status_code {
        0 => return AggregateState::Offline,
        401 => return AggregateState::AuthFailed,
        c if c >= 400 => return AggregateState::Offline,
        _ => {}
    }
    let runs = body.get("runs");
    let active = runs.and_then(|r| r.get("active")).and_then(as_f64).unwrap_or(0.0);
    let failed = runs
        .and_then(|r| r.get("failed_recent"))
        .and_then(as_f64)
        .unwrap_or(0.0);
    let circuit = body
        .get("circuit")
        .and_then(|c| c.as_str())
        .unwrap_or("closed");
    if failed > 0.0 || circuit == "open" {
        return AggregateState::Failed;
    }
    if active > 0.0 {
        return AggregateState::Running;
    }
    AggregateState::Idle
}

fn as_f64(v: &Value) -> Option<f64> {
    v.as_f64()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn precedence_failed_over_running_over_idle() {
        let body = json!({"runs": {"active": 2, "failed_recent": 1}, "circuit": "closed"});
        assert_eq!(aggregate(200, &body), AggregateState::Failed);

        let body = json!({"runs": {"active": 2, "failed_recent": 0}, "circuit": "closed"});
        assert_eq!(aggregate(200, &body), AggregateState::Running);

        let body = json!({"runs": {"active": 0, "failed_recent": 0}, "circuit": "closed"});
        assert_eq!(aggregate(200, &body), AggregateState::Idle);
    }

    #[test]
    fn open_circuit_is_failed_even_with_zero_counters() {
        let body = json!({"runs": {"active": 0, "failed_recent": 0}, "circuit": "open"});
        assert_eq!(aggregate(200, &body), AggregateState::Failed);
    }

    #[test]
    fn transport_error_and_auth_are_distinct() {
        let body = json!({});
        assert_eq!(aggregate(0, &body), AggregateState::Offline);
        assert_eq!(aggregate(401, &body), AggregateState::AuthFailed);
        assert_eq!(aggregate(500, &body), AggregateState::Offline);
    }

    #[test]
    fn malformed_body_defaults_idle_not_panic() {
        assert_eq!(aggregate(200, &Value::Null), AggregateState::Idle);
        assert_eq!(aggregate(200, &json!([])), AggregateState::Idle);
    }
}
