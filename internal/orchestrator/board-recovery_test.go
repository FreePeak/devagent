// Package file mirrors test/orchestrator/board-recovery.test.ts (FR-GO-07, issue #194).
// The CLI-spawn (`devagent board-recovery`) and shell-wiring describes are
// CliWiring-sibling scope and intentionally not ported here.
package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// brTask mirrors the TS t(id, status, attempts=2) fixture.
func brTask(id, status string, attempts ...int) map[string]any {
	a := 2
	if len(attempts) > 0 {
		a = attempts[0]
	}
	return map[string]any{"id": id, "title": id, "prompt": "p", "dependsOn": []any{}, "status": status, "attempts": a}
}

// brBoardRepo mirrors boardRepo: a repo whose .devagent-project.json holds exactly tasks.
func brBoardRepo(t *testing.T, tasks []map[string]any) string {
	t.Helper()
	repo := t.TempDir()
	board := map[string]any{
		"goal":      "ship the factory",
		"createdAt": "2026-09-01T00:00:00Z",
		"updatedAt": "2026-09-06T00:00:00Z",
		"roles":     map[string]any{"planner": "omp", "executor": "omp"},
		"tasks":     tasks,
	}
	data, err := json.MarshalIndent(board, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, BoardFile), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// brWithResilience mirrors withResilience: point the repo's paging webhook +
// retention bound at Q16 values.
func brWithResilience(t *testing.T, repo string, resilience map[string]any) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"resilience": resilience})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "devagent.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// brBoardFile reads the fixture board back.
func brBoardFile(t *testing.T, repo string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, BoardFile))
	if err != nil {
		t.Fatal(err)
	}
	var board map[string]any
	if err := json.Unmarshal(data, &board); err != nil {
		t.Fatal(err)
	}
	return board
}

func brBoardTasks(t *testing.T, repo string) []map[string]any {
	t.Helper()
	raw, ok := brBoardFile(t, repo)["tasks"].([]any)
	if !ok {
		t.Fatal("board tasks missing")
	}
	tasks := make([]map[string]any, len(raw))
	for i, el := range raw {
		tasks[i], _ = el.(map[string]any)
	}
	return tasks
}

