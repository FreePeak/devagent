//! Pipeline DAG reducer (FR-UI-08): folds SSE run-log rows (or /history
//! fallback rows) into a per-run stage timeline scout → plan → implement →
//! gates → PR. The run log carries two row families we can map:
//!   {ts, level, stage, message, runId?}   — RunLogEntry (internal/ledger)
//!   {ts, kind:"event", event, loop, phase} — loopdriver breadcrumbs
//! Stage badges are derived, never invented: a stage exists in the timeline
//! only if the run log actually named it.

use std::collections::HashMap;

use serde::{Deserialize, Serialize};
use serde_json::Value;

// PIPELINE_STAGES is the canonical order for `ordered()`; it is referenced
// from main.rs only through `ordered()`, but stays public for the UI contract.
#[allow(dead_code)]
pub const PIPELINE_STAGES: [&str; 5] = ["scout", "plan", "implement", "gates", "pr"];

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum StageStatus {
    Pending,
    Running,
    Pass,
    Fail,
    Skip,
}


/// One stage node in the timeline.
#[derive(Clone, Debug, Serialize)]
pub struct StageNode {
    pub name: String,
    pub status: StageStatus,
    pub elapsed_ms: u64,
    pub retries: u32,
}

/// The per-run pipeline timeline.
#[derive(Clone, Debug, Default, Serialize)]
pub struct RunTimeline {
    pub run_id: String,
    pub stages: Vec<StageNode>,
    #[serde(skip)]
    /// Stage name → timestamp of the first-seen row.
    first_seen_ms: HashMap<String, u64>,
    #[serde(skip)]
    /// Stage name → timestamp of the latest row (start of the current span).
    last_seen_ms: HashMap<String, u64>,
}

impl RunTimeline {
    pub fn new(run_id: &str) -> Self {
        Self {
            run_id: run_id.to_string(),
            ..Default::default()
        }
    }

    /// Applies one run-log row. Rows are applied in stream order; out-of-order
    /// rows (history replay) still converge because status is recomputed from
    /// level/verdict fields, not from row position.
    pub fn apply(&mut self, row: &Value, ts_ms: u64) {
        let name = stage_of(row);
        let name = match name {
            Some(n) => n,
            None => return,
        };
        let status = row_status(row);
        if !self.first_seen_ms.contains_key(&name) {
            self.first_seen_ms.insert(name.clone(), ts_ms);
            self.stages.push(StageNode {
                name: name.clone(),
                status,
                elapsed_ms: 0,
                retries: 0,
            });
        } else if let Some(node) = self.stages.iter_mut().find(|s| s.name == name) {
            // A stage re-entering Running after a terminal status is a retry
            // (attempt 2+): count it, reset the span.
            if matches!(node.status, StageStatus::Pass | StageStatus::Fail | StageStatus::Skip)
                && status == StageStatus::Running
            {
                node.retries += 1;
            }
            node.status = status;
        }
        if let Some(prev) = self.last_seen_ms.get(&name) {
            let span = ts_ms.saturating_sub(*prev);
            if let Some(node) = self.stages.iter_mut().find(|s| s.name == name) {
                node.elapsed_ms += span;
            }
        }
        self.last_seen_ms.insert(name, ts_ms);
    }

    /// Orders stages along the canonical pipeline; unknown-but-present stages
    /// sort after the canonical five. Called by the UI via get_pipeline; the
    /// unit tests exercise it directly.
    #[allow(dead_code)]
    pub fn ordered(&self) -> Vec<&StageNode> {
        let rank = |name: &str| -> i32 {
            PIPELINE_STAGES
                .iter()
                .position(|s| *s == name)
                .map(|p| p as i32)
                .unwrap_or(PIPELINE_STAGES.len() as i32)
        };
        let mut v: Vec<&StageNode> = self.stages.iter().collect();
        v.sort_by(|a, b| rank(&a.name).cmp(&rank(&b.name)).then(a.name.cmp(&b.name)));
        v
    }
}

/// Parses a ledger ISO-8601 ts ("2026-09-09T04:11:22.333Z") to epoch ms;
/// rows without a parseable ts fall back to 0 (ordering then relies on row
/// order alone).
pub fn parse_ts_ms(row: &Value) -> u64 {
    let ts = row.get("ts").and_then(|s| s.as_str()).unwrap_or("");
    if ts.is_empty() {
        return 0;
    }
    time::OffsetDateTime::parse(ts, &time::format_description::well_known::Iso8601::DEFAULT)
        .map(|dt| {
            let ms = dt.unix_timestamp_nanos() / 1_000_000;
            if ms < 0 {
                0
            } else {
                ms as u64
            }
        })
        .unwrap_or(0)
}

/// Builds a timeline from raw ledger rows (the /history static fallback for
/// FR-UI-08). Rows are applied in the order given (the daemon returns oldest
/// → newest for a tail).
pub fn timeline_from_rows(run_id: &str, rows: &[Value]) -> RunTimeline {
    let mut t = RunTimeline::new(run_id);
    for row in rows {
        t.apply(row, parse_ts_ms(row));
    }
    t
}

