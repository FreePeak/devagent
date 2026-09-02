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
# PO role runs pi per the operator tool mapping (2026-09-02): scout=omp,
# PO=pi, dev=pi, reviewer=pi — claude-code is not the default tool.
CLAUDE_BIN="${SELFBUILD_CLAUDE:-pi --mode json --no-extensions --model omniroute/dev}"
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

{
  # Phase 1-3: research, analyze, propose (single agent pass).
  cat > "$CURLOG/research/curation-$DAY.md" <<EOF || true
# Curation $STAMP
prompt: analyze repo + delivery history; refresh PRD section 17/18
EOF

  $CLAUDE_BIN "You are the PRD curator for the DevAgent repository ($(pwd)).
Produce ONE curation pass. Work read-only except for docs/PRD.md.

Research inputs (inspect all):
1. git log --oneline origin/main -30 and bodies of recently merged PRs (gh pr list --state merged --limit 10; gh pr view N) to learn what shipped.
2. docs/PRD.md sections 17 (Roadmap) and 18 (Open Questions).
3. .selfbuild/lessons.md if present, plus any failed-loop evidence in .selfbuild/ledger.jsonl.
4. Repo reality check: skim src/, test/, package.json to confirm claimed capabilities actually exist.

Then edit docs/PRD.md, and ONLY these parts:
- Section 17: items that shipped get moved out of 'Phase 4 - Expansion' into a dated completion note under the right phase heading. Refresh the Phase 4 backlog so it lists 5-8 concrete, currently-missing, high-value items, each one line with a short rationale. Derive new items from what you learned shipping recently (defects hit, friction observed, capability gaps).
- Section 18: drop questions that recent commits answered; you may add at most 2 new questions with owner and needed-by phase.
Keep the existing voice and formatting. Total diff must stay under 60 lines.

Do NOT touch any other file. Do NOT commit, stage, push, or open a PR - leave edits in the working tree.
Finish by printing exactly one first line: either 'CURATION: changed' or 'CURATION: noop', followed by up to 5 bullets summarizing the reasoning." \
    > "$CURLOG/research/curation-$DAY.out" 2>&1 \
    || echo "[research] curator agent errored (see $CURLOG/research/curation-$DAY.out)"

  head -6 "$CURLOG/research/curation-$DAY.out" || true

  if [ "$DRY_RUN" = 1 ]; then
    echo "[dry-run] would publish PRD diff:"; git diff --stat -- docs/PRD.md
    exit 0
  fi

  if git diff --quiet -- docs/PRD.md; then
    printf '{"ts":"%s","status":"noop"}\n' "$STAMP" >> "$CURLOG/log.jsonl"
    echo "[noop] PRD already accurate"
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
  git commit -m "Docs: PRD curation $DAY - refresh roadmap backlog from delivery history"
  git push -u origin "$BRANCH"
  gh pr create --title "Docs: PRD curation $DAY" \
    --body "$(head -20 "$CURLOG/research/curation-$DAY.out")" \
    || { echo "[push] pr create failed"; exit 1; }
  git checkout main


  printf '{"ts":"%s","status":"ok","branch":"%s"}\n' "$STAMP" "$BRANCH" >> "$CURLOG/log.jsonl"
  echo "[ok] curation PR opened ($BRANCH)"
} 2>&1 | tee -a "$CURLOG/curation-$DAY.log"

echo "=== prd curation end $(date -u +%FT%TZ) ==="