func brArchiveFiles(t *testing.T, repo, prefix string) []string {
	t.Helper()
	dir := filepath.Join(repo, ".devagent", "archive")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

// brWriteArchive seeds .devagent/archive with a single file.
func brWriteArchive(t *testing.T, repo, name string) {
	t.Helper()
	dir := filepath.Join(repo, ".devagent", "archive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// brNow = 2026-09-07 01:02:03 local; BASE mirrors the TS BASE options.
var brNow = time.Date(2026, 9, 7, 1, 2, 3, 0, time.Local)

func brBase() RunBoardRecoveryOptions {
	return RunBoardRecoveryOptions{RecoveryOptions: RecoveryOptions{ParkedPolls: 6, RequeueAfter: 6, PollSecs: 600}, Now: brNow}
}

func brOpts(base RunBoardRecoveryOptions, mutate func(*RunBoardRecoveryOptions)) RunBoardRecoveryOptions {
	mutate(&base)
	return base
}

func TestBoardRecoveryCountBoard(t *testing.T) {
	t.Run("classifies done/open/stuck/pending the way the shell counted", func(t *testing.T) {
		counts := CountBoard([]BoardTaskLike{
			{Status: "done"}, {Status: "failed"}, {Status: "blocked"}, {Status: "pending"},
			{Status: "ready"}, {Status: "dispatched"}, {Status: "untrusted"}, {Status: "ask"},
		})
		want := BoardCounts{Total: 8, Done: 1, Open: 5, Stuck: 2, Pending: 1}
		if counts != want {
			t.Errorf("counts = %+v, want %+v", counts, want)
		}
	})

	t.Run("tolerates malformed rows: missing/odd statuses count as open, never crash", func(t *testing.T) {
		counts := CountBoard([]BoardTaskLike{{}, {Status: float64(42)}, {Status: "done"}})
		want := BoardCounts{Total: 3, Done: 1, Open: 2, Stuck: 0, Pending: 0}
		if counts != want {
			t.Errorf("counts = %+v, want %+v", counts, want)
		}
	})
}

func TestBoardRecoveryDecide(t *testing.T) {
	parked := BoardCounts{Total: 3, Done: 1, Open: 0, Stuck: 2, Pending: 0}
	base := RecoveryOptions{ParkedPolls: 6, RequeueAfter: 6, PollSecs: 600}

	t.Run("a fully-done board takes the infinity-cycle archive", func(t *testing.T) {
		got := DecideBoardRecovery(BoardCounts{Total: 4, Done: 4}, base)
		if got.Kind != IntentArchiveComplete {
			t.Errorf("kind = %q, want archive-complete (%+v)", got.Kind, got)
		}
	})

	t.Run("below the threshold the board waits, counter and sleep quoted verbatim", func(t *testing.T) {
		v := DecideBoardRecovery(parked, RecoveryOptions{ParkedPolls: 2, RequeueAfter: 6, PollSecs: 600})
		want := "2 task(s) failed/blocked (2/6); sleeping 600s"
		if v.Kind != IntentWait || v.Reason != want {
			t.Errorf("got %+v, want wait/%q", v, want)
		}
	})

	t.Run("requeue-after 0 parks forever: wait with the disabled note, never requeue", func(t *testing.T) {
		v := DecideBoardRecovery(parked, RecoveryOptions{ParkedPolls: 6, RequeueAfter: 0, PollSecs: 600})
		want := "2 task(s) failed/blocked; sleeping 600s (requeue disabled)"
		if v.Kind != IntentWait || v.Reason != want {
			t.Errorf("got %+v, want wait/%q", v, want)
		}
	})

	t.Run("at the threshold the gate requeues first", func(t *testing.T) {
		got := DecideBoardRecovery(parked, base)
		if got.Kind != IntentRequeueNow {
			t.Errorf("kind = %q, want requeue-now", got.Kind)
		}
	})

	t.Run("defensive: open work or an empty board verdicts wait, never an action", func(t *testing.T) {
		v := DecideBoardRecovery(BoardCounts{Total: 2, Open: 1, Stuck: 1}, base)
		if v.Kind != IntentWait || v.Reason != "board has 1 open task(s)" {
			t.Errorf("got %+v, want open-wait", v)
		}
		v = DecideBoardRecovery(BoardCounts{}, base)
		if v.Kind != IntentWait || v.Reason != "board has no tasks" {
			t.Errorf("got %+v, want empty-wait", v)
		}
	})

	t.Run("still-stuck after the reset archives with the failed/blocked tally", func(t *testing.T) {
		v := DecidePostRequeue(BoardCounts{Total: 3, Open: 1, Stuck: 2, Pending: 1})
		if v.Kind != PostRequeueIntentKindArchive || v.Detail != "board stuck (2 failed/blocked)" {
			t.Errorf("got %+v, want stuck-archive", v)
		}
	})

	t.Run("every task pending means the scheduler cannot dispatch: archive for the bridge", func(t *testing.T) {
		v := DecidePostRequeue(BoardCounts{Total: 2, Pending: 2})
		if v.Kind != PostRequeueIntentKindArchive || v.Detail != "board all-pending but undispatchable" {
			t.Errorf("got %+v, want all-pending-archive", v)
		}
	})

	t.Run("a reset board with done work re-dispatches: requeue verdict, no archive", func(t *testing.T) {
		v := DecidePostRequeue(BoardCounts{Total: 3, Done: 1, Pending: 2})
		if v.Kind != PostRequeueIntentKindRequeue {
			t.Errorf("got %+v, want requeue", v)
		}
	})
}

func TestBoardRecoveryFormat(t *testing.T) {
	t.Run("stamps like `date +%Y%m%d-%H%M%S` in local time, zero-padded", func(t *testing.T) {
		got := FormatTimestamp(time.Date(2026, 1, 5, 9, 8, 7, 0, time.Local))
		if got != "20260105-090807" {
			t.Errorf("stamp = %q, want 20260105-090807", got)
		}
	})

	t.Run("renders the verdict word before the first colon (the shell parses on it)", func(t *testing.T) {
		got := FormatVerdict(BoardRecoveryVerdict{Action: RecoveryActionArchive, Reason: "board stuck"})
		if got != "archive: board stuck" {
			t.Errorf("verdict = %q, want %q", got, "archive: board stuck")
		}
	})
}

func TestBoardRecoveryRun(t *testing.T) {
	t.Run("completed board: archived as board-<stamp>.json, verdict wait (next cycle re-bridges)", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done"), brTask("b", "done")})
		v, err := RunBoardRecovery(repo, brBase())
		if err != nil {
			t.Fatal(err)
		}
		want := "board complete (2 done); archived to .devagent/archive/board-20260907-010203.json"
		if v.Action != RecoveryActionWait || v.Reason != want {
			t.Errorf("verdict = %+v, want wait/%q", v, want)
		}
		if _, err := os.Stat(filepath.Join(repo, BoardFile)); !os.IsNotExist(err) {
			t.Error("board file must be gone after archive")
		}
		if got := brArchiveFiles(t, repo, "board-2026"); len(got) != 1 || got[0] != "board-20260907-010203.json" {
			t.Errorf("archive files = %v", got)
		}
	})

	t.Run("completed board prunes merged worktrees through the repo script, safe-gated on executability", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done")})
		if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(repo, "scripts", "git-cleanup-merged.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch \"$2/.cleanup-ran\"\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := RunBoardRecovery(repo, brBase()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(repo, ".cleanup-ran")); err != nil {
			t.Error("executable cleanup script must have run")
		}

		repo2 := brBoardRepo(t, []map[string]any{brTask("a", "done")})
		if err := os.MkdirAll(filepath.Join(repo2, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo2, "scripts", "git-cleanup-merged.sh"), []byte("not executable"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := RunBoardRecovery(repo2, brBase()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(repo2, ".cleanup-ran")); !os.IsNotExist(err) {
			t.Error("non-executable cleanup script must not run")
		}
	})

	t.Run("parked below threshold: wait, board untouched byte-for-byte", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done"), brTask("b", "failed"), brTask("c", "blocked")})
		before, err := os.ReadFile(filepath.Join(repo, BoardFile))
		if err != nil {
			t.Fatal(err)
		}
		v, err := RunBoardRecovery(repo, brOpts(brBase(), func(o *RunBoardRecoveryOptions) { o.ParkedPolls = 3 }))
		if err != nil {
			t.Fatal(err)
		}
		want := "2 task(s) failed/blocked (3/6); sleeping 600s"
		if v.Action != RecoveryActionWait || v.Reason != want {
			t.Errorf("verdict = %+v, want wait/%q", v, want)
		}
		after, err := os.ReadFile(filepath.Join(repo, BoardFile))
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Error("board must be untouched byte-for-byte on wait")
		}
		if got := brArchiveFiles(t, repo, "board-"); len(got) != 0 {
			t.Errorf("archive files = %v, want none", got)
		}
	})

	t.Run("requeue disabled: wait with the disabled note, board untouched", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "failed")})
		v, err := RunBoardRecovery(repo, brOpts(brBase(), func(o *RunBoardRecoveryOptions) { o.RequeueAfter = 0 }))
		if err != nil {
			t.Fatal(err)
		}
		want := "1 task(s) failed/blocked; sleeping 600s (requeue disabled)"
		if v.Action != RecoveryActionWait || v.Reason != want {
			t.Errorf("verdict = %+v, want wait/%q", v, want)
		}
		if got := brBoardTasks(t, repo)[0]["status"]; got != "failed" {
			t.Errorf("status = %v, want failed", got)
		}
	})

	t.Run("threshold with done work: requeues in place (pending, attempts reset), verdict requeue", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done"), brTask("b", "failed", 5), brTask("c", "blocked", 3)})
		v, err := RunBoardRecovery(repo, brBase())
		if err != nil {
			t.Fatal(err)
		}
		want := "reset 2 parked task(s) to pending; sleeping 600s"
		if v.Action != RecoveryActionRequeue || v.Reason != want {
			t.Errorf("verdict = %+v, want requeue/%q", v, want)
		}
		tasks := brBoardTasks(t, repo)
		for i, wantStatus := range []string{"done", "pending", "pending"} {
			if tasks[i]["status"] != wantStatus {
				t.Errorf("task %d status = %v, want %v", i, tasks[i]["status"], wantStatus)
			}
		}
		for i, wantAttempts := range []float64{2, 0, 0} {
			if tasks[i]["attempts"] != wantAttempts {
				t.Errorf("task %d attempts = %v, want %v", i, tasks[i]["attempts"], wantAttempts)
			}
		}
		if got := brArchiveFiles(t, repo, "board-"); len(got) != 0 {
			t.Errorf("archive files = %v, want none", got)
		}
	})

	t.Run("threshold on an all-failed board: requeue then undispatchable archive, verdict archive", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "failed", 4), brTask("b", "blocked", 9)})
		v, err := RunBoardRecovery(repo, brBase())
		if err != nil {
			t.Fatal(err)
		}
		want := "reset 2 parked task(s) to pending; board all-pending but undispatchable; archived to .devagent/archive/board-stuck-20260907-010203.json"
		if v.Action != RecoveryActionArchive || v.Reason != want {
			t.Errorf("verdict = %+v, want archive/%q", v, want)
		}
		if _, err := os.Stat(filepath.Join(repo, BoardFile)); !os.IsNotExist(err) {
			t.Error("board file must be gone after archive")
		}
		if got := brArchiveFiles(t, repo, "board-stuck-"); len(got) != 1 || got[0] != "board-stuck-20260907-010203.json" {
			t.Errorf("archive files = %v", got)
		}
		// the archive keeps the requeued shape — the shell wrote the reset before moving the file
		data, err := os.ReadFile(filepath.Join(repo, ".devagent", "archive", "board-stuck-20260907-010203.json"))
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Tasks []struct {
				Status   string `json:"status"`
				Attempts int    `json:"attempts"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatal(err)
		}
		for _, x := range parsed.Tasks {
			if x.Status != "pending" || x.Attempts != 0 {
				t.Errorf("archived task = %+v, want pending/0", x)
			}
		}
	})

	t.Run("missing or corrupt board reads as wait (the shell guard keeps the gate out; conservative fallback)", func(t *testing.T) {
		empty := t.TempDir()
		v, err := RunBoardRecovery(empty, brBase())
		if err != nil {
			t.Fatal(err)
		}
		if v.Action != RecoveryActionWait || v.Reason != "board unreadable" {
			t.Errorf("verdict = %+v, want wait/board unreadable", v)
		}
		corrupt := brBoardRepo(t, []map[string]any{brTask("a", "done")})
		if err := os.WriteFile(filepath.Join(corrupt, BoardFile), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		v, err = RunBoardRecovery(corrupt, brBase())
		if err != nil {
			t.Fatal(err)
		}
		if v.Action != RecoveryActionWait || v.Reason != "board unreadable" {
			t.Errorf("verdict = %+v, want wait/board unreadable", v)
		}
	})

	t.Run("archiveBoard stamps the filename and preserves the board content", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done")})
		rel, err := ArchiveBoard(repo, filepath.Join(repo, BoardFile), "board", "20260907-010203", ArchiveBoardOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(".devagent", "archive", "board-20260907-010203.json")
		if rel != want {
			t.Errorf("rel = %q, want %q", rel, want)
		}
		data, err := os.ReadFile(filepath.Join(repo, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"goal": "ship the factory"`) {
			t.Errorf("archived board content missing goal: %s", data)
		}
	})
}

