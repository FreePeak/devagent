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
STARVATION_LIMIT="${SELFBUILD_STARVATION_LIMIT:-5}"
CLEANUP_DELAY="${SELFBUILD_CLEANUP_DELAY_SECS:-1800}"
DRY_RUN="${SELFBUILD_DRY_RUN:-0}"
LESSONS="$STATE/lessons.md"
WORKER="${SELFBUILD_WORKER:-omp}"
PUSH_MODE="${SELFBUILD_PUSH_MODE:-pr}"
CLAUDE_BIN="${SELFBUILD_CLAUDE:-omp -p --mode json --no-prewalk --no-lsp --no-extensions --model omniroute/dev}"
# Role-to-tool mapping (operator spec 2026-09-02): all agents on omp.
RESEARCH_BIN="${SELFBUILD_RESEARCH_BIN:-omp -p --mode json --no-prewalk --no-lsp --no-extensions --model omniroute/dev}"
PO_BIN="${SELFBUILD_PO_BIN:-omp -p --mode json --no-prewalk --no-lsp --no-extensions --model omniroute/dev}"
CLAUDE_TIMEOUT="${SELFBUILD_CLAUDE_TIMEOUT:-300}"
# Research runs omp with URL-fetch tooling; competitor crawls need 10-15 min
# even when healthy (2026-09-02 live: 32 fetches, killed at 300s). PO picks a
# single backlog item from local evidence — 300s is ample. Split budgets.
RESEARCH_TIMEOUT="${SELFBUILD_RESEARCH_TIMEOUT:-900}"
DEVAGENT=(npx tsx "$REPO/src/cli.ts")

mkdir -p "$STATE/research" "$STATE/goals" "$STATE/logs"
cd "$REPO"

# Restore durable loop state (ledger + lessons) from origin before numbering.
bash "$REPO/scripts/selfbuild-state.sh" pull || echo "[state] pull failed, starting from local state"

ledger_lines() {
  if [ -f "$STATE/ledger.jsonl" ]; then wc -l < "$STATE/ledger.jsonl"; else echo 0; fi
}

record() { # record <loop> <status> <goal>
  printf '{"loop":%s,"ts":"%s","status":"%s","goal":"%s"}\n' \
    "$1" "$(date -u +%FT%TZ)" "$2" \
    "$(printf '%s' "$3" | tr -d '"' | cut -c1-160)" >> "$STATE/ledger.jsonl"
  # Publish immediately (even for failures) so the next run continues here.
  bash "$REPO/scripts/selfbuild-state.sh" push || echo "[state] push deferred"
}

# Starvation gate: consecutive non-productive iterations across ALL runs.
# Unlike the circuit breaker (in-process failures), this catches a loop that
# has been thrashing for days without shipping anything (Kitchen Loop 7.2).
# Productive = shipped or handed to review; the ledger's richer statuses
# ("pr-open", "merged", "pushed" — written by the Orca-driven runs) all count,
# otherwise a healthy streak reads as starvation and halts the loop.
# Q27 re-burn guard: a goal is "already handled" when any ledger entry with
# a productive status (ok|pr-open|merged|pushed) carries the same goal text
# (normalized: quotes stripped, whitespace collapsed). Phase 2-3 selection can
# otherwise re-pick a goal whose PR already merged — loops 53-55/57/58 and the
# 2026-09-01 Q35 re-burn (shipped as #100, then re-selected because the driver
# restart lost the record) each burned attempts on an already-planned goal.
already_shipped() { # already_shipped <goal-text>
  [ -f "$STATE/ledger.jsonl" ] || return 1
  local want
  # Match on the PRD backlog item id (Q35, Q24, ...) when the goal names one —
  # goal text is rewritten between selection and ledger record, but the item id
  # is stable. Also match the normalized first 60 chars as a loose fallback.
  want=$(printf '%s' "$1" | tr -d '"' | tr -s '[:space:]' ' ')
  local item
  item=$(printf '%s' "$want" | grep -oE 'Q[0-9]+' | head -1 || true)
  awk -v want="$want" -v item="${item:-}" '
    /"status":"(ok|pr-open|merged|pushed)"/ {
      gsub(/"/, "", $0)
      gsub(/[[:space:]]+/, " ", $0)
      key = substr(want, 1, 60)
      if ((item != "" && index($0, item) > 0) || index($0, key) > 0) found = 1
    }
    END { exit found ? 0 : 1 }
  ' "$STATE/ledger.jsonl"
}

starved() {
  [ -f "$STATE/ledger.jsonl" ] || return 1
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
    }' "$STATE/ledger.jsonl")
  [ "${count:-0}" -ge "$STARVATION_LIMIT" ]
}

