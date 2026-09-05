#!/usr/bin/env bash
# DevAgent self-build infinity loop driver.
# Executes one full product cycle (Research -> Ideas -> Validate -> Plan ->
# Implement -> Testing -> Push) per iteration, forever, using DevAgent's own
# pipeline (`devagent task`) as the implementation engine.
# Protocol: docs/SELF-BUILD-LOOP.md
set -euo pipefail

REPO="${SELFBUILD_REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
# Single-instance lock (2026-09-05: two drivers ran concurrently — an orphaned
# ppid=1 driver survived a hub stop and raced a fresh start on loop numbering,
# ledger writes, and pane sweeps). Portable mkdir lock (atomic on POSIX,
# including macOS where flock is unavailable); a second driver exits
# immediately instead of corrupting state. Stale lock: mkdir fails while the
# holder lives; a crashed holder leaves the dir but the pid check below
# clears it.
LOCK_DIR="${SELFBUILD_LOCK_DIR:-$REPO/.selfbuild/loop.lock.d}"
if mkdir "$LOCK_DIR" 2>/dev/null; then
  echo $$ > "$LOCK_DIR/pid"
  trap 'rm -rf "$LOCK_DIR"' EXIT
else
  HOLDER_PID=$(cat "$LOCK_DIR/pid" 2>/dev/null || echo '?')
  if [ "$HOLDER_PID" != "?" ] && ! kill -0 "$HOLDER_PID" 2>/dev/null; then
    echo "[lock] stale holder pid $HOLDER_PID is gone — clearing lock and retrying"
    rm -rf "$LOCK_DIR"
    if mkdir "$LOCK_DIR" 2>/dev/null; then
      echo $$ > "$LOCK_DIR/pid"
      trap 'rm -rf "$LOCK_DIR"' EXIT
    else
      echo "[lock] another driver still holds $LOCK_DIR — exiting"; exit 0
    fi
  else
    echo "[lock] another selfbuild-loop driver already holds $LOCK_DIR (pid $HOLDER_PID) — exiting"
    exit 0
  fi
fi

STATE="$REPO/.selfbuild"
MAX_ITERS="${SELFBUILD_MAX_ITERATIONS:-0}"
MAX_FAILS="${SELFBUILD_MAX_CONSECUTIVE_FAILURES:-3}"
STARVATION_LIMIT="${SELFBUILD_STARVATION_LIMIT:-5}"
CLEANUP_DELAY="${SELFBUILD_CLEANUP_DELAY_SECS:-1800}"
DRY_RUN="${SELFBUILD_DRY_RUN:-0}"
LESSONS="$STATE/lessons.md"
WORKER="${SELFBUILD_WORKER:-omp}"
PUSH_MODE="${SELFBUILD_PUSH_MODE:-pr}"
# Research/PO dispatch (FR-VIS-04, 2026-09-05): these phases run INSIDE a
# herdr pane via `devagent pane-run` (operator-visible), not bare headless
# child processes. Overridable per role for provider pinning.
RESEARCH_BIN="${SELFBUILD_RESEARCH_BIN:-omp -p --mode json --no-prewalk --no-lsp --no-extensions --model omniroute/dev}"
PO_BIN="${SELFBUILD_PO_BIN:-omp -p --mode json --no-prewalk --no-lsp --no-extensions --model omniroute/dev}"
CLAUDE_TIMEOUT="${SELFBUILD_CLAUDE_TIMEOUT:-600}"
# Research runs omp with URL-fetch tooling; competitor crawls need 10-15 min
# even when healthy (2026-09-02 live: 32 fetches, killed at 300s). PO picks a
# single backlog item from local evidence, but a full generation + teardown
# measured 244s + ~56s on omniroute/dev (2026-09-03 live: 300s budget fired at
# 300s on a completed agent) — keep 600s so a slow provider cannot kill the
# loop via the unguarded dispatch below.
RESEARCH_TIMEOUT="${SELFBUILD_RESEARCH_TIMEOUT:-900}"

