// Package pipeline is the Go port of src/pipeline.ts, src/task.ts,
// src/consume.ts (the parts not already in internal/gates), src/planner.ts,
// src/orchestrator/planner.ts, src/fleet.ts, src/create.ts, src/remote.ts,
// src/runregistry.ts and src/resilience/reaper.ts (FR-GO-07 remainder,
// issue #223): the ticket→PR pipeline state machine and the command-body
// helpers the cli wires.
//
// Byte-parity: error strings and printed shapes mirror the TypeScript
// originals exactly; tests pin them.
package pipeline

import (
	"fmt"
	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/scout"
	"strings"
)

// RunLog mirrors the RunLogger surface the TS pipeline consumes
// (src/logger.ts). *ledger.RunLogger satisfies it; tests use a no-op.
type RunLog interface {
	Info(stage ledger.RunStage, message string, data []ledger.KV)
	Warn(stage ledger.RunStage, message string, data []ledger.KV)
	Error(stage ledger.RunStage, message string, data []ledger.KV)
}

// TicketClass mirrors types.ts TicketClass.
type TicketClass = scout.TicketClass

// WorkerName mirrors types.ts WorkerName.
type WorkerName = string

// ExecutorFailureClass mirrors types.ts ExecutorFailureClass.
type ExecutorFailureClass = string

// TicketSpec mirrors types.ts TicketSpec (via the scout port).
type TicketSpec = scout.TicketSpec

// ImplementationPlan mirrors planner.ts ImplementationPlan.
type ImplementationPlan = scout.ImplementationPlan

// CleanupMode mirrors the TS 'auto' | 'keep' | 'always' union.
type CleanupMode = string

const (
	CleanupAuto   CleanupMode = "auto"
	CleanupKeep   CleanupMode = "keep"
	CleanupAlways CleanupMode = "always"
)

// RunConfig mirrors pipeline.ts RunConfig (the cli.ts cfg object shape).
type RunConfig struct {
	TicketID          string
	RepoPath          string
	Worker            WorkerName
	Model             string // "" = unset
	Variant           string // "" = unset
	AutoMerge         bool
	AutoPr            bool
	Interactive       bool
	MaxLoops          int
	TimeoutMs         int
	DryRun            bool
	Cleanup           CleanupMode
	DropOrcaWorkspace bool
}

// StageOutcome mirrors the pipeline.ts StageOutcome union: one struct with a
// discriminating Stage field; only the fields the TS variant carries are set.
type StageOutcome struct {
	Stage        string     `json:"stage"` // clarify|plan|implement|validate|publish|failed
	Question     string     `json:"question,omitempty"`
	Summary      string     `json:"summary,omitempty"`
	Tasks        []string   `json:"tasks,omitempty"`
	Worker       WorkerName `json:"worker,omitempty"`
	WorktreePath string     `json:"worktreePath,omitempty"`
	Branch       string     `json:"branch,omitempty"`
	Attempts     int        `json:"attempts,omitempty"`
	OK           bool       `json:"ok,omitempty"`
	KgEvidence   any        `json:"kgEvidence,omitempty"`
	Passed       bool       `json:"passed,omitempty"`
	PRURL        string     `json:"prUrl,omitempty"`
	Note         string     `json:"note,omitempty"`
	Reason       string     `json:"reason,omitempty"`
}

// ImplementResult mirrors pipeline.ts ImplementResult.
type ImplementResult struct {
	OK           bool
	Worker       WorkerName
	WorktreePath string // "" = undefined
	Branch       string // "" = undefined
	Attempts     int
	FailureClass ExecutorFailureClass // "" = undefined
	KgEvidence   any                  // nil = undefined
}

// GateCheck mirrors the runGateG3 return shape ({passed, findings, detail?}).
type GateCheck struct {
	Passed   bool
	Findings []gates.Finding
	Detail   string
}

// G2G4Result mirrors the async gate result shape ({passed, skipped?, detail?}).
type G2G4Result struct {
	Passed  bool
	Skipped bool
	Detail  string
}

// ReadinessResult mirrors the runGateG0 return shape (validation/readiness-gate).
type ReadinessResult struct {
	Gate      string
	Passed    bool
	Skipped   bool
	Score     int
	Threshold int
	Findings  []gates.Finding
	Detail    string
}

