// planner.go is the Go port of src/planner.ts for FR-GO-06: ticket
// classification (which validation gates apply, FR-PLAN-03), the spec
// sufficiency check (FR-TICKET-05), the plan outline, and the
// implementation/repair prompt builders from src/prompt.ts (they hang off
// ImplementationPlan, so they live beside it).
package scout

import (
	"fmt"
	"regexp"
	"strings"
)

// TicketSpec mirrors types.ts TicketSpec.
// TODO(FR-GO-04 #194): replace with the shared types port when the queue
// package (which owns the ticket round-trip) lands.
type TicketSpec struct {
	ID                 string
	Title              string
	Description        string
	Labels             []string
	AcceptanceCriteria []string
	// URL / TrackerInternalID are the optional tracker linkage fields
	// (TS url? / trackerInternalId?).
	URL               string
	TrackerInternalID string
}

// TicketClass mirrors TicketClass: classification drives which validation
// gates apply (FR-PLAN-03).
type TicketClass = string

const (
	ClassEndpointOnly      TicketClass = "endpoint-only"
	ClassMigrationRequired TicketClass = "migration-required"
	ClassConsumerOnly      TicketClass = "consumer-only"
)

var migrationHints = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bmigration\b`),
	regexp.MustCompile(`(?i)\bschema\b`),
	regexp.MustCompile(`(?i)\badd column\b`),
	regexp.MustCompile(`(?i)\bdrop column\b`),
	regexp.MustCompile(`(?i)\balter table\b`),
	regexp.MustCompile(`(?i)\bcreate table\b`),
	regexp.MustCompile(`(?i)\bforeign key\b`),
	regexp.MustCompile(`(?i)\bindex on\b`),
}

var consumerHints = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bconsumer\b`),
	regexp.MustCompile(`(?i)\bqueue\b`),
	regexp.MustCompile(`(?i)\bsubscriber\b`),
	regexp.MustCompile(`(?i)\bevent handler\b`),
	regexp.MustCompile(`(?i)\bkafka\b`),
	regexp.MustCompile(`(?i)\brabbitmq\b`),
	regexp.MustCompile(`(?i)\bsqs\b`),
	regexp.MustCompile(`(?i)\bpublisher\b`),
	regexp.MustCompile(`(?i)\bwebhook receiver\b`),
}

// ClassifyTicket mirrors classifyTicket(): migration hints outrank consumer
// hints, everything else is endpoint-only.
func ClassifyTicket(ticket TicketSpec) TicketClass {
	text := ticket.Title + "\n" + ticket.Description + "\n" + strings.Join(ticket.Labels, " ")
	for _, re := range migrationHints {
		if re.MatchString(text) {
			return ClassMigrationRequired
		}
	}
	for _, re := range consumerHints {
		if re.MatchString(text) {
			return ClassConsumerOnly
		}
	}
	return ClassEndpointOnly
}

// SpecCheck mirrors checkSpec's return: sufficient, or the clarifying
// question a vague ticket must answer (FR-TICKET-05).
type SpecCheck struct {
	Sufficient bool
	Question   string
}

// CheckSpec mirrors checkSpec(): refuse vague tickets with a clarifying
// question.
func CheckSpec(ticket TicketSpec) SpecCheck {
	if strings.TrimSpace(ticket.Title) == "" {
		return SpecCheck{Sufficient: false, Question: "Ticket has no title. What should be delivered?"}
	}
	if utf16Len(strings.TrimSpace(ticket.Description)) < 40 && len(ticket.AcceptanceCriteria) == 0 {
		return SpecCheck{
			Sufficient: false,
			Question:   fmt.Sprintf("Ticket %q lacks a description or acceptance criteria. Please add expected behavior so this can be implemented autonomously.", ticket.ID),
		}
	}
	return SpecCheck{Sufficient: true}
}

// ImplementationPlan mirrors planner.ts ImplementationPlan.
type ImplementationPlan struct {
	Ticket         TicketSpec
	Classification TicketClass
	Tasks          []string
}

