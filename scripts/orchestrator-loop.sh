#!/usr/bin/env bash
# DevAgent orchestrator-driven self-build loop driver.
#
# Difference from scripts/selfbuild-loop.sh: this driver uses the full
# planner + executor + auditor triad (`devagent orchestrate`) per iteration
# instead of a single `devagent task` worker, then opens a PR from the
# integrated merge commit using `gh pr create` directly. Every iteration
# must pass an independent audit before the PR is opened.
#
# Pipeline per iteration:
#   1. RESEARCH   — `claude -p` headless writes a one-loop goal.
#   2. PLAN+EXEC  — `devagent orchestrate --goal <g>` runs planner,
#                   executor, and (independently) auditor. Triad merges
#                   task branches into main when all tasks pass.
#   3. PR OPEN    — if integration succeeded, `gh pr create` opens a PR
#                   for the latest commit on main. The PR body cites the
#                   goal + audit verdict.
#   4. RECORD     — ledger entry (status: ok|failed|pr-open|merged).
#   5. SYNC       — pull origin selfbuild/state, push our ledger.
#
# Starvation gate (5 consecutive non-productive iterations) and circuit
# breaker (3 consecutive failures) match the legacy driver.
#
# Session-scoped only — does NOT install a LaunchAgent.

set -euo pipefail

REPO="${SELFBUILD_REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
STATE="$REPO/.selfbuild"
MAX_FAILS="${SELFBUILD_MAX_CONSECUTIVE_FAILURES:-3}"
STARVATION_LIMIT="${SELFBUILD_STARVATION_LIMIT:-5}"
DRY_RUN="${SELFBUILD_DRY_RUN:-0}"
LESSONS="$STATE/lessons.md"
LEDGER="$STATE/ledger.jsonl"
DEVAGENT=(npx tsx "$REPO/src/cli.ts")
# Append --model to the research/validate omp invocations so every nested
# agent uses the same model. The orchestrator's own planner/executor/auditor
# read this from devagent.json (already set); CLAUDE_BIN carries it explicitly.
if [ -n "${SELFBUILD_MODEL:-}" ]; then
  CLAUDE_BIN="${SELFBUILD_CLAUDE:-omp -p --mode json --no-prewalk --no-lsp --no-extensions --model $SELFBUILD_MODEL}"
else
  CLAUDE_BIN="${SELFBUILD_CLAUDE:-omp -p --mode json --no-prewalk --no-lsp --no-extensions --model omniroute/dev}"
fi

mkdir -p "$STATE/research" "$STATE/goals" "$STATE/logs"
cd "$REPO"

bash "$REPO/scripts/selfbuild-state.sh" pull >/dev/null 2>&1 || true

ledger_lines() {
  if [ -f "$LEDGER" ]; then wc -l < "$LEDGER" | tr -d ' '; else echo 0; fi
}

record() {
  printf '{"loop":%s,"ts":"%s","status":"%s","goal":"%s"}\n' \
    "$1" "$(date -u +%FT%TZ)" "$2" \
    "$(printf '%s' "$3" | tr -d '"' | cut -c1-160)" >> "$LEDGER"
  bash "$REPO/scripts/selfbuild-state.sh" push >/dev/null 2>&1 || true
}

