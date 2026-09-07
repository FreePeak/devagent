package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The ledger JSONL schema is the migration contract: byte-compatible in both
// directions. Fixtures under testdata/ are real Node-written rows (copied
// verbatim from .devagent/runs/orchestration/events.jsonl and the shapes the
// Node test/ledger.test.ts pins); Go-written rows must parse in Node and
// Node-written rows must parse here.

func fixtureRepo(t *testing.T, name string) string {
	t.Helper()
	repo := t.TempDir()
	dir := filepath.Join(repo, LedgerDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func repoWithLedger(t *testing.T, lines ...string) string {
	t.Helper()
	repo := t.TempDir()
	if len(lines) == 0 {
		return repo
	}
	dir := filepath.Join(repo, LedgerDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// failWithUnmet mirrors the vitest helper.
func failWithUnmet(criterion string) Verdict {
	return Verdict{
		Verdict:   "fail",
		Integrity: "clean",
		CriteriaResults: []CriterionResult{
			{Criterion: criterion, Met: false, Evidence: "grep found nothing"},
		},
		Summary: "not done",
	}
}

var passVerdict = Verdict{
	Verdict:   "pass",
	Integrity: "clean",
	CriteriaResults: []CriterionResult{
		{Criterion: "tests green", Met: true, Evidence: "npm test: 3 passed"},
	},
	Summary: "verified",
}

// --- Node-written fixtures parse in Go (one direction of the contract) ---

func TestRealNodeRowsParse(t *testing.T) {
	repo := fixtureRepo(t, "events-node-real.jsonl")
	tail := ReadLedgerTail(repo, "", 0)
	if len(tail) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(tail))
	}
	if tail[0].Event != "operator-degraded" || tail[0].TaskID != "operator-preflight" || tail[0].Attempt != 3 {
		t.Fatalf("row 0 mismatch: %+v", tail[0])
	}
	if tail[1].Event != "loop-result" {
		t.Fatalf("row 1 mismatch: %+v", tail[1])
	}
	if tail[2].Event != "watchdog-health" || tail[2].TaskID != "TASK-mtn9f85c-83s7" {
		t.Fatalf("row 2 mismatch: %+v", tail[2])
	}
	if tail[4].Event != "loop-phase" {
		t.Fatalf("row 4 mismatch: %+v", tail[4])
	}
	// Unknown fields on real rows must not break the read (schema-drift
	// guard: fields the Go structs never heard of — loop, status, runtime,
	// visible, ... — are simply carried and ignored by the read).
	if got := ReadLedger(repo, ""); len(got) != 0 {
		t.Fatalf("event-only fixture must yield no audit rows, got %d", len(got))
	}
}

// --- Go-written rows are byte-identical to what Node writes ---

func TestAuditRecordByteShape(t *testing.T) {
	rec := MakeAuditRecord("T1", 1, failWithUnmet("b holds"), "2026-08-24T02:00:00Z")
	line, err := marshalLine(rec)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ts":"2026-08-24T02:00:00Z","kind":"audit","taskId":"T1","attempt":1,"verdict":"fail","integrity":"clean","unmetCriteria":["b holds"],"summary":"not done"}` + "\n"
	if string(line) != want {
		t.Fatalf("byte shape mismatch:\n got %s\nwant %s", line, want)
	}
	// A pass audit serializes "unmetCriteria":[] — never omitted.
	recPass := MakeAuditRecord("T1", 2, passVerdict, "2026-08-24T02:05:00Z")
	linePass, _ := marshalLine(recPass)
	if !strings.Contains(string(linePass), `"unmetCriteria":[]`) {
		t.Fatalf("pass audit must carry empty unmetCriteria: %s", linePass)
	}
	if !strings.Contains(string(linePass), `"integrity":"clean"`) {
		t.Fatalf("identity fields missing: %s", linePass)
	}
}

func TestAppendAuditRoundTrip(t *testing.T) {
	// Port of 'appends JSONL records with identity blocks and reads them back'.
	repo := t.TempDir()
	AppendAuditRecord(repo, MakeAuditRecord("T1", 1, failWithUnmet("b holds"), "2026-08-24T02:00:00Z"))
	AppendAuditRecord(repo, MakeAuditRecord("T1", 2, passVerdict, "2026-08-24T02:05:00Z"))
	all := ReadLedger(repo, "")
	if len(all) != 2 {
		t.Fatalf("expected 2 records, got %d", len(all))
	}
	if all[1].Kind != "audit" || all[1].TaskID != "T1" || all[1].Attempt != 2 || all[1].Verdict != "pass" {
		t.Fatalf("record mismatch: %+v", all[1])
	}
	raw, err := os.ReadFile(filepath.Join(repo, LedgerDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 raw lines, got %d", len(lines))
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("Go-written row must be valid JSON: %v", err)
	}
}

func TestUnmetCriteriaAndTaskFilter(t *testing.T) {
	// Port of 'records unmet criteria for failed audits and filters by task'.
	repo := t.TempDir()
	AppendAuditRecord(repo, MakeAuditRecord("T1", 1, failWithUnmet("schema exists"), ""))
	AppendAuditRecord(repo, MakeAuditRecord("T2", 1, passVerdict, ""))
	t1 := ReadLedger(repo, "T1")
	if len(t1) != 1 {
		t.Fatalf("expected 1 record for T1, got %d", len(t1))
	}
	if !reflect.DeepEqual(t1[0].UnmetCriteria, []string{"schema exists"}) {
		t.Fatalf("unmetCriteria = %v", t1[0].UnmetCriteria)
	}
	if len(t1[0].Summary) > 500 {
		t.Fatalf("summary not bounded: %d", len(t1[0].Summary))
	}
}

func TestCorruptLinesAndMissingFile(t *testing.T) {
	// Port of 'tolerates corrupt lines and missing ledger files'.
	repo := t.TempDir()
	if got := ReadLedger(repo, ""); len(got) != 0 {
		t.Fatalf("missing ledger must read as empty, got %v", got)
	}
	AppendAuditRecord(repo, MakeAuditRecord("T1", 1, passVerdict, ""))
	file := filepath.Join(repo, LedgerDir, "events.jsonl")
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{broken json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	all := ReadLedger(repo, "")
	if len(all) != 1 { // corrupt line skipped, good record kept
		t.Fatalf("expected 1 record after corruption, got %d", len(all))
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("ledger file must exist")
	}
}

func TestNodeFixtureAuditsParse(t *testing.T) {
	// The Node-shaped fixture carries a corrupt line and a trailing blank
	// line: both must be skipped, all four valid rows parsed, two audits read.
	repo := fixtureRepo(t, "audits-node.jsonl")
	all := ReadLedger(repo, "")
	if len(all) != 3 {
		t.Fatalf("expected 3 audits, got %d", len(all))
	}
	if all[2].TaskID != "T2" || !reflect.DeepEqual(all[2].UnmetCriteria, []string{"schema exists"}) {
		t.Fatalf("row mismatch: %+v", all[2])
	}
	// kind:"event" rows in the same file are invisible to the audit filter.
	t1 := ReadLedger(repo, "T1")
	if len(t1) != 2 {
		t.Fatalf("task filter got %d", len(t1))
	}
}

// --- Tail / summary analytics (fixture parity) ---

func TestLedgerTailForFixture(t *testing.T) {
	// Port of 'returns newest-first compact history capped at n'.
	repo := fixtureRepo(t, "tail-fixture.jsonl")
	tail := LedgerTailFor(repo, "T1", 0)
	if len(tail) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(tail))
	}
	if tail[0].Attempt != 3 || tail[1].Attempt != 2 || tail[2].Attempt != 1 {
		t.Fatalf("order mismatch: %+v", tail)
	}
	if tail[0].Verdict != "pass" || tail[0].Integrity != "clean" {
		t.Fatalf("newest entry mismatch: %+v", tail[0])
	}
	if got := LedgerTailFor(repo, "T1", 2); len(got) != 2 {
		t.Fatalf("cap mismatch: %d", len(got))
	}
	if got := LedgerTailFor(repo, "TX", 0); len(got) != 0 {
		t.Fatalf("unknown task must be empty, got %d", len(got))
	}
}

func TestSummarizeLedgerFixture(t *testing.T) {
	// Port of 'computes resolved rate and mean attempts-to-pass across
	// tasks': T1 fail->pass@2, T2 pass@1, T3 never passes.
	repo := fixtureRepo(t, "summary-fixture.jsonl")
	sum := SummarizeLedger(repo)
	if sum.Tasks != 3 || sum.Audits != 4 || sum.Resolved != 2 || sum.Unresolved != 1 {
		t.Fatalf("summary mismatch: %+v", sum)
	}
	if sum.MeanAttemptsToPass == nil || *sum.MeanAttemptsToPass != 1.5 {
		t.Fatalf("meanAttemptsToPass mismatch: %v", sum.MeanAttemptsToPass)
	}
}

func TestSummarizeLedgerEmpty(t *testing.T) {
	// Port of 'reports zero state for an empty ledger without dividing by zero'.
	repo := t.TempDir()
	sum := SummarizeLedger(repo)
	if sum.Tasks != 0 || sum.Audits != 0 || sum.Resolved != 0 || sum.Unresolved != 0 {
		t.Fatalf("zero state mismatch: %+v", sum)
	}
	if sum.MeanAttemptsToPass != nil {
		t.Fatalf("empty ledger must leave meanAttemptsToPass null, got %v", *sum.MeanAttemptsToPass)
	}
}

// --- Failure clusters (fixture parity with the vitest expectations) ---

func TestClusterFailuresFixture(t *testing.T) {
	// Port of 'groups unmet criteria across tasks, counts open tasks, ranks
	// by frequency': "tests green" hits T1 (later passes) and T2 (never
	// passes), twice total, case/whitespace-normalized into one cluster.
	repo := fixtureRepo(t, "clusters-fixture.jsonl")
	clusters := ClusterFailures(repo)
	if len(clusters) != 2 {
		t.Fatalf("expected 2 clusters, got %d: %+v", len(clusters), clusters)
	}
	want0 := FailureCluster{Criterion: "Tests Green", Occurrences: 2, Tasks: []string{"T1", "T2"}, OpenTasks: 1}
	if !reflect.DeepEqual(clusters[0], want0) {
		t.Fatalf("cluster 0 mismatch:\n got %+v\nwant %+v", clusters[0], want0)
	}
	if clusters[1].Criterion != "schema exists" || clusters[1].Occurrences != 1 || clusters[1].OpenTasks != 1 {
		t.Fatalf("cluster 1 mismatch: %+v", clusters[1])
	}
}

func TestClusterFailuresIgnoresPassAndEmpty(t *testing.T) {
	repo := t.TempDir()
	AppendAuditRecord(repo, MakeAuditRecord("T1", 1, passVerdict, ""))
	if got := ClusterFailures(repo); len(got) != 0 {
		t.Fatalf("passing audits must not cluster, got %d", len(got))
	}
	if got := ClusterFailures(t.TempDir()); len(got) != 0 {
		t.Fatalf("empty ledger must yield no clusters, got %d", len(got))
	}
}

func TestClusterFailuresDistinctWording(t *testing.T) {
	// Port of 'keeps distinct criteria with different wording in separate
	// clusters' (no semantic normalization).
	repo := repoWithLedger(t,
		`{"ts":"x","kind":"audit","taskId":"T1","attempt":1,"verdict":"fail","integrity":"clean","unmetCriteria":["branch pushed"],"summary":"s"}`,
		`{"ts":"x","kind":"audit","taskId":"T2","attempt":1,"verdict":"fail","integrity":"clean","unmetCriteria":["pushed branch"],"summary":"s"}`,
	)
	clusters := ClusterFailures(repo)
	if len(clusters) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(clusters))
	}
	got := []string{clusters[0].Criterion, clusters[1].Criterion}
	if got[0] != "branch pushed" || got[1] != "pushed branch" {
		t.Fatalf("criteria = %v", got)
	}
}

