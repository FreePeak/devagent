package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/orchestrator"
)

// fltBaseOpts mirrors the TS baseOpts helper.
func fltBaseOpts(runOne func(FleetRunArgs) FleetRunResult) FleetOptions {
	return FleetOptions{
		TicketIDs:   []string{"ENG-1"},
		Entries:     []FleetEntry{{Name: "api", Path: "/repos/api"}, {Name: "worker", Path: "/repos/worker"}},
		Concurrency: orchestrator.ConcurrencyValue{Value: 2},
		TimeoutMs:   1000,
		Worker:      "claude-code",
		AutoPr:      false,
		MaxLoops:    3,
		RunOne:      runOne,
	}
}

func TestFleetRunsEveryTicketRepoCombination(t *testing.T) {
	var calls int32
	runOne := func(FleetRunArgs) FleetRunResult {
		atomic.AddInt32(&calls, 1)
		return FleetRunResult{OK: true, Summary: "completed"}
	}
	opts := fltBaseOpts(runOne)
	opts.TicketIDs = []string{"A", "B"}
	r := RunFleet(opts)
	if len(r.Items) != 4 {
		t.Fatalf("items = %d, want 4", len(r.Items))
	}
	if r.Succeeded != 4 || r.Failed != 0 {
		t.Fatalf("succeeded/failed = %d/%d, want 4/0", r.Succeeded, r.Failed)
	}
	if calls != 4 {
		t.Fatalf("runOne calls = %d, want 4", calls)
	}
}

func TestFleetRespectsConcurrencyBound(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	var calls int32
	runOne := func(FleetRunArgs) FleetRunResult {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		atomic.AddInt32(&calls, 1)
		return FleetRunResult{OK: true, Summary: "completed"}
	}
	opts := fltBaseOpts(runOne)
	opts.TicketIDs = []string{"1", "2", "3", "4", "5"}
	opts.Concurrency = orchestrator.ConcurrencyValue{Value: 2}
	r := RunFleet(opts)
	mu.Lock()
	p := peak
	mu.Unlock()
	if p > 2 {
		t.Fatalf("peak concurrency = %d, want <= 2", p)
	}
	if calls != 10 {
		t.Fatalf("runOne calls = %d, want 10 (5 tickets x 2 repos)", calls)
	}
	if r.Succeeded != 10 || r.Failed != 0 {
		t.Fatalf("succeeded/failed = %d/%d, want 10/0", r.Succeeded, r.Failed)
	}
}

func TestFleetAutoDefaultsToTwoWithoutGovernor(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	runOne := func(FleetRunArgs) FleetRunResult {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return FleetRunResult{OK: true, Summary: "completed"}
	}
	opts := fltBaseOpts(runOne)
	opts.TicketIDs = []string{"1", "2", "3", "4", "5"}
	opts.Concurrency = orchestrator.ConcurrencyValue{Auto: true}
	RunFleet(opts)
	mu.Lock()
	p := peak
	mu.Unlock()
	if p > 2 {
		t.Fatalf("auto without governor: peak concurrency = %d, want <= 2 (TS default)", p)
	}
}

func TestFleetAutoUsesGovernorEffectiveAuto(t *testing.T) {
	g := orchestrator.NewResourceGovernor(nil)
	g.InjectSnapshot(&orchestrator.OsSnapshot{TotalMem: 8 << 30, FreeMem: 8 << 30, Cpus: 4})
	var mu sync.Mutex
	inFlight, peak := 0, 0
	runOne := func(FleetRunArgs) FleetRunResult {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return FleetRunResult{OK: true, Summary: "completed"}
	}
	opts := fltBaseOpts(runOne)
	opts.TicketIDs = []string{"1", "2", "3", "4", "5", "6", "7", "8"}
	opts.Concurrency = orchestrator.ConcurrencyValue{Auto: true}
	opts.Governor = g
	r := RunFleet(opts)
	mu.Lock()
	p := peak
	mu.Unlock()
	// 8GiB / 1GiB-per-worker est and 4 CPUs cap the pool below 8.
	want := g.EffectiveAuto(*g.GetSnapshotSync())
	if want > 8 {
		want = 8
	}
	if p > want {
		t.Fatalf("auto with governor: peak = %d, want <= effective %d", p, want)
	}
	if r.Succeeded != 16 {
		t.Fatalf("succeeded = %d, want 16", r.Succeeded)
	}
}

