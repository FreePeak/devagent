package loopdriver

// PRD intake (issue #370): the operator's open `- [ ]` items in docs/PRD.md
// become queue rows the same iteration can claim.
//
// This is the sanctioned exception to "the PRD is a state document, never a
// backlog" (docs/SELF-BUILD-LOOP.md, 2026-09-07 policy): the lane has been
// empty since that policy landed, and an empty lane is precisely what makes
// the driver spend its runtime repairing itself instead of shipping product
// (issue #355). A checkbox is not prose — it is the operator typing "build
// this" — and intake is deterministic (no LLM, no network), so it costs one
// file read per iteration.
//
// Placement is load-bearing: intake runs after the PRD-currency gate (which
// guarantees the file matches a commit, so a mid-edit draft is never read as
// intent) and before pickIssue/claimQueueTask (so a fresh item is built by
// this iteration, not the next one). A failure here is logged and never
// fails the iteration: intake is a work source, not a dependency.

import (
	"fmt"
	"io"
	"os"

	"github.com/FreePeak/devagent/internal/prdintake"
)

// runPrdIntake ingests docs/PRD.md into the queue and breadcrumbs the result
// into the iteration log. Returns the number of rows newly queued.
func (d *driver) runPrdIntake(n int, logF io.Writer) int {
	cfg := d.cfg
	if cfg.PRDIntake != nil && !*cfg.PRDIntake {
		return 0
	}
	ingest := cfg.Intake
	if ingest == nil {
		ingest = prdintake.Ingest
	}
	report, err := ingest(prdintake.Options{
		RepoPath: cfg.Repo,
		MaxItems: cfg.PRDIntakeMax,
		// A dry run rehearses the verdict: parse and report, write nothing.
		DryRun: cfg.DryRun,
	})
	if err != nil {
		// A repo without docs/PRD.md, or an unreadable one, is a work source
		// that is simply unavailable — never an iteration failure.
		if os.IsNotExist(err) {
			_, _ = fmt.Fprintln(logF, "[prd-intake] no docs/PRD.md to ingest — continuing")
		} else {
			_, _ = fmt.Fprintf(logF, "[prd-intake] skipped: %v\n", err)
		}
		return 0
	}
	if len(report.Queued) == 0 {
		if report.Open > 0 {
			// Every open item is already a queue row; saying so keeps
			// "nothing queued" from reading as "nothing found".
			_, _ = fmt.Fprintf(logF, "[prd-intake] %d open PRD item(s) already in the queue\n", report.Open)
		}
		return 0
	}
	verb := "queued"
	if cfg.DryRun {
		verb = "would queue"
	}
	for _, it := range report.Queued {
		_, _ = fmt.Fprintf(logF, "[prd-intake] %s %s: %s (docs/PRD.md:%d)\n", verb, it.ID, it.Title, it.Line)
	}
	d.phase(n, "prd-intake", fmt.Sprintf("%s %d operator item(s) from docs/PRD.md", verb, len(report.Queued)))
	return len(report.Queued)
}
