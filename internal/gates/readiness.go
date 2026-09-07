package gates

import (
	"fmt"
	"regexp"
	"strings"
)

// Gate G0 (issue readiness): deterministic, type-specific scoring of an
// incoming ticket against ready-for-dev criteria, evaluated BEFORE worker
// dispatch so under-specified tickets never burn worker credits.
//
// Shape mirrors the G5 STRIDE gate: a pure regex/length rubric over the
// ticket text — no LLM, no network, never panics on missing fields. Every
// unmet criterion becomes a Finding and the summed score decides pass/reject
// against the class threshold.

// G0ReadinessThreshold is the minimum score (0-100) for a ticket to be
// dispatched. Uniform across classes.
const G0ReadinessThreshold = 60

// ReadinessTicket is the ticket slice evaluateReadiness consumes
// (TS Pick<TicketSpec, ...>).
type ReadinessTicket struct {
	ID                 string
	Title              string
	Description        string
	Labels             []string
	AcceptanceCriteria []string
}

// ReadinessGateResult mirrors src/validation/readiness-gate.ts
// ReadinessGateResult.
type ReadinessGateResult struct {
	Gate      string    `json:"gate"`
	Passed    bool      `json:"passed"`
	Skipped   bool      `json:"skipped,omitempty"` // no rubric applies (unknown classification)
	Score     int       `json:"score"`
	Threshold int       `json:"threshold"`
	Findings  []Finding `json:"findings"`
	Detail    string    `json:"detail"`
}

// readinessRule mirrors the TS ReadinessRule: points awarded when the
// pattern matches anywhere in the ticket text.
type readinessRule struct {
	ruleID   string
	severity Severity
	weight   int
	pattern  *regexp.Regexp
	gap      string
}

// readinessClassRules mirrors the TS CLASS_RULES: type-specific
// ready-for-dev signals, one rubric per FR-PLAN-03 classification. Weights:
// common criteria (title/description/criteria) carry 65 points; the two
// class signals carry 35.
var readinessClassRules = map[string][]readinessRule{
	TicketClassEndpointOnly: {
		{
			ruleID:   "G0-ENDPOINT-SURFACE",
			severity: SeverityMedium,
			weight:   20,
			pattern:  icase(`\b(get|post|put|patch|delete|head|options)\b[^.\n]{0,60}/[a-z0-9._~-]|/api/|/v\d+/`),
			gap:      "name the HTTP surface (method + path, e.g. GET /api/things)",
		},
		{
			ruleID:   "G0-VERIFICATION",
			severity: SeverityMedium,
			weight:   15,
			pattern:  icase(`\b(test|tests|spec|specs|curl|expect|expects|assert|verif|coverage|integration|unit)\b|returns? \d{3}|status code`),
			gap:      "state how the endpoint is verified (tests, expected status/body)",
		},
	},
	TicketClassMigrationRequired: {
		{
			ruleID:   "G0-SCHEMA-ENTITY",
			severity: SeverityMedium,
			weight:   20,
			pattern:  icase(`\b(table|column|schema|index|constraint|foreign key|primary key|migration)\b`),
			gap:      "name the affected schema entities (table/column/index)",
		},
		{
			ruleID:   "G0-REVERSIBILITY",
			severity: SeverityMedium,
			weight:   15,
			pattern:  icase(`\b(down migration|down\.sql|rollback|revert|reverse|reversible|back out|backout)\b`),
			gap:      "state the down-migration/rollback expectation",
		},
	},
	TicketClassConsumerOnly: {
		{
			ruleID:   "G0-TRANSPORT",
			severity: SeverityMedium,
			weight:   20,
			pattern:  icase(`\b(queue|topic|consumer|publisher|subscriber|subscription|kafka|sqs|rabbitmq|webhook|stream)\b`),
			gap:      "name the transport (queue/topic/webhook and its name)",
		},
		{
			ruleID:   "G0-DELIVERY",
			severity: SeverityMedium,
			weight:   15,
			pattern:  icase(`\b(idempoten|duplicate|at.least.once|exactly.once|dedup|retry|retries|redeliver|offset|ack)\b`),
			gap:      "state delivery semantics (idempotency, duplicate/retry handling)",
		},
	},
}