func TestFleetIsolatesFailures(t *testing.T) {
	runOne := func(args FleetRunArgs) FleetRunResult {
		if args.RepoPath == "/repos/api" {
			panic("repo exploded")
		}
		return FleetRunResult{OK: true, Summary: "completed"}
	}
	opts := fltBaseOpts(runOne)
	opts.TicketIDs = []string{"A", "B"}
	r := RunFleet(opts)
	if r.Succeeded != 2 || r.Failed != 2 {
		t.Fatalf("succeeded/failed = %d/%d, want 2/2", r.Succeeded, r.Failed)
	}
	for _, item := range r.Items {
		if item.OK {
			continue
		}
		if want := "repo exploded"; item.Summary != want {
			t.Fatalf("failed item summary = %q, want %q", item.Summary, want)
		}
	}
}

func TestFleetRecordsPerRunLogPaths(t *testing.T) {
	runOne := func(FleetRunArgs) FleetRunResult { return FleetRunResult{OK: true, Summary: "completed"} }
	r := RunFleet(fltBaseOpts(runOne))
	for _, item := range r.Items {
		if got := item.LogPath; len(got) < 6 || got[len(got)-6:] != ".jsonl" {
			t.Fatalf("logPath = %q, want a .jsonl path", got)
		}
	}
}

func TestFleetHandlesEmptyInputs(t *testing.T) {
	called := false
	runOne := func(FleetRunArgs) FleetRunResult { called = true; return FleetRunResult{} }
	opts := fltBaseOpts(runOne)
	opts.Entries = nil
	r := RunFleet(opts)
	if len(r.Items) != 0 {
		t.Fatalf("items = %d, want 0", len(r.Items))
	}
	if called {
		t.Fatal("runOne must not be called for empty entries")
	}
}

func TestFleetPassesRunOneArgs(t *testing.T) {
	var got FleetRunArgs
	runOne := func(args FleetRunArgs) FleetRunResult {
		got = args
		return FleetRunResult{OK: true, Summary: "completed"}
	}
	opts := fltBaseOpts(runOne)
	opts.TicketIDs = []string{"E-9"}
	r := RunFleet(opts)
	if len(r.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(r.Items))
	}
	if got.TicketID != "E-9" {
		t.Fatalf("ticketId = %q, want E-9", got.TicketID)
	}
	if got.Worker != "claude-code" || got.AutoPr || got.MaxLoops != 3 || got.TimeoutMs != 1000 {
		t.Fatalf("runOne args mismatch: %+v", got)
	}
	if got.RepoPath != "/repos/api" && got.RepoPath != "/repos/worker" {
		t.Fatalf("repoPath = %q, want one of the entries", got.RepoPath)
	}
	if got.Log == nil {
		t.Fatal("runOne must receive a non-nil log")
	}
}

