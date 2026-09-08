// Package curator is the Go port of src/curator (issue #202, PRD §Q15):
// the PRD-coverage audit that the curation cycle runs after doc-sync.
package curator

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/queue"
)

// CuratorPrdDir mirrors the TS CURATOR_PRD_DIR: the PRD directory the
// curator writes, relative to the target repo.
func CuratorPrdDir() string { return filepath.Join("docs", "prds") }

// DefaultStaleAfterMs mirrors the TS DEFAULT_STALE_AFTER_MS: the default
// mtime age after which a still-open covered PRD reads as stale (14 days).
const DefaultStaleAfterMs = 14 * 24 * 60 * 60 * 1000

// PrdAuditKind mirrors the TS PrdAuditKind union: 'unqueued' | 'stale'.
type PrdAuditKind string

// Finding kinds, mutually exclusive per file (unqueued wins).
const (
	// KindUnqueued: no queue task covers the PRD at all.
	KindUnqueued PrdAuditKind = "unqueued"
	// KindStale: a covering task is still open (pending/claimed) and the
	// PRD's mtime is older than the threshold — queued but not moving.
	KindStale PrdAuditKind = "stale"
)

// AuditOptions mirrors the TS CuratorAuditOptions.
type AuditOptions struct {
	// Stale threshold in ms of PRD mtime age; 0 selects DefaultStaleAfterMs.
	StaleAfterMs int64
	// Injectable clock (epoch ms); nil uses time.Now. Tests pass a fixed
	// value for determinism.
	Now func() int64
}

// PrdAuditFinding mirrors the TS PrdAuditFinding.
type PrdAuditFinding struct {
	Kind PrdAuditKind `json:"kind"`
	// PRD path relative to the repo, as the warning names it.
	File string `json:"file"`
	// File stem — the id the queue would carry.
	Stem string `json:"stem"`
	// PRD mtime age in ms at scan time (never negative).
	AgeMs int64 `json:"ageMs"`
	// Queue task ids covering the PRD; empty for an unqueued finding.
	TaskIDs []string `json:"taskIds"`
	// One-line human warning, prefixed by the caller.
	Warning string `json:"warning"`
}

// AuditReport mirrors the TS CuratorAuditReport (same JSON key order for
// the --json surface).
type AuditReport struct {
	RepoPath string `json:"repoPath"`
	// Absolute path of the scanned PRD directory (may not exist).
	PrdsDir string `json:"prdsDir"`
	// docs/prds/*.md files considered.
	Scanned int `json:"scanned"`
	// Queue tasks read for coverage.
	Tasks        int               `json:"tasks"`
	StaleAfterMs int64             `json:"staleAfterMs"`
	Findings     []PrdAuditFinding `json:"findings"`
	// Convenience projection of Findings[].Warning, same order.
	Warnings []string `json:"warnings"`
	// Always 0: the audit is advisory-only by contract (Q15); the field
	// exists so a machine caller can assert the invariant.
	Enqueued int `json:"enqueued"`
}

// FormatPrdAge mirrors the TS formatPrdAge: whole days once the age covers
// a day, whole (floored, never negative) hours before that.
func FormatPrdAge(ageMs int64) string {
	days := ageMs / (24 * 60 * 60 * 1000)
	if days >= 1 {
		return fmt.Sprintf("%dd", days)
	}
	hours := ageMs / (60 * 60 * 1000)
	if hours < 0 {
		hours = 0
	}
	return fmt.Sprintf("%dh", hours)
}