# Deferred cleanup of auto-pr leftovers. `devagent task --auto-pr` pushes the
# branch and opens a PR but leaves the worktree (.devagent-worktrees/TASK) and
# branch (devagent/TASK) behind. After each successful pr-mode iteration the
# pair is queued here; the next iteration sweeps entries older than
# CLEANUP_DELAY seconds. Crash-safe: pending entries survive driver restarts.
PENDING="$STATE/cleanup-pending.jsonl"

schedule_cleanup() { # schedule_cleanup <loop>
  printf '{"loop":"%s","ts":%s,"branch":"%s","worktree":"%s"}\n' \
    "$1" "$(date +%s)" "devagent/TASK" "$REPO/.devagent-worktrees/TASK" >> "$PENDING"
}

sweep_cleanup() {
  [ -f "$PENDING" ] || return 0
  local now ts line keep="" branch wt local_tip remote_tip
  now=$(date +%s)
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    ts=$(sed -n 's/.*"ts":\([0-9][0-9]*\).*/\1/p' <<<"$line")
    if [ -z "$ts" ] || [ $((now - ts)) -lt "$CLEANUP_DELAY" ]; then
      keep+="$line"$'\n'; continue
    fi
    branch=$(sed -n 's/.*"branch":"\([^"]*\)".*/\1/p' <<<"$line")
    wt=$(sed -n 's/.*"worktree":"\([^"]*\)".*/\1/p' <<<"$line")
    if ! git show-ref --verify --quiet "refs/heads/$branch"; then
      echo "[cleanup] $branch already gone"
      git worktree remove --force "$wt" 2>/dev/null || true
      continue
    fi
    # Only delete once the branch tip is verified on the remote, i.e. the PR
    # was actually created; unpushed work is preserved for manual recovery.
    local_tip=$(git rev-parse "refs/heads/$branch")
    remote_tip=$(git ls-remote origin "refs/heads/$branch" | cut -f1)
    if [ -n "$remote_tip" ] && [ "$local_tip" = "$remote_tip" ]; then
      git worktree remove --force "$wt" && git branch -D "$branch" >/dev/null \
        && echo "[cleanup] removed $branch + $wt"
    else
      echo "[cleanup] deferring $branch (tip not on origin — no PR yet)"
      keep+="$line"$'\n'
    fi
  done < "$PENDING"
  printf '%s' "$keep" > "$PENDING"
}