// TestFleetPressureWaitExitsOnTimeout pins the auto-mode pressure loop: with a
// governor whose effective concurrency stays below active runs, the worker
// must stop waiting after pressureWaitTimeoutMs (~60s default is too slow for
// a test, so we only pin that the loop exits and drains all jobs via the
// injected snapshot path instead).
func TestFleetAutoDrainsJobsDespitePressure(t *testing.T) {
	g := orchestrator.NewResourceGovernor(nil)
	g.InjectSnapshot(&orchestrator.OsSnapshot{TotalMem: 1 << 30, FreeMem: 0, Cpus: 1})
	var mu sync.Mutex
	var calls int32
	runOne := func(FleetRunArgs) FleetRunResult {
		mu.Lock()
		calls++
		mu.Unlock()
		return FleetRunResult{OK: true, Summary: "completed"}
	}
	opts := fltBaseOpts(runOne)
	opts.TicketIDs = []string{"1", "2", "3"}
	opts.Entries = []FleetEntry{{Name: "solo", Path: "/repos/solo"}}
	opts.Concurrency = orchestrator.ConcurrencyValue{Auto: true}
	opts.Governor = g
	r := RunFleet(opts)
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("calls = %d, want 3 (pressure wait must not drop jobs)", calls)
	}
	if r.Failed != 0 || r.Succeeded != 3 {
		t.Fatalf("succeeded/failed = %d/%d, want 3/0", r.Succeeded, r.Failed)
	}
}

func TestFleetConcurrencyFloorOfOne(t *testing.T) {
	// TS: non-auto resolveEffective = max(1, floor(n)) — 0/negative clamps to 1.
	var mu sync.Mutex
	inFlight, peak := 0, 0
	runOne := func(FleetRunArgs) FleetRunResult {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return FleetRunResult{OK: true, Summary: "completed"}
	}
	opts := fltBaseOpts(runOne)
	opts.TicketIDs = []string{"1", "2", "3"}
	opts.Concurrency = orchestrator.ConcurrencyValue{Value: 0}
	RunFleet(opts)
	mu.Lock()
	p := peak
	mu.Unlock()
	if p != 1 {
		t.Fatalf("concurrency 0 clamps to pool 1: peak = %d, want 1", p)
	}
}

// TestFleetAutoGovernorSnapshotInjection mirrors the TS governorSnapshot
// option: an injected snapshot bypasses the governor's cached read.
func TestFleetAutoGovernorSnapshotInjection(t *testing.T) {
	g := orchestrator.NewResourceGovernor(nil)
	g.InjectSnapshot(&orchestrator.OsSnapshot{TotalMem: 1 << 30, FreeMem: 0, Cpus: 1})
	injected := orchestrator.OsSnapshot{TotalMem: 32 << 30, FreeMem: 32 << 30, Cpus: 16}
	opts := fltBaseOpts(func(FleetRunArgs) FleetRunResult { return FleetRunResult{OK: true, Summary: "completed"} })
	opts.TicketIDs = []string{fmt.Sprintf("t%d", 1)}
	opts.Concurrency = orchestrator.ConcurrencyValue{Auto: true}
	opts.Governor = g
	opts.GovernorSnapshot = &injected
	// The injected snapshot must decide the pool: 16 cpus / 32GiB → much
	// larger than the cached 1-cpu snapshot would allow. Peak is capped by
	// job count (2), so just pin that the run completes and the effective
	// resolution matches the injected snapshot.
	r := RunFleet(opts)
	if r.Succeeded != 2 {
		t.Fatalf("succeeded = %d, want 2", r.Succeeded)
	}
	if eff := g.EffectiveAuto(injected); eff < 2 {
		t.Fatalf("EffectiveAuto(injected) = %d, want >= 2 (pool size uses it)", eff)
	}
}

// --- planner (orchestrate_planner.go) tests ---