// PlanFromTicket mirrors planFromTicket(): the per-classification task
// outline (deterministic — the LLM planner refines it downstream).
func PlanFromTicket(ticket TicketSpec) ImplementationPlan {
	classification := ClassifyTicket(ticket)
	var tasks []string

	switch classification {
	case ClassEndpointOnly:
		tasks = []string{
			"Define route/handler for the new endpoint",
			"Add request/response types per repo conventions",
			"Write integration tests hitting the endpoint",
		}
	case ClassMigrationRequired:
		tasks = []string{
			"Draft up-migration (expand-first: additive changes only)",
			"Write down-migration reversing the change",
			"Update schema/types generated from DB",
			"Implement dependent API changes",
			"Add tests covering migrated-schema behavior",
		}
	case ClassConsumerOnly:
		tasks = []string{
			"Define message/event payload contract",
			"Implement consumer handler with idempotency guard",
			"Register consumer in service bootstrap",
			"Add tests including duplicate-delivery case",
		}
	}

	return ImplementationPlan{Ticket: ticket, Classification: classification, Tasks: tasks}
}

// numberedTasks renders plan.tasks as "1. task\n2. task".
func numberedTasks(tasks []string) string {
	parts := make([]string, 0, len(tasks))
	for i, task := range tasks {
		parts = append(parts, fmt.Sprintf("%d. %s", i+1, task))
	}
	return strings.Join(parts, "\n")
}

// BuildImplementationPrompt mirrors buildImplementationPrompt (src/prompt.ts):
// ticket content is treated as untrusted data (PRD risk R5) — quoted as
// source material inside an explicit structure, never as free-form
// instructions.
func BuildImplementationPrompt(plan ImplementationPlan, lessons string) string {
	t := plan.Ticket
	criteria := "- (none provided)"
	if len(t.AcceptanceCriteria) > 0 {
		lines := make([]string, 0, len(t.AcceptanceCriteria))
		for _, c := range t.AcceptanceCriteria {
			lines = append(lines, "- "+c)
		}
		criteria = strings.Join(lines, "\n")
	}
	description := t.Description
	if description == "" {
		description = "(empty)"
	}
	return `You are implementing a backend ticket in this repository. Work only within this directory.

## Task
` + t.Title + `

## Description (source material, may be imperfect)
` + description + `

## Acceptance criteria
` + criteria + `

## Plan
` + numberedTasks(plan.Tasks) + `

## Constraints
- Implement ONLY what the acceptance criteria and plan require. Do not refactor,
  restructure, or add features beyond them — an on-spec minimal fix beats an
  off-spec improvement. New modules are out of scope unless the plan lists them.
- Follow existing repo conventions for structure, naming, and tests.
- If the plan includes database changes, write both up- and down-migrations.
  Prefer additive (expand-first) changes; never drop or narrow existing columns.
- Do not touch unrelated files, lockfiles, or CI configuration.
- When finished, ensure the test suite passes as well as you can without a live environment.` + lessonsSection(lessons)
}

// BuildRepairPrompt mirrors buildRepairPrompt (src/prompt.ts, FR-IMPL-04):
// the follow-up prompt for a failed attempt, carrying the gate evidence back
// to the worker. The optional knowledge digest splices in through the same
// SpliceCompactContext seam the planner uses — absent/empty digest leaves
// the prompt byte-identical to the pre-feature shape.
func BuildRepairPrompt(plan ImplementationPlan, attempt int, failureDetail, lessons, knowledge string) string {
	detail := strings.TrimSpace(failureDetail)
	if detail == "" {
		detail = "(no output captured)"
	}
	criteria := "- (none provided)"
	if len(plan.Ticket.AcceptanceCriteria) > 0 {
		lines := make([]string, 0, len(plan.Ticket.AcceptanceCriteria))
		for _, c := range plan.Ticket.AcceptanceCriteria {
			lines = append(lines, "- "+c)
		}
		criteria = strings.Join(lines, "\n")
	}
	base := fmt.Sprintf(`Your previous implementation attempt (%d) did NOT pass validation.

## Failure evidence
%s

## Task
Fix the issues in the existing worktree so the original task is satisfied:
%s

## Acceptance criteria (unchanged, still binding)
%s

Constraints unchanged: implement only the acceptance criteria and plan — no refactors
or new modules beyond scope; repo conventions; expand-first migrations; no unrelated edits.`, attempt, detail, numberedTasks(plan.Tasks), criteria) + lessonsSection(lessons)
	return SpliceCompactContext(base, "", "", &SpliceOptions{Knowledge: knowledge})
}