// --- Failure-class clusters (fixture parity, taskInterrupt half) ---

func TestClusterFailureClassesFixture(t *testing.T) {
	// Port of 'groups taskInterrupt event rows by failureClass with
	// occurrences, distinct tasks, and a first-seen exemplar'. Audit rows
	// never join the failureClass view (readLedger's filter is why
	// clusterFailures could not see these rows); rows with a missing or
	// blank failureClass are skipped.
	repo := fixtureRepo(t, "interrupts-fixture.jsonl")
	classes := ClusterFailureClasses(repo)
	if len(classes) != 2 {
		t.Fatalf("expected 2 classes, got %d: %+v", len(classes), classes)
	}
	want0 := FailureClassCluster{FailureClass: "test-gate", Occurrences: 3, Tasks: []string{"T1", "T2"}, Exemplar: "npm test: 3 failed (same every time)"}
	if !reflect.DeepEqual(classes[0], want0) {
		t.Fatalf("class 0 mismatch:\n got %+v\nwant %+v", classes[0], want0)
	}
	want1 := FailureClassCluster{FailureClass: "worker-error", Occurrences: 1, Tasks: []string{"T3"}, Exemplar: "worker CLI crashed"}
	if !reflect.DeepEqual(classes[1], want1) {
		t.Fatalf("class 1 mismatch:\n got %+v\nwant %+v", classes[1], want1)
	}
}