func TestPlannerParsePlanValid(t *testing.T) {
	out := "Sure! Here is the plan:\n```json\n" +
		`[{"id":"T2","title":"Add helper","prompt":"write helper","acceptanceCriteria":["src/x.ts exists","npm test passes","  "],"constraints":["do not touch src/other.ts"],"dependsOn":["T1"]},` +
		` {"id":"T1","title":"First","prompt":"do it","dependsOn":[]}]` +
		"\n```\nHope that helps."
	tasks := ParsePlan(out)
	if tasks == nil {
		t.Fatal("plan rejected, want accepted")
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(tasks))
	}
	if tasks[0].ID != "T2" || tasks[1].ID != "T1" {
		t.Fatalf("ids = %q,%q", tasks[0].ID, tasks[1].ID)
	}
	if len(tasks[0].AcceptanceCriteria) != 2 {
		t.Fatalf("criteria = %v, want blanks filtered", tasks[0].AcceptanceCriteria)
	}
	if len(tasks[0].BoundaryConstraints) != 1 {
		t.Fatalf("constraints = %v", tasks[0].BoundaryConstraints)
	}
	if len(tasks[0].DependsOn) != 1 || tasks[0].DependsOn[0] != "T1" {
		t.Fatalf("dependsOn = %v, want [T1]", tasks[0].DependsOn)
	}
	if tasks[0].Status != orchestrator.TaskStatusPending || tasks[0].Attempts != 0 {
		t.Fatalf("status/attempts = %v/%d", tasks[0].Status, tasks[0].Attempts)
	}
}

func TestPlannerParsePlanSynthesizesIDs(t *testing.T) {
	tasks := ParsePlan(`[{"title":"a","prompt":"b"},{"title":"c","prompt":"d"}]`)
	if tasks == nil || tasks[0].ID != "T1" || tasks[1].ID != "T2" {
		t.Fatalf("tasks = %+v", tasks)
	}
}

func TestPlannerParsePlanRejections(t *testing.T) {
	cases := []string{
		"",
		"no json here",
		"[]",
		`[{"prompt":"b"}]`,
		`[{"title":"  ","prompt":"b"}]`,
		`[{"title":"a"}]`,
		`[{"title":"a","prompt":"b"},{"id":"T1","title":"c","prompt":"d"}]`,
	}
	for i, c := range cases {
		if got := ParsePlan(c); got != nil {
			t.Fatalf("case %d accepted, want nil: %q", i, c)
		}
	}
	cycle := `[{"id":"A","title":"a","prompt":"p","dependsOn":["B"]},{"id":"B","title":"b","prompt":"p","dependsOn":["A"]}]`
	if got := ParsePlan(cycle); got != nil {
		t.Fatal("cycle accepted, want nil")
	}
}

func TestPlannerParsePlanTitleAndCriteriaCaps(t *testing.T) {
	longTitle := strings.Repeat("x", 200)
	crit := `["c1","c2","c3","c4","c5","c6","c7","c8","c9","c10","c11","c12"]`
	out := `[{"title":"` + longTitle + `","prompt":"p","acceptanceCriteria":` + crit + `}]`
	tasks := ParsePlan(out)
	if tasks == nil {
		t.Fatal("plan rejected")
	}
	if len([]rune(tasks[0].Title)) != 120 {
		t.Fatalf("title runes = %d, want 120", len([]rune(tasks[0].Title)))
	}
	if len(tasks[0].AcceptanceCriteria) != 10 {
		t.Fatalf("criteria = %d, want capped at 10", len(tasks[0].AcceptanceCriteria))
	}
}

func TestPlannerParsePlanOverTwelveRejected(t *testing.T) {
	var items []string
	for i := 0; i < 13; i++ {
		items = append(items, fmt.Sprintf(`{"title":"t%d","prompt":"p"}`, i))
	}
	if got := ParsePlan("[" + strings.Join(items, ",") + "]"); got != nil {
		t.Fatal("13 tasks accepted, want nil")
	}
}

func TestPlannerParsePlanUnknownDepsDropped(t *testing.T) {
	tasks := ParsePlan(`[{"id":"A","title":"a","prompt":"p","dependsOn":["Z","A","A"]}]`)
	if tasks == nil {
		t.Fatal("plan rejected")
	}
	if len(tasks[0].DependsOn) != 0 {
		t.Fatalf("dependsOn = %v, want [] (unknown + self dropped)", tasks[0].DependsOn)
	}
}

