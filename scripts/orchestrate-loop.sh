#!/usr/bin/env bash
# DevAgent orchestrator loop (self-build factory, DAG role).
# Drives the .devagent-project.json board via `devagent orchestrate --resume`
# until every task reaches a terminal state, then idles. When no board exists
# it plans one from ORCHESTRATOR_GOAL (env) or .devagent/orchestrator-goal.txt.
#
# Env knobs:
#   ORCHESTRATOR_REPO       repo path                  (default: script's parent)
#   ORCHESTRATOR_GOAL       goal text used for planning (fallback: repo goal file)
#   ORCHESTRATOR_MAX_FAILS  circuit breaker            (default 3 consecutive failures)
#   ORCHESTRATOR_POLL_SECS  idle wait between cycles   (default 600)
#   ORCHESTRATOR_REQUEUE_AFTER  reset failed/blocked tasks to pending after this many
#                             consecutive parked polls (0 = never; default 6 ~= 1h)
#   ORCHESTRATOR_PLAN_ONLY  1 = plan once, never execute (default 0)
#   ORCHESTRATOR_DRY_RUN    1 = log intent only        (default 0)
set -euo pipefail

REPO="${ORCHESTRATOR_REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
MAX_FAILS="${ORCHESTRATOR_MAX_FAILS:-3}"
POLL_SECS="${ORCHESTRATOR_POLL_SECS:-600}"
REQUEUE_AFTER="${ORCHESTRATOR_REQUEUE_AFTER:-6}"
PLAN_ONLY="${ORCHESTRATOR_PLAN_ONLY:-0}"
DRY_RUN="${ORCHESTRATOR_DRY_RUN:-0}"
DEVAGENT=(node "$REPO/dist/src/cli.js")
BOARD="$REPO/.devagent-project.json"
GOAL_FILE="$REPO/.devagent/orchestrator-goal.txt"

cd "$REPO"

resolve_goal() {
  if [ -n "${ORCHESTRATOR_GOAL:-}" ]; then printf '%s' "$ORCHESTRATOR_GOAL"; return; fi
  if [ -f "$GOAL_FILE" ]; then head -c 2000 "$GOAL_FILE"; return; fi
  printf 'Continue the devagent self-build loop per docs/ORCHESTRATOR-FACTORY.md'
}

board_open_tasks() { # count tasks not in done/failed/blocked
  [ -f "$BOARD" ] || { echo 0; return; }
  node -e '
    const b = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
    console.log(b.tasks.filter(t => !["done","failed","blocked"].includes(t.status)).length);
  ' "$BOARD" 2>/dev/null || echo 0
}

board_done_tasks() {
  [ -f "$BOARD" ] || { echo 0; return; }
  node -e '
    const b = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
    console.log(b.tasks.filter(t => t.status === "done").length);
  ' "$BOARD" 2>/dev/null || echo 0
}

board_total_tasks() {
  [ -f "$BOARD" ] || { echo 0; return; }
  node -e '
    const b = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
    console.log(b.tasks.length);
  ' "$BOARD" 2>/dev/null || echo 0
}

board_stuck_tasks() { # count tasks in failed/blocked (the dispatch-dead states)
  [ -f "$BOARD" ] || { echo 0; return; }
  node -e '
    const b = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
    console.log(b.tasks.filter(t => ["failed","blocked"].includes(t.status)).length);
  ' "$BOARD" 2>/dev/null || echo 0
}

board_pending_tasks() { # count tasks waiting to become ready
  [ -f "$BOARD" ] || { echo 0; return; }
  node -e '
    const b = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
    console.log(b.tasks.filter(t => t.status === "pending").length);
  ' "$BOARD" 2>/dev/null || echo 0
}

cleanup_merged_worktrees() { # remove worktrees/branches whose PRs merged (safe-gated)
  if [ -x "$REPO/scripts/git-cleanup-merged.sh" ]; then
    echo "[cleanup] pruning merged branches/worktrees"
    "$REPO/scripts/git-cleanup-merged.sh" --root "$REPO" --apply >/dev/null 2>&1 || true
  fi
}

requeue_parked() { # reset failed/blocked tasks back to pending; prints count reset
  [ -f "$BOARD" ] || { echo 0; return; }
  node -e '
    const fs = require("fs");
    const f = process.argv[1];
    const b = JSON.parse(fs.readFileSync(f, "utf8"));
    let n = 0;
    for (const t of b.tasks) {
      if (["failed", "blocked"].includes(t.status)) { t.status = "pending"; t.attempts = 0; n++; }
    }
    if (n > 0) fs.writeFileSync(f, JSON.stringify(b, null, 2) + "\n");
    console.log(n);
  ' "$BOARD" 2>/dev/null || echo 0
}