func TestClusterFailureClassesIgnoresNonInterrupt(t *testing.T) {
	// Port of 'ignores audit-only and non-interrupt event ledgers; [] when
	// nothing clusters' — the release-created event row in the fixture must
	// not produce a class.
	repo := t.TempDir()
	if got := ClusterFailureClasses(repo); len(got) != 0 {
		t.Fatalf("empty ledger must yield no classes, got %d", len(got))
	}
	AppendAuditRecord(repo, MakeAuditRecord("T1", 1, failWithUnmet("tests green"), ""))
	AppendReleaseRecord(repo, ReleaseRecord{
		TS: "2026-09-07T01:00:00Z", Kind: "event", Event: "release-created",
		TaskID: "release/0.1.0", Attempt: 1, Tag: "v0.1.0", SHA: "abc123",
		Version: "0.1.0", Source: "cli",
	})
	if got := ClusterFailureClasses(repo); len(got) != 0 {
		t.Fatalf("audit+release ledger must yield no classes, got %d", len(got))
	}
}

func TestTaskInterruptRecordShape(t *testing.T) {
	// Port of 'appends a taskInterrupt row with post-mortem payload'.
	repo := t.TempDir()
	AppendTaskInterruptRecord(repo, TaskInterruptRecord{
		TS: "2026-09-01T08:00:00Z", Kind: "event", Event: "taskInterrupt",
		TaskID: "T1", Attempt: 3,
		Goal: "ship release gate", FailureClass: "test-gate",
		LastGateExcerpt: "npm test: 3 failed (same every time)",
		Attempts:        3, TrailHash: "abc123def456",
	})
	raw, err := os.ReadFile(filepath.Join(repo, LedgerDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ts":"2026-09-01T08:00:00Z","kind":"event","taskId":"T1","attempt":3,"event":"taskInterrupt","goal":"ship release gate","failureClass":"test-gate","lastGateExcerpt":"npm test: 3 failed (same every time)","attempts":3,"trailHash":"abc123def456"}` + "\n"
	if string(raw) != want {
		t.Fatalf("taskInterrupt byte shape mismatch:\n got %s\nwant %s", raw, want)
	}
}

func TestAppendCreatesMissingLedgerDir(t *testing.T) {
	// Port of 'tolerates missing ledger directory (best-effort write)' —
	// exercised for every record kind.
	repo := t.TempDir()
	AppendTaskInterruptRecord(repo, TaskInterruptRecord{
		TS: "2026-09-01T09:00:00Z", Kind: "event", Event: "taskInterrupt",
		TaskID: "T2", Attempt: 2, Goal: "fix build", FailureClass: "worker-error",
		LastGateExcerpt: "worker crashed", Attempts: 2, TrailHash: "xyz",
	})
	AppendReleaseRecord(repo, ReleaseRecord{
		TS: "2026-09-03T11:00:00Z", Kind: "event", Event: "release-created",
		TaskID: "release/0.2.0", Attempt: 1, Tag: "v0.2.0", SHA: "deadbeef",
		Version: "0.2.0", Source: "cli",
	})
	AppendFixerRecord(repo, FixerRecord{
		TS: "2026-09-01T10:00:00Z", Kind: "event", Event: "ci-fix-dispatched",
		TaskID: "T3", Attempt: 1, PR: 42, FailedChecks: []string{"lint"},
	})
	AppendPrHygieneRecord(repo, PrHygieneRecord{
		TS: "2026-09-01T11:00:00Z", Kind: "event", Event: "pr-hygiene",
		TaskID: "T4", Attempt: 1, PR: 7, Action: "closed", Reason: "base-superseded",
	})
	AppendStashRecord(repo, StashRecord{
		TS: "2026-09-01T12:00:00Z", Kind: "event", Event: "merge-back-stash",
		TaskID: "T5", Attempt: 1, StashSHA: "abc", Outcome: "restored",
	})
	AppendOperatorAttachRecord(repo, OperatorAttachRecord{
		TS: "2026-09-01T13:00:00Z", Kind: "event", Event: "operator-attached",
		TaskID: "T6", Attempt: 1, PaneID: "p1", Session: "s1",
	})
	AppendOperatorDegradedRecord(repo, OperatorDegradedRecord{
		TS: "2026-09-01T14:00:00Z", Kind: "event", Event: "operator-degraded",
		TaskID: "operator-preflight", Attempt: 1, Role: "selfbuild",
		Worker: "omp", Model: "auto/coding", OK: false, Attempts: 3,
	})
	AppendWatchdogHealthRecord(repo, WatchdogHealthRecord{
		TS: "2026-09-01T15:00:00Z", Kind: "event", Event: "watchdog-health",
		TaskID: "T7", Attempt: 1, Site: "herdr-pane", Worker: "omp",
		NoProgressTimeoutMs: 600000, WatchdogFired: true, ColdStartFired: false,
		WallClockMs: 1000, ClockResets: 1, MeaningfulBytes: 10, IdleMs: 5,
	})
	AppendWorkerCostRecord(repo, WorkerCostRecord{
		TS: "2026-09-01T16:00:00Z", Kind: "event", Event: "worker-cost",
		TaskID: "T8", Attempt: 1, Worker: "grok", CostUsdTicks: 12345,
	})
	data, err := os.ReadFile(filepath.Join(repo, LedgerDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Split(strings.TrimRight(string(data), "\n"), "\n")); got != 9 {
		t.Fatalf("expected 9 rows, got %d", got)
	}
}

func TestOptionalFieldsOmitAndSerialize(t *testing.T) {
	// TS `field?:` = key absent when undefined, present when defined — even
	// zero-valued. And TS required-but-nullable fields serialize null.
	repo := t.TempDir()
	AppendPrHygieneRecord(repo, PrHygieneRecord{
		TS: "x", Kind: "event", Event: "pr-hygiene", TaskID: "T1", Attempt: 1,
		PR: 1, Action: "flagged", Reason: "red-across-grace", GraceAgeHours: nil,
	})
	AppendPrHygieneRecord(repo, PrHygieneRecord{
		TS: "x", Kind: "event", Event: "pr-hygiene", TaskID: "T2", Attempt: 1,
		PR: 2, Action: "closed", Reason: "base-superseded",
		GraceAgeHours: floatPtr(0), Detail: strPtr(""),
	})
	raw, _ := os.ReadFile(filepath.Join(repo, LedgerDir, "events.jsonl"))
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if strings.Contains(lines[0], `"detail"`) {
		t.Fatalf("nil detail must be omitted: %s", lines[0])
	}
	if !strings.Contains(lines[0], `"graceAgeHours":null`) {
		t.Fatalf("nullable field must serialize null: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"graceAgeHours":0`) || !strings.Contains(lines[1], `"detail":""`) {
		t.Fatalf("zero-valued optionals must serialize: %s", lines[1])
	}
}

func TestSummaryBoundedTo500UTF16(t *testing.T) {
	// TS .slice(0, 500) counts UTF-16 code units, not bytes or runes.
	// Case 1: "a" x499 (499 units) + one 2-unit emoji = 501 units -> the
	// emoji is dropped whole, leaving 499 ASCII bytes.
	long := strings.Repeat("a", 499) + "🚀"
	rec := MakeAuditRecord("T1", 1, Verdict{Verdict: "fail", Integrity: "clean", Summary: long}, "")
	if got := len(rec.Summary); got != 499 {
		t.Fatalf("emoji spanning the 500-unit boundary must drop whole: %d", got)
	}
	// Case 2: 250 emoji = exactly 500 units -> all kept (1000 bytes, 250
	// runes), the 10 trailing b's are cut.
	allEmoji := strings.Repeat("🚀", 250) + strings.Repeat("b", 10)
	rec2 := MakeAuditRecord("T1", 1, Verdict{Verdict: "fail", Integrity: "clean", Summary: allEmoji}, "")
	if got := len([]rune(rec2.Summary)); got != 250 || len(rec2.Summary) != 1000 {
		t.Fatalf("250 emoji = exactly 500 units, all kept: runes=%d bytes=%d", len([]rune(rec2.Summary)), len(rec2.Summary))
	}
}