func TestPlannerHasCycle(t *testing.T) {
	a := orchestrator.OrchestratorTask{ID: "A", DependsOn: []string{"B"}}
	b := orchestrator.OrchestratorTask{ID: "B"}
	if PlannerHasCycle([]orchestrator.OrchestratorTask{a, b}) {
		t.Fatal("acyclic graph reported cyclic")
	}
	c := orchestrator.OrchestratorTask{ID: "B", DependsOn: []string{"A"}}
	if !PlannerHasCycle([]orchestrator.OrchestratorTask{a, c}) {
		t.Fatal("cyclic graph reported acyclic")
	}
}

func TestPlannerFallbackPlan(t *testing.T) {
	tasks := FallbackPlan("ship the feature")
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if tasks[0].ID != "T1" || tasks[0].Title != "Implement goal: ship the feature" || tasks[0].Prompt != "ship the feature" {
		t.Fatalf("fallback = %+v", tasks[0])
	}
	if len(tasks[0].DependsOn) != 0 || tasks[0].Status != orchestrator.TaskStatusPending {
		t.Fatalf("fallback = %+v", tasks[0])
	}
}

func TestPlannerFallbackPlanTruncatesTitle(t *testing.T) {
	goal := strings.Repeat("g", 120)
	tasks := FallbackPlan(goal)
	if want := "Implement goal: " + strings.Repeat("g", 80); tasks[0].Title != want {
		t.Fatalf("title = %q", tasks[0].Title)
	}
	if tasks[0].Prompt != goal {
		t.Fatal("prompt must carry the full goal")
	}
}

// plnFakeDispatcher records prompts and returns canned responses.
type plnFakeDispatcher struct {
	prompts   []string
	responses []orchestrator.WorkerDispatchResult
}

func (d *plnFakeDispatcher) Dispatch(req orchestrator.WorkerDispatchRequest) orchestrator.WorkerDispatchResult {
	d.prompts = append(d.prompts, req.Prompt)
	if len(d.responses) == 0 {
		return orchestrator.WorkerDispatchResult{ExitCode: 0}
	}
	r := d.responses[0]
	d.responses = d.responses[1:]
	return r
}

func TestPlannerRunPlannerParsesAndDispatches(t *testing.T) {
	fake := &plnFakeDispatcher{responses: []orchestrator.WorkerDispatchResult{{
		ExitCode:   0,
		ResultText: `[{"id":"T1","title":"one","prompt":"do it"}]`,
	}}}
	goal := "the goal"
	tasks, err := RunPlanner(goal, t.TempDir(), "omp", 1000, PlannerOptions{Dispatch: fake})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "T1" {
		t.Fatalf("tasks = %+v", tasks)
	}
	if len(fake.prompts) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(fake.prompts))
	}
	if want := plannerSystemPrompt + "\n\n## Goal\n" + goal; fake.prompts[0] != want {
		t.Fatalf("prompt mismatch:\n got %q\nwant %q", fake.prompts[0], want)
	}
}

func TestPlannerRunPlannerEmptyOutputRetriesOnce(t *testing.T) {
	fake := &plnFakeDispatcher{responses: []orchestrator.WorkerDispatchResult{
		{ExitCode: 0, ResultText: ""},
		{ExitCode: 0, ResultText: `[{"id":"T1","title":"retry","prompt":"p"}]`},
	}}
	repo := t.TempDir()
	tasks, err := RunPlanner("g", repo, "omp", 1000, PlannerOptions{Dispatch: fake})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(tasks) != 1 || tasks[0].Title != "retry" {
		t.Fatalf("tasks = %+v", tasks)
	}
	if len(fake.prompts) != 2 {
		t.Fatalf("dispatches = %d, want 2 (one retry)", len(fake.prompts))
	}
	if _, err := os.Stat(filepath.Join(repo, ".devagent-planner")); !os.IsNotExist(err) {
		t.Fatal("successful retry must not write diagnostics")
	}
}

