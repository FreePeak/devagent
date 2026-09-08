#!/usr/bin/env bash
# DevAgent PRD curator loop driver.
# Executes ONE research-and-curation pass: analyze repo state and recent
# delivery history, then update the PRD roadmap backlog (section 17) and open
# questions (section 18) so the self-build loop always has fresh, accurate
# goals to pull from. Changes ship as a PR (push policy: docs may go direct,
# but PR keeps the diff reviewable and lets autoMerge handle it).
# Protocol: docs/SELF-BUILD-LOOP.md (PRD curation section)
set -euo pipefail

REPO="${SELFBUILD_REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
STATE="$REPO/.selfbuild"
CURLOG="$STATE/curation"
DRY_RUN="${SELFBUILD_DRY_RUN:-0}"
# PO role runs omp per the operator tool mapping (2026-09-02): all agents on omp.
CLAUDE_BIN="${SELFBUILD_CLAUDE:-omp -p --mode json --no-prewalk --no-lsp --no-extensions --model onegw/free}"
# Forward the configured model so the curator doesn't fall back to the
# settings.json default (same unrecognized_model failure mode as the
# scout worker path fixed in 3c67178 / PR #53).
if [ -z "${SELFBUILD_CLAUDE:-}" ] && [ -n "${SELFBUILD_MODEL:-}" ]; then
  CLAUDE_BIN="$CLAUDE_BIN --model $SELFBUILD_MODEL"
fi

mkdir -p "$CURLOG/research"
cd "$REPO"

STAMP="$(date -u +%FT%TZ)"
DAY="$(date -u +%Y%m%d)"

# Always curate from an up-to-date main.
git pull --ff-only || echo "[sync] skipped (pull failed)"

echo "=== prd curation start $STAMP ==="

# Operator preflight (Q40): probe the provider before spending the cycle on
# the curator agent. On failure the operator-degraded ledger row is written
# and this cycle exits nonzero - visible degradation, not a silent noop.
if [ "$DRY_RUN" != 1 ] && ! npx tsx "$REPO/src/cli.ts" preflight --role prd-curator --repo "$REPO"; then
  echo "[preflight] provider degraded - skipping curation this cycle (ledger row written)"
  exit 1
fi