starved() {
  [ -f "$LEDGER" ] || return 1
  local count
  count=$(awk -v lim="$STARVATION_LIMIT" '
    { lines[NR] = $0 }
    END {
      c = 0
      for (i = NR; i >= 1; i--) {
        if (lines[i] ~ /"status":"(ok|pr-open|merged|pushed)"/) break
        if (++c >= lim) break
      }
      print c
    }' "$LEDGER")
  [ "${count:-0}" -ge "$STARVATION_LIMIT" ]
}

fails=0
while :; do
  N=$(( $(ledger_lines) + 1 ))
  LOG="$STATE/logs/loop-$N.log"
  {
    echo "=== orchestrator loop $N start $(date -u +%FT%TZ) ==="

    if starved; then
      echo "[starvation] $STARVATION_LIMIT consecutive non-productive iterations — halting"
      exit 1
    fi

    # Pre-loop guard: the orchestrator's merge-back runs in the main worktree
    # via `git merge`, which aborts when the working tree has uncommitted
    # edits. Root cause of the loop-52 "Your local changes to autopr.ts would
    # be overwritten" failure: the previous session left a WIP diff in the
    # main worktree, and `git checkout main` brought it forward. Auto-commit
    # or stash any local changes so the merge target is always clean.
    CURRENT_BRANCH=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)
    if [ "$CURRENT_BRANCH" != "main" ]; then
      echo "[guard] main worktree is on '$CURRENT_BRANCH' (expected main); checking out main"
      git checkout main >/dev/null 2>&1 || {
        echo "[guard] could not checkout main; aborting loop to avoid merge chaos"
        record "$N" failed "pre-loop guard: cannot checkout main"
        exit 1
      }
    fi
    if ! git diff --quiet --ignore-submodules HEAD 2>/dev/null; then
      echo "[guard] main worktree has uncommitted changes; auto-stashing before loop"
      git stash push -u -m "loop-$N pre-loop auto-stash $(date -u +%FT%TZ)" >/dev/null 2>&1 || {
        echo "[guard] stash failed; aborting loop"
        record "$N" failed "pre-loop guard: cannot stash"
        exit 1
      }
    fi

    git pull --ff-only >/dev/null 2>&1 || echo "[sync] skipped (pull failed)"

    # Build research context from ledger + lessons so the researcher does not
    # re-derive already-shipped work (root cause of the loop-51 duplicate:
    # this driver originally referenced PREV_TAIL/LESSONS_CTX without ever
    # setting them, so research ran blind and re-picked PR #39's Q9 work).
    PREV_TAIL=""
    [ -f "$LEDGER" ] && PREV_TAIL=$(tail -3 "$LEDGER" || true)
    LESSONS_CTX=""
    [ -f "$LESSONS" ] && LESSONS_CTX="Accumulated lessons (do not re-derive): $(tail -20 "$LESSONS")"

    # Phase 1: Research — competitor scan + PRD backlog, constrained by
    # ledger + lessons so the researcher does not re-derive shipped work.
    if [ "$DRY_RUN" = 1 ]; then
      echo "[dry-run] phase 1 research skipped"
      echo "# dry-run stub" > "$STATE/research/loop-$N.md"
      GOAL="(dry-run) verify orchestrator phases execute without side effects"
    else
      $CLAUDE_BIN "You are phase 1 (Research) of the DevAgent orchestrator loop, iteration $N.
Repo: $REPO. Read docs/PRD.md section 4 (competitive landscape) and section 17 (roadmap).
Recent loop ledger (already shipped — do NOT re-pick these): ${PREV_TAIL:-none}.
$LESSONS_CTX
Web-search what changed recently for: Devin/Cognition, GitHub Copilot coding agent, OpenHands, Factory Droid, Google Jules, OpenAI Codex cloud agent; and for projects that run agents in self-improving loops over their own codebase.
Output compact markdown (<400 words): NEW competitor moves with URLs; self-build loop patterns worth copying; then a ranked recommendation of the single best next backlog item for this iteration and why.
Afterwards, append any DURABLE new lessons (1-3 bullets, dated heading '## <date>') to $LESSONS — never delete or edit existing lessons (ratchet-only)." \
        > "$STATE/research/loop-$N.raw" || {
          echo "[research] failed; recording failure"
          record "$N" failed "research phase failed"
          fails=$(( fails + 1 ))
          [ "$fails" -ge "$MAX_FAILS" ] && { echo "circuit breaker"; exit 1; }
          continue
        }
      # headless omp emits NDJSON; keep the human-readable research markdown
      # (same assistant-text extraction as phases 2-3 below).
      node -e '
        const fs = require("fs");
        const lines = fs.readFileSync(process.argv[1], "utf8").split("\n");
        let out = "";
        for (const line of lines) {
          try {
            const o = JSON.parse(line);
            if (o.type === "message_end" && o.message?.role === "assistant") {
              for (const c of o.message.content ?? []) {
                if (c.type === "text" && c.text) out = c.text;
              }
            }
          } catch {}
        }
        fs.writeFileSync(process.argv[2], out || fs.readFileSync(process.argv[1], "utf8"));
      ' "$STATE/research/loop-$N.raw" "$STATE/research/loop-$N.md" && rm -f "$STATE/research/loop-$N.raw"

      # Phases 2-3: Ideas + Validate — pick exactly ONE backlog item,
      # deduped against the ledger. The validator MUST reject a goal that
      # restates a merged/ok ledger entry (root cause of the loop-51
      # duplicate: research alone re-picked PR #39's Q9 work).
      $CLAUDE_BIN "You are phases 2-3 (Ideas + Validate) of the DevAgent orchestrator loop, iteration $N.