# Spawn visibility (FR-VIS-04): visible is the default (2026-09-04 operator
# direction — workers run in attachable herdr panes like a human session);
# --headless / DEVAGENT_VISIBILITY=headless restores CI/LaunchAgent behavior.
# Precedence: argv flag > DEVAGENT_VISIBILITY env > SELFBUILD_VISIBILITY env
# > visible. Exported so every `devagent task` dispatch below inherits it.
VISIBILITY="${DEVAGENT_VISIBILITY:-${SELFBUILD_VISIBILITY:-visible}}"
NO_SYNC_DOCS="${SELFBUILD_NO_SYNC_DOCS:-0}"
while [ $# -gt 0 ]; do
  case "$1" in
    --headless) VISIBILITY=headless ;;
    --visible) VISIBILITY=visible ;;
    --no-sync-docs) NO_SYNC_DOCS=1 ;;
    *) echo "unknown argument: $1 (supported: --headless, --visible, --no-sync-docs)" >&2; exit 2 ;;
  esac
  shift
done
export DEVAGENT_VISIBILITY="$VISIBILITY"
DEVAGENT=(npx tsx "$REPO/src/cli.ts")

mkdir -p "$STATE/research" "$STATE/goals" "$STATE/logs"
cd "$REPO"
# GRADIENT adjacent-category scan (PRD Phase 4): canonical text printed by the
# `devagent scan-text` subcommand from src/research/scan-text.ts — embedded
# verbatim in the research/PO prompts below so they cannot drift from the module.
# Runs after `cd "$REPO"` like every other DEVAGENT call (a LaunchAgent cwd must
# not break npx resolution); a failed capture degrades to empty, not a dead driver.
GRADIENT_SCAN_TEXT="$("${DEVAGENT[@]}" scan-text 2>/dev/null)" || GRADIENT_SCAN_TEXT=""
# Degrade with a message (repo convention: selfbuild-state pull, queue-claim) —
# a silent empty string would hollow out both prompts below with no trace.
[ -n "$GRADIENT_SCAN_TEXT" ] || echo "[gradient] scan-text dispatch failed — prompts run without the adjacent-category scan" >&2

# Restore durable loop state (ledger + lessons) from origin before numbering.
bash "$REPO/scripts/selfbuild-state.sh" pull || echo "[state] pull failed, starting from local state"


record() { # record <loop> <status> <goal>
  local goal_txt
  goal_txt="$(printf '%s' "$3" | tr '\n\t' '  ' | tr -d '"' | cut -c1-160)"
  printf '{"loop":%s,"ts":"%s","status":"%s","goal":"%s"}\n' \
    "$1" "$(date -u +%FT%TZ)" "$2" "$goal_txt" >> "$STATE/ledger.jsonl"
  # Publish immediately (even for failures) so the next run continues here.
  bash "$REPO/scripts/selfbuild-state.sh" push || echo "[state] push deferred"
  # Q39 impact telemetry: mirror one loop-result event row per iteration so
  # lesson impact scoring can join lessons-eval rows (devagent lessons --loop)
  # to the deterministic loop outcome in .devagent/runs/orchestration/events.jsonl.
  EVENTS="$REPO/.devagent/runs/orchestration/events.jsonl"
  mkdir -p "$(dirname "$EVENTS")"
  printf '{"ts":"%s","kind":"event","event":"loop-result","loop":%s,"status":"%s","goal":"%s"}\n' \
    "$(date -u +%FT%TZ)" "$1" "$2" "$goal_txt" >> "$EVENTS"
}