echo "[orchestrator] loop start repo=$REPO dry_run=$DRY_RUN plan_only=$PLAN_ONLY poll=${POLL_SECS}s"
fails=0
parked_polls=0
while :; do
  # Session-scoped hygiene: close idle/agentless panes left in the devagent
  # herdr session by earlier crashed runs. The session name is the trust
  # boundary — everything in it is automation-spawned; other sessions and
  # non-herdr processes are never touched. Non-fatal if herdr is down.
  SWEEP_OUT="$("${DEVAGENT[@]}" herdr-sweep 2>&1)" || true
  [ -n "$SWEEP_OUT" ] && echo "$SWEEP_OUT" | tail -3

  OPEN="$(board_open_tasks)"
  DONE_COUNT="$(board_done_tasks)"
  TOTAL="$(board_total_tasks)"

  if [ "$OPEN" -eq 0 ] && [ "$TOTAL" -gt 0 ]; then
    if [ "$DONE_COUNT" -eq "$TOTAL" ]; then
      # Infinity cycle: archive the completed board so the next iteration
      # re-bridges scouted queue items and plans a fresh board from the goal.
      TS="$(date +%Y%m%d-%H%M%S)"
      mkdir -p "$REPO/.devagent/archive"
      mv "$BOARD" "$REPO/.devagent/archive/board-$TS.json"
      echo "[cycle] board complete ($DONE_COUNT done); archived to .devagent/archive/board-$TS.json"
      cleanup_merged_worktrees
    else
      # Board is stuck: every task is failed/blocked. Requeue periodically so a
      # transient upstream failure does not park the factory forever.
      if [ "$REQUEUE_AFTER" -gt 0 ]; then
        parked_polls=$(( parked_polls + 1 ))
        echo "[parked] $((TOTAL - DONE_COUNT)) task(s) failed/blocked ($parked_polls/$REQUEUE_AFTER); sleeping ${POLL_SECS}s"
        if [ "$parked_polls" -ge "$REQUEUE_AFTER" ]; then
          N="$(requeue_parked)"
          echo "[requeue] reset $N parked task(s) to pending"
          parked_polls=0
          # Requeue cannot unstick a task whose attempts budget is spent
          # (scheduler.ts only re-selects failed tasks with attempts <
          # maxTaskRetries), so after two fruitless requeue rounds archive
          # the stuck board: the bridge then plans a fresh board from the
          # oldest queued goal, same as the completed-board infinity cycle.
          STUCK="$(board_stuck_tasks)"
          PENDING_COUNT="$(board_pending_tasks)"
          ARCHIVED=0
          if [ "$STUCK" -gt 0 ]; then
            TS="$(date +%Y%m%d-%H%M%S)"
            mkdir -p "$REPO/.devagent/archive"
            mv "$BOARD" "$REPO/.devagent/archive/board-stuck-$TS.json"
            echo "[cycle] board stuck ($STUCK failed/blocked); archived to .devagent/archive/board-stuck-$TS.json"
            ARCHIVED=1
          elif [ "$PENDING_COUNT" -eq "$TOTAL" ] && [ "$TOTAL" -gt 0 ]; then
            # Requeued but the scheduler still cannot dispatch (attempts budget
            # spent): archive so the queue bridge can take over.
            TS="$(date +%Y%m%d-%H%M%S)"
            mkdir -p "$REPO/.devagent/archive"
            mv "$BOARD" "$REPO/.devagent/archive/board-stuck-$TS.json"
            echo "[cycle] board all-pending but undispatchable; archived to .devagent/archive/board-stuck-$TS.json"
            ARCHIVED=1
          fi
        fi
      else
        echo "[parked] $((TOTAL - DONE_COUNT)) task(s) failed/blocked; sleeping ${POLL_SECS}s (requeue disabled)"
      fi
    fi
    if [ "${ARCHIVED:-0}" -eq 1 ]; then
      # Board was archived (stuck): fall through to the queue bridge this
      # cycle instead of sleeping, so the factory re-bridges the oldest
      # queued goal immediately. (Previously the parked block always did
      # `sleep POLL_SECS; continue`, leaving the board absent and the
      # factory idle for a whole poll interval.)
      :
    else
      sleep "$POLL_SECS"
      continue
    fi
  fi
  parked_polls=0

  # Proxy health gate: when the model provider is hard-down (rate-limited
  # empty streams), every worker attempt dies within seconds. Burned attempts
  # do not queue work; they only churn herdr panes and risk the 2-attempt
  # logic-fail path on the old classifier. Probe cheaply; wait out the outage.
  # Operator observability (TASK-mtj08w93): probe outcomes are recorded in
  # .devagent/proxy-state.json so `devagent status --providers` can report
  # the last proxy-probe result and circuit state before dispatch.
  if [ "$DRY_RUN" != "1" ] && [ -n "${ORCHESTRATOR_MODEL_PROBE:-1}" ]; then
    PROBE_OK=0
    PROBE_ATTEMPT=0
    for _ in 1 2 3; do
      PROBE_ATTEMPT=$(( PROBE_ATTEMPT + 1 ))
      if timeout 30 omp -p "OK" --mode json --no-prewalk --no-lsp --no-extensions --model "$(node -e 'console.log(JSON.parse(require("fs").readFileSync("devagent.json","utf8")).model || "")' 2>/dev/null)" 2>/dev/null | grep -q '"text":"OK"'; then
        PROBE_OK=1; break
      fi
      sleep 5
    done
    # Operator observability: record the gate decision in the repo-scoped
    # proxy state that `devagent status --providers` reads. Uses the compiled
    # module so circuit logic stays in one place.
    ( \
      [ -f "$REPO/dist/src/resilience/proxy-state.js" ] || exit 0; \
      node -e '
        const stateMod = process.argv[1];
        const repo = process.argv[2];
        const ok = process.argv[3];
        const detail = process.argv[4];
        import("file://" + stateMod).then((m) => { m.recordProxyProbe(repo, { ok: ok === "1", ...(detail ? { detail } : {}) }); }).catch(() => {});
      ' "$REPO/dist/src/resilience/proxy-state.js" "$REPO" "$PROBE_OK" "attempt $PROBE_ATTEMPT/3" 2>/dev/null || true \
    )
    if [ "$PROBE_OK" -ne 1 ]; then
      echo "[proxy-down] all 3 probes failed; sleeping ${POLL_SECS}s before retry"
      sleep "$POLL_SECS"
      continue
    fi
  fi

  GOAL="$(resolve_goal)"
  # Autonomous chain: scouted queue items become the board when none exists
  # yet, or when the existing board cannot dispatch anything (stale/failed
  # board would otherwise wedge the factory while the queue fills up).
  if [ "$DRY_RUN" != "1" ]; then
    if [ ! -f "$BOARD" ]; then
      if BRIDGE_OUT="$("${DEVAGENT[@]}" queue bridge --repo "$REPO" 2>&1)"; then
        echo "$BRIDGE_OUT" | tail -2
      fi
    elif [ "$OPEN" -eq 0 ] && [ "$(board_pending_tasks)" -eq 0 ] && [ "$parked_polls" -ge "$REQUEUE_AFTER" ]; then
      echo "[bridge] board has no dispatchable tasks; archiving to re-bridge queue"
      TS="$(date +%Y%m%d-%H%M%S)"
      mkdir -p "$REPO/.devagent/archive"
      mv "$BOARD" "$REPO/.devagent/archive/board-stuck-$TS.json"
      if BRIDGE_OUT="$("${DEVAGENT[@]}" queue bridge --repo "$REPO" 2>&1)"; then
        echo "$BRIDGE_OUT" | tail -2
      fi
    fi
  fi
  ARGS=(orchestrate --repo "$REPO" --goal "$GOAL")
  if [ -f "$BOARD" ]; then ARGS+=(--resume); fi
  [ "$PLAN_ONLY" = "1" ] && ARGS+=(--plan-only)

  echo "=== orchestrator cycle open=$OPEN done=$DONE_COUNT resume=$([ -f "$BOARD" ] && echo 1 || echo 0) ==="

  if [ "$DRY_RUN" = "1" ]; then
    echo "[dry-run] would run: ${DEVAGENT[*]} ${ARGS[*]}"
    fails=0
    sleep 1
    continue
  fi

  set +e
  OUT="$("${DEVAGENT[@]}" "${ARGS[@]}" 2>&1)"
  RC=$?
  set -e
  echo "$OUT" | tail -5

  NEXT_OPEN="$(board_open_tasks)"
  if [ "$RC" -eq 0 ] || [ "$NEXT_OPEN" -lt "$OPEN" ]; then
    echo "[ok] cycle complete (open $OPEN -> $NEXT_OPEN)"
    fails=0
  else
    fails=$(( fails + 1 ))
    echo "[fail] orchestrator cycle ($fails/$MAX_FAILS)"
    if [ "$fails" -ge "$MAX_FAILS" ]; then
      echo "circuit breaker: $fails consecutive failures — halting orchestrator"
      exit 1
    fi
  fi

  sleep 5
done
