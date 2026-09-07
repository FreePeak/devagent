package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Round-trip and schema-drift guards for the migration contract:
// Node-written ledgers must parse in Go, Go-written ledgers must parse in
// Node, and a field added on one side must not break the other.

// goWrittenRows are the exact bytes each append*Record must emit for a
// fully-populated record (interface declaration order, TS `JSON.stringify`
// key order). They double as the Node-side parse fixtures: paste any line
// into a Node REPL's JSON.parse and it yields the same object.
func TestGoWrittenRowsParseInNodeShape(t *testing.T) {
	repo := t.TempDir()
	AppendFixerRecord(repo, FixerRecord{
		TS: "t", Kind: "event", Event: "ci-fix-outcome", TaskID: "T1", Attempt: 1,
		PR: 9, FailedChecks: []string{"test"}, Outcome: strPtr("failed-then-green"),
		Detail: strPtr("d"),
	})
	AppendWorkerCostRecord(repo, WorkerCostRecord{
		TS: "t", Kind: "event", Event: "worker-cost", TaskID: "T1", Attempt: 1,
		Worker: "grok", CostUsdTicks: 42,
	})
	AppendWatchdogHealthRecord(repo, WatchdogHealthRecord{
		TS: "t", Kind: "event", Event: "watchdog-health", TaskID: "T1", Attempt: 1,
		Site: "herdr-pane", Worker: "omp", NoProgressTimeoutMs: 600000,
		WatchdogFired: false, ColdStartFired: true, WallClockMs: 10, ClockResets: 2,
		MeaningfulBytes: 3, IdleMs: 4, Runtime: strPtr("herdr-pane"),
		Visible: boolPtr(true), Visibility: strPtr("herdr-pane"),
	})

	raw, _ := os.ReadFile(filepath.Join(repo, LedgerDir, "events.jsonl"))
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")

	want := []string{
		`{"ts":"t","kind":"event","taskId":"T1","attempt":1,"event":"ci-fix-outcome","pr":9,"failedChecks":["test"],"outcome":"failed-then-green","detail":"d"}`,
		`{"ts":"t","kind":"event","taskId":"T1","attempt":1,"event":"worker-cost","worker":"grok","costUsdTicks":42}`,
		`{"ts":"t","kind":"event","taskId":"T1","attempt":1,"event":"watchdog-health","site":"herdr-pane","worker":"omp","noProgressTimeoutMs":600000,"watchdogFired":false,"coldStartFired":true,"wallClockMs":10,"clockResets":2,"meaningfulBytes":3,"idleMs":4,"runtime":"herdr-pane","visible":true,"visibility":"herdr-pane"}`,
	}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("row %d byte mismatch:\n got %s\nwant %s", i, lines[i], w)
		}
	}
}

func TestRoundTripAllRecordKinds(t *testing.T) {
	// Write every record kind with Go, then read the ledger back through the
	// tolerant reader: kinds, events and identity blocks must survive.
	repo := t.TempDir()
	AppendAuditRecord(repo, MakeAuditRecord("T1", 1, failWithUnmet("c"), "ts1"))
	AppendTaskInterruptRecord(repo, TaskInterruptRecord{TS: "ts2", Kind: "event", Event: "taskInterrupt", TaskID: "T2", Attempt: 2, Goal: "g", FailureClass: "fc", LastGateExcerpt: "e", Attempts: 2, TrailHash: "h"})
	AppendReleaseRecord(repo, ReleaseRecord{TS: "ts3", Kind: "event", Event: "release-created", TaskID: "T3", Attempt: 3, Tag: "v1", SHA: "s", Version: "1", Source: "cli"})
	AppendOperatorDegradedRecord(repo, OperatorDegradedRecord{TS: "ts4", Kind: "event", Event: "operator-degraded", TaskID: "T4", Attempt: 4, Role: "po", Worker: "omp", Model: "m", OK: true, Attempts: 1})
	AppendStashRecord(repo, StashRecord{TS: "ts5", Kind: "event", Event: "merge-back-stash", TaskID: "T5", Attempt: 5, StashSHA: "ss", Outcome: "retained"})
	AppendOperatorAttachRecord(repo, OperatorAttachRecord{TS: "ts6", Kind: "event", Event: "operator-attached", TaskID: "T6", Attempt: 6, PaneID: "p", Session: "s"})

	tail := ReadLedgerTail(repo, "", 0)
	if len(tail) != 6 {
		t.Fatalf("expected 6 rows, got %d", len(tail))
	}
	wantEvents := []struct {
		kind, event, taskID string
		attempt             int
	}{
		{"audit", "", "T1", 1},
		{"event", "taskInterrupt", "T2", 2},
		{"event", "release-created", "T3", 3},
		{"event", "operator-degraded", "T4", 4},
		{"event", "merge-back-stash", "T5", 5},
		{"event", "operator-attached", "T6", 6},
	}
	for i, w := range wantEvents {
		g := tail[i]
		if g.Kind != w.kind || g.Event != w.event || g.TaskID != w.taskID || g.Attempt != w.attempt {
			t.Fatalf("row %d round-trip mismatch: %+v", i, g)
		}
	}
	// Audit-only read sees exactly the one audit row.
	audits := ReadLedger(repo, "")
	if len(audits) != 1 || audits[0].TaskID != "T1" {
		t.Fatalf("audit filter round-trip mismatch: %+v", audits)
	}
	// Tail limit keeps the newest rows (TS out.slice(-limit)).
	if got := ReadLedgerTail(repo, "", 2); len(got) != 2 || got[0].TaskID != "T5" {
		t.Fatalf("limit mismatch: %+v", got)
	}
}

