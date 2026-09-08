package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/queue"
)

// runPrdAuditCapture executes the full CLI with the given argv, capturing
// os.Stdout and os.Stderr separately (the audit prints its summary line to
// stdout and warning lines to stderr).
func runPrdAuditCapture(t *testing.T, args ...string) (string, string) {
	t.Helper()
	root := NewRoot()
	root.SetArgs(args)
	root.SilenceUsage = true

	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() { var b bytes.Buffer; _, _ = io.Copy(&b, rOut); outCh <- b.String() }()
	go func() { var b bytes.Buffer; _, _ = io.Copy(&b, rErr); errCh <- b.String() }()

	execErr := root.Execute()
	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	out, errOut := <-outCh, <-errCh
	if execErr != nil {
		t.Fatalf("execute %v: %v", args, execErr)
	}
	return out, errOut
}

// TestPrdAuditWiring: prd-audit is removed from the exit-3 stub map, holds
// a wired command, and carries the frozen flag surface with exact TS types
// (repo: string, json: boolean switch).
func TestPrdAuditWiring(t *testing.T) {
	if _, stubbed := notPortedIssue["prd-audit"]; stubbed {
		t.Error("prd-audit still listed in notPortedIssue (would exit 3)")
	}
	cmd := findImplemented(NewRoot(), "prd-audit")
	if cmd.Name() != "prd-audit" {
		t.Fatalf("root resolves %q, want prd-audit", cmd.Name())
	}
	if cmd.Flags().Lookup("json") == nil || cmd.Flags().Lookup("json").Value.Type() != "bool" {
		t.Error("--json must be a boolean flag")
	}
	if cmd.Flags().Lookup("repo") == nil || cmd.Flags().Lookup("repo").Value.Type() != "string" {
		t.Error("--repo must be a string flag")
	}
}

// prdAuditFixture: temp repo with one unqueued PRD and one unrelated queue
// task, so the audit has both sides of the join to chew on.
func prdAuditFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	prds := filepath.Join(repo, "docs", "prds")
	if err := os.MkdirAll(prds, 0o755); err != nil {
		t.Fatal(err)
	}
	orph := filepath.Join(prds, "orphan.md")
	if err := os.WriteFile(orph, []byte("# orphan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-3 * 24 * time.Hour)
	if err := os.Chtimes(orph, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(queue.QueueDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	other, err := json.MarshalIndent(queue.QueuedTask{ID: "other", Status: queue.StatusDone}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(queue.QueueDir(repo), "other.json"), other, 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// TestPrdAuditEndToEnd: golden output split — the scanned summary on
// stdout, the per-finding warn line on stderr, exit code 0 (advisory-only
// contract: a finding is information, not a cycle failure).
func TestPrdAuditEndToEnd(t *testing.T) {
	repo := prdAuditFixture(t)
	out, errOut := runPrdAuditCapture(t, "prd-audit", "--repo", repo)
	if commandExitCode != nil && *commandExitCode != 0 {
		t.Fatalf("commandExitCode = %v, want 0", *commandExitCode)
	}
	wantSummary := "[prd-audit] scanned 1 PRD(s) in " + filepath.Join(repo, "docs", "prds") + " against 1 queue task(s) — 1 warning(s), advisory only (Q15: no enqueue)\n"
	if out != wantSummary {
		t.Fatalf("stdout =\n%q\nwant\n%q", out, wantSummary)
	}
	wantWarn := "[prd-audit] warn unqueued docs/prds/orphan.md (3d old) has no queue task covering it — the next scout cycle should enqueue it or the curator should retire the file\n"
	if errOut != wantWarn {
		t.Fatalf("stderr =\n%q\nwant\n%q", errOut, wantWarn)
	}
}

// TestPrdAuditJSON: --json prints the machine report to stdout (nothing on
// stderr) with the TS report fields.
func TestPrdAuditJSON(t *testing.T) {
	repo := prdAuditFixture(t)
	out, errOut := runPrdAuditCapture(t, "prd-audit", "--json", "--repo", repo)
	if errOut != "" {
		t.Fatalf("stderr = %q, want empty", errOut)
	}
	var report struct {
		RepoPath string `json:"repoPath"`
		PrdsDir  string `json:"prdsDir"`
		Scanned  int    `json:"scanned"`
		Tasks    int    `json:"tasks"`
		Findings []struct {
			Kind    string   `json:"kind"`
			File    string   `json:"file"`
			TaskIds []string `json:"taskIds"`
			Warning string   `json:"warning"`
		} `json:"findings"`
		Warnings []string `json:"warnings"`
		Enqueued int      `json:"enqueued"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if report.Scanned != 1 || report.Tasks != 1 || report.Enqueued != 0 {
		t.Fatalf("scanned=%d tasks=%d enqueued=%d, want 1/1/0", report.Scanned, report.Tasks, report.Enqueued)
	}
	if report.RepoPath != repo || report.PrdsDir != filepath.Join(repo, "docs", "prds") {
		t.Fatalf("paths = %q / %q", report.RepoPath, report.PrdsDir)
	}
	if len(report.Findings) != 1 || report.Findings[0].Kind != "unqueued" || report.Findings[0].File != "docs/prds/orphan.md" {
		t.Fatalf("findings = %+v", report.Findings)
	}
	if len(report.Findings[0].TaskIds) != 0 {
		t.Fatalf("unqueued taskIds = %v, want empty", report.Findings[0].TaskIds)
	}
	if len(report.Warnings) != 1 || report.Warnings[0] != report.Findings[0].Warning {
		t.Fatalf("warnings = %v, want projection of findings", report.Warnings)
	}
}

// TestPrdAuditCleanExitZero: no findings, no warnings, still exit 0.
func TestPrdAuditCleanExitZero(t *testing.T) {
	repo := t.TempDir()
	out, errOut := runPrdAuditCapture(t, "prd-audit", "--repo", repo)
	if commandExitCode != nil && *commandExitCode != 0 {
		t.Fatalf("commandExitCode = %v, want 0", *commandExitCode)
	}
	want := "[prd-audit] scanned 0 PRD(s) in " + filepath.Join(repo, "docs", "prds") + " against 0 queue task(s) — 0 warning(s), advisory only (Q15: no enqueue)\n"
	if out != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", out, want)
	}
	if errOut != "" {
		t.Fatalf("stderr = %q, want empty", errOut)
	}
}
