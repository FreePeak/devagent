package orchestrator

import "testing"

// TS deps.every() is true for an empty array: a no-deps pending task must
// promote to 'ready' (Executor flagged this; regression guard).
func TestRecomputeReadinessEmptyDeps(t *testing.T) {
	got := RecomputeReadiness([]OrchestratorTask{
		{ID: "A", Status: TaskStatusPending, DependsOn: nil},
	})
	if got[0].Status != TaskStatusReady {
		t.Fatalf("status = %s, want ready", got[0].Status)
	}
}