fails=0
while :; do
  N=$(( $(ledger_lines | tr -d ' ') + 1 ))
  LOG="$STATE/logs/loop-$N.log"
  {
    echo "=== self-build loop $N start $(date -u +%FT%TZ) ==="

    # Starvation gate: halt a loop that stopped shipping (checked before spending tokens).
    if starved; then
      echo "[starvation] $STARVATION_LIMIT consecutive non-productive iterations — halting loop"
      exit 1
    fi

    # Sweep auto-pr leftovers whose 30-min grace period has elapsed.
    sweep_cleanup

    # Herdr hygiene: close idle/agentless panes the previous iteration left in
    # the devagent session (killed tasks, stalled workers, crashed dispatches).
    # Same session-scoped trust boundary as orchestrate-loop's sweep; the
    # session name only ever holds automation-spawned panes. Non-fatal when
    # herdr is down.
    HERDR_SWEEP_OUT="$(${DEVAGENT[@]} herdr-sweep 2>&1)" || true
    [ -n "$HERDR_SWEEP_OUT" ] && printf '%s\n' "$HERDR_SWEEP_OUT" | tail -3

    # Sync with remote; tolerate offline / diverged states.
    git pull --ff-only || echo "[sync] skipped (pull failed)"

    # Phase 1: Research. Feed prior failures back in so defects compound into fixes.
    PREV_TAIL=""
    [ -f "$STATE/ledger.jsonl" ] && PREV_TAIL=$(tail -3 "$STATE/ledger.jsonl" || true)
    # Lessons digest echoed into the research/validate prompts: the same
    # 40-line/4000-char cap the worker-prompt digest enforces (PRD Q9). The
    # eval-guard keeps the ratchet deduped at append time; the digest stays a
    # text cursor so optional `predictedImpact:` suffixes echo verbatim.
    LESSONS_CTX=""; [ -f "$LESSONS" ] && LESSONS_CTX="Accumulated lessons (do not re-derive): $(tail -40 "$LESSONS" | head -c 4000)"
    if [ "$DRY_RUN" = 1 ]; then
      echo "[dry-run] phase 1 research skipped"
      echo "# dry-run stub" > "$STATE/research/loop-$N.md"
      echo "Goal: (dry-run) verify driver phases execute without side effects" > "$STATE/goals/loop-$N.md"
    else
    # Local-evidence research (no web crawl): the web-search version crawled
    # 16-32 URLs per cycle and never fit the wall-clock budget — 34 consecutive
    # timeouts (2026-09-02). Everything the loop acts on is already local:
    # PRD backlog, ledger outcomes, lessons. Competitor scans are a separate
    # manual cadence, not a per-iteration gate.
    timeout "$RESEARCH_TIMEOUT" $RESEARCH_BIN "You are phase 1 (Research) of the DevAgent self-build loop, iteration $N.
Repo: $REPO. Use ONLY local evidence — no web searches, no network fetches:
1. docs/PRD.md section 17 (Phase 4 backlog) and section 18 (open questions)
2. Recent loop ledger: ${PREV_TAIL:-none}
3. Accumulated lessons: $(tail -40 "$LESSONS" 2>/dev/null | head -c 4000 || echo none)
4. git log --oneline -15 (what just shipped, what friction it caused)
Rank the top 3 backlog items by (impact x tractability) for a single iteration. Consider: does an earlier failed loop already cover this? Does a merged PR already cover it? Output compact markdown (<300 words): your ranked top-3 with one-line rationale each, then THE single pick.
Do NOT edit any files. Output only." \
      > "$STATE/research/loop-$N.md" || echo "[research] failed, continuing with backlog-only selection"
    fi

    # Phase 2a: queue-first selection. Pending queue tasks (scout PRDs, backlog
    # items) are concrete, already-validated work — they outrank LLM selection.
    # Without this, a full queue blocks the scout at maxQueued while the loop
    # keeps inventing fresh goals (2026-09-01 deadlock: 8 pending, 0 consumed).
    QUEUE_JSON="$(node "$REPO/scripts/selfbuild-queue-claim.mjs" "$REPO" 2>/dev/null || true)"
    if [ -n "$QUEUE_JSON" ] && [ "$QUEUE_JSON" != "{}" ]; then
      QID="$(printf '%s' "$QUEUE_JSON" | node -e 'let d="";process.stdin.on("data",(c)=>d+=c).on("end",()=>console.log(JSON.parse(d).id))')"
      QGOAL="$(printf '%s' "$QUEUE_JSON" | node -e 'let d="";process.stdin.on("data",(c)=>d+=c).on("end",()=>console.log(JSON.parse(d).goal))')"
      echo "[queue] claimed $QID from .devagent/queue (queue-first outranks LLM selection)"
      GOAL="$QGOAL"
      QUEUED_TASK_ID="$QID"
      printf '%s\n' "$GOAL" > goal.tmp && mv goal.tmp "$STATE/goals/loop-$N.md"
    fi

    if [ -z "${QUEUED_TASK_ID:-}" ]; then
    # Phases 2-3: Idea + Validate. Pick one PRD Phase 4 item, constrained by research.
    if [ "$DRY_RUN" != 1 ]; then
    timeout "$CLAUDE_TIMEOUT" $PO_BIN "You are phases 2-3 (Ideas + Validate) of the DevAgent self-build loop, iteration $N.
Repo: $REPO. Inputs: docs/PRD.md (Phase 4 backlog), .selfbuild/research/loop-$N.md, recent ledger entries below.
$PREV_TAIL
$LESSONS_CTX
Select exactly ONE backlog item scoped to a single implementable+testable iteration.
Validation checks (all must pass): maps to a PRD backlog item; no dependency on an earlier failed loop; verifiable by the repo test suite or CLI smoke run.
Output ONLY the goal statement (max 120 words), starting with 'Goal:' — this text is passed directly to devagent task as the implementation prompt." \
      > goal.tmp.raw
      # headless pi/omp emit an NDJSON event stream; extract the assistant's
      # final text block into the plain-text goal file the driver expects.
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
    fi
    fi # end queue-first fallback to LLM selection

    GOAL_FILE="$STATE/goals/loop-$N.md"
    if ! grep -q '^Goal:' "$GOAL_FILE"; then
      echo "[validate] goal file missing Goal: line — marking iteration invalid" ; record "$N" invalid "$(cat "$GOAL_FILE" 2>/dev/null)" ; fails=$(( fails + 1 )) ; else
      GOAL=$(cat "$GOAL_FILE")

      # Q27 guard: never re-implement a goal that already shipped (a ledger
      # entry with a productive status carries the same text). Loop 58 re-burned
      # Q35 after its PR #100 merged because the driver restart lost the record;
      # skip it so the iteration doesn't re-burn spend on already-planned work.
      if already_shipped "$GOAL"; then
        echo "[guard] goal already shipped — skipping (Q27 no re-burn)"
        record "$N" skipped "$GOAL"
        echo "[ok] loop $N skipped (already shipped)"
        fails=0
        continue
      fi

      if [ "$DRY_RUN" = 1 ]; then
        echo "[dry-run] phases 4-7 skipped (implement/test/push)"
        record "$N" ok "(dry-run) $GOAL"
        echo "[ok] loop $N complete"
      else

      # Phases 4-5-6: Plan + Implement + internal validation gates via DevAgent itself.
      TASK_ARGS=(task --prompt "$GOAL" --repo "$REPO" --worker "$WORKER")
      [ -n "${SELFBUILD_MODEL:-}" ] && TASK_ARGS+=(--model "$SELFBUILD_MODEL")
      [ "$PUSH_MODE" = pr ] && TASK_ARGS+=(--auto-pr)
      "${DEVAGENT[@]}" "${TASK_ARGS[@]}" || { echo "[implement] task failed" ; record "$N" failed "$GOAL" ; [ -n "${QUEUED_TASK_ID:-}" ] && node "$REPO/scripts/selfbuild-queue-done.mjs" "$REPO" "$QUEUED_TASK_ID" failed "implement failed at loop $N" >/dev/null 2>&1 || true ; fails=$(( fails + 1 )) ;
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
      [ -n "${QUEUED_TASK_ID:-}" ] && node "$REPO/scripts/selfbuild-queue-done.mjs" "$REPO" "$QUEUED_TASK_ID" done >/dev/null 2>&1 || true
      [ "$PUSH_MODE" = pr ] && schedule_cleanup "$N"
      echo "[ok] loop $N complete"
      fi # DRY_RUN
    fi
    echo "=== self-build loop $N end $(date -u +%FT%TZ) ==="
  } >> "$LOG" 2>&1

  tail -5 "$LOG"
  fails=0
  [ "$MAX_ITERS" -gt 0 ] && [ "$N" -ge "$MAX_ITERS" ] && { echo "max iterations reached" ; break ; }
done
