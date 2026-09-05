# simple-smoke fixture

Hermetic stub used by `devagent init --smoke` (issue #144 R2).

The runtime path is a **stub** (no live provider, no network): config written →
fixture dispatched → gates/audit stub passed → `done`. A receipt is written to
`.devagent/smoke/last.json` in the target repo.

Unit coverage lives in `test/init.test.ts` (temp repo + stub PATH).
