// Package resilience seeds the Go port of src/resilience/preflight.ts
// (Q40). Only the single probe function lands here with FR-GO-02 — the
// typed gate, ledger rows, circuit, and pager arrive with the orchestrator
// port (FR-GO-07, issue #194).
package resilience

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/FreePeak/devagent/internal/spawn"
)

// One probe outcome.
type Probe struct {
	OK     bool
	Detail string
}

// boundDetail bounds an error excerpt for ledger/log output.
func boundDetail(text string) string {
	return trunc(strings.Join(strings.Fields(text), " "), 200)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// OMPStartupWedgePattern: omp's own startup-watchdog signature
// ("Still starting after <n>s — phase: ...").
var OMPStartupWedgePattern = regexp.MustCompile(`Still starting after \d+s`)

// RunPreflightProbe asks the worker CLI to reply to "OK" and require an
// answer. `--mode json` (omp) success looks like an event stream containing
// `"text":"OK"`; grok's `--output-format streaming-json` emits the answer as
// a text chunk — `{"type":"text","data":"OK"}` (captured 2026-09-06) — so
// both shapes count. A probe that exits 0 without either is degraded. This
// mirrors the orchestrate-loop probe but runs through the same spawn path as
// the worker adapters so env hardening stays consistent.
func RunPreflightProbe(cmd string, args []string, dir string, timeoutMs int) Probe {
	if timeoutMs <= 0 {
		timeoutMs = 60_000
	}
	run := spawn.RunCli(cmd, args, spawn.Options{Dir: dir, TimeoutMs: timeoutMs})
	ok := run.ExitCode == 0 &&
		(strings.Contains(run.Stdout, `"text":"OK"`) ||
			strings.Contains(run.Stdout, `"type":"text","data":"OK"`))
	if ok {
		return Probe{OK: true}
	}
	detail := boundDetail(run.Stderr + "\n" + run.Stdout)
	if detail == "" {
		detail = "exit " + strconv.Itoa(run.ExitCode)
	}
	return Probe{OK: false, Detail: detail}
}
