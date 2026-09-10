#!/usr/bin/env bash
# FR-VAL-05 nightly quality-drift ratchet (issue #293), scheduled by
# launchagents/com.devagent.eval-drift-nightly.plist.
#
# It scores the last N merged PRs of this checkout against the checked-in rubric
# (docs/eval/rubric.md) using the configured worker/model as judge, appends one
# `eval-score` row per PR to .devagent/runs/orchestration/events.jsonl, then
# reports the ratchet. The self-build loop reads the same ledger through
# `devagent ledger --clusters`, so a regression named here is a regression the
# next research prompt sees.
#
# Alert-only by default: EVAL_MAX_DROP=0 means the drift view never fails. Once
# real scores accumulate, set EVAL_MAX_DROP to a point budget — a drop larger
# than it exits 1, which is what launchd's KeepAlive/throttle sees as a bad run.
#
# The judge is a worker CLI plus its provider credentials, and the ledger is
# per-checkout, so this must run where the loop runs (never a CI runner whose
# events.jsonl starts empty and dies with the job).
set -uo pipefail

REPO="${EVAL_REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
LAST="${EVAL_LAST:-10}"
WINDOW="${EVAL_WINDOW:-10}"
MAX_DROP="${EVAL_MAX_DROP:-0}"
DEVAGENT_BIN="$(command -v devagent || true)"
[ -n "$DEVAGENT_BIN" ] || DEVAGENT_BIN="${HOME}/.local/bin/devagent"

if [ ! -x "$DEVAGENT_BIN" ]; then
  echo "[eval-drift] no devagent binary (PATH or ~/.local/bin/devagent) — nothing scored" >&2
  exit 1
fi

echo "[eval-drift] $(date -u +%FT%TZ) repo=$REPO last=$LAST window=$WINDOW max-drop=$MAX_DROP"

# Scoring is best-effort: a degraded provider or a vanished PR drops that row,
# never the night. With nothing scored, the drift view below reports the
# insufficient baseline instead of a clean bill of health it did not earn.
"$DEVAGENT_BIN" eval score --repo "$REPO" --last "$LAST"
score_rc=$?

"$DEVAGENT_BIN" eval drift --repo "$REPO" --window "$WINDOW" --max-drop "$MAX_DROP"
drift_rc=$?

if [ "$score_rc" -ne 0 ]; then
  echo "[eval-drift] scoring incomplete (rc=$score_rc); drift verdict covers existing rows only" >&2
fi
exit "$drift_rc"