func TestPlannerRunPlannerFallsBackAndPersistsDiagnostics(t *testing.T) {
	fake := &plnFakeDispatcher{responses: []orchestrator.WorkerDispatchResult{
		{ExitCode: 0, ResultText: "garbage output no json"},
	}}
	repo := t.TempDir()
	tasks, err := RunPlanner("ship it", repo, "omp", 1000, PlannerOptions{Dispatch: fake})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "T1" || tasks[0].Prompt != "ship it" {
		t.Fatalf("fallback plan = %+v", tasks)
	}
	entries, err := os.ReadDir(filepath.Join(repo, ".devagent-planner"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("diagnostics dir = %v (%v), want one plan file", entries, err)
	}
	data, _ := os.ReadFile(filepath.Join(repo, ".devagent-planner", entries[0].Name()))
	if !strings.Contains(string(data), "--- timedOut=false exitCode=0 resultBytes=22 stderr=n/a ---") {
		t.Fatalf("diagnostics header wrong: %q", data)
	}
	if !strings.Contains(string(data), "garbage output no json") {
		t.Fatalf("diagnostics must contain raw output: %q", data)
	}
}

func TestPlannerRunPlannerTimeoutSkipsRetryAndFallsBack(t *testing.T) {
	fake := &plnFakeDispatcher{responses: []orchestrator.WorkerDispatchResult{
		{ExitCode: -1, TimedOut: true},
	}}
	tasks, err := RunPlanner("g", t.TempDir(), "omp", 1000, PlannerOptions{Dispatch: fake})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(tasks) != 1 || tasks[0].Prompt != "g" {
		t.Fatalf("fallback plan = %+v", tasks)
	}
	if len(fake.prompts) != 1 {
		t.Fatalf("dispatches = %d, want 1 (no retry after timeout)", len(fake.prompts))
	}
}

func TestPlannerRunPlannerUnknownWorkerErrors(t *testing.T) {
	if _, err := RunPlanner("g", t.TempDir(), "not-a-worker", 1000, PlannerOptions{}); err == nil {
		t.Fatal("unknown worker must return an error")
	} else if !strings.Contains(err.Error(), "not-a-worker") {
		t.Fatalf("err = %v, want unknown-worker message", err)
	}
}

// plnRecoveryArgs builds the RunRecoveryPlanner args literal.
func plnRecoveryArgs(goal string, task orchestrator.OrchestratorTask, repo string, opts PlannerOptions) struct {
	Goal          string
	Task          orchestrator.OrchestratorTask
	RepoPath      string
	PlannerWorker WorkerName
	TimeoutMs     int
	Opts          PlannerOptions
} {
	return struct {
		Goal          string
		Task          orchestrator.OrchestratorTask
		RepoPath      string
		PlannerWorker WorkerName
		TimeoutMs     int
		Opts          PlannerOptions
	}{goal, task, repo, "omp", 1000, opts}
}

func TestPlannerRunRecoveryPlannerContract(t *testing.T) {
	fake := &plnFakeDispatcher{responses: []orchestrator.WorkerDispatchResult{{
		ExitCode:   0,
		ResultText: `{"prompt":"new approach","acceptanceCriteria":["tests pass"]}`,
	}}}
	task := orchestrator.OrchestratorTask{
		ID: "T1", Title: "the title", Prompt: "old prompt",
		AcceptanceCriteria: []string{"ac1", "ac2"}, Attempts: 2,
		EvidenceGaps: []string{"gap1"},
	}
	c := RunRecoveryPlanner(plnRecoveryArgs("the goal", task, t.TempDir(), PlannerOptions{Dispatch: fake}))
	if c == nil {
		t.Fatal("contract = nil, want parsed")
	}
	if c.Prompt != "new approach" || len(c.AcceptanceCriteria) != 1 || c.AcceptanceCriteria[0] != "tests pass" {
		t.Fatalf("contract = %+v", c)
	}
	p := fake.prompts[0]
	for _, want := range []string{
		recoverySystemPrompt,
		"## Project goal\nthe goal",
		"## Failed task T1: the title\nold prompt",
		"Acceptance criteria were:\n- ac1\n- ac2",
		"Evidence gaps:\n- gap1",
		"\nAttempts used: 2",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("recovery prompt missing %q:\n%s", want, p)
		}
	}
}