func TestBoardRecoveryCumulativeCap(t *testing.T) {
	t.Run("refuses the reset at/above the cap: the capped task stays failed, the board archives with the refusal", func(t *testing.T) {
		a := brTask("a", "done")
		b := brTask("b", "failed", 5)
		b["totalAttempts"] = float64(3)
		c := brTask("c", "blocked", 2)
		c["totalAttempts"] = float64(1)
		repo := brBoardRepo(t, []map[string]any{a, b, c})
		v, err := RunBoardRecovery(repo, brOpts(brBase(), func(o *RunBoardRecoveryOptions) { o.MaxTotalAttempts = 3 }))
		if err != nil {
			t.Fatal(err)
		}
		want := "reset 1 parked task(s) to pending; 1 task(s) over cumulative attempt cap 3; board stuck (1 failed/blocked); archived to .devagent/archive/board-stuck-20260907-010203.json"
		if v.Action != RecoveryActionArchive || v.Reason != want {
			t.Errorf("verdict = %+v, want archive/%q", v, want)
		}
		// the archive keeps the decision shape: b refused the fresh budget (still
		// dispatch-dead, per-round attempts intact); c reset attempts but its
		// lifetime counter survives untouched — totalAttempts is never reset.
		data, err := os.ReadFile(filepath.Join(repo, ".devagent", "archive", "board-stuck-20260907-010203.json"))
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Tasks []map[string]any `json:"tasks"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatal(err)
		}
		wantRows := []struct {
			id       string
			status   string
			attempts float64
			hasTotal bool
			total    float64
		}{
			{"a", "done", 2, false, 0},
			{"b", "failed", 5, true, 3},
			{"c", "pending", 0, true, 1},
		}
		if len(parsed.Tasks) != len(wantRows) {
			t.Fatalf("tasks = %d rows, want %d", len(parsed.Tasks), len(wantRows))
		}
		for i, w := range wantRows {
			row := parsed.Tasks[i]
			if row["id"] != w.id || row["status"] != w.status || row["attempts"] != w.attempts {
				t.Errorf("row %d = %v, want id=%s status=%s attempts=%v", i, row, w.id, w.status, w.attempts)
			}
			if _, has := row["totalAttempts"]; has != w.hasTotal {
				t.Errorf("row %d totalAttempts presence = %v, want %v", i, has, w.hasTotal)
			}
			if w.hasTotal && row["totalAttempts"] != w.total {
				t.Errorf("row %d totalAttempts = %v, want %v", i, row["totalAttempts"], w.total)
			}
		}
	})

	t.Run("cap 0 keeps the legacy unbounded requeue regardless of lifetime history", func(t *testing.T) {
		b := brTask("b", "failed", 9)
		b["totalAttempts"] = float64(99)
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done"), b})
		v, err := RunBoardRecovery(repo, brOpts(brBase(), func(o *RunBoardRecoveryOptions) { o.MaxTotalAttempts = 0 }))
		if err != nil {
			t.Fatal(err)
		}
		want := "reset 1 parked task(s) to pending; sleeping 600s"
		if v.Action != RecoveryActionRequeue || v.Reason != want {
			t.Errorf("verdict = %+v, want requeue/%q", v, want)
		}
		tasks := brBoardTasks(t, repo)
		if tasks[1]["status"] != "pending" || tasks[1]["attempts"] != float64(0) {
			t.Errorf("task b = %v/%v, want pending/0", tasks[1]["status"], tasks[1]["attempts"])
		}
	})

	t.Run("every dead task capped: nothing resets, the board archives stuck with the refusal count", func(t *testing.T) {
		a := brTask("a", "failed", 4)
		a["totalAttempts"] = float64(4)
		b := brTask("b", "blocked", 9)
		b["totalAttempts"] = float64(6)
		repo := brBoardRepo(t, []map[string]any{a, b})
		v, err := RunBoardRecovery(repo, brOpts(brBase(), func(o *RunBoardRecoveryOptions) { o.MaxTotalAttempts = 4 }))
		if err != nil {
			t.Fatal(err)
		}
		if v.Action != RecoveryActionArchive {
			t.Errorf("action = %q, want archive", v.Action)
		}
		for _, want := range []string{
			"reset 0 parked task(s) to pending",
			"2 task(s) over cumulative attempt cap 4",
			"board stuck (2 failed/blocked)",
		} {
			if !strings.Contains(v.Reason, want) {
				t.Errorf("reason missing %q: %q", want, v.Reason)
			}
		}
		data, err := os.ReadFile(filepath.Join(repo, ".devagent", "archive", "board-stuck-20260907-010203.json"))
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Tasks []struct {
				Status string `json:"status"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatal(err)
		}
		for i, want := range []string{"failed", "blocked"} {
			if parsed.Tasks[i].Status != want {
				t.Errorf("archived task %d = %q, want %q", i, parsed.Tasks[i].Status, want)
			}
		}
	})

	t.Run("missing or non-numeric totalAttempts counts as zero lifetime history — the reset proceeds", func(t *testing.T) {
		c := brTask("c", "blocked", 3)
		c["totalAttempts"] = "many"
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done"), brTask("b", "failed", 2), c})
		v, err := RunBoardRecovery(repo, brOpts(brBase(), func(o *RunBoardRecoveryOptions) { o.MaxTotalAttempts = 3 }))
		if err != nil {
			t.Fatal(err)
		}
		want := "reset 2 parked task(s) to pending; sleeping 600s"
		if v.Action != RecoveryActionRequeue || v.Reason != want {
			t.Errorf("verdict = %+v, want requeue/%q", v, want)
		}
	})
}

