#!/usr/bin/env bash
# DevAgent War Room: goal-driven infinity loop (docs/WAR-ROOM.md).
# One vague idea in → researched, specced (refine-until-clear), then endless
# devagent-task iterations until every acceptance criterion has evidence.
# Built for new products and hackathons. Guard semantics mirror the selfbuild
# loop driver (circuit breaker + starvation gate), with a conservative judge
# as the only exit besides failure guards.
#
# Usage:
#   npm run warroom -- --goal "<what to build>" [--repo /path/to/target]
#
# Knobs (env): WARMROOM_WORKER WARMROOM_MODEL WARMROOM_AGENT_BIN
#   WARMROOM_MAX_ITERS (0=∞) WARMROOM_MAX_HOURS (0=∞)
#   WARMROOM_MAX_CONSECUTIVE_FAILURES WARMROOM_SPEC_PASSES WARMROOM_DRY_RUN
set -euo pipefail

GOAL=""
REPO_OVERRIDE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --goal) GOAL="${2:?--goal requires text}"; shift 2 ;;
    --repo) REPO_OVERRIDE="${2:?}"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[ -n "$GOAL" ] || { echo "--goal is required" >&2; exit 2; }

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO="${REPO_OVERRIDE:-$(cd "$SCRIPT_DIR/.." && pwd)}"
STATE="$REPO/.warroom"
WORKER="${WARMROOM_WORKER:-omp}"
MODEL="${WARMROOM_MODEL:-}"
MAX_ITERS="${WARMROOM_MAX_ITERS:-0}"
MAX_FAILS="${WARMROOM_MAX_CONSECUTIVE_FAILURES:-3}"
SPEC_PASSES="${WARMROOM_SPEC_PASSES:-3}"
MAX_HOURS="${WARMROOM_MAX_HOURS:-0}"
DRY_RUN="${WARMROOM_DRY_RUN:-0}"
LEDGER="$STATE/progress.jsonl"

read -ra AGENT <<< "${WARMROOM_AGENT_BIN:-claude -p}"
DEVAGENT=(npx tsx "$REPO/src/cli.ts")

mkdir -p "$STATE/logs"
cd "$REPO"

now_iso() { date -u +%FT%TZ; }
record() { # record <status> <note>
  printf '{"phase":"%s","ts":"%s","status":"%s","note":"%s"}\n' \
    "${WARROOM_PHASE:-loop}" "$(now_iso)" "$1" \
    "$(printf '%s' "${2:-}" | tr -d '"' | tr '\n' ' ' | cut -c1-200)" >> "$LEDGER"
}

deadline_s() { [ "$MAX_HOURS" -gt 0 ] && echo $(( $(date +%s) + MAX_HOURS * 3600 )) || echo 0; }
DEADLINE=$(deadline_s)
expired() { [ "$DEADLINE" -gt 0 ] && [ "$(date +%s)" -ge "$DEADLINE" ]; }

