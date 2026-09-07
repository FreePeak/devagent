package gates

import (
	"regexp"
	"strings"
	"testing"
)

func readinessTicket(title, description string, labels, criteria []string) ReadinessTicket {
	return ReadinessTicket{
		ID:                 "ENG-1",
		Title:              title,
		Description:        description,
		Labels:             labels,
		AcceptanceCriteria: criteria,
	}
}

func defaultTicket() ReadinessTicket {
	return readinessTicket(
		"Add GET /health endpoint",
		"Endpoint returns service status as JSON including uptime and version.",
		[]string{},
		[]string{"returns 200", "has integration test"},
	)
}

func findingIDs(findings []Finding) []string {
	out := []string{}
	for _, f := range findings {
		out = append(out, f.RuleID)
	}
	return out
}

func TestEvaluateReadinessPassesWellSpecifiedTicket(t *testing.T) {
	r := EvaluateReadiness(defaultTicket(), TicketClassEndpointOnly)
	if r.Gate != "G0-readiness" {
		t.Errorf("gate = %q", r.Gate)
	}
	if !r.Passed {
		t.Errorf("expected pass, got %+v", r)
	}
	if r.Score < G0ReadinessThreshold {
		t.Errorf("score = %d, want >= %d", r.Score, G0ReadinessThreshold)
	}
	if len(r.Findings) != 0 {
		t.Errorf("expected no findings, got %v", findingIDs(r.Findings))
	}
}

func TestEvaluateReadinessRejectsUnderSpecifiedTicket(t *testing.T) {
	r := EvaluateReadiness(readinessTicket("Fix it", "", nil, nil), TicketClassEndpointOnly)
	if r.Passed {
		t.Fatalf("expected rejection: %+v", r)
	}
	if r.Score >= G0ReadinessThreshold {
		t.Errorf("score = %d, want < %d", r.Score, G0ReadinessThreshold)
	}
	ids := strings.Join(findingIDs(r.Findings), ",")
	for _, want := range []string{"G0-TITLE", "G0-DESCRIPTION", "G0-ACCEPTANCE", "G0-ENDPOINT-SURFACE", "G0-VERIFICATION"} {
		if !strings.Contains(ids, want) {
			t.Errorf("missing %s in %s", want, ids)
		}
	}
	hasHigh := false
	for _, f := range r.Findings {
		if f.Severity == SeverityHigh {
			hasHigh = true
		}
	}
	if !hasHigh {
		t.Error("common gaps should be high severity")
	}
}

func TestEvaluateReadinessTypeSpecificCriteria(t *testing.T) {
	// Migration ticket: schema entities named, but no rollback/down-migration
	// signal anywhere in title/description/criteria.
	migration := EvaluateReadiness(readinessTicket(
		"Add profile column to users table",
		"Alter table users to add a nullable profile_url column. Migration required for the new schema entity.",
		nil,
		[]string{"column exists after up", "schema matches expectation"},
	), TicketClassMigrationRequired)
	if ids := strings.Join(findingIDs(migration.Findings), ","); !strings.Contains(ids, "G0-REVERSIBILITY") {
		t.Errorf("expected G0-REVERSIBILITY, got %s", ids)
	} else if strings.Contains(ids, "G0-SCHEMA-ENTITY") {
		t.Errorf("G0-SCHEMA-ENTITY should be satisfied, got %s", ids)
	}

	// Same rules must not leak into endpoint classification.
	endpoint := EvaluateReadiness(defaultTicket(), TicketClassMigrationRequired)
	if ids := strings.Join(findingIDs(endpoint.Findings), ","); !strings.Contains(ids, "G0-SCHEMA-ENTITY") {
		t.Errorf("migration rules should apply to a non-migration ticket, got %s", ids)
	}
}

func TestEvaluateReadinessTieredAcceptanceScoring(t *testing.T) {
	two := EvaluateReadiness(defaultTicket(), TicketClassEndpointOnly)
	one := EvaluateReadiness(readinessTicket(
		"Add GET /health endpoint",
		"Endpoint returns service status as JSON including uptime and version.",
		nil, []string{"returns 200"},
	), TicketClassEndpointOnly)
	if one.Score >= two.Score {
		t.Errorf("one criterion (%d) should score below two (%d)", one.Score, two.Score)
	}
	// Zero criteria is the only finding tier; one criterion earns partial
	// credit without a finding.
	for _, r := range []ReadinessGateResult{two, one} {
		if ids := strings.Join(findingIDs(r.Findings), ","); strings.Contains(ids, "G0-ACCEPTANCE") {
			t.Errorf("unexpected G0-ACCEPTANCE with %d criteria", len(r.Findings))
		}
	}
}

func TestEvaluateReadinessSkipsUnknownClassification(t *testing.T) {
	r := EvaluateReadiness(defaultTicket(), "nonsense")
	if !r.Skipped || !r.Passed {
		t.Errorf("expected honest skip, got %+v", r)
	}
	if len(r.Findings) != 0 {
		t.Errorf("expected no findings, got %v", r.Findings)
	}
	if !strings.Contains(r.Detail, "skipped") {
		t.Errorf("detail = %q", r.Detail)
	}
}

func TestEvaluateReadinessToleratesMissingFields(t *testing.T) {
	r := EvaluateReadiness(readinessTicket("", "", nil, nil), TicketClassConsumerOnly)
	if r.Passed {
		t.Errorf("empty ticket must not pass: %+v", r)
	}
	if len(r.Findings) < 4 {
		t.Errorf("expected >= 4 findings, got %d", len(r.Findings))
	}
}

func TestEvaluateReadinessDeterministicDetailBlock(t *testing.T) {
	r := EvaluateReadiness(readinessTicket("No",
		"Endpoint returns service status as JSON including uptime and version.",
		nil, nil), TicketClassEndpointOnly)
	if r.Passed {
		t.Fatalf("expected rejection: %+v", r)
	}
	want := regexp.MustCompile(`G0 readiness \d+/100 \(threshold 60\) for endpoint-only: rejected`)
	if !want.MatchString(r.Detail) {
		t.Errorf("detail head mismatch: %q", r.Detail)
	}
	for _, id := range []string{"G0-TITLE", "G0-ACCEPTANCE"} {
		if !strings.Contains(r.Detail, id) {
			t.Errorf("detail missing %s: %q", id, r.Detail)
		}
	}
	// G0-DESCRIPTION absent because the fixture description is substantive.
	if strings.Contains(r.Detail, "G0-DESCRIPTION") {
		t.Errorf("detail should not contain G0-DESCRIPTION: %q", r.Detail)
	}
}