func TestBoardRecoveryPaging(t *testing.T) {
	const webhook = "https://pager.invalid/hook"

	t.Run("fires board-archived for the completed-board verdict with the injected clock", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done"), brTask("b", "done")})
		brWithResilience(t, repo, map[string]any{"degradeWebhookUrl": webhook})
		var calls []struct {
			url   string
			alert BoardArchivedAlert
		}
		v, err := RunBoardRecovery(repo, brOpts(brBase(), func(o *RunBoardRecoveryOptions) {
			o.Notify = func(url string, alert BoardArchivedAlert) error {
				calls = append(calls, struct {
					url   string
					alert BoardArchivedAlert
				}{url, alert})
				return nil
			}
		}))
		if err != nil {
			t.Fatal(err)
		}
		if v.Action != RecoveryActionWait {
			t.Errorf("action = %q, want wait", v.Action)
		}
		if len(calls) != 1 {
			t.Fatalf("calls = %d, want 1", len(calls))
		}
		if calls[0].url != webhook {
			t.Errorf("url = %q, want %q", calls[0].url, webhook)
		}
		alert := calls[0].alert
		if alert.Event != "board-archived" || alert.Repo != repo || alert.Prefix != "board" ||
			alert.Path != filepath.Join(".devagent", "archive", "board-20260907-010203.json") ||
			alert.Reason != "board complete (2 done)" {
			t.Errorf("alert = %+v", alert)
		}
		// The fake clock drives the alert ts, not wall time.
		if alert.Ts != brNow.UTC().Format("2006-01-02T15:04:05.000Z07:00") {
			t.Errorf("ts = %q, want injected clock ISO", alert.Ts)
		}
	})

	t.Run("fires board-archived for the stuck verdict carrying the refusal detail", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "failed", 4), brTask("b", "blocked", 9)})
		brWithResilience(t, repo, map[string]any{"degradeWebhookUrl": webhook})
		var alerts []BoardArchivedAlert
		_, err := RunBoardRecovery(repo, brOpts(brBase(), func(o *RunBoardRecoveryOptions) {
			o.Notify = func(_ string, alert BoardArchivedAlert) error {
				alerts = append(alerts, alert)
				return nil
			}
		}))
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 1 {
			t.Fatalf("alerts = %d, want 1", len(alerts))
		}
		a := alerts[0]
		if a.Event != "board-archived" || a.Prefix != "board-stuck" ||
			a.Path != filepath.Join(".devagent", "archive", "board-stuck-20260907-010203.json") ||
			a.Reason != "board all-pending but undispatchable" {
			t.Errorf("alert = %+v", a)
		}
	})

	t.Run("a paging transport throw never fails the archive (best-effort, Q16)", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done")})
		brWithResilience(t, repo, map[string]any{"degradeWebhookUrl": webhook})
		v, err := RunBoardRecovery(repo, brOpts(brBase(), func(o *RunBoardRecoveryOptions) {
			o.Notify = func(string, BoardArchivedAlert) error { return errPagingBoom }
		}))
		if err != nil {
			t.Fatal(err)
		}
		if v.Action != RecoveryActionWait {
			t.Errorf("action = %q, want wait", v.Action)
		}
		if _, err := os.Stat(filepath.Join(repo, BoardFile)); !os.IsNotExist(err) {
			t.Error("board must be archived despite paging failure")
		}
		if got := brArchiveFiles(t, repo, "board-2026"); len(got) != 1 || got[0] != "board-20260907-010203.json" {
			t.Errorf("archive files = %v", got)
		}
	})

	t.Run("does not page when resilience.degradeWebhookUrl is unset (opt-in)", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done")})
		calls := 0
		v, err := RunBoardRecovery(repo, brOpts(brBase(), func(o *RunBoardRecoveryOptions) {
			o.Notify = func(string, BoardArchivedAlert) error { calls++; return nil }
		}))
		if err != nil {
			t.Fatal(err)
		}
		if calls != 0 {
			t.Errorf("calls = %d, want 0", calls)
		}
		if v.Action != RecoveryActionWait {
			t.Errorf("action = %q, want wait", v.Action)
		}
		if got := brArchiveFiles(t, repo, "board-2026"); len(got) != 1 || got[0] != "board-20260907-010203.json" {
			t.Errorf("archive files = %v", got)
		}
	})

	t.Run("a broken devagent.json does not turn paging into an archive failure", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done")})
		if err := os.WriteFile(filepath.Join(repo, "devagent.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		v, err := RunBoardRecovery(repo, brOpts(brBase(), func(o *RunBoardRecoveryOptions) {
			o.Notify = func(string, BoardArchivedAlert) error { return nil }
		}))
		if err != nil {
			t.Fatal(err)
		}
		if v.Action != RecoveryActionWait {
			t.Errorf("action = %q, want wait", v.Action)
		}
		if got := brArchiveFiles(t, repo, "board-2026"); len(got) != 1 || got[0] != "board-20260907-010203.json" {
			t.Errorf("archive files = %v", got)
		}
	})
}