// PipelineDeps mirrors pipeline.ts PipelineDeps. Optional hooks are nil.
type PipelineDeps struct {
	FetchTicket       func(ticketID string) (TicketSpec, error)
	PostTicketComment func(trackerInternalID, comment string) error
	// RunGateG0: issue-readiness gate before worker dispatch.
	RunGateG0 func(ticket TicketSpec, classification TicketClass) ReadinessResult
	RunGateG3 func(repoPath string, classification TicketClass) GateCheck
	// ImplementStage: real worker dispatch; nil = not configured.
	ImplementStage func(cfg RunConfig, plan ImplementationPlan, log RunLog) (ImplementResult, error)
	// RunGateG2: migration-apply gate.
	RunGateG2 func(worktreePath string, timeoutMs int) (G2G4Result, error)
	// RunGateG4: async-review gate.
	RunGateG4 func(worktreePath string) (G2G4Result, error)
	// RunGateG1: test gate.
	RunGateG1 func(worktreePath string, timeoutMs int) (G2G4Result, error)
	// PublishStage: PR publishing; nil = not configured.
	PublishStage func(cfg RunConfig, plan ImplementationPlan, impl ImplementResult) (string, error)
}

// RunPipeline mirrors runPipeline(): fetch → plan → G0 → spec-check →
// implement → G1/G2/G3/G4 → publish.
func RunPipeline(cfg RunConfig, deps PipelineDeps, log RunLog) ([]StageOutcome, error) {
	outcomes := []StageOutcome{}

	// Stage: fetch
	log.Info("fetch", fmt.Sprintf("Fetching ticket %s", cfg.TicketID), nil)
	ticket, err := deps.FetchTicket(cfg.TicketID)
	if err != nil {
		return nil, err
	}
	log.Info("fetch", fmt.Sprintf("Fetched %q", ticket.Title), []ledger.KV{{Key: "labels", Value: strings.Join(ticket.Labels, ",")}})

	// Stage: plan
	plan := scout.PlanFromTicket(ticket)
	log.Info("plan", fmt.Sprintf("Classified as %s", plan.Classification), []ledger.KV{{Key: "tasks", Value: strings.Join(plan.Tasks, ",")}})

	// Gate G0 (issue readiness): rejected tickets get actionable findings
	// posted to the tracker so credits are not burned on under-specified work.
	if deps.RunGateG0 != nil {
		g0 := deps.RunGateG0(ticket, plan.Classification)
		state := "failed"
		if g0.Skipped {
			state = "skipped"
		} else if g0.Passed {
			state = "passed"
		}
		detail := g0.Detail
		if i := strings.IndexByte(detail, '\n'); i >= 0 {
			detail = detail[:i]
		}
		log.Info("validate", fmt.Sprintf("G0 %s: %s", state, detail), []ledger.KV{
			{Key: "score", Value: g0.Score},
			{Key: "threshold", Value: g0.Threshold},
		})
		outcomes = append(outcomes, StageOutcome{Stage: "validate", Passed: g0.Passed})
		if !g0.Passed {
			log.Warn("clarify", "G0 readiness gate rejected ticket", []ledger.KV{{Key: "findings", Value: len(g0.Findings)}})
			if ticket.TrackerInternalID != "" && deps.PostTicketComment != nil {
				comment := g0.Detail
				if comment == "" {
					comment = "G0 readiness gate rejected this ticket"
				}
				if err := deps.PostTicketComment(ticket.TrackerInternalID, comment); err != nil {
					log.Warn("clarify", fmt.Sprintf("Failed to post G0 rejection comment: %s", err.Error()), nil)
				}
			}
			reason := g0.Detail
			if reason == "" {
				reason = "score below threshold"
			}
			outcomes = append(outcomes, StageOutcome{Stage: "failed", Reason: fmt.Sprintf("G0 readiness gate rejected: %s", reason)})
			return outcomes, nil
		}
	}

	// Spec check (FR-TICKET-05)
	spec := scout.CheckSpec(ticket)
	if !spec.Sufficient && spec.Question != "" {
		log.Warn("clarify", "Insufficient specification", nil)
		outcomes = append(outcomes, StageOutcome{Stage: "clarify", Question: spec.Question})
		if ticket.TrackerInternalID != "" && deps.PostTicketComment != nil {
			if err := deps.PostTicketComment(ticket.TrackerInternalID, spec.Question); err != nil {
				log.Warn("clarify", fmt.Sprintf("Failed to post clarification comment: %s", err.Error()), nil)
			}
		}
		return outcomes, nil
	}
	outcomes = append(outcomes, StageOutcome{Stage: "plan", Summary: plan.Classification, Tasks: plan.Tasks})

	if cfg.DryRun {
		log.Info("plan", "Dry-run: stopping before implement", nil)
		return outcomes, nil
	}

	// Stage: implement
	if deps.ImplementStage == nil {
		outcomes = append(outcomes, StageOutcome{Stage: "failed", Reason: "no worker dispatch configured"})
		return outcomes, nil
	}
	impl, err := deps.ImplementStage(cfg, plan, log)
	if err != nil {
		return nil, err
	}
	outcomes = append(outcomes, StageOutcome{
		Stage:        "implement",
		Worker:       impl.Worker,
		WorktreePath: impl.WorktreePath,
		Branch:       impl.Branch,
		Attempts:     impl.Attempts,
		OK:           impl.OK,
		KgEvidence:   impl.KgEvidence,
	})
	if !impl.OK {
		outcomes = append(outcomes, StageOutcome{Stage: "failed", Reason: "worker failed to produce a diff within retry budget"})
		return outcomes, nil
	}

	// Stage: validate — G1 (tests) then G2/G3 (migration) then G4 (async review)
	gatePath := impl.WorktreePath
	if gatePath == "" {
		gatePath = cfg.RepoPath
	}

	if deps.RunGateG1 != nil {
		g1, err := deps.RunGateG1(gatePath, cfg.TimeoutMs)
		if err != nil {
			return nil, err
		}
		logGateResult(log, "G1", g1)
		outcomes = append(outcomes, StageOutcome{Stage: "validate", Passed: g1.Passed})
		if !g1.Passed {
			detail := g1.Detail
			if detail == "" {
				detail = "no detail"
			}
			outcomes = append(outcomes, StageOutcome{Stage: "failed", Reason: fmt.Sprintf("test gate failed: %s", detail)})
			return outcomes, nil
		}
	}

	if deps.RunGateG2 != nil && plan.Classification == "migration-required" {
		g2, err := deps.RunGateG2(gatePath, cfg.TimeoutMs)
		if err != nil {
			return nil, err
		}
		logGateResult(log, "G2", g2)
		outcomes = append(outcomes, StageOutcome{Stage: "validate", Passed: g2.Passed})
		if !g2.Passed {
			detail := g2.Detail
			if detail == "" {
				detail = "no detail"
			}
			outcomes = append(outcomes, StageOutcome{Stage: "failed", Reason: fmt.Sprintf("migration apply gate failed: %s", detail)})
			return outcomes, nil
		}
	}

	if deps.RunGateG3 != nil {
		g3 := deps.RunGateG3(gatePath, plan.Classification)
		detail := g3.Detail
		if detail == "" {
			detail = ""
		}
		log.Info("validate", fmt.Sprintf("G3 %t: %s", g3.Passed, detail), nil)
		outcomes = append(outcomes, StageOutcome{Stage: "validate", Passed: g3.Passed})
		if !g3.Passed {
			outcomes = append(outcomes, StageOutcome{Stage: "failed", Reason: "migration static gate failed"})
			return outcomes, nil
		}
	}

	if deps.RunGateG4 != nil && plan.Classification == "consumer-only" {
		g4, err := deps.RunGateG4(gatePath)
		if err != nil {
			return nil, err
		}
		log.Info("validate", fmt.Sprintf("G4 %t: %s", g4.Passed, g4.Detail), nil)
		outcomes = append(outcomes, StageOutcome{Stage: "validate", Passed: g4.Passed})
		if !g4.Passed {
			detail := g4.Detail
			if detail == "" {
				detail = "no detail"
			}
			outcomes = append(outcomes, StageOutcome{Stage: "failed", Reason: fmt.Sprintf("async review gate failed: %s", detail)})
			return outcomes, nil
		}
	}

	// Stage: publish — headless mode publishes; interactive mode defers
	if cfg.AutoPr && deps.PublishStage != nil {
		prURL, err := deps.PublishStage(cfg, plan, impl)
		if err != nil {
			log.Error("publish", fmt.Sprintf("PR creation failed: %s", err.Error()), nil)
			outcomes = append(outcomes, StageOutcome{Stage: "failed", Reason: fmt.Sprintf("PR creation failed: %s", err.Error())})
			return outcomes, nil
		}
		log.Info("publish", fmt.Sprintf("PR opened: %s", prURL), nil)
		outcomes = append(outcomes, StageOutcome{Stage: "publish", PRURL: prURL, Note: "auto-pr enabled"})
	} else {
		note := "awaiting approval"
		if cfg.AutoPr {
			note = "no publisher configured"
		}
		outcomes = append(outcomes, StageOutcome{Stage: "publish", Note: note})
	}
	return outcomes, nil
}

// logGateResult renders the shared "G1 skipped|passed|failed[: first-line]"
// validate log line (pipeline.ts:151/161 shape).
func logGateResult(log RunLog, name string, g G2G4Result) {
	state := "failed"
	if g.Skipped {
		state = "skipped"
	} else if g.Passed {
		state = "passed"
	}
	detail := ""
	if g.Detail != "" {
		detail = fmt.Sprintf(": %s", firstLine(g.Detail))
	}
	log.Info("validate", fmt.Sprintf("%s %s%s", name, state, detail), nil)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