/// Maps one log row to a pipeline stage name (canonical or raw ledger stage).
/// Gate rows (stage "validate" or message naming G0..G5) collapse to "gates".
fn stage_of(row: &Value) -> Option<String> {
    let stage = row.get("stage").and_then(|s| s.as_str()).unwrap_or("");
    let event = row.get("event").and_then(|s| s.as_str()).unwrap_or("");
    // Loopdriver breadcrumb: {kind:"event", event:"loop-phase", phase:"task"}
    if event == "loop-phase" {
        return loop_phase_stage(row.get("phase").and_then(|s| s.as_str()).unwrap_or(""));
    }
    if !stage.is_empty() {
        return Some(normalize_stage(stage));
    }
    if !event.is_empty() {
        return Some(normalize_stage(event));
    }
    None
}

fn loop_phase_stage(phase: &str) -> Option<String> {
    // Loop driver phases (record.go): sync / preflight / research / issue /
    // po / task. They are dispatch-loop mechanics, not pipeline stages —
    // mapped onto the nearest pipeline concept so the timeline stays honest:
    // research/issue/po → scout, task → implement.
    match phase {
        "research" | "issue" | "po" => Some("scout".to_string()),
        "task" => Some("implement".to_string()),
        other => Some(normalize_stage(other)),
    }
}

fn normalize_stage(stage: &str) -> String {
    match stage {
        "fetch" | "clarify" | "queue" | "create" => "scout".to_string(),
        "scout" => "scout".to_string(),
        "plan" => "plan".to_string(),
        "implement" | "task" => "implement".to_string(),
        "validate" | "audit" => "gates".to_string(),
        "publish" => "pr".to_string(),
        other => {
            // Gate names: G0..G5 collapse into the gates node.
            let upper = other.to_uppercase();
            if upper.starts_with('G') && upper.len() >= 2 && upper.as_bytes()[1].is_ascii_digit() {
                return "gates".to_string();
            }
            other.to_string()
        }
    }
}

fn row_status(row: &Value) -> StageStatus {
    // Verdict fields win (ledger audit rows), then level, then presence.
    if let Some(v) = row.get("verdict").and_then(|s| s.as_str()) {
        return match v {
            "pass" => StageStatus::Pass,
            "fail" => StageStatus::Fail,
            "skip" => StageStatus::Skip,
            "ask" => StageStatus::Running,
            _ => StageStatus::Running,
        };
    }
    if let Some(g) = row.get("gate") {
        // GateResult rows: {gate, passed, skipped}
        if g.get("skipped").and_then(|b| b.as_bool()).unwrap_or(false) {
            return StageStatus::Skip;
        }
        return if g.get("passed").and_then(|b| b.as_bool()).unwrap_or(false) {
            StageStatus::Pass
        } else {
            StageStatus::Fail
        };
    }
    match row.get("level").and_then(|s| s.as_str()) {
        Some("error") => StageStatus::Fail,
        Some("warn") => StageStatus::Running,
        _ => StageStatus::Running,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn ts(row_ms: u64) -> u64 {
        row_ms
    }

    #[test]
    fn canonical_order_from_mixed_rows() {
        let mut t = RunTimeline::new("r1");
        t.apply(&json!({"stage":"implement","level":"info"}), ts(100));
        t.apply(&json!({"stage":"scout","level":"info"}), ts(50));
        t.apply(&json!({"stage":"plan","level":"info"}), ts(75));
        let names: Vec<&str> = t.ordered().iter().map(|n| n.name.as_str()).collect();
        assert_eq!(names, vec!["scout", "plan", "implement"]);
    }

    #[test]
    fn gate_rows_collapse_into_gates_node() {
        let mut t = RunTimeline::new("r1");
        t.apply(&json!({"stage":"validate","gate":{"gate":"G1-tests","passed":true,"findings":[]}}), 10);
        t.apply(&json!({"stage":"validate","gate":{"gate":"G0-readiness","passed":false,"findings":[]}}), 20);
        let nodes = t.ordered();
        assert_eq!(nodes.len(), 1);
        assert_eq!(nodes[0].name, "gates");
        assert_eq!(nodes[0].status, StageStatus::Fail); // last gate failed
    }

    #[test]
    fn loop_phase_rows_map_onto_pipeline() {
        let mut t = RunTimeline::new("r1");
        t.apply(&json!({"kind":"event","event":"loop-phase","phase":"research"}), 1);
        t.apply(&json!({"kind":"event","event":"loop-phase","phase":"task"}), 2);
        let names: Vec<&str> = t.ordered().iter().map(|n| n.name.as_str()).collect();
        assert_eq!(names, vec!["scout", "implement"]);
    }

    #[test]
    fn retry_counts_on_stage_reentry() {
        let mut t = RunTimeline::new("r1");
        t.apply(&json!({"stage":"implement","level":"info"}), 100);
        t.apply(&json!({"stage":"implement","level":"error"}), 200);
        t.apply(&json!({"stage":"implement","level":"info"}), 300);
        let nodes = t.ordered();
        assert_eq!(nodes[0].retries, 1);
        assert_eq!(nodes[0].status, StageStatus::Running);
        // elapsed spans: 100 (first) + 100 (retry span)
        assert_eq!(nodes[0].elapsed_ms, 200);
    }

    #[test]
    fn verdict_row_terminal_states() {
        let mut t = RunTimeline::new("r1");
        t.apply(&json!({"stage":"audit","verdict":"pass"}), 10);
        assert_eq!(t.ordered()[0].status, StageStatus::Pass);
    }

    #[test]
    fn irrelevant_rows_are_ignored() {
        let mut t = RunTimeline::new("r1");
        t.apply(&json!({"ts":"x"}), 1);
        assert!(t.stages.is_empty());
    }
}