{
  # Advisory PRD-coverage audit (Q15, PRD:912). Runs after the sync above so the
  # scan sees the freshly pulled docs/prds/, and its warnings land in this
  # cycle's log for the next scout cycle to read. Advisory-only: `prd-audit`
  # never enqueues and always exits 0, and the `|| echo` absorbs a crashed CLI,
  # so a broken audit can never stall curation under `set -e`.
  npx tsx "$REPO/src/cli.ts" prd-audit --repo "$REPO" \
    || echo "[prd-audit] skipped (audit errored; advisory only - cycle continues)"

  # Phase 1-3: research, analyze, propose (single agent pass).
  cat > "$CURLOG/research/curation-$DAY.md" <<EOF || true
# Curation $STAMP
prompt: reconcile GitHub issues + refresh PRD state sections
EOF

  $CLAUDE_BIN "You are the tracker curator for the DevAgent repository ($(pwd)).
Produce ONE curation pass. The task tracker is the GitHub issue queue (labels:
selfbuild for loop-consumable items, priority:P0 > P1 > P2 for order). The
docs/PRD.md is the state document — it reflects what the repo IS, never a
backlog of items to build. Work read-only except for docs/PRD.md and GitHub
issue state (gh issue create/close/edit/comment).

Research inputs (inspect all):
1. git log --oneline origin/main -30 and bodies of recently merged PRs (gh pr list --state merged --limit 10; gh pr view N) to learn what shipped.
2. The tracker: gh issue list --label selfbuild --state open --limit 50 --json number,title,labels and recently closed issues.
3. docs/PRD.md — sections 17-21 state claims and 18 open questions.
4. .selfbuild/lessons.md if present, plus any failed-loop evidence in .selfbuild/ledger.jsonl.
5. Repo reality check: skim src/, test/, package.json to confirm claimed capabilities actually exist.

Then do THREE things, in this order:

1. Reconcile the tracker (mutating gh issue state is expected):
   - For every open selfbuild-labeled issue whose work verifiably shipped
     (merged PR evidence, capability present in repo, or an 'ok' ledger row),
     close it: gh issue close N --comment '<evidence: PR #, commit, or test>'.
   - File at most 3 NEW issues for concrete, currently-missing, high-value
     work learned from recent shipping (defects hit, friction observed,
     capability gaps). Order matters: create them in priority sequence, label
     each 'selfbuild' plus exactly one priority:P0|P1|P2 label, one short
     rationale line each. Never file what an open issue already covers.
   - Re-prioritize: every open selfbuild issue carries exactly one
     priority:P* label matching current impact x tractability (edit labels as
     needed).
2. Strike shipped backlog lines still standing in docs/PRD.md section 17
   (wrap in ~~ strikethrough) so the state document stops offering shipped work.
3. Refresh docs/PRD.md as a STATE document, and ONLY these parts:
   - Section 17: move shipped items into dated completion notes under the
     right phase heading; refresh the status blockquote to match reality.
     Do NOT add a static backlog list — open work lives in GitHub issues and
     the roadmap references them by number instead.
   - Section 18: drop questions that recent commits answered; you may add at
     most 2 new questions with owner and needed-by phase.
   - Update the '*Last updated:*' footer to today with a one-line summary.
Keep the existing voice and formatting. Total docs/PRD.md diff must stay
   under 60 lines, EXCEPT the operator-owned migration scope: docs/PRD.md
   section 22 (Go migration addendum) and its issues #190-#207 are a decision
   record the operator owns — never compress, strike, reword, or
   re-prioritize them; go-migration-labeled issues are deliberately NOT
   selfbuild-labeled and are out of reconciliation scope (do not close,
   relabel, or comment on them).
Do NOT touch any other file. Do NOT commit, stage, push, or open a PR - leave
docs/PRD.md edits in the working tree (the script publishes them).
Finish by printing exactly one first line: either 'CURATION: changed' or
'CURATION: noop', followed by up to 5 bullets summarizing the reasoning." \
    > "$CURLOG/research/curation-$DAY.out" 2>&1 \
    || echo "[research] curator agent errored (see $CURLOG/research/curation-$DAY.out)"

  head -6 "$CURLOG/research/curation-$DAY.out" || true

  if [ "$DRY_RUN" = 1 ]; then
    echo "[dry-run] would publish PRD diff:"; git diff --stat -- docs/PRD.md
    exit 0
  fi

  # Issue-side mutations are already live on GitHub (the agent ran gh issue
  # commands); the PR publishes only the docs/PRD.md state diff. Distinguish
  # three outcomes: full noop (PRD quiet + agent says noop), tracker-only
  # (PRD quiet but issues were created/closed), and the normal PR path.
  if git diff --quiet -- docs/PRD.md; then
    if grep -q '^CURATION: noop' "$CURLOG/research/curation-$DAY.out" 2>/dev/null; then
      printf '{"ts":"%s","status":"noop"}\n' "$STAMP" >> "$CURLOG/log.jsonl"
      echo "[noop] tracker and PRD both current"
    else
      printf '{"ts":"%s","status":"ok-tracker-only","note":"issue mutations without PRD diff"}\n' "$STAMP" >> "$CURLOG/log.jsonl"
      echo "[ok] tracker reconciled (no PRD diff to publish)"
    fi
    git checkout main 2>/dev/null || true
    exit 0
  fi

  # Guardrail: the agent must only have touched the PRD.
  if ! git diff --quiet; then
    other=$(git diff --name-only | grep -v '^docs/PRD.md$' || true)
    if [ -n "$other" ]; then
      echo "[guard] curator touched non-PRD files, aborting: $other"
      git checkout -- $other
      printf '{"ts":"%s","status":"guard-abort","files":"%s"}\n' "$STAMP" "$(echo $other | tr '\n' ' ')" >> "$CURLOG/log.jsonl"
      exit 1
    fi
  fi

  # Phase 4-7: validate, plan n/a, implement n/a, push as PR.
  BRANCH="docs/prd-curation-$DAY-$(date -u +%H%M)"
  git checkout -b "$BRANCH"
  git add docs/PRD.md
  git commit -m "Docs: tracker curation $DAY - reconcile issues + refresh PRD state"
  git push -u origin "$BRANCH"
  gh pr create --title "Docs: PRD curation $DAY" \
    --body "$(head -20 "$CURLOG/research/curation-$DAY.out")" \
    || { echo "[push] pr create failed"; exit 1; }
  git checkout main


  printf '{"ts":"%s","status":"ok","branch":"%s"}\n' "$STAMP" "$BRANCH" >> "$CURLOG/log.jsonl"
  echo "[ok] curation PR opened ($BRANCH)"
} 2>&1 | tee -a "$CURLOG/curation-$DAY.log"

echo "=== prd curation end $(date -u +%FT%TZ) ==="