var readinessSeverityTag = map[Severity]string{
	SeverityCritical: "C",
	SeverityHigh:     "H",
	SeverityMedium:   "M",
	SeverityLow:      "L",
}

// formatReadinessDetail mirrors the TS formatDetail().
func formatReadinessDetail(passed bool, score, threshold int, classification string, findings []Finding) string {
	verdict := "rejected"
	if passed {
		verdict = "pass"
	}
	head := fmt.Sprintf("G0 readiness %d/100 (threshold %d) for %s: %s", score, threshold, classification, verdict)
	if len(findings) == 0 {
		return head
	}
	lines := []string{head}
	for _, f := range findings {
		lines = append(lines, fmt.Sprintf("- [%s] %s: %s", readinessSeverityTag[f.Severity], f.RuleID, f.Message))
	}
	return strings.Join(lines, "\n")
}

// EvaluateReadiness mirrors evaluateReadiness(): score a ticket against the
// readiness rubric for its classification. Tolerates missing fields (tracker
// adapters return partial data); unknown classifications skip honestly like
// the other gates.
func EvaluateReadiness(ticket ReadinessTicket, classification string) ReadinessGateResult {
	rules, ok := readinessClassRules[classification]
	if !ok {
		return ReadinessGateResult{
			Gate:      "G0-readiness",
			Passed:    true,
			Skipped:   true,
			Score:     0,
			Threshold: 0,
			Findings:  []Finding{},
			Detail:    "skipped: unknown ticket classification",
		}
	}

	title := strings.TrimSpace(ticket.Title)
	description := strings.TrimSpace(ticket.Description)
	criteria := []string{}
	for _, c := range ticket.AcceptanceCriteria {
		if strings.TrimSpace(c) != "" {
			criteria = append(criteria, c)
		}
	}
	labels := ticket.Labels
	if labels == nil {
		labels = []string{}
	}
	text := strings.Join([]string{title, description, strings.Join(labels, " "), strings.Join(criteria, "\n")}, "\n")

	findings := []Finding{}
	score := 0

	// Common criterion: substantive title.
	if len(title) >= 10 {
		score += 20
	} else {
		findings = append(findings, Finding{
			RuleID:   "G0-TITLE",
			Severity: SeverityHigh,
			Message:  fmt.Sprintf("title too short (%d chars, need >= 10); state what is delivered", len(title)),
		})
	}

	// Common criterion: substantive description.
	if len(description) >= 40 {
		score += 20
	} else {
		findings = append(findings, Finding{
			RuleID:   "G0-DESCRIPTION",
			Severity: SeverityHigh,
			Message:  fmt.Sprintf("description too short (%d chars, need >= 40); add context, scope, and expected behavior", len(description)),
		})
	}

	// Common criterion: machine-checkable acceptance criteria (tiered).
	switch {
	case len(criteria) >= 2:
		score += 25
	case len(criteria) == 1:
		score += 15
	default:
		findings = append(findings, Finding{
			RuleID:   "G0-ACCEPTANCE",
			Severity: SeverityHigh,
			Message:  "no acceptance criteria; add machine-checkable completion signals",
		})
	}

	// Type-specific ready-for-dev signals.
	for _, rule := range rules {
		if rule.pattern.MatchString(text) {
			score += rule.weight
		} else {
			findings = append(findings, Finding{RuleID: rule.ruleID, Severity: rule.severity, Message: rule.gap})
		}
	}

	threshold := G0ReadinessThreshold
	passed := score >= threshold
	return ReadinessGateResult{
		Gate:      "G0-readiness",
		Passed:    passed,
		Score:     score,
		Threshold: threshold,
		Findings:  findings,
		Detail:    formatReadinessDetail(passed, score, threshold, classification, findings),
	}
}
