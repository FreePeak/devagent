package loopdriver

import (
	"io"
	"os"
	"strconv"
	"time"
)

// LoopConfig carries every knob the bash driver reads from the SELFBUILD_*
// environment (defaults mirror scripts/selfbuild-loop.sh lines 43-66).
type LoopConfig struct {
	// Repo is the working repository (SELFBUILD_REPO); all state lives under
	// Repo/.selfbuild and every subprocess runs with Dir=Repo.
	Repo string
	// MaxIterations caps the iteration count (SELFBUILD_MAX_ITERATIONS,
	// 0 = unbounded); checked at loop head so skip/continue paths cannot
	// cycle forever (2026-09-04 smoke evidence).
	MaxIterations int
	// MaxConsecutiveFailures trips the circuit breaker (default 3).
	MaxConsecutiveFailures int
	// StarvationLimit is the non-productive-iteration gate (default 5).
	StarvationLimit int
	// CleanupDelaySecs is the auto-pr leftover grace period (default 1800).
	CleanupDelaySecs int
	// IssueLabel filters the GitHub tracker queue (default selfbuild).
	IssueLabel string
	// IssueMax bounds the tracker listing (default 50).
	IssueMax int
	// DryRun executes phases 1-3 + record only (SELFBUILD_DRY_RUN=1).
	DryRun bool
	// Worker selects the executor (default omp).
	Worker string
	// PushMode is "pr" (auto-pr + cleanup scheduling) or "main"
	// (commit + push to the current branch) (default pr).
	PushMode string
	// ResearchBin / POBin are the phase-1 / phase-2-3 dispatch commands
	// (word-split like the unquoted bash expansion; default omp router).
	ResearchBin string
	POBin       string
	// TestCmd is the post-merge-back repo-level test gate command
	// (word-split like the unquoted bash expansion). SELFBUILD_TEST_CMD
	// overrides it. Default is `go test ./...`: the Node tree was retired
	// in PR #240, so an `npm test` default can only fail (issue #300).
	TestCmd string
	// ClaudeTimeout bounds the PO dispatch (default 600s).
	ClaudeTimeout int
	// ResearchTimeout bounds the research dispatch (default 900s).
	ResearchTimeout int
	// Visibility rides DEVAGENT_VISIBILITY on every dispatch
	// (flag > DEVAGENT_VISIBILITY > SELFBUILD_VISIBILITY > visible).
	Visibility string
	// NoSyncDocs skips the doc-freshness gate (SELFBUILD_NO_SYNC_DOCS=1).
	NoSyncDocs bool
	// GHRepo is owner/repo for the tracker queue; empty derives from
	// `git remote get-url origin`.
	GHRepo string
	// Model optionally pins the task worker model (SELFBUILD_MODEL).
	Model string
	// TaskTimeout bounds the whole task dispatch (default 7200s).
	TaskTimeout int
	// APIMaxAttempts bounds executor retry budget (default 40).
	APIMaxAttempts int
	// NoProgressTimeoutMS bounds executor no-progress hang detection
	// (default 600000).
	NoProgressTimeoutMS int
	// SyncRetrySecs is the pause between degraded iterations (default 60).
	SyncRetrySecs int
	// DevagentBin is the CLI invoked for the not-yet-ported surfaces
	// (pane-run, task, preflight, sync-docs, scan-text, ledger --clusters,
	// herdr-sweep, page-degrade-breach). Default "devagent".
	// SELFBUILD_DEVAGENT_BIN overrides it — the FR-GO-15 self-hosting soak
	// sets this to the Go binary so every shelled subcommand executes the
	// Go implementation instead of the npm-linked Node CLI. As subcommand
	// ports land on main, the honest-soak goal is DevagentBin == the Go
	// binary and the shelled set shrinking to empty.
	DevagentBin string
	// DevagentArgs are prepended to every DevagentBin invocation (e.g. an
	// npx/tsx wrapper for a from-source checkout).
	DevagentArgs []string
	// GhBin is the gh executable (default "gh"); test seam.
	GhBin string
	// LockDir overrides the default Repo/.selfbuild/loop.lock.d; test seam.
	LockDir string
	// Now / Sleep / Stdout / Stderr are hermetic-test seams.
	Now    func() time.Time
	Sleep  func(time.Duration)
	Stdout io.Writer
	Stderr io.Writer
}

const (
	defaultMaxFails        = 3
	defaultStarvationLimit = 5
	defaultCleanupDelay    = 1800
	defaultClaudeTimeout   = 600
	defaultResearchTimeout = 900
	defaultTaskTimeout     = 7200
	defaultAPIMaxAttempts  = 40
	defaultNoProgressMS    = 600000
	defaultSyncRetrySecs   = 60
	defaultIssueMax        = 50

	defaultDispatchBin = "omp -p --mode json --no-prewalk --no-lsp --no-extensions --model onegw/free"

	defaultTestCmd = "go test ./..."
)

func getenvOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

