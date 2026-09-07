// Package gates ports DevAgent's validation gates to Go (FR-GO-08):
// G1 tests (test-gate), G2 migration apply, G3 migration static rules,
// G5 STRIDE over a unified diff (rubric + gate-executor adapter), the G0
// readiness rubric, the CI check rollup, and the single-PR regression
// oracle (PRD §17 Phase 4).
//
// Byte-parity contract: error strings, detail strings, and JSON field names
// mirror the TypeScript originals in src/validation, src/gates, and
// src/consume.ts so ledger evidence stays byte-compatible across runtimes.
package gates

import (
	"github.com/FreePeak/devagent/internal/spawn"
)

// Severity mirrors src/types.ts Severity.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

// TicketClass mirrors src/types.ts TicketClass.
const (
	TicketClassEndpointOnly      = "endpoint-only"
	TicketClassMigrationRequired = "migration-required"
	TicketClassConsumerOnly      = "consumer-only"
)

// Gate identifiers (the TS GateResult gate union relevant to this package).
const (
	GateG1Tests           = "G1-tests"
	GateG2MigrationApply  = "G2-migration-apply"
	GateG3MigrationStatic = "G3-migration-static"
)

// Finding mirrors src/types.ts Finding. The JSON tags are the ledger
// evidence contract and must stay byte-compatible.
type Finding struct {
	RuleID   string   `json:"ruleId"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	File     string   `json:"file,omitempty"`
	Line     *int     `json:"line,omitempty"`
}

// GateResult mirrors src/types.ts GateResult.
type GateResult struct {
	Gate     string    `json:"gate"`
	Passed   bool      `json:"passed"`
	Skipped  bool      `json:"skipped,omitempty"`
	Findings []Finding `json:"findings"`
	Detail   string    `json:"detail,omitempty"`
}

// TestCommand mirrors src/validation/test-gate.ts TestCommand.
type TestCommand struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
}

// Runner shells out to CLIs (docker/compose, test runners, npm ci). It is an
// interface so gate tests stay hermetic: fakes script exit codes instead of
// spawning real processes — the TS suite mocks spawnCli/runCli the same way.
// The production implementation delegates to internal/spawn.
type Runner interface {
	RunCli(name string, args []string, opts spawn.Options) spawn.Result
}

// SpawnRunner is the production Runner backed by internal/spawn.
type SpawnRunner struct{}

// RunCli implements Runner via spawn.RunCli.
func (SpawnRunner) RunCli(name string, args []string, opts spawn.Options) spawn.Result {
	return spawn.RunCli(name, args, opts)
}

// runWith executes a CLI through the injected runner, falling back to the
// real spawn-backed runner when none is provided.
func runWith(r Runner, name string, args []string, opts spawn.Options) spawn.Result {
	if r != nil {
		return r.RunCli(name, args, opts)
	}
	return spawn.RunCli(name, args, opts)
}
