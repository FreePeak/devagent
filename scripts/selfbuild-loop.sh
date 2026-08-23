#!/usr/bin/env bash
# DevAgent self-build infinity loop driver.
# Executes one full product cycle (Research -> Ideas -> Validate -> Plan ->
# Implement -> Testing -> Push) per iteration, forever, using DevAgent's own
# pipeline (`devagent task`) as the implementation engine.
# Protocol: docs/SELF-BUILD-LOOP.md
set -euo pipefail

REPO="${SELFBUILD_REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
STATE="$REPO/.selfbuild"
MAX_ITERS="${SELFBUILD_MAX_ITERATIONS:-0}"
MAX_FAILS="${SELFBUILD_MAX_CONSECUTIVE_FAILURES:-3}"
WORKER="${SELFBUILD_WORKER:-claude-code}"
PUSH_MODE="${SELFBUILD_PUSH_MODE:-pr}"
CLAUDE_BIN="${SELFBUILD_CLAUDE:-claude -p --dangerously-skip-permissions}"
DEVAGENT=(npx tsx "$REPO/src/cli.ts")

mkdir -p "$STATE/research" "$STATE/goals" "$STATE/logs"
cd "$REPO"

ledger_lines() { wc -l < "$STATE/ledger.jsonl" 2>/dev/null || echo 0; }

record() { # record <loop> <status> <goal>
  printf '{"loop":%s,"ts":"%s","status":"%s","goal":"%s"}\n' \
    "$1" "$(date -u +%FT%TZ)" "$2" \
    "$(printf '%s' "$3" | tr -d '"' | cut -c1-160)" >> "$STATE/ledger.jsonl"
}

fails=0
while :; do
  N=$(( "$(ledger_lines | tr -d ' ')" + 1 ))
  LOG="$STATE/logs/loop-$N.log"
  {
    echo "=== self-build loop $N start $(date -u +%FT%TZ) ==="

    # Sync with remote; tolerate offline / diverged states.
    git pull --ff-only || echo "[sync] skipped (pull failed)"

    # Phase 1: Research. Feed prior failures back in so defects compound into fixes.
    PREV_TAIL=""
    [ -f "$STATE/ledger.jsonl" ] && PREV_TAIL=$(tail -3 "$STATE/ledger.jsonl" || true)
    $CLAUDE_BIN "You are phase 1 (Research) of the DevAgent self-build loop, iteration $N.
Repo: $REPO. Read docs/PRD.md section 4 (competitive landscape) and section 17 (roadmap).
Recent loop ledger: ${PREV_TAIL:-none}.
Web-search what changed recently for: Devin/Cognition, GitHub Copilot coding agent, OpenHands, Factory Droid, Google Jules, OpenAI Codex cloud agent; and for projects that run agents in self-improving loops over their own codebase.
Output compact markdown (<400 words): NEW competitor moves with URLs; self-build loop patterns worth copying; then a ranked recommendation of the single best next backlog item for this iteration and why." \
      > "$STATE/research/loop-$N.md" || echo "[research] failed, continuing with backlog-only selection"

    # Phases 2-3: Idea + Validate. Pick one PRD Phase 4 item, constrained by research.
    $CLAUDE_BIN "You are phases 2-3 (Ideas + Validate) of the DevAgent self-build loop, iteration $N.
Repo: $REPO. Inputs: docs/PRD.md (Phase 4 backlog), .selfbuild/research/loop-$N.md, recent ledger entries below.
$PREV_TAIL
Select exactly ONE backlog item scoped to a single implementable+testable iteration.
Validation checks (all must pass): maps to a PRD backlog item; no dependency on an earlier failed loop; verifiable by the repo test suite or CLI smoke run.
Output ONLY the goal statement (max 120 words), starting with 'Goal:' — this text is passed directly to devagent task as the implementation prompt." \
      > goal.tmp && mv goal.tmp "$STATE/goals/loop-$N.md"

    GOAL_FILE="$STATE/goals/loop-$N.md"
    if ! grep -q '^Goal:' "$GOAL_FILE"; then
      echo "[validate] goal file missing Goal: line — marking iteration invalid" ; record "$N" invalid "$(cat "$GOAL_FILE" 2>/dev/null)" ; fails=$(( fails + 1 )) ; else
      GOAL=$(cat "$GOAL_FILE")

      # Phases 4-5-6: Plan + Implement + internal validation gates via DevAgent itself.
      TASK_ARGS=(task --prompt "$GOAL" --repo "$REPO" --worker "$WORKER")
      [ "$PUSH_MODE" = pr ] && TASK_ARGS+=(--auto-pr)
      "${DEVAGENT[@]}" "${TASK_ARGS[@]}" || { echo "[implement] task failed" ; record "$N" failed "$GOAL" ; fails=$(( fails + 1 )) ;
        [ "$fails" -ge "$MAX_FAILS" ] && { echo "circuit breaker: $fails consecutive failures" ; exit 1 ; } ; continue ; }

      # Post-merge-back repo-level test gate.
      npm test || { echo "[testing] repo tests failed after merge-back" ; record "$N" failed-tests "$GOAL" ; fails=$(( fails + 1 )) ;
        [ "$fails" -ge "$MAX_FAILS" ] && exit 1 ; continue ; }

      # Phase 7: Push (commit mode only; pr mode already pushed inside task).
      if [ "$PUSH_MODE" = main ]; then
        git add -A && git commit -m "self-build loop $N: $(head -1 <<<"$GOAL" | cut -c1-90)" \
          && git push || { echo "[push] failed" ; record "$N" push-failed "$GOAL" ; exit 1 ; }
      fi

      record "$N" ok "$GOAL"
      echo "[ok] loop $N complete"
    fi
    echo "=== self-build loop $N end $(date -u +%FT%TZ) ==="
  } >> "$LOG" 2>&1

  tail -5 "$LOG"
  fails=0
  [ "$MAX_ITERS" -gt 0 ] && [ "$N" -ge "$MAX_ITERS" ] && { echo "max iterations reached" ; break ; }
done