func TestPlannerRunRecoveryPlannerAuditNote(t *testing.T) {
	fake := &plnFakeDispatcher{responses: []orchestrator.WorkerDispatchResult{{ExitCode: 0, ResultText: `{"prompt":"retry with fresh eyes"}`}}}
	task := orchestrator.OrchestratorTask{ID: "T1", Title: "t", Prompt: "p", Audit: &orchestrator.AuditVerdict{
		Verdict: "fail", Integrity: "clean",
		CriteriaResults: []orchestrator.CriterionResult{{Criterion: "c1", Met: false, Evidence: "nope"}},
	}}
	c := RunRecoveryPlanner(plnRecoveryArgs("g", task, t.TempDir(), PlannerOptions{Dispatch: fake}))
	if c == nil {
		t.Fatal("contract = nil")
	}
	if !strings.Contains(fake.prompts[0], "Latest audit (fail/clean):\n- c1: UNMET — nope") {
		t.Fatalf("audit note missing:\n%s", fake.prompts[0])
	}
}

func TestPlannerRunRecoveryPlannerFailurePaths(t *testing.T) {
	task := orchestrator.OrchestratorTask{ID: "T1", Title: "t", Prompt: "p"}
	// timeout → nil
	fake := &plnFakeDispatcher{responses: []orchestrator.WorkerDispatchResult{{ExitCode: -1, TimedOut: true}}}
	if c := RunRecoveryPlanner(plnRecoveryArgs("g", task, t.TempDir(), PlannerOptions{Dispatch: fake})); c != nil {
		t.Fatal("timeout must yield nil contract")
	}
	// malformed output → nil
	fake2 := &plnFakeDispatcher{responses: []orchestrator.WorkerDispatchResult{{ExitCode: 0, ResultText: "no contract"}}}
	if c := RunRecoveryPlanner(plnRecoveryArgs("g", task, t.TempDir(), PlannerOptions{Dispatch: fake2})); c != nil {
		t.Fatal("malformed output must yield nil contract")
	}
	// unknown worker → error surfaces from RunPlanner (same seam)
	if _, err := RunPlanner("g", t.TempDir(), "bogus", 1000, PlannerOptions{}); err == nil {
		t.Fatal("unknown worker must error")
	}
}

func TestPlannerParseRecoveryContract(t *testing.T) {
	if c := ParseRecoveryContract(`prose {"prompt":"p","acceptanceCriteria":["a","  "]} tail`); c != nil {
		t.Fatal("blank criterion accepted, want nil")
	}
	if c := ParseRecoveryContract(`{"prompt":"p","acceptanceCriteria":["a","b"]}`); c == nil || len(c.AcceptanceCriteria) != 2 {
		t.Fatalf("contract = %+v", c)
	}
	if c := ParseRecoveryContract(`{"prompt":"  "}`); c != nil {
		t.Fatal("blank prompt accepted")
	}
	if c := ParseRecoveryContract(`{"acceptanceCriteria":["a"]}`); c != nil {
		t.Fatal("missing prompt accepted")
	}
	if c := ParseRecoveryContract(`{"prompt":"p","acceptanceCriteria":[1]}`); c != nil {
		t.Fatal("non-string criterion accepted")
	}
	if c := ParseRecoveryContract(`{"prompt":"p"}`); c == nil || c.AcceptanceCriteria != nil {
		t.Fatalf("absent criteria must stay nil: %+v", c)
	}
	if c := ParseRecoveryContract("no braces"); c != nil {
		t.Fatal("no-braces text accepted")
	}
}
