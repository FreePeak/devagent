package loopdriver

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/prdintake"
	"github.com/FreePeak/devagent/internal/queue"
)

// The intake step is the route from an operator's PRD edit into the same
// iteration's claim (issue #370). These tests pin the three properties that
// make it safe to run unattended at the head of every iteration: it queues
// intent, it can be switched off, and it can never be the reason an
// iteration fails.

func intakeDriver(t *testing.T, repo string, mutate func(*LoopConfig)) *driver {
	t.Helper()
	cfg := loopConfigFor(t, repo, mutate)
	return &driver{cfg: cfg, stateDir: filepath.Join(repo, ".selfbuild")}
}

// TestRunPrdIntakeQueuesForTheSameIteration: an open `- [ ]` item in
// docs/PRD.md becomes a claimable queue row before phase 2a runs, so the
// iteration that reads the operator's edit is the iteration that builds it.
func TestRunPrdIntakeQueuesForTheSameIteration(t *testing.T) {
	repo := initFixtureRepo(t)
	writeRepoFile(t, repo, "docs/PRD.md", "## 12. CLI\n\n- [ ] Add the intake lane\n  - queues the item\n")

	var log bytes.Buffer
	d := intakeDriver(t, repo, func(c *LoopConfig) { c.DryRun = false })
	if n := d.runPrdIntake(1, &log); n != 1 {
		t.Fatalf("runPrdIntake queued %d, want 1 (log: %s)", n, log.String())
	}

	rows := queue.ListTasks(repo, queue.StatusPending)
	if len(rows) != 1 {
		t.Fatalf("queue rows = %d, want 1", len(rows))
	}
	if !strings.HasPrefix(rows[0].Goal, "Goal: ") {
		t.Errorf("goal is off the dispatch contract: %q", rows[0].Goal)
	}
	if got := *rows[0].Source; got != "prd" {
		t.Errorf("source = %q, want prd", got)
	}
	if !strings.Contains(log.String(), "queued PRD-") {
		t.Errorf("log = %q, want the queued row named", log.String())
	}

	// The claim the driver makes right after intake is that row.
	claimed := claimQueueTask(repo)
	if claimed == nil || claimed.ID != rows[0].ID {
		t.Fatalf("claim = %+v, want the intake row %s", claimed, rows[0].ID)
	}
}

// TestRunPrdIntakeIsIdempotentAcrossIterations: every iteration runs intake,
// so a second pass over an unchanged PRD must queue nothing — a re-queued
// item would re-burn a dispatch on work already shipped.
func TestRunPrdIntakeIsIdempotentAcrossIterations(t *testing.T) {
	repo := initFixtureRepo(t)
	writeRepoFile(t, repo, "docs/PRD.md", "## Scope\n\n- [ ] Build the lane\n")

	var log bytes.Buffer
	d := intakeDriver(t, repo, func(c *LoopConfig) { c.DryRun = false })
	if n := d.runPrdIntake(1, &log); n != 1 {
		t.Fatalf("first pass queued %d, want 1", n)
	}
	log.Reset()
	if n := d.runPrdIntake(2, &log); n != 0 {
		t.Fatalf("second pass queued %d, want 0", n)
	}
	if !strings.Contains(log.String(), "already in the queue") {
		t.Errorf("second pass log = %q, want the already-known report", log.String())
	}
	if len(queue.ListTasks(repo, "")) != 1 {
		t.Errorf("queue holds %d rows, want 1", len(queue.ListTasks(repo, "")))
	}
}

// TestRunPrdIntakeKnobAndFailurePaths: SELFBUILD_PRD_INTAKE=0 is the operator
// off-switch, and an intake that cannot read the PRD is a missing work source
// — never an iteration failure, and never a starvation/breaker increment.
func TestRunPrdIntakeKnobAndFailurePaths(t *testing.T) {
	repo := initFixtureRepo(t)
	writeRepoFile(t, repo, "docs/PRD.md", "## Scope\n\n- [ ] Build the lane\n")

	off := false
	var log bytes.Buffer
	d := intakeDriver(t, repo, func(c *LoopConfig) { c.PRDIntake = &off; c.DryRun = false })
	if n := d.runPrdIntake(1, &log); n != 0 || log.Len() != 0 {
		t.Errorf("disabled intake queued %d and logged %q, want neither", n, log.String())
	}
	if len(queue.ListTasks(repo, "")) != 0 {
		t.Errorf("SELFBUILD_PRD_INTAKE=0 still wrote queue rows")
	}

	// A repo with no PRD at all.
	empty := initFixtureRepo(t)
	log.Reset()
	d = intakeDriver(t, empty, func(c *LoopConfig) { c.DryRun = false })
	if n := d.runPrdIntake(1, &log); n != 0 {
		t.Errorf("missing PRD queued %d, want 0", n)
	}
	if !strings.Contains(log.String(), "no docs/PRD.md") {
		t.Errorf("log = %q, want the reason", log.String())
	}

	// A seam that errors for another reason still cannot fail the iteration.
	log.Reset()
	d = intakeDriver(t, repo, func(c *LoopConfig) {
		c.DryRun = false
		c.Intake = func(prdintake.Options) (prdintake.Report, error) {
			return prdintake.Report{}, os.ErrPermission
		}
	})
	if n := d.runPrdIntake(1, &log); n != 0 {
		t.Errorf("failing intake queued %d, want 0", n)
	}
	if !strings.Contains(log.String(), "[prd-intake] skipped") {
		t.Errorf("log = %q, want the skip report", log.String())
	}
}

// TestRunPrdIntakeHonoursDryRun: a dry run rehearses the verdict and touches
// nothing, so the row it reports is the row it would have written.
func TestRunPrdIntakeHonoursDryRun(t *testing.T) {
	repo := initFixtureRepo(t)
	writeRepoFile(t, repo, "docs/PRD.md", "## Scope\n\n- [ ] Build the lane\n")

	var log bytes.Buffer
	d := intakeDriver(t, repo, nil) // loopConfigFor defaults DryRun = true
	if n := d.runPrdIntake(1, &log); n != 1 {
		t.Fatalf("dry run reported %d queued, want 1", n)
	}
	if !strings.Contains(log.String(), "would queue") {
		t.Errorf("log = %q, want the would-queue wording", log.String())
	}
	if len(queue.ListTasks(repo, "")) != 0 {
		t.Errorf("dry run wrote queue rows")
	}
}

// TestConfigHonoursPRDIntakeKnob: the tri-state env knob must keep "unset"
// distinct from "0", because the default is on — a plain bool would silently
// turn the lane off for every driver that never mentioned it.
func TestConfigHonoursPRDIntakeKnob(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	cfg := ConfigFromGetenv("/r", env(map[string]string{})).WithDefaults()
	if cfg.PRDIntake == nil || !*cfg.PRDIntake {
		t.Errorf("unset env must default intake on")
	}
	cfg = ConfigFromGetenv("/r", env(map[string]string{"SELFBUILD_PRD_INTAKE": "0"})).WithDefaults()
	if cfg.PRDIntake == nil || *cfg.PRDIntake {
		t.Errorf("SELFBUILD_PRD_INTAKE=0 must disable intake")
	}
	cfg = ConfigFromGetenv("/r", env(map[string]string{"SELFBUILD_PRD_INTAKE_MAX": "2"}))
	if cfg.PRDIntakeMax != 2 {
		t.Errorf("PRDIntakeMax = %d, want 2", cfg.PRDIntakeMax)
	}
}
