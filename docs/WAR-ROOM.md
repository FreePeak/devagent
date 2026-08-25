# War Room Mode

**One command, one goal, run forever until it ships.** Built for new products and hackathons: you bring an idea — even a vague one — and the war room researches it, writes the spec it needs, then loops DevAgent implementation iterations until every acceptance criterion has evidence.

```bash
npm run warroom -- --goal "Build a CLI that turns podcast feeds into daily digests" [--repo /path/to/target]
```

## Lifecycle

```
┌─────────────────────────────────────────────────────────────────┐
│ PHASE 0  INIT      goal.txt written; state dir .warroom/        │
│ PHASE 1  RESEARCH  domain scan → research.md (once, idempotent) │
│ PHASE 2  SPEC      draft SPEC.md → critique pass → revise       │
│                    …repeat until verdict CLEAR or passes spent  │
│ PHASE 3  LOOP      pick next unchecked AC from SPEC.md          │
│                    → devagent task --auto-pr (isolated worktree,│
│                    gates G1-G4, PR) → tick AC → JUDGE           │
│ PHASE 4  JUDGE     every AC evidenced? repo suite green?        │
│                    ├─ DONE → final npm test → exit 0            │
│                    └─ NEXT → top gap fed into next iteration    │
└─────────────────────────────────────────────────────────────────┘
   guards: circuit breaker · starvation gate · optional time budget
```

The loop runs **forever** by default (`WARMROOM_MAX_ITERS=0`) — "until the goal is achieved". Judges are deliberately conservative: `DONE` requires cited evidence per criterion plus a green suite; anything less produces the next concrete gap instead of a premature exit.

## State layout (`<repo>/.warroom/`)

| File | Purpose |
|---|---|
| `goal.txt` | Original goal, verbatim |
| `research.md` | Phase 1 domain scan: prior art, competitors, risks, what "done" means here |
| `SPEC.md` | The living spec: overview, non-goals, and the machine-trackable `- [ ] AC-n` checklist |
| `spec-review.md` | Critique-pass log (ambiguities, untestable criteria, verdicts) |
| `progress.jsonl` | Iteration ledger — same schema discipline as `.selfbuild/ledger.jsonl` |
| `logs/iter-N.log` | Full output per iteration |

All phases are idempotent: kill the process anywhere, re-run the same command, and it resumes (research/spec skipped when present; unchecked ACs continue).

## Knobs (env)

| Variable | Default | Meaning |
|---|---|---|
| `WARMROOM_WORKER` | `claude-code` | Implementation worker for `devagent task` (`opencode` supported) |
| `WARMROOM_MODEL` | *(unset)* | Model override passed through (`--model`, e.g. `opencode-go/ox-alpha-free`) |
| `WARMROOM_AGENT_BIN` | `claude -p` | Agent CLI used for research/spec/judge phases (array-expanded; e.g. `opencode run`) |
| `WARMROOM_MAX_ITERS` | `0` (∞) | Hard cap on implement iterations |
| `WARMROOM_MAX_HOURS` | `0` (∞) | Wall-clock budget |
| `WARMROOM_MAX_CONSECUTIVE_FAILURES` | `3` | Circuit breaker |
| `WARMROOM_SPEC_PASSES` | `3` | Max spec refine→critique rounds before proceeding with warnings |
| `WARMROOM_DRY_RUN` | `0` | Exercise all plumbing with stub agents/tasks, no side effects |

## Guardrails (inherited from the selfbuild loop)

- **Circuit breaker** — N consecutive failed task iterations halt the run.
- **Starvation gate** — halts when the last K ledger entries contain no productive status (`ok | pr-open | merged | pushed | judge-done | spec-refined`). Status vocabulary drift bug fixed in the shared ancestor logic.
- **Evidence over claims** — the judge must cite file paths/tests per criterion; worker self-reports are treated as untrusted, matching the orchestrator's audit philosophy.

## Relationship to `npm run selfbuild`

Selfbuild picks its own backlog item from the PRD each loop (product evolving itself). War room points the same machinery at **your** goal (product built for you). Both share the task pipeline, PR delivery, and guard semantics.
