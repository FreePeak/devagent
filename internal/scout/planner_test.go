package scout

import (
	"strings"
	"testing"
)

// Ported from test/planner-pipeline.test.ts (planner halves only — the
// logger/config halves belong to their own packages' ports).

func ticket(over func(*TicketSpec)) TicketSpec {
	tk := TicketSpec{
		ID:                 "LINEAR-1",
		Title:              "Add health endpoint",
		Description:        "Add a GET /health endpoint returning service status as JSON.",
		Labels:             nil,
		AcceptanceCriteria: []string{"returns 200", "includes uptime"},
	}
	if over != nil {
		over(&tk)
	}
	return tk
}

func TestClassifyTicket(t *testing.T) {
	if got := ClassifyTicket(ticket(func(tk *TicketSpec) { tk.Title = "Add column to users" })); got != ClassMigrationRequired {
		t.Errorf("migration keyword = %q", got)
	}
	if got := ClassifyTicket(ticket(func(tk *TicketSpec) { tk.Title = "New schema index" })); got != ClassMigrationRequired {
		t.Errorf("schema keyword = %q", got)
	}
	if got := ClassifyTicket(ticket(func(tk *TicketSpec) { tk.Title = "Kafka consumer for orders" })); got != ClassConsumerOnly {
		t.Errorf("consumer keyword = %q", got)
	}
	if got := ClassifyTicket(ticket(nil)); got != ClassEndpointOnly {
		t.Errorf("default = %q", got)
	}
	// Migration hints outrank consumer hints (TS switch order).
	both := ticket(func(tk *TicketSpec) { tk.Title = "Kafka consumer for the new schema" })
	if got := ClassifyTicket(both); got != ClassMigrationRequired {
		t.Errorf("migration must outrank consumer, got %q", got)
	}
	// Word boundaries: "schematics" must not trip the schema hint.
	near := ticket(func(tk *TicketSpec) { tk.Title = "Fix schematics rendering" })
	if got := ClassifyTicket(near); got != ClassEndpointOnly {
		t.Errorf("\\b boundary leak: %q", got)
	}
}

func TestCheckSpec(t *testing.T) {
	if r := CheckSpec(ticket(nil)); !r.Sufficient {
		t.Error("sufficient spec must pass")
	}
	r := CheckSpec(ticket(func(tk *TicketSpec) { tk.Description = "fix it"; tk.AcceptanceCriteria = nil }))
	if r.Sufficient {
		t.Fatal("vague spec must be refused")
	}
	if !strings.Contains(r.Question, "acceptance criteria") {
		t.Errorf("question = %q", r.Question)
	}
	// Long-enough description without criteria is sufficient.
	long := ticket(func(tk *TicketSpec) {
		tk.Description = "This description is deliberately longer than forty characters on purpose."
		tk.AcceptanceCriteria = nil
	})
	if r := CheckSpec(long); !r.Sufficient {
		t.Error("40+ char description must suffice without criteria")
	}
	// Short description but criteria present is sufficient.
	crit := ticket(func(tk *TicketSpec) { tk.Description = "fix it"; tk.AcceptanceCriteria = []string{"tests pass"} })
	if r := CheckSpec(crit); !r.Sufficient {
		t.Error("criteria must satisfy the spec check")
	}
	// Empty title always refused.
	noTitle := ticket(func(tk *TicketSpec) { tk.Title = "   " })
	if r := CheckSpec(noTitle); r.Sufficient || r.Question != "Ticket has no title. What should be delivered?" {
		t.Errorf("empty title check = %+v", r)
	}
}

func TestPlanFromTicket(t *testing.T) {
	plan := PlanFromTicket(ticket(func(tk *TicketSpec) { tk.Title = "Alter table users add column" }))
	if plan.Classification != ClassMigrationRequired {
		t.Fatalf("classification = %q", plan.Classification)
	}
	foundDown := false
	for _, task := range plan.Tasks {
		if strings.Contains(strings.ToLower(task), "down-migration") {
			foundDown = true
		}
	}
	if !foundDown {
		t.Error("migration plan must include the down-migration task")
	}
	if len(plan.Tasks) != 5 {
		t.Errorf("migration outline = %v", plan.Tasks)
	}

	consumer := PlanFromTicket(ticket(func(tk *TicketSpec) { tk.Title = "RabbitMQ subscriber for payments" }))
	if consumer.Classification != ClassConsumerOnly {
		t.Fatalf("classification = %q", consumer.Classification)
	}
	if len(consumer.Tasks) != 4 {
		t.Errorf("consumer outline = %v", consumer.Tasks)
	}

	endpoint := PlanFromTicket(ticket(nil))
	if endpoint.Classification != ClassEndpointOnly || len(endpoint.Tasks) != 3 {
		t.Errorf("endpoint outline = %+v", endpoint)
	}
}