func getintOr(getenv func(string) string, key string, def int) int {
	v := getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getenvSet(getenv func(string) string, key string) bool {
	return getenv(key) == "1"
}

// WithDefaults fills zero-valued fields with the bash defaults. GHRepo
// stays empty here: RunLoop derives it from `git remote get-url origin`
// via ghRepoFromRemote when unset (the bash fallback).
func (c LoopConfig) WithDefaults() LoopConfig {
	if c.MaxConsecutiveFailures == 0 {
		c.MaxConsecutiveFailures = defaultMaxFails
	}
	if c.StarvationLimit == 0 {
		c.StarvationLimit = defaultStarvationLimit
	}
	if c.CleanupDelaySecs == 0 {
		c.CleanupDelaySecs = defaultCleanupDelay
	}
	if c.IssueLabel == "" {
		c.IssueLabel = "selfbuild"
	}
	if c.IssueMax == 0 {
		c.IssueMax = defaultIssueMax
	}
	if c.Worker == "" {
		c.Worker = "omp"
	}
	if c.PushMode == "" {
		c.PushMode = "pr"
	}
	if c.ResearchBin == "" {
		c.ResearchBin = defaultDispatchBin
	}
	if c.POBin == "" {
		c.POBin = defaultDispatchBin
	}
	if c.TestCmd == "" {
		c.TestCmd = defaultTestCmd
	}
	if c.ClaudeTimeout == 0 {
		c.ClaudeTimeout = defaultClaudeTimeout
	}
	if c.ResearchTimeout == 0 {
		c.ResearchTimeout = defaultResearchTimeout
	}
	if c.Visibility == "" {
		c.Visibility = "visible"
	}
	if c.TaskTimeout == 0 {
		c.TaskTimeout = defaultTaskTimeout
	}
	if c.APIMaxAttempts == 0 {
		c.APIMaxAttempts = defaultAPIMaxAttempts
	}
	if c.NoProgressTimeoutMS == 0 {
		c.NoProgressTimeoutMS = defaultNoProgressMS
	}
	if c.SyncRetrySecs == 0 {
		c.SyncRetrySecs = defaultSyncRetrySecs
	}
	if c.DevagentBin == "" {
		c.DevagentBin = "devagent"
	}
	if c.GhBin == "" {
		c.GhBin = "gh"
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = time.Sleep
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
	return c
}

// ConfigFromEnv builds a LoopConfig from the process environment with the
// same variable names and precedence the bash driver used. visibility
// follows flag > DEVAGENT_VISIBILITY > SELFBUILD_VISIBILITY > visible.
func ConfigFromEnv(repo string) LoopConfig {
	return ConfigFromGetenv(repo, os.Getenv)
}

// ConfigFromGetenv is the injectable form of ConfigFromEnv.
func ConfigFromGetenv(repo string, getenv func(string) string) LoopConfig {
	visibility := "visible"
	if v := getenv("SELFBUILD_VISIBILITY"); v != "" {
		visibility = v
	}
	if v := getenv("DEVAGENT_VISIBILITY"); v != "" {
		visibility = v
	}
	return LoopConfig{
		Repo:                   repo,
		MaxIterations:          getintOr(getenv, "SELFBUILD_MAX_ITERATIONS", 0),
		MaxConsecutiveFailures: getintOr(getenv, "SELFBUILD_MAX_FAILS", defaultMaxFails),
		StarvationLimit:        getintOr(getenv, "SELFBUILD_STARVATION_LIMIT", defaultStarvationLimit),
		CleanupDelaySecs:       getintOr(getenv, "SELFBUILD_CLEANUP_DELAY", defaultCleanupDelay),
		IssueLabel:             getenvOr(getenv, "SELFBUILD_ISSUE_LABEL", "selfbuild"),
		IssueMax:               getintOr(getenv, "SELFBUILD_ISSUE_MAX", defaultIssueMax),
		DryRun:                 getenvSet(getenv, "SELFBUILD_DRY_RUN"),
		Worker:                 getenvOr(getenv, "SELFBUILD_WORKER", "omp"),
		PushMode:               getenvOr(getenv, "SELFBUILD_PUSH_MODE", "pr"),
		ResearchBin:            getenvOr(getenv, "SELFBUILD_RESEARCH_BIN", defaultDispatchBin),
		POBin:                  getenvOr(getenv, "SELFBUILD_PO_BIN", defaultDispatchBin),
		TestCmd:                getenvOr(getenv, "SELFBUILD_TEST_CMD", defaultTestCmd),
		ClaudeTimeout:          getintOr(getenv, "SELFBUILD_CLAUDE_TIMEOUT", defaultClaudeTimeout),
		ResearchTimeout:        getintOr(getenv, "SELFBUILD_RESEARCH_TIMEOUT", defaultResearchTimeout),
		Visibility:             visibility,
		NoSyncDocs:             getenvSet(getenv, "SELFBUILD_NO_SYNC_DOCS"),
		GHRepo:                 getenv("SELFBUILD_GH_REPO"),
		Model:                  getenv("SELFBUILD_MODEL"),
		TaskTimeout:            getintOr(getenv, "SELFBUILD_TASK_TIMEOUT", defaultTaskTimeout),
		APIMaxAttempts:         getintOr(getenv, "SELFBUILD_API_MAX_ATTEMPTS", defaultAPIMaxAttempts),
		NoProgressTimeoutMS:    getintOr(getenv, "SELFBUILD_NO_PROGRESS_TIMEOUT_MS", defaultNoProgressMS),
		SyncRetrySecs:          getintOr(getenv, "SELFBUILD_SYNC_RETRY_SECS", defaultSyncRetrySecs),
		DevagentBin:            getenv("SELFBUILD_DEVAGENT_BIN"),
	}
}
