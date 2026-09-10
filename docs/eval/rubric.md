# DevAgent shipped-artifact rubric (FR-VAL-05, issue #293)

Machine-read by `devagent eval score`, which sends this rubric plus one PR's
evidence to the configured worker model (the judge) and records the verdict as
an `eval-score` ledger row. Each criterion scores 0 to its own weight; the
weights sum to 100. No evidence for a criterion means 0 for that criterion, not
a generous read of the diff.

This file is the rubric of record (tracked, so scores stay re-readable against
the criteria that produced them); a repo may shadow it at
`<repo>/.devagent/eval/rubric.md`, and `--rubric <path>` shadows both.

One line per criterion, exactly `- [<weight>] <id>: <what to score>`.
**Bump `version:` whenever a criterion id or weight changes**: the version is
recorded with every score and the drift ratchet (`devagent eval drift`,
`devagent ledger --clusters`) only ever compares scores that share it, so an
edit to this rubric cannot silently redefine the baseline.

version: 1

- [30] requirement-coverage: The diff implements every acceptance criterion of the ticket or issue — not a subset, not an adjacent idea. Score proportionally to what a reviewer could verify from the linked issue; a stub, a placeholder path, or a TODO that defers a named criterion is unmet by definition.
- [25] test-evidence: The PR proves the behavior it claims: a test that fails without the change and passes with it, or a quoted run of the changed path (command plus observed output). A green suite that never exercises the new code scores below half; no runnable evidence at all scores 0.
- [20] diff-relevance: Every hunk serves the stated goal. Drive-by refactors, reformatting churn, renamed-but-unchanged files, and unrelated cleanup subtract from this criterion.
- [15] prd-currency: docs/PRD.md reflects the change — the affected status claims, architecture notes and roadmap rows are updated and the *Last updated* footer is bumped in the same PR.
- [10] no-regression-claim: The PR names what could break and the evidence that it did not: caller/contract sweep, migration reversibility, CI flake explained. An unverified "nothing else changed" assertion scores 0; honesty about an unchecked edge scores higher than a claim.