func TestBoardRecoveryPrune(t *testing.T) {
	t.Run("keeps the newest keep stamped archives and deletes older ones", func(t *testing.T) {
		repo := t.TempDir()
		dir := filepath.Join(repo, ".devagent", "archive")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{"20260101-000000", "20260102-000000", "20260103-000000", "20260104-000000"} {
			brWriteArchive(t, repo, "board-"+s+".json")
		}
		brWriteArchive(t, repo, "board-stuck-20251231-235959.json")
		if err := os.WriteFile(filepath.Join(dir, "not-an-archive.txt"), []byte("keep me"), 0o644); err != nil {
			t.Fatal(err)
		}
		removed := PruneArchive(repo, 2)
		// Newest two by stamp survive; the four older stamped files go.
		want := []string{
			"board-20260102-000000.json",
			"board-20260101-000000.json",
			"board-stuck-20251231-235959.json",
		}
		if strings.Join(removed, ",") != strings.Join(want, ",") {
			t.Errorf("removed = %v, want %v", removed, want)
		}
		for _, keep := range []string{"board-20260104-000000.json", "board-20260103-000000.json", "not-an-archive.txt"} {
			if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
				t.Errorf("%s must survive: %v", keep, err)
			}
		}
	})

	t.Run("keep 0 is unbounded (never prunes)", func(t *testing.T) {
		repo := t.TempDir()
		brWriteArchive(t, repo, "board-20260101-000000.json")
		brWriteArchive(t, repo, "board-20260102-000000.json")
		if removed := PruneArchive(repo, 0); len(removed) != 0 {
			t.Errorf("removed = %v, want none", removed)
		}
		entries, err := os.ReadDir(filepath.Join(repo, ".devagent", "archive"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Errorf("entries = %d, want 2", len(entries))
		}
	})

	t.Run("a missing archive dir prunes to nothing without throwing", func(t *testing.T) {
		repo := t.TempDir()
		if removed := PruneArchive(repo, 1); len(removed) != 0 {
			t.Errorf("removed = %v, want none", removed)
		}
	})

	t.Run("runBoardRecovery prunes to resilience.archiveKeep on every archive", func(t *testing.T) {
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done")})
		for _, s := range []string{"20260101-000000", "20260102-000000", "20260103-000000"} {
			brWriteArchive(t, repo, "board-"+s+".json")
		}
		brWithResilience(t, repo, map[string]any{"archiveKeep": 2})
		if _, err := RunBoardRecovery(repo, brBase()); err != nil { // archives board-20260907-010203.json
			t.Fatal(err)
		}
		// Newest two: the fresh archive plus 20260103; the two older ones pruned.
		want := "board-20260103-000000.json,board-20260907-010203.json"
		var kept []string
		for _, f := range brArchiveFiles(t, repo, "board-") {
			kept = append(kept, f)
		}
		sortStrings(kept)
		if strings.Join(kept, ",") != want {
			t.Errorf("kept = %v, want [%s]", kept, want)
		}
	})

	t.Run("falls back to ArchiveRetentionKeep when archiveKeep is unset", func(t *testing.T) {
		if ArchiveRetentionKeep <= 1 {
			t.Fatalf("ArchiveRetentionKeep = %d, want > 1", ArchiveRetentionKeep)
		}
		repo := brBoardRepo(t, []map[string]any{brTask("a", "done")})
		// Seed more than the default bound so the prune has work to do.
		for i := range ArchiveRetentionKeep + 3 {
			s := time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC).Format("20060102-000000")
			brWriteArchive(t, repo, "board-"+s+".json")
		}
		if _, err := RunBoardRecovery(repo, brBase()); err != nil {
			t.Fatal(err)
		}
		kept := 0
		for _, f := range brArchiveFiles(t, repo, "board-") {
			if len(f) == len("board-YYYYMMDD-HHMMSS.json") {
				kept++
			}
		}
		// The default bound holds: exactly ArchiveRetentionKeep stamped archives remain.
		if kept != ArchiveRetentionKeep {
			t.Errorf("kept = %d, want %d", kept, ArchiveRetentionKeep)
		}
	})
}

// sortStrings is a tiny ascending sort for fixture expectations.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// errPagingBoom is the paging-throw fixture.
var errPagingBoom = &pagingError{}

type pagingError struct{}

func (*pagingError) Error() string { return "connect ECONNREFUSED 127.0.0.1:443" }
