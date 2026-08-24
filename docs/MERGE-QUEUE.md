# Merge queue — loops 43-51

*Verified 2026-08-24 via `git merge-tree --write-tree` simulation: every pair
below merges clean, including cross-stack pairs.*

Merge in this order (each PR's base retargets to main automatically once its
parent lands; verified conflict-free including the #7 -> #12 extension, 2026-08-24 loop 53):

| Order | PR | Contents | Base |
|---|---|---|---|
| 1 | #4 | `devagent_answer` MCP tool + pendingQuestions | main |
| 2 | #6 | token-gated `POST /api/answer` HTTP endpoint | #4 |
| 3 | #3 | recovery contracts (`--max-recoveries`) | main |
| 4 | #7 | `orchestrate --plan-only` | #3 |
| 5 | #9 | run ledger (persisted audit verdicts) | main |
| 6 | #10 | `devagent_ledger` MCP tool | #9 |
| 7 | #11 | audit history in `devagent project` | #10 |
| 8 | #12 | repeat-gap escalation to recovery contracts | #7 |
| any | #8 | orchestration guide docs | main |

After all land: 253+ tests green on the combined tree (verified per-branch;
re-run `npm test` after each merge).

## Unblocked backlog (research doc priorities)

Once #3/#7 are in, implement from [openhands-sweagent.md](research/openhands-sweagent.md):

~~L2 — consecutive-failure escalation~~ — shipped as #12
2. L1 — board-level spend/wave budget ceiling (cli + scheduler)
3. L4 — converge board/log/ledger onto the ledger stream