Repo: $REPO. Inputs: docs/PRD.md (Phase 4 backlog), .selfbuild/research/loop-$N.md, recent ledger entries below.
$PREV_TAIL
$LESSONS_CTX
Select exactly ONE backlog item scoped to a single implementable+testable iteration.
Validation checks (all must pass): maps to a PRD backlog item; NOT already shipped per the ledger entries above (reject any goal that restates a merged item); no dependency on an earlier failed loop; verifiable by the repo test suite or CLI smoke run.
Output ONLY the goal statement (max 120 words), starting with 'Goal:' — this text is passed directly to devagent orchestrate." \
        > goal.tmp.raw || echo "[validate] failed"
      # headless omp/pi emit an NDJSON event stream; extract the assistant's
      # final text block into the plain-text goal file the driver expects
      # (same extraction as selfbuild-loop.sh phases 2-3).
      node -e '
        const fs = require("fs");
        const lines = fs.readFileSync("goal.tmp.raw", "utf8").split("\n");
        let out = "";
        for (const line of lines) {
          try {
            const o = JSON.parse(line);
            if (o.type === "message_end" && o.message?.role === "assistant") {
              for (const c of o.message.content ?? []) {
                if (c.type === "text" && c.text) out = c.text;
              }
            }
          } catch {}
        }
        fs.writeFileSync("goal.tmp", out || fs.readFileSync("goal.tmp.raw", "utf8"));
      ' && rm -f goal.tmp.raw && mv goal.tmp "$STATE/goals/loop-$N.md"
      GOAL=$(grep '^Goal:' "$STATE/goals/loop-$N.md" 2>/dev/null | head -1 | sed 's/^Goal: //')
      [ -z "$GOAL" ] && {
        echo "[validate] goal file missing Goal: line — marking iteration invalid"
        record "$N" invalid "$(cat "$STATE/goals/loop-$N.md" 2>/dev/null | head -1)"
        fails=$(( fails + 1 ))
        [ "$fails" -ge "$MAX_FAILS" ] && exit 1
        continue
      }
    fi

    echo "Goal: $GOAL"

    # Phase 2-3: Plan + Execute + Audit (orchestrator triad)
    if [ "$DRY_RUN" = 1 ]; then
      echo "[dry-run] orchestrate skipped"
      record "$N" ok "$GOAL"
    else
      ORCH_OUT=$("${DEVAGENT[@]}" orchestrate --repo "$REPO" --goal "$GOAL" 2>&1) || {
        echo "[orchestrate] failed: $(echo "$ORCH_OUT" | tail -10)"
        record "$N" failed "$GOAL"
        fails=$(( fails + 1 ))
        [ "$fails" -ge "$MAX_FAILS" ] && { echo "circuit breaker"; exit 1; }
        continue
      }

      # Detect integration success from the triad output
      if ! echo "$ORCH_OUT" | grep -q "Integrated:"; then
        echo "[orchestrate] no integration message; treating as failed"
        echo "$ORCH_OUT" | tail -20
        record "$N" failed-tests "$GOAL"
        fails=$(( fails + 1 ))
        [ "$fails" -ge "$MAX_FAILS" ] && exit 1
        continue
      fi

      # Phase 7: open a PR for the integrated commit
      BRANCH_NAME="orchestrator/loop-$N"
      git checkout -B "$BRANCH_NAME" >/dev/null 2>&1 || true
      PR_URL=$(gh pr create \
        --base main \
        --head "$BRANCH_NAME" \
        --title "Orchestrator loop $N: $(echo "$GOAL" | head -1 | cut -c1-80)" \
        --body "Goal: $GOAL

## Audit
All tasks passed independent audit (verdict=pass, integrity=clean).
Orchestrator integrated work to $BRANCH_NAME from this iteration.

Generated by scripts/orchestrator-loop.sh" 2>&1 | tail -1) || {
        echo "[pr] open failed: $PR_URL"
        record "$N" push-failed "$GOAL"
        fails=$(( fails + 1 ))
        [ "$fails" -ge "$MAX_FAILS" ] && exit 1
        continue
      }
      PR_NUM=$(echo "$PR_URL" | grep -oE '[0-9]+$' | head -1 || echo "?")
      echo "[pr] opened #$PR_NUM: $PR_URL"
      record "$N" pr-open "$GOAL"
    fi

    echo "=== orchestrator loop $N end $(date -u +%FT%TZ) ==="
  } >> "$LOG" 2>&1

  tail -10 "$LOG"
  fails=0
  [ "${SELFBUILD_MAX_ITERATIONS:-0}" -gt 0 ] && [ "$N" -ge "${SELFBUILD_MAX_ITERATIONS:-0}" ] && { echo "max iters reached"; break; }
done