func TestSchemaDriftUnknownFieldsTolerated(t *testing.T) {
	// Adding a field on the Node side must not break the Go read: extra keys
	// on audit rows, an unknown event kind, and a field whose type changed.
	repo := repoWithLedger(t,
		`{"ts":"t","kind":"audit","taskId":"T1","attempt":1,"verdict":"fail","integrity":"clean","unmetCriteria":["c"],"summary":"s","brandNewField":{"nested":[1,2]},"reviewer":"bot"}`,
		`{"ts":"t","kind":"event","taskId":"T2","attempt":1,"event":"some-future-event","whatever":true}`,
		`{"ts":"t","kind":"audit","taskId":"T3","attempt":1,"verdict":"pass","integrity":"clean","unmetCriteria":[],"summary":"s","extra":1}`,
	)
	audits := ReadLedger(repo, "")
	if len(audits) != 2 {
		t.Fatalf("unknown fields must not drop audit rows, got %d", len(audits))
	}
	if !reflect.DeepEqual(audits[0].UnmetCriteria, []string{"c"}) {
		t.Fatalf("known fields lost: %+v", audits[0])
	}
	tail := ReadLedgerTail(repo, "", 0)
	if len(tail) != 3 {
		t.Fatalf("unknown event kind must still read, got %d", len(tail))
	}
	if tail[1].Event != "some-future-event" {
		t.Fatalf("event name mismatch: %+v", tail[1])
	}
	// Analytics must not blow up on drifted rows either.
	if got := ClusterFailures(repo); len(got) != 1 || got[0].Criterion != "c" {
		t.Fatalf("clusters on drifted ledger: %+v", got)
	}
	if got := SummarizeLedger(repo); got.Tasks != 2 || got.Resolved != 1 {
		t.Fatalf("summary on drifted ledger: %+v", got)
	}
}

func TestSchemaDriftGoWritesWhatNodeReads(t *testing.T) {
	// The other direction: a Go-written row must decode into the TS field
	// names a Node consumer expects (no Go-style snake/PascalCase leakage).
	repo := t.TempDir()
	AppendReleaseRecord(repo, ReleaseRecord{TS: "t", Kind: "event", Event: "release-created", TaskID: "T1", Attempt: 1, Tag: "v0.1.0", SHA: "abc", Version: "0.1.0", Source: "cli"})
	raw, _ := os.ReadFile(filepath.Join(repo, LedgerDir, "events.jsonl"))
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ts", "kind", "taskId", "attempt", "event", "tag", "sha", "version", "source"} {
		if _, ok := row[key]; !ok {
			t.Fatalf("missing TS field %q in Go-written row: %s", key, raw)
		}
	}
	if len(row) != 9 {
		t.Fatalf("unexpected extra keys: %s", raw)
	}
}

func TestRealFixtureRoundTripsThroughGoWriter(t *testing.T) {
	// Read the real Node rows and re-emit one through the Go writer: the
	// identity block must survive unchanged (byte-compatible both ways).
	repo := fixtureRepo(t, "events-node-real.jsonl")
	tail := ReadLedgerTail(repo, "", 0)
	out := t.TempDir()
	for _, r := range tail {
		if r.Event == "operator-degraded" {
			AppendOperatorDegradedRecord(out, OperatorDegradedRecord{
				TS: r.TS, Kind: r.Kind, Event: r.Event, TaskID: r.TaskID, Attempt: r.Attempt,
			})
		}
	}
	back := ReadLedgerTail(out, "", 0)
	if len(back) != 1 {
		t.Fatalf("expected 1 re-emitted row, got %d", len(back))
	}
	if back[0].TS != tail[0].TS || back[0].TaskID != tail[0].TaskID || back[0].Attempt != tail[0].Attempt {
		t.Fatalf("identity block drifted: %+v vs %+v", back[0], tail[0])
	}
}