# Human-visible phase tracking (operator ask: "how can I track/jump in the
# loop progress?"): every phase boundary appends a loop-phase row to the
# repo orchestration stream — the same JSONL the daemon's /events SSE
# follows (after the follower-source fix) and the TUI history renders.
# Phases: sync → preflight → research → po → task → done. detail carries
# the one human-readable breadcrumb (task id, gate verdict, …).
phase() { # phase <loop> <phase> [detail]
  local detail_txt
  detail_txt="$(printf '%s' "${3:-}" | tr '\n\t' '  ' | tr -d '"' | cut -c1-120)"
  EVENTS="$REPO/.devagent/runs/orchestration/events.jsonl"
  mkdir -p "$(dirname "$EVENTS")"
  printf '{"ts":"%s","kind":"event","event":"loop-phase","loop":%s,"phase":"%s"%s}\n' \
    "$(date -u +%FT%TZ)" "$1" "$2" \
    "$([ -n "$detail_txt" ] && printf ',"detail":"%s"' "$detail_txt")" >> "$EVENTS"
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
        # Degraded rows are expected pauses, not evidence of a thrashing
        # loop — never count them toward starvation:
        #   operator-degraded  operator absence / dirty PRD / omp wedge skip
        #   provider-degraded  preflight found the provider down (no spend);
        #                      2026-09-05: three circuit-outage rows tripped
        #                      the 5-strike gate and halted the factory after
        #                      the provider had already recovered.
        if (lines[i] ~ /"status":"(operator-degraded|provider-degraded)"/) continue
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
  # Next iteration number = max(loop)+1 over the ledger (line-count numbering
  # collided after state-merge dedupe collapsed repeated numbers).
  N=$(awk 'match($0,/"loop":[0-9]+/){n=substr($0,RSTART+7,RLENGTH-7)+0; if(n>m)m=n} END{print m+1}' "$STATE/ledger.jsonl" 2>/dev/null || echo 1)
  N=$(( N + 0 ))
  LOG="$STATE/logs/loop-$N.log"
  # Iteration cap checked at loop head: the tail check was unreachable for
  # skip/continue paths (Q27 guard, preflight, research) — a capped run could
  # skip-cycle forever until starvation halted it (2026-09-04 smoke evidence).
  [ "$MAX_ITERS" -gt 0 ] && [ "$N" -ge "$MAX_ITERS" ] && { echo "max iterations reached" ; break ; }
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

    # Operator preflight (Q40): probe the provider before spending the
    # iteration on research + PO + task. On failure the operator-degraded
    # ledger row is written and this cycle is skipped - a degraded factory
    # stays visible instead of surfacing as a bogus research/validate noop.
    if [ "$DRY_RUN" != 1 ]; then
      phase "$N" preflight "role=selfbuild"
      if ! "${DEVAGENT[@]}" preflight --role selfbuild --repo "$REPO"; then
        echo "[preflight] provider degraded - skipping iteration $N (ledger row written)"
        record "$N" provider-degraded "preflight: provider probe failed"
        fails=$(( fails + 1 ))
        [ "$fails" -ge "$MAX_FAILS" ] && { echo "circuit breaker: $fails consecutive failures" ; exit 1 ; }
        continue
      fi
    fi

    # Doc freshness gate (operator PRD-freshness fix): research + PO select
    # from docs/PRD.md, so a manual PRD update must be pulled before selection
    # or the loop keeps building the older doc version. fetch + ff-only; on a
    # dirty PRD (operator mid-edit) or diverged branch, skip the LLM phases
    # and record the degraded row instead of silently building stale work.
    # A dirty-PRD refusal is the EXPECTED operator-mid-edit state, not a
    # factory fault: it neither increments the breaker nor counts toward
    # starvation (recorded as operator-degraded, not skipped) — 3 rapid empty
    # iterations would otherwise circuit-break the factory (continue paths
    # skip the tail fails=0 reset). Offline/diverged still counts as failure.
    if [ "$NO_SYNC_DOCS" != 1 ]; then
      SYNC_OUT="$(git fetch origin main 2>&1 && git merge --ff-only origin/main 2>&1)" || {
        echo "[sync-docs] PRD refresh failed: $SYNC_OUT"
        if printf '%s' "$SYNC_OUT" | grep -q "locally modified\|Your local changes"; then
          record "$N" operator-degraded "doc-sync deferred: PRD locally modified"
        else
          record "$N" provider-degraded "doc-sync failed: $(printf '%s' "$SYNC_OUT" | tail -1 | cut -c1-120)"
          fails=$(( fails + 1 ))
          [ "$fails" -ge "$MAX_FAILS" ] && { echo "circuit breaker: $fails consecutive failures" ; exit 1 ; }
        fi
        sleep "${SELFBUILD_SYNC_RETRY_SECS:-60}"
        continue
      }
      printf '%s\n' "$SYNC_OUT" | grep -q "Already up to date" || echo "[sync-docs] $SYNC_OUT"
    fi

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
    phase "$N" research "timeout ${RESEARCH_TIMEOUT}s"
    # Local-evidence research (no web crawl): the web-search version crawled
    # 16-32 URLs per cycle and never fit the wall-clock budget — 34 consecutive
    # timeouts (2026-09-02). Everything the loop acts on is already local:
    # PRD backlog, ledger outcomes, lessons. Competitor scans are a separate
    # manual cadence, not a per-iteration gate.
    # FR-VIS-04: pane-runnable research — the phase runs inside a herdr pane
    # so the operator sees it live; on pane unavailability (pane-run exits 3)
    # the driver falls back to its own direct dispatch so the loop never
    # stalls on the visibility runtime.
    # The worker's raw NDJSON stdout lands in a scratch file; the phase file
    # the PO prompt names (loop-$N.md) holds the assistant's final text only.
    # Loop-90: the PO burned its whole 600s budget chewing a 2.3 MB raw
    # research stream and timed out with no goal (invalid row).
    RESEARCH_RAW="$STATE/research/loop-$N.ndjson"
    RESEARCH_OUT="$STATE/research/loop-$N.md"
    RESEARCH_PROMPT="You are phase 1 (Research) of the DevAgent self-build loop, iteration $N.
Repo: $REPO. Use ONLY local evidence — no web searches, no network fetches:
1. docs/PRD.md section 17 (Phase 4 backlog) and section 18 (open questions)
2. Recent loop ledger: ${PREV_TAIL:-none}
3. Accumulated lessons: $(tail -40 "$LESSONS" 2>/dev/null | head -c 4000 || echo none)
4. git log --oneline -15 (what just shipped, what friction it caused)

$GRADIENT_SCAN_TEXT

Rank the top 3 backlog items by (impact x tractability) for a single iteration. Consider: does an earlier failed loop already cover this? Does a merged PR already cover it? Output compact markdown (<300 words): your ranked top-3 with one-line rationale each, then THE single pick.
Do NOT edit any files. Output only."
    rm -f "$STATE/research/.loop-$N.done"
    "${DEVAGENT[@]}" pane-run --cwd "$REPO" --timeout "$RESEARCH_TIMEOUT" \
      --out "$RESEARCH_RAW" --err "$STATE/research/loop-$N.err" \
      --done "$STATE/research/.loop-$N.done" \
      -- $RESEARCH_BIN "$RESEARCH_PROMPT" </dev/null >/dev/null 2>&1
    [ -f "$STATE/research/.loop-$N.done" ] || timeout "$RESEARCH_TIMEOUT" $RESEARCH_BIN "$RESEARCH_PROMPT" </dev/null > "$RESEARCH_RAW" || true
    rm -f "$STATE/research/.loop-$N.done" "$STATE/research/loop-$N.err"
    node "$REPO/scripts/selfbuild-extract-text.mjs" "$RESEARCH_RAW" "$RESEARCH_OUT" --sentinel || cp "$RESEARCH_RAW" "$RESEARCH_OUT"
    rm -f "$RESEARCH_RAW"
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
      # PO preflight (Q40): the goal-selection dispatch dies silently under a
      # degraded provider (empty goal.tmp -> "[validate] goal file missing");
      # gate it so the skip lands as an operator-degraded row instead.
      if ! "${DEVAGENT[@]}" preflight --role po --repo "$REPO"; then
        echo "[preflight] provider degraded - skipping PO selection (ledger row written)"
        record "$N" provider-degraded "preflight(po): provider probe failed"
        fails=$(( fails + 1 ))
        [ "$fails" -ge "$MAX_FAILS" ] && { echo "circuit breaker: $fails consecutive failures" ; exit 1 ; }
        continue
      fi
    phase "$N" po "timeout ${CLAUDE_TIMEOUT}s"
    PO_PROMPT="You are phases 2-3 (Ideas + Validate) of the DevAgent self-build loop, iteration $N.
Repo: $REPO. Inputs: docs/PRD.md (Phase 4 backlog), .selfbuild/research/loop-$N.md, recent ledger entries below.
$PREV_TAIL
$LESSONS_CTX
$GRADIENT_SCAN_TEXT
Select exactly ONE backlog item scoped to a single implementable+testable iteration.
Validation checks (all must pass): maps to a PRD backlog item; no dependency on an earlier failed loop; verifiable by the repo test suite or CLI smoke run.
Output ONLY the goal statement (max 120 words), starting with 'Goal:' — this text is passed directly to devagent task as the implementation prompt."
    rm -f goal.tmp.raw "$STATE/goals/.loop-$N.done"
    "${DEVAGENT[@]}" pane-run --cwd "$REPO" --timeout "$CLAUDE_TIMEOUT" \
      --out goal.tmp.raw --err goal.tmp.err \
      --done "$STATE/goals/.loop-$N.done" \
      -- $PO_BIN "$PO_PROMPT" </dev/null >/dev/null 2>&1 || echo "[po] pane-run dispatch failed (rc=$?) — attempting partial extraction"
    if [ ! -f "$STATE/goals/.loop-$N.done" ]; then
      timeout "$CLAUDE_TIMEOUT" $PO_BIN "$PO_PROMPT" </dev/null > goal.tmp.raw || { echo "[po] direct dispatch failed (rc=$?) — attempting partial extraction" ; }
    fi
    rm -f "$STATE/goals/.loop-$N.done" goal.tmp.err
    # NDJSON event stream -> plain-text goal file (shared with research).
    # --sentinel: no assistant text in an NDJSON stream writes a small
    # "[extract-aborted]" diagnostic that fails the ^Goal: gate below,
    # instead of the old `out || raw` fallback that published the session
    # header as the ledger goal (loop-81/90 poison rows).
    # Extract to goal.tmp, then publish. The `|| mv raw` fallback covers a
    # helper crash (never a normal no-text run — that writes the sentinel and
    # exits 0). Guard the whole chain so a hard failure can't trip `set -e`
    # before the ^Goal: gate sees the file; goal.tmp is always moved out.
    node "$REPO/scripts/selfbuild-extract-text.mjs" goal.tmp.raw goal.tmp --sentinel \
      || mv goal.tmp.raw goal.tmp
    mv goal.tmp "$STATE/goals/loop-$N.md"
    rm -f goal.tmp.raw
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
        # Mark a queue-claimed item done too, or the queue-first selector
        # re-claims the same already-shipped goal every iteration (2026-09-04:
        # SCOUT-20260903-fallback skipped twice, then burned a worker dispatch).
        [ -n "${QUEUED_TASK_ID:-}" ] && node "$REPO/scripts/selfbuild-queue-done.mjs" "$REPO" "$QUEUED_TASK_ID" done "already shipped (Q27 guard)" >/dev/null 2>&1 || true
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
      # Hard wall-clock cap + bounded retry budget: worker retries default to
      # Infinity (src/config.ts resilience) with 200 infra retries in executor.ts,
      # so one provider storm serializes into a 12h silent iteration (2026-09-03:
      # 11h45m). Outer timeout bounds the whole dispatch; env caps bound the
      # retry budget and no-progress hang detection inside it.
      TASK_ARGS=(task --prompt "$GOAL" --repo "$REPO" --worker "$WORKER")
      # Jump-in breadcrumb: the phase-4 card in the TUI (and `devagent sessions`)
      # points at the worker pane once this event lands on the SSE stream.
      phase "$N" task "$(head -1 <<<"$GOAL" | cut -c1-100)"
      [ -n "${SELFBUILD_MODEL:-}" ] && TASK_ARGS+=(--model "$SELFBUILD_MODEL")
      [ "$PUSH_MODE" = pr ] && TASK_ARGS+=(--auto-pr)
      DEVAGENT_API_MAX_ATTEMPTS="${SELFBUILD_API_MAX_ATTEMPTS:-40}" \
      DEVAGENT_NO_PROGRESS_TIMEOUT_MS="${SELFBUILD_NO_PROGRESS_TIMEOUT_MS:-600000}" \
      DEVAGENT_VISIBILITY="$VISIBILITY" \
        timeout "${SELFBUILD_TASK_TIMEOUT:-7200}" "${DEVAGENT[@]}" "${TASK_ARGS[@]}" || { echo "[implement] task failed" ; record "$N" failed "$GOAL" ; [ -n "${QUEUED_TASK_ID:-}" ] && node "$REPO/scripts/selfbuild-queue-done.mjs" "$REPO" "$QUEUED_TASK_ID" failed "implement failed at loop $N" >/dev/null 2>&1 || true ; fails=$(( fails + 1 )) ;
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
  # Success resets the consecutive-failure breaker (pre-loop-tail behavior).
  fails=0
done