# Starvation gate: halt when recent iterations ship nothing. Productive =
# shipped or handed to review (statuses shared with the Orca-driven ledger).
starved() {
  [ -f "$LEDGER" ] || return 1
  local count
  count=$(awk -v lim="${STARVATION_LIMIT:-5}" '
    { lines[NR] = $0 }
    END {
      c = 0
      for (i = NR; i >= 1; i--) {
        if (lines[i] ~ /"status":"(ok|pr-open|merged|pushed|judge-done|spec-refined)"/) break
        if (++c >= lim) break
      }
      print c
    }' "$LEDGER")
  [ "${count:-0}" -ge "${STARVATION_LIMIT:-5}" ]
}

agent_run() { # agent_run <outfile> <prompt>
  if [ "$DRY_RUN" = 1 ]; then
    printf '# dry-run stub\nGoal: (dry-run) %s\n' "$(head -c 120 <<<"$GOAL")" > "$1"
    return 0
  fi
  "${AGENT[@]}" "$2" > "$1"
}

# ---- Phase 0: init -----------------------------------------------------------
if [ -f "$STATE/goal.txt" ] && ! diff -q <(cat "$STATE/goal.txt") <(printf '%s' "$GOAL") >/dev/null 2>&1; then
  echo "[init] .warroom/goal.txt holds a DIFFERENT goal — refusing to mix missions." >&2
  exit 2
fi
printf '%s' "$GOAL" > "$STATE/goal.txt"
git pull --ff-only >/dev/null 2>&1 || echo "[sync] skipped (pull failed)"
echo "[init] war room open. goal: $(head -c 120 <<<"$GOAL")"

# ---- Phase 1: research -------------------------------------------------------
WARROOM_PHASE=research
if [ ! -s "$STATE/research.md" ]; then
  echo "[research] scanning the domain…"
  agent_run "$STATE/research.md" "You are the research phase of a war-room build.
Product goal (may be abstract): $GOAL
Produce compact markdown (<500 words): closest prior art / competitors with URLs;
the 3 riskiest unknowns; what a hackathon-grade 'done' means for THIS goal;
recommended minimal scope cut. Output research only." \
    || { echo "[research] failed — continuing without domain scan"; record research-failed ""; }
else
  echo "[research] cached research.md found"
fi

# ---- Phase 2: spec, refine until clear ---------------------------------------
WARROOM_PHASE=spec
SPEC="$STATE/SPEC.md"
pass=1; verdict_clear=0
while [ "$pass" -le "$SPEC_PASSES" ]; do
  if [ "$pass" -eq 1 ] && [ ! -s "$SPEC" ]; then
    echo "[spec] drafting SPEC.md (pass 1/$SPEC_PASSES)…"
    agent_run "$SPEC" "You are the spec phase of a war-room build toward: $GOAL
Research: $(head -c 1500 "$STATE/research.md" 2>/dev/null || echo none)
Write SPEC.md content in markdown: ## Overview; ## Non-goals; ## Acceptance criteria
as a checklist of SMALL independently-verifiable items, each line formatted exactly
'- [ ] AC-<n>: <criterion>' ordered so early items make the product runnable/demoable,
then ## Notes (architecture decisions worth pinning). Every criterion must be
verifiable from the repository (tests, CLI behavior, file existence)." \
      && record spec-refined "drafted (pass $pass)"
  elif [ "$pass" -gt 1 ]; then
    echo "[spec] revising SPEC.md (pass $pass/$SPEC_PASSES)…"
    agent_run "$SPEC.new" "Revise this spec to resolve the critique below. Keep the exact
'- [ ] AC-n:' checklist format; change only what the critique demands.
=== CURRENT SPEC ===
$(cat "$SPEC")
=== CRITIQUE ===
$(tail -c 1500 "$STATE/spec-review.md" 2>/dev/null || echo none)" \
      && mv "$SPEC.new" "$SPEC" && record spec-refined "revised (pass $pass)"
  fi
  [ -s "$SPEC" ] || { echo "[spec] draft failed; retrying next pass"; pass=$((pass+1)); continue; }

  echo "[spec] critique pass ${pass}…"
  agent_run "$STATE/spec-review.md" "Strictly review this spec for a war-room build.
Spec:
$(cat "$SPEC")
List ambiguities, untestable criteria, and missing non-goals as bullet lines.
FIRST LINE must be exactly 'VERDICT: CLEAR' if an engineer could build each item
without asking questions, otherwise 'VERDICT: ISSUES'." \
    || record spec-warn "critique call failed (pass $pass)"

  if head -1 "$STATE/spec-review.md" 2>/dev/null | grep -q 'VERDICT: CLEAR'; then
    verdict_clear=1
    echo "[spec] CLEAR after $pass pass(es)."
    break
  fi
  pass=$((pass+1))
done
[ "$verdict_clear" = 1 ] || { echo "[spec] passes spent with issues outstanding — proceeding (hackathon pragmatism)"; record spec-warn "proceeding with unresolved critique"; }

# ---- Phases 3-4: implement loop + judge --------------------------------------
WARROOM_PHASE=loop
fails=0; N=0
while :; do
  N=$((N+1))
  LOG="$STATE/logs/iter-$N.log"
  {
    echo "=== war-room iter $N start $(now_iso) ==="

    if starved; then
      echo "[starvation] ${STARVATION_LIMIT:-5} consecutive non-productive entries — halting"
      exit 1
    fi
    if expired; then
      echo "[budget] WARMROOM_MAX_HOURS=$MAX_HOURS elapsed — halting"
      exit 1
    fi
    [ "$MAX_ITERS" -gt 0 ] && [ "$N" -gt "$MAX_ITERS" ] && { echo "max iterations reached"; exit 1; }

    GAP_CTX=""
    AC_LINE_NO=$(grep -m1 -nE '^[[:space:]]*- \[ \]' "$SPEC" | cut -d: -f1 || true)
    if [ -n "$AC_LINE_NO" ]; then
      AC_TEXT=$(sed -n "${AC_LINE_NO}p" "$SPEC" | sed -E 's/^[[:space:]]*- \[ \][[:space:]]*//')
      AC_ID=$(printf '%s' "$AC_TEXT" | grep -oE 'AC-[0-9]+' | head -1 || true)
      echo "[implement] next criterion: ${AC_ID:-line $AC_LINE_NO}: $(head -c 100 <<<"$AC_TEXT")"
    else
      echo "[judge] all boxes ticked — verifying against reality…"
      JUDGE_OUT="$STATE/logs/judge-$N.out"
      if [ "$DRY_RUN" = 1 ]; then printf 'JUDGE: DONE\n' > "$JUDGE_OUT"; else
        "${AGENT[@]}" "War-room judge. Goal: $GOAL
Spec checklist:
$(cat "$SPEC")
For EACH checked criterion decide whether the CURRENT repository actually
demonstrates it; cite file paths or tests as evidence; treat claims without
evidence as unmet. If everything is evidenced AND you believe the full test
suite passes, output exactly 'JUDGE: DONE'. Otherwise output exactly
'JUDGE: NEXT <single most important gap>'." > "$JUDGE_OUT"
      fi
      if grep -q 'JUDGE: DONE' "$JUDGE_OUT"; then
        if [ "$DRY_RUN" = 1 ]; then echo "[dry-run] final npm test skipped"; record judge-done "(dry-run)"; echo "WAR ROOM COMPLETE"; exit 0; fi
        if npm test; then
          record judge-done "all criteria evidenced; suite green"
          echo "WAR ROOM COMPLETE — goal achieved after $((N-1)) implementation iterations."
          exit 0
        fi
        echo "[judge] suite failed despite DONE verdict — reopening loop"
        record judge-failed-tests "DONE verdict but suite red"
        continue
      fi
      GAP_CTX="$(grep -m1 'JUDGE: NEXT' "$JUDGE_OUT" | cut -c12-400)"
      AC_TEXT="Close this judge-identified gap first: ${GAP_CTX:-unspecified}."
      AC_ID="gap-$N"
      echo "[judge] next gap: $(head -c 120 <<<"$GAP_CTX")"
    fi

    PROMPT="You are iteration $N of a war-room build toward this product goal: $(cat "$STATE/goal.txt")
Read .warroom/SPEC.md fully and honor its non-goals and notes.${GAP_CTX:+
A previous review identified this top gap: $GAP_CTX.}
THIS ITERATION implements exactly one thing: $AC_TEXT
Rules: smallest change that satisfies it; include or adjust tests; the full
existing test suite must stay green; never modify anything under .warroom/."

    if [ "$DRY_RUN" = 1 ]; then
      echo "[dry-run] task skipped"; record ok "(dry-run) $AC_ID"
      # Mirror production bookkeeping so the dry run exercises the same
      # progress mechanics (a missing tick here loops forever on AC-1).
      if [ -n "$AC_LINE_NO" ]; then
        awk -v n="$AC_LINE_NO" 'NR==n{sub(/- \[ \]/,"- [x]")}1' "$SPEC" > "$SPEC.ticked" \
          && mv "$SPEC.ticked" "$SPEC"
      fi
      echo "=== war-room iter $N end $(now_iso) ==="
    else
      TASK_ARGS=(task --prompt "$PROMPT" --repo "$REPO" --worker "$WORKER")
      [ -n "$MODEL" ] && TASK_ARGS+=(--model "$MODEL")
      TASK_ARGS+=(--auto-pr)
      if "${DEVAGENT[@]}" "${TASK_ARGS[@]}"; then
        record ok "$AC_ID shipped (PR opened)"
        awk -v n="$AC_LINE_NO" 'NR==n{sub(/- \[ \]/,"- [x]")}1' "$SPEC" > "$SPEC.ticked" \
          && mv "$SPEC.ticked" "$SPEC"
        fails=0
      else
        echo "[implement] task failed for $AC_ID"
        record task-failed "$AC_ID"
        fails=$((fails+1))
        [ "$fails" -ge "$MAX_FAILS" ] && { echo "circuit breaker: $fails consecutive failures"; exit 1; }
        continue
      fi
    fi
    echo "=== war-room iter $N end $(now_iso) ==="
  } >> "$LOG" 2>&1
  tail -3 "$LOG"
done