// AuditPrdCoverage mirrors the TS auditPrdCoverage: scan
// <repo>/docs/prds/*.md against queue coverage and return the advisory
// findings. Read-only: no queue write, no PRD write, no error on a missing
// or unreadable directory (it simply scans nothing).
//
// Coverage model: a docs/prds/<stem>.md file counts as queued when any task
// names that stem, either through its id or through the basename of its
// prdPath (the queue keeps its own copy under .devagent/prds/, so the file
// stem is the join key between the two directories).
//
// Determinism: file order is the sorted docs/prds listing and the clock is
// injectable, so a temp-repo test controls both the listing and every mtime
// age without touching system time.
func AuditPrdCoverage(repoPath string, opts AuditOptions) AuditReport {
	staleAfterMs := opts.StaleAfterMs
	if staleAfterMs == 0 {
		staleAfterMs = DefaultStaleAfterMs
	}
	nowMs := time.Now().UnixMilli()
	if opts.Now != nil {
		nowMs = opts.Now()
	}
	prdsDir := filepath.Join(repoPath, CuratorPrdDir())

	tasks := queue.ListTasks(repoPath, "")
	// Join key per task: its id, plus the PRD file stem when the queue
	// points at a differently-named file.
	cover := make(map[string][]*queue.QueuedTask)
	for _, t := range tasks {
		keys := []string{t.ID}
		if t.PrdPath != nil && *t.PrdPath != "" {
			prdKey := strings.TrimSuffix(filepath.Base(*t.PrdPath), ".md")
			if prdKey != "" && prdKey != t.ID {
				keys = append(keys, prdKey)
			}
		}
		for _, k := range keys {
			cover[k] = append(cover[k], t)
		}
	}

	var files []string
	if entries, err := os.ReadDir(prdsDir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".md") {
				files = append(files, e.Name())
			}
		}
		// TS readdirSync(...).sort(): lexicographic; os.ReadDir is already
		// name-sorted, the sort only pins the contract.
		sort.Strings(files)
	}

	findings := make([]PrdAuditFinding, 0)
	for _, f := range files {
		stem := strings.TrimSuffix(f, ".md")
		file := filepath.Join(CuratorPrdDir(), f)
		info, err := os.Stat(filepath.Join(prdsDir, f))
		if err != nil {
			continue // vanished mid-scan — never fail an advisory pass
		}
		ageMs := nowMs - info.ModTime().UnixMilli()
		if ageMs < 0 {
			ageMs = 0
		}
		covering := cover[stem]
		if len(covering) == 0 {
			findings = append(findings, PrdAuditFinding{
				Kind:    KindUnqueued,
				File:    file,
				Stem:    stem,
				AgeMs:   ageMs,
				TaskIDs: []string{},
				Warning: fmt.Sprintf("%s (%s old) has no queue task covering it — the next scout cycle should enqueue it or the curator should retire the file", file, FormatPrdAge(ageMs)),
			})
			continue
		}
		var open []*queue.QueuedTask
		for _, t := range covering {
			if t.Status == queue.StatusPending || t.Status == queue.StatusClaimed {
				open = append(open, t)
			}
		}
		if len(open) > 0 && ageMs >= staleAfterMs {
			ids := make([]string, 0, len(covering))
			for _, t := range covering {
				ids = append(ids, t.ID)
			}
			parts := make([]string, 0, len(open))
			for _, t := range open {
				parts = append(parts, fmt.Sprintf("%s (%s)", t.ID, t.Status))
			}
			findings = append(findings, PrdAuditFinding{
				Kind:    KindStale,
				File:    file,
				Stem:    stem,
				AgeMs:   ageMs,
				TaskIDs: ids,
				Warning: fmt.Sprintf("%s is queued as %s but has sat unchanged for %s >= %s threshold — check why the board has not picked it up",
					file, strings.Join(parts, ", "), FormatPrdAge(ageMs), FormatPrdAge(staleAfterMs)),
			})
		}
	}

	warnings := make([]string, 0, len(findings))
	for _, f := range findings {
		warnings = append(warnings, f.Warning)
	}
	return AuditReport{
		RepoPath:     repoPath,
		PrdsDir:      prdsDir,
		Scanned:      len(files),
		Tasks:        len(tasks),
		StaleAfterMs: staleAfterMs,
		Findings:     findings,
		Warnings:     warnings,
		Enqueued:     0,
	}
}

// FormatAuditReport mirrors the TS formatAuditReport: the summary line plus
// one [prd-audit]-prefixed warning line per finding, joined with newlines
// (empty when clean). The CLI prints the summary to stdout and each warning
// to stderr, so it routes the lines separately.
func FormatAuditReport(report AuditReport) string {
	lines := []string{
		fmt.Sprintf("[prd-audit] scanned %d PRD(s) in %s against %d queue task(s) — %d warning(s), advisory only (Q15: no enqueue)",
			report.Scanned, report.PrdsDir, report.Tasks, len(report.Findings)),
	}
	for _, f := range report.Findings {
		lines = append(lines, fmt.Sprintf("[prd-audit] warn %s %s", f.Kind, f.Warning))
	}
	return strings.Join(lines, "\n")
}
