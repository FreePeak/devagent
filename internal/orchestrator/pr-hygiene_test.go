// Package file mirrors test/pr-hygiene.test.ts (FR-GO-07, issue #194).

package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hygieneTempRepo(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// hygieneTaskPr mirrors taskPr: raw gh JSON for one open TASK PR.
func hygieneTaskPr(t *testing.T, overrides map[string]any) string {
	t.Helper()
	pr := map[string]any{
		"number":            9,
		"title":             "T1",
		"headRefName":       "devagent/TASK-mtioq4ik-T1-a0",
		"baseRefName":       "devagent/TASK-mtioq4ik-T0-a0",
		"state":             "OPEN",
		"mergeable":         "MERGEABLE",
		"reviewDecision":    "",
		"headRefOid":        "abc123",
		"updatedAt":         time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		"author":            map[string]any{"login": "devagent[bot]"},
		"statusCheckRollup": []map[string]any{{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"}},
	}
	for k, v := range overrides {
		pr[k] = v
	}
	b, err := json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var (
	hygieneGreen   = []map[string]any{{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"}}
	hygieneRed     = []map[string]any{{"name": "test", "status": "COMPLETED", "conclusion": "FAILURE"}}
	hygienePending = []map[string]any{{"name": "test", "status": "IN_PROGRESS", "conclusion": nil}}
)

// scriptedGh mirrors the TS scriptedGh: responses keyed by subcommand
// ("pr" → args[1], else args[0]); an error value makes the call throw.
// Records every call.
// scriptedGh mirrors the TS scriptedGh: responses keyed by subcommand
// ("pr" → args[1], else args[0]); an error value makes the call throw.
// Records every call.
func scriptedGh(responses map[string]any) (RunGh, *[][]string) {
	calls := &[][]string{}
	run := RunGh(func(args []string, cwd string) (*GhResult, error) {
		*calls = append(*calls, args)
		key := args[0]
		if args[0] == "pr" && len(args) > 1 {
			key = args[1]
		}
		r, ok := responses[key]
		if !ok {
			r = ""
		}
		switch v := r.(type) {
		case error:
			return nil, v
		case string:
			return &GhResult{Stdout: v}, nil
		default:
			return &GhResult{}, nil
		}
	})
	return run, calls
}

// scriptedGhWithDeadBases mirrors the TS helper: branch lookups (api) 404
// for the given base names, succeed for other api calls.
func scriptedGhWithDeadBases(deadBases []string, rest map[string]any) (RunGh, *[][]string) {
	calls := &[][]string{}
	run := RunGh(func(args []string, cwd string) (*GhResult, error) {
		*calls = append(*calls, args)
		if args[0] == "api" {
			joined := strings.Join(args, " ")
			for _, b := range deadBases {
				if strings.Contains(joined, b) {
					return nil, fmt.Errorf("gh: Not Found (404)")
				}
			}
			return &GhResult{}, nil
		}
		key := args[0]
		if args[0] == "pr" && len(args) > 1 {
			key = args[1]
		}
		r, ok := rest[key]
		if !ok {
			r = ""
		}
		switch v := r.(type) {
		case error:
			return nil, v
		case string:
			return &GhResult{Stdout: v}, nil
		default:
			return &GhResult{}, nil
		}
	})
	return run, calls
}

// readLedgerRows mirrors the TS helper: ledger rows under a repo, oldest first.
func readLedgerRows(t *testing.T, repo string) []map[string]any {
	t.Helper()
	file := filepath.Join(repo, ".devagent/runs/orchestration", "events.jsonl")
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	rows := []map[string]any{}
	for _, line := range splitLines(string(data)) {
		if trimSpace(line) == "" {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad ledger row %q: %v", line, err)
		}
		rows = append(rows, r)
	}
	return rows
}

func hygieneRows(t *testing.T, repo string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range readLedgerRows(t, repo) {
		if r["event"] == "pr-hygiene" {
			out = append(out, r)
		}
	}
	return out
}

func fl(t *testing.T, v any) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("expected number, got %v (%T)", v, v)
	}
	return f
}

func TestGraceAgeHours(t *testing.T) {
	t.Run("computes non-negative hours since updatedAt", func(t *testing.T) {
		now := time.Now().UnixMilli()
		ts := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		age := GraceAgeHours(ts, now)
		if age == nil {
			t.Fatal("expected non-nil age")
		}
		if *age < 47.9 || *age >= 48.1 {
			t.Fatalf("age = %v, want ~48", *age)
		}
	})

	t.Run("returns null for missing or unparseable timestamps", func(t *testing.T) {
		now := time.Now().UnixMilli()
		if GraceAgeHours("", now) != nil {
			t.Fatal("empty timestamp must be nil")
		}
		if GraceAgeHours("not-a-date", now) != nil {
			t.Fatal("garbage timestamp must be nil")
		}
	})
}

func TestSweepTaskPrHygiene(t *testing.T) {
	// Past the auto-close age floor (closeAgeFloorHours) so these fixtures
	// still exercise the close arms; the floor itself is pinned by the
	// fresh-PR subtests below.
	aged := time.Now().Add(-30 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00")
	t.Run("closes a base-superseded TASK PR (base branch deleted) and writes a ledger row", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := scriptedGhWithDeadBases([]string{"devagent/TASK-mtioq4ik-T0-a0"}, map[string]any{
			"list":    "[" + hygieneTaskPr(t, map[string]any{"updatedAt": aged}) + "]",
			"comment": "",
			"close":   "",
		})
		dry := false
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry}, run)
		if res.SkipAutoMerge {
			t.Fatal("skipAutoMerge must be false")
		}
		o := res.Outcomes[0]
		if o.Action != "closed" || o.Reason != "base-superseded" {
			t.Fatalf("outcome = %v/%v", o.Action, o.Reason)
		}
		if !strings.Contains(o.Detail, "merged or deleted") {
			t.Fatalf("detail = %q", o.Detail)
		}
		foundClose := false
		foundComment := false
		for _, c := range *calls {
			if len(c) > 1 && c[1] == "close" {
				if fmt.Sprint(c) != fmt.Sprint([]string{"pr", "close", "9"}) {
					t.Fatalf("close args = %v", c)
				}
				foundClose = true
			}
			if len(c) > 1 && c[1] == "comment" && strings.Contains(strings.Join(c, " "), "merged or deleted") {
				foundComment = true
			}
		}
		if !foundClose || !foundComment {
			t.Fatalf("close=%v comment=%v calls=%v", foundClose, foundComment, *calls)
		}
		rows := hygieneRows(t, repo)
		if len(rows) != 1 {
			t.Fatalf("expected 1 ledger row, got %d", len(rows))
		}
		r := rows[0]
		if r["kind"] != "event" || r["event"] != "pr-hygiene" ||
			fl(t, r["pr"]) != 9 || r["action"] != "closed" || r["reason"] != "base-superseded" {
			t.Fatalf("unexpected row: %v", r)
		}
		if _, ok := r["graceAgeHours"].(float64); !ok {
			t.Fatalf("graceAgeHours must be a number: %v", r["graceAgeHours"])
		}
		if _, ok := r["ts"].(string); !ok {
			t.Fatal("ts must be a string")
		}
		if _, ok := r["taskId"].(string); !ok {
			t.Fatal("taskId must be a string")
		}
		if _, ok := r["attempt"].(float64); !ok {
			t.Fatal("attempt must be a number")
		}
	})

	t.Run("a spawn timeout on the base probe leaves the PR untouched (never closes on a hiccup)", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := scriptedGh(map[string]any{
			"list": "[" + hygieneTaskPr(t, nil) + "]",
			"api":  &GhError{Message: "gh api repos/{owner}/{repo}/branches/devagent/TASK-mtioq4ik-T0-a0 exited -1: ", Code: -1}, // 30s spawn timeout: no stderr
		})
		dry := false
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry}, run)
		if res.SkipAutoMerge {
			t.Fatal("skipAutoMerge must be false")
		}
		if len(res.Outcomes) != 1 || res.Outcomes[0].Action != "untouched" || res.Outcomes[0].Reason != "base-unknown" {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		for _, c := range *calls {
			if len(c) > 1 && (c[1] == "close" || c[1] == "comment") {
				t.Fatalf("transport noise must not act: %v", c)
			}
		}
		if rows := readLedgerRows(t, repo); len(rows) != 0 {
			t.Fatalf("expected no ledger rows, got %d", len(rows))
		}
	})

	t.Run("flags base-superseded in dry-run: no comment, no close, still one ledger row", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := scriptedGhWithDeadBases([]string{"devagent/TASK-mtioq4ik-T0-a0"}, map[string]any{
			"list": "[" + hygieneTaskPr(t, map[string]any{"updatedAt": aged}) + "]",
		})
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{}, run)
		if res.SkipAutoMerge {
			t.Fatal("skipAutoMerge must be false")
		}
		if res.Outcomes[0].Action != "flagged" || !strings.Contains(res.Outcomes[0].Detail, "[dry-run]") {
			t.Fatalf("outcome = %+v", res.Outcomes[0])
		}
		for _, c := range *calls {
			if len(c) > 1 && (c[1] == "close" || c[1] == "comment") {
				t.Fatalf("must not act in dry-run: %v", c)
			}
		}
		rows := hygieneRows(t, repo)
		if len(rows) != 1 || rows[0]["action"] != "flagged" || rows[0]["reason"] != "base-superseded" {
			t.Fatalf("rows = %v", rows)
		}
	})

	t.Run("flags a red-across-grace PR, sets skipAutoMerge with autoMerge on, and writes a ledger row", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		stale := time.Now().Add(-30 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		run, calls := scriptedGh(map[string]any{
			"list": "[" + hygieneTaskPr(t, map[string]any{"baseRefName": "main", "statusCheckRollup": hygieneRed, "updatedAt": stale}) + "]",
			"api":  "",
		})
		dry := false
		on := true
		grace := 24.0
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry, AutoMerge: &on, GraceHours: &grace}, run)
		if !res.SkipAutoMerge {
			t.Fatal("skipAutoMerge must be true")
		}
		o := res.Outcomes[0]
		if o.Action != "flagged" || o.Reason != "red-across-grace" {
			t.Fatalf("outcome = %v/%v", o.Action, o.Reason)
		}
		if !strings.Contains(o.Detail, "24h grace") || !strings.Contains(o.Detail, "autoMerge skipped") {
			t.Fatalf("detail = %q", o.Detail)
		}
		// flagged, never closed
		for _, c := range *calls {
			if len(c) > 1 && c[1] == "close" {
				t.Fatalf("must not close: %v", c)
			}
		}
		rows := hygieneRows(t, repo)
		if len(rows) != 1 || rows[0]["action"] != "flagged" || rows[0]["reason"] != "red-across-grace" || fl(t, rows[0]["pr"]) != 9 {
			t.Fatalf("rows = %v", rows)
		}
		if fl(t, rows[0]["graceAgeHours"]) < 29 {
			t.Fatalf("graceAgeHours = %v, want >= 29", rows[0]["graceAgeHours"])
		}
	})

	t.Run("reports red-across-grace without skipAutoMerge when autoMerge is off", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		stale := time.Now().Add(-30 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		run, _ := scriptedGh(map[string]any{
			"list": "[" + hygieneTaskPr(t, map[string]any{"baseRefName": "main", "statusCheckRollup": hygieneRed, "updatedAt": stale}) + "]",
			"api":  "",
		})
		dry := false
		off := false
		grace := 24.0
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry, AutoMerge: &off, GraceHours: &grace}, run)
		if res.SkipAutoMerge {
			t.Fatal("skipAutoMerge must be false when autoMerge is off")
		}
	})

	t.Run("leaves a red PR within the grace window untouched (no skip, no ledger row)", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		fresh := time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		run, calls := scriptedGh(map[string]any{
			"list": "[" + hygieneTaskPr(t, map[string]any{"baseRefName": "main", "statusCheckRollup": hygieneRed, "updatedAt": fresh}) + "]",
			"api":  "",
		})
		dry := false
		on := true
		grace := 24.0
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry, AutoMerge: &on, GraceHours: &grace}, run)
		if res.SkipAutoMerge {
			t.Fatal("skipAutoMerge must be false")
		}
		if res.Outcomes[0].Action != "untouched" || res.Outcomes[0].Reason != "red-within-grace" {
			t.Fatalf("outcome = %+v", res.Outcomes[0])
		}
		for _, c := range *calls {
			if len(c) > 1 && (c[1] == "close" || c[1] == "comment") {
				t.Fatalf("must not act: %v", c)
			}
		}
		if rows := readLedgerRows(t, repo); len(rows) != 0 {
			t.Fatalf("expected no ledger rows, got %d", len(rows))
		}
	})

	t.Run("treats an unparseable updatedAt as overdue for red-across-grace", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, _ := scriptedGh(map[string]any{
			"list": "[" + hygieneTaskPr(t, map[string]any{"baseRefName": "main", "statusCheckRollup": hygieneRed, "updatedAt": "not-a-date"}) + "]",
			"api":  "",
		})
		dry := false
		grace := 24.0
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry, GraceHours: &grace}, run)
		o := res.Outcomes[0]
		if o.Action != "flagged" || o.Reason != "red-across-grace" {
			t.Fatalf("outcome = %v/%v", o.Action, o.Reason)
		}
		if o.GraceAgeHours != nil {
			t.Fatalf("graceAgeHours must be nil, got %v", *o.GraceAgeHours)
		}
	})

	t.Run("leaves green and pending TASK PRs untouched with an intact base", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := scriptedGh(map[string]any{
			"list": "[" + hygieneTaskPr(t, map[string]any{"number": 3, "statusCheckRollup": hygieneGreen}) + "," +
				hygieneTaskPr(t, map[string]any{"number": 4, "statusCheckRollup": hygienePending}) + "]",
			"api": "",
		})
		dry := false
		on := true
		grace := 0.0
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry, AutoMerge: &on, GraceHours: &grace}, run)
		if res.SkipAutoMerge {
			t.Fatal("skipAutoMerge must be false")
		}
		if len(res.Outcomes) != 2 ||
			res.Outcomes[0].PR != 3 || res.Outcomes[0].Action != "untouched" || res.Outcomes[0].Reason != "green" ||
			res.Outcomes[1].PR != 4 || res.Outcomes[1].Action != "untouched" || res.Outcomes[1].Reason != "pending" {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		for _, c := range *calls {
			if len(c) > 1 && (c[1] == "close" || c[1] == "comment") {
				t.Fatalf("must not act: %v", c)
			}
		}
		if rows := readLedgerRows(t, repo); len(rows) != 0 {
			t.Fatalf("expected no ledger rows, got %d", len(rows))
		}
	})

	t.Run("never touches non-TASK PRs regardless of state", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		stale := time.Now().Add(-100 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		listJSON := "[" +
			hygieneTaskPr(t, map[string]any{"number": 5, "headRefName": "feature/manual", "baseRefName": "gone-branch"}) + "," +
			hygieneTaskPr(t, map[string]any{"number": 6, "headRefName": "hotfix/x", "baseRefName": "main", "statusCheckRollup": hygieneRed, "updatedAt": stale}) +
			"]"
		calls := &[][]string{}
		run := RunGh(func(args []string, cwd string) (*GhResult, error) {
			*calls = append(*calls, args)
			if args[0] == "pr" && args[1] == "list" {
				return &GhResult{Stdout: listJSON}, nil
			}
			if args[0] == "api" {
				if strings.Contains(strings.Join(args, " "), "gone-branch") {
					return nil, fmt.Errorf("gh: Not Found (404)")
				}
				return &GhResult{}, nil
			}
			return &GhResult{}, nil
		})
		dry := false
		on := true
		grace := 24.0
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry, AutoMerge: &on, GraceHours: &grace}, run)
		if res.SkipAutoMerge {
			t.Fatal("skipAutoMerge must be false")
		}
		var reasons []string
		for _, o := range res.Outcomes {
			if o.Action == "untouched" {
				reasons = append(reasons, o.Reason)
			}
		}
		if len(reasons) != 2 || reasons[0] != "not-a-task-pr" || reasons[1] != "not-a-task-pr" {
			t.Fatalf("reasons = %v", reasons)
		}
		for _, c := range *calls {
			if len(c) > 1 && (c[1] == "close" || c[1] == "comment") {
				t.Fatalf("must not act: %v", c)
			}
		}
		if rows := readLedgerRows(t, repo); len(rows) != 0 {
			t.Fatalf("expected no ledger rows, got %d", len(rows))
		}
	})

	t.Run("skips non-open PRs", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := scriptedGh(map[string]any{
			"list": "[" + hygieneTaskPr(t, map[string]any{"state": "CLOSED"}) + "]",
			"api":  fmt.Errorf("gh: Not Found (404)"),
		})
		dry := false
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry}, run)
		if res.Outcomes[0].Action != "skipped" {
			t.Fatalf("outcome = %v", res.Outcomes[0].Action)
		}
		for _, c := range *calls {
			if len(c) > 1 && (c[1] == "close" || c[1] == "comment") {
				t.Fatalf("must not act: %v", c)
			}
		}
		if rows := readLedgerRows(t, repo); len(rows) != 0 {
			t.Fatalf("expected no ledger rows, got %d", len(rows))
		}
	})

	t.Run("respects a zero grace window (any red is flagged immediately)", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, _ := scriptedGh(map[string]any{
			"list": "[" + hygieneTaskPr(t, map[string]any{"baseRefName": "main", "statusCheckRollup": hygieneRed}) + "]",
			"api":  "",
		})
		dry := false
		on := true
		grace := 0.0
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry, AutoMerge: &on, GraceHours: &grace}, run)
		if !res.SkipAutoMerge {
			t.Fatal("skipAutoMerge must be true")
		}
		if res.Outcomes[0].Action != "flagged" || res.Outcomes[0].Reason != "red-across-grace" {
			t.Fatalf("outcome = %v/%v", res.Outcomes[0].Action, res.Outcomes[0].Reason)
		}
	})

	t.Run("closes a red-within-grace PR whose base is gone (dead base cannot be fixed by waiting)", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := scriptedGhWithDeadBases([]string{"devagent/TASK-mtioq4ik-T0-a0"}, map[string]any{
			"list":    "[" + hygieneTaskPr(t, map[string]any{"statusCheckRollup": hygieneRed, "updatedAt": aged}) + "]",
			"comment": "",
			"close":   "",
		})
		dry := false
		grace := 24.0
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry, GraceHours: &grace}, run)
		if res.SkipAutoMerge {
			t.Fatal("skipAutoMerge must be false")
		}
		if res.Outcomes[0].Action != "closed" || res.Outcomes[0].Reason != "base-superseded" {
			t.Fatalf("outcome = %v/%v", res.Outcomes[0].Action, res.Outcomes[0].Reason)
		}
		foundClose := false
		for _, c := range *calls {
			if len(c) > 1 && c[1] == "close" {
				if fmt.Sprint(c) != fmt.Sprint([]string{"pr", "close", "9"}) {
					t.Fatalf("close args = %v", c)
				}
				foundClose = true
			}
		}
		if !foundClose {
			t.Fatal("expected a close call")
		}
		rows := hygieneRows(t, repo)
		if len(rows) != 1 || rows[0]["action"] != "closed" || rows[0]["reason"] != "base-superseded" {
			t.Fatalf("rows = %v", rows)
		}
	})

	t.Run("writes one ledger row per action across a mixed dry-run sweep", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		stale := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		listJSON := "[" +
			hygieneTaskPr(t, map[string]any{"number": 11, "updatedAt": aged}) + "," + // base-superseded -> flagged (dry-run), past the age floor
			hygieneTaskPr(t, map[string]any{"number": 12, "baseRefName": "main", "statusCheckRollup": hygieneRed, "updatedAt": stale}) + "," + // red across grace -> flagged
			hygieneTaskPr(t, map[string]any{"number": 13, "baseRefName": "main"}) + // green -> untouched
			"]"
		run := RunGh(func(args []string, cwd string) (*GhResult, error) {
			if args[0] == "pr" && args[1] == "list" {
				return &GhResult{Stdout: listJSON}, nil
			}
			if args[0] == "api" {
				if strings.Contains(strings.Join(args, " "), "devagent/TASK-mtioq4ik-T0-a0") {
					return nil, fmt.Errorf("gh: Not Found (404)")
				}
				return &GhResult{}, nil
			}
			return &GhResult{}, nil
		})
		on := true
		grace := 24.0
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{AutoMerge: &on, GraceHours: &grace}, run)
		want := [][3]any{
			{11, "flagged", "base-superseded"},
			{12, "flagged", "red-across-grace"},
			{13, "untouched", "green"},
		}
		if len(res.Outcomes) != 3 {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		for i, w := range want {
			o := res.Outcomes[i]
			if o.PR != w[0] || o.Action != w[1] || o.Reason != w[2] {
				t.Fatalf("outcome[%d] = %d/%v/%v, want %v", i, o.PR, o.Action, o.Reason, w)
			}
		}
		// red-across-grace flags hold autoMerge even in a dry run
		if !res.SkipAutoMerge {
			t.Fatal("skipAutoMerge must be true")
		}
		rows := hygieneRows(t, repo)
		if len(rows) != 2 {
			t.Fatalf("rows = %d", len(rows))
		}
		if fl(t, rows[0]["pr"]) != 11 || rows[0]["action"] != "flagged" || rows[0]["reason"] != "base-superseded" ||
			fl(t, rows[1]["pr"]) != 12 || rows[1]["action"] != "flagged" || rows[1]["reason"] != "red-across-grace" {
			t.Fatalf("rows = %v", rows)
		}
	})

	t.Run("a fresh PR is never auto-closed before the age floor (base branch deleted)", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := scriptedGhWithDeadBases([]string{"devagent/TASK-mtioq4ik-T0-a0"}, map[string]any{
			"list":    "[" + hygieneTaskPr(t, nil) + "]", // updatedAt: now
			"comment": "",
			"close":   "",
		})
		dry := false
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry}, run)
		if len(res.Outcomes) != 1 {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		o := res.Outcomes[0]
		if o.Action != "untouched" || o.Reason != "base-superseded-age-floor" {
			t.Fatalf("outcome = %s/%s, want untouched/base-superseded-age-floor", o.Action, o.Reason)
		}
		if !strings.Contains(o.Detail, "auto-close floor") || !strings.Contains(o.Detail, "merged or deleted") {
			t.Fatalf("detail must keep the verdict and name the floor: %q", o.Detail)
		}
		for _, c := range *calls {
			if len(c) > 1 && (c[1] == "close" || c[1] == "comment") {
				t.Fatalf("fresh PR must not be acted on: %v", c)
			}
		}
		if rows := readLedgerRows(t, repo); len(rows) != 0 {
			t.Fatalf("expected no ledger rows, got %d", len(rows))
		}
	})

	t.Run("an unknown PR age is floored: no close without a timestamp", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := scriptedGhWithDeadBases([]string{"devagent/TASK-mtioq4ik-T0-a0"}, map[string]any{
			"list":    "[" + hygieneTaskPr(t, map[string]any{"updatedAt": "not-a-date"}) + "]",
			"comment": "",
			"close":   "",
		})
		dry := false
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &dry}, run)
		o := res.Outcomes[0]
		if o.Action != "untouched" || o.Reason != "base-superseded-age-floor" || o.GraceAgeHours != nil {
			t.Fatalf("outcome = %+v", o)
		}
		if !strings.Contains(o.Detail, "age is unknown") {
			t.Fatalf("detail = %q", o.Detail)
		}
		for _, c := range *calls {
			if len(c) > 1 && (c[1] == "close" || c[1] == "comment") {
				t.Fatalf("unknown age must not be acted on: %v", c)
			}
		}
	})
}

// landingScriptGh drives the landing-evidence triage: a PR list, issue
// states keyed by number, and the main-branch commit listing.
func landingScriptGh(t *testing.T, list string, issues map[int]string, mainCommits []map[string]any) (RunGh, *[][]string) {
	t.Helper()
	commitsJSON, err := json.Marshal(mainCommits)
	if err != nil {
		t.Fatal(err)
	}
	calls := &[][]string{}
	run := RunGh(func(args []string, cwd string) (*GhResult, error) {
		*calls = append(*calls, args)
		switch args[0] {
		case "api":
			return &GhResult{Stdout: string(commitsJSON)}, nil
		case "issue":
			var n int
			if _, err := fmt.Sscanf(args[2], "%d", &n); err != nil {
				t.Fatalf("bad issue view args %v", args)
			}
			state, ok := issues[n]
			if !ok {
				state = "OPEN"
			}
			return &GhResult{Stdout: fmt.Sprintf(`{"state":%q}`, state)}, nil
		}
		key := args[0]
		if args[0] == "pr" && len(args) > 1 {
			key = args[1]
		}
		if key == "list" {
			return &GhResult{Stdout: list}, nil
		}
		return &GhResult{}, nil
	})
	return run, calls
}

func TestSweepTaskPrHygieneLandingEvidence(t *testing.T) {
	old := time.Now().Add(-30 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00")
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
	grace := 24.0
	on := true
	off := false
	mainWithLanding := []map[string]any{
		{"sha": "f00dc0de", "commit": map[string]any{"message": "implement the thing (#7)"}},
	}
	mainEmpty := []map[string]any{}

	prJSON := func(body string, rollup []map[string]any, updated string) string {
		pr := map[string]any{
			"number": 9, "title": "T1",
			"headRefName": "devagent/TASK-mtioq4ik-T1-a0",
			"baseRefName": "devagent/TASK-mtioq4ik-T0-a0",
			"state":       "OPEN", "mergeable": "MERGEABLE",
			"headRefOid": "abc123", "updatedAt": updated, "body": body,
			"statusCheckRollup": rollup,
		}
		b, err := json.Marshal([]map[string]any{pr})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	t.Run("shipped-elsewhere: closed issue with a landing commit on main closes the PR citing the sha", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := landingScriptGh(t, prJSON("Closes #7", hygieneGreen, old),
			map[int]string{7: "CLOSED"}, mainWithLanding)
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &off, GraceHours: &grace}, run)
		if len(res.Outcomes) != 1 {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		o := res.Outcomes[0]
		if o.Action != "closed" || o.Reason != "shipped-elsewhere" {
			t.Fatalf("outcome = %s/%s, want closed/shipped-elsewhere", o.Action, o.Reason)
		}
		if !strings.Contains(o.Detail, "f00dc0de") {
			t.Fatalf("detail must cite the landing sha: %s", o.Detail)
		}
		commented, closed := false, false
		for _, c := range *calls {
			if c[0] == "pr" && c[1] == "comment" && strings.Contains(strings.Join(c, " "), "f00dc0de") {
				commented = true
			}
			if c[0] == "pr" && c[1] == "close" {
				closed = true
			}
		}
		if !commented || !closed {
			t.Fatalf("comment=%v close=%v", commented, closed)
		}
		rows := hygieneRows(t, repo)
		if len(rows) != 1 || rows[0]["action"] != "closed" || rows[0]["reason"] != "shipped-elsewhere" {
			t.Fatalf("rows = %v", rows)
		}
	})

	t.Run("superseded: closed issue without a landing commit closes the PR as not shipped", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := landingScriptGh(t, prJSON("Closes #7", hygieneGreen, old),
			map[int]string{7: "CLOSED"}, mainEmpty)
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &off, GraceHours: &grace}, run)
		if len(res.Outcomes) != 1 {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		o := res.Outcomes[0]
		if o.Action != "closed" || o.Reason != "superseded" {
			t.Fatalf("outcome = %s/%s, want closed/superseded", o.Action, o.Reason)
		}
		if !strings.Contains(o.Detail, "not shipped") {
			t.Fatalf("detail must state not shipped: %s", o.Detail)
		}
		commented := false
		for _, c := range *calls {
			if c[0] == "pr" && c[1] == "comment" && strings.Contains(strings.Join(c, " "), "not shipped") {
				commented = true
			}
		}
		if !commented {
			t.Fatal("expected an explicit not-shipped comment")
		}
		rows := hygieneRows(t, repo)
		if len(rows) != 1 || rows[0]["action"] != "closed" || rows[0]["reason"] != "superseded" {
			t.Fatalf("rows = %v", rows)
		}
	})

	t.Run("transport noise on the landing listing never closes the PR as not shipped", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		calls := &[][]string{}
		run := RunGh(func(args []string, cwd string) (*GhResult, error) {
			*calls = append(*calls, args)
			switch {
			case args[0] == "pr" && len(args) > 1 && args[1] == "list":
				return &GhResult{Stdout: prJSON("Closes #7", hygieneGreen, now)}, nil
			case args[0] == "issue":
				return &GhResult{Stdout: `{"state":"CLOSED"}`}, nil
			case strings.Contains(strings.Join(args, " "), "commits?sha=main"):
				return nil, fmt.Errorf("gh api repos/{owner}/{repo}/commits exited 1: gh: Internal Server Error (HTTP 500)")
			case args[0] == "api":
				return &GhResult{}, nil // the branch lookup answered: base intact
			}
			return &GhResult{}, nil
		})
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &off, GraceHours: &grace}, run)
		if len(res.Outcomes) != 1 {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		o := res.Outcomes[0]
		if o.Action != "untouched" || o.Reason != "landing-unknown" {
			t.Fatalf("outcome = %s/%s, want untouched/landing-unknown", o.Action, o.Reason)
		}
		for _, c := range *calls {
			if c[0] == "pr" && (c[1] == "comment" || c[1] == "close") {
				t.Fatalf("transport noise must not act: %v", c)
			}
		}
		if rows := readLedgerRows(t, repo); len(rows) != 0 {
			t.Fatalf("expected no ledger rows, got %d", len(rows))
		}
	})

	t.Run("an unparseable landing listing is unknown evidence, not an empty main", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := scriptedGh(map[string]any{
			"list":  prJSON("Closes #7", hygieneGreen, now),
			"api":   "{not json",
			"issue": `{"state":"CLOSED"}`,
		})
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &off, GraceHours: &grace}, run)
		if len(res.Outcomes) != 1 {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		o := res.Outcomes[0]
		if o.Action != "untouched" || o.Reason != "landing-unknown" {
			t.Fatalf("outcome = %s/%s, want untouched/landing-unknown", o.Action, o.Reason)
		}
		for _, c := range *calls {
			if c[0] == "pr" && (c[1] == "comment" || c[1] == "close") {
				t.Fatalf("unparseable evidence must not act: %v", c)
			}
		}
	})

	t.Run("still-wanted: open issue keeps a red-across-grace PR open with an evidence comment", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := landingScriptGh(t, prJSON("Closes #7", hygieneRed, old),
			map[int]string{7: "OPEN"}, mainEmpty)
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &off, AutoMerge: &on, GraceHours: &grace}, run)
		if len(res.Outcomes) != 1 {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		o := res.Outcomes[0]
		if o.Action != "flagged" || o.Reason != "red-across-grace" {
			t.Fatalf("outcome = %s/%s, want flagged/red-across-grace", o.Action, o.Reason)
		}
		if !res.SkipAutoMerge {
			t.Fatal("skipAutoMerge must hold for a still-wanted red PR")
		}
		commentCount := func() int {
			n := 0
			for _, c := range *calls {
				if c[0] == "pr" && c[1] == "comment" && strings.Contains(strings.Join(c, " "), "still wanted") {
					n++
				}
			}
			return n
		}
		if commentCount() != 1 {
			t.Fatalf("evidence comment count = %d, want 1", commentCount())
		}
		// Second sweep: the evidence comment must not be reposted.
		SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &off, GraceHours: &grace}, run)
		if commentCount() != 1 {
			t.Fatalf("evidence comment reposted: count = %d", commentCount())
		}
	})

	t.Run("dry-run: triage flags without commenting or closing", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := landingScriptGh(t, prJSON("Closes #7", hygieneGreen, old),
			map[int]string{7: "CLOSED"}, mainWithLanding)
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{GraceHours: &grace}, run)
		if len(res.Outcomes) != 1 || res.Outcomes[0].Action != "flagged" {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		if !strings.HasPrefix(res.Outcomes[0].Detail, "[dry-run] ") {
			t.Fatalf("dry-run detail = %s", res.Outcomes[0].Detail)
		}
		for _, c := range *calls {
			if c[0] == "pr" && (c[1] == "comment" || c[1] == "close") {
				t.Fatalf("unexpected acting call %v", c)
			}
		}
	})

	t.Run("no closed issue: green PR is untouched and no issue lookups happen", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := landingScriptGh(t, prJSON("Closes #7", hygieneGreen, now),
			map[int]string{7: "OPEN"}, mainEmpty)
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &off, GraceHours: &grace}, run)
		if len(res.Outcomes) != 1 || res.Outcomes[0].Action != "untouched" || res.Outcomes[0].Reason != "green" {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		for _, c := range *calls {
			if c[0] == "pr" && (c[1] == "comment" || c[1] == "close") {
				t.Fatalf("unexpected acting call %v", c)
			}
		}
		if len(*calls) == 0 {
			t.Fatal("sweep must still list PRs")
		}
	})

	t.Run("closing-keyword refs win over incidental #N mentions", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := landingScriptGh(t, prJSON("Related to #12, see also #13. Closes #7", hygieneGreen, old),
			map[int]string{7: "CLOSED"}, mainWithLanding)
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &off, GraceHours: &grace}, run)
		if len(res.Outcomes) != 1 || res.Outcomes[0].Action != "closed" || res.Outcomes[0].Reason != "shipped-elsewhere" {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		for _, c := range *calls {
			if c[0] == "issue" && strings.Join(c, " ") != "issue view 7 --json state" {
				t.Fatalf("incidental issue lookup: %v", c)
			}
		}
	})

	t.Run("a fresh PR is never auto-closed before the age floor (issue shipped elsewhere)", func(t *testing.T) {
		repo := hygieneTempRepo(t)
		run, calls := landingScriptGh(t, prJSON("Closes #7", hygieneGreen, now), // fresh
			map[int]string{7: "CLOSED"}, mainWithLanding)
		res := SweepTaskPrHygiene(repo, PrHygieneOptions{DryRun: &off, GraceHours: &grace}, run)
		if len(res.Outcomes) != 1 {
			t.Fatalf("outcomes = %+v", res.Outcomes)
		}
		o := res.Outcomes[0]
		if o.Action != "untouched" || o.Reason != "landing-age-floor" {
			t.Fatalf("outcome = %s/%s, want untouched/landing-age-floor", o.Action, o.Reason)
		}
		if !strings.Contains(o.Detail, "f00dc0de") || !strings.Contains(o.Detail, "auto-close floor") {
			t.Fatalf("detail must keep the landing sha and name the floor: %q", o.Detail)
		}
		for _, c := range *calls {
			if c[0] == "pr" && (c[1] == "comment" || c[1] == "close") {
				t.Fatalf("fresh PR must not be acted on: %v", c)
			}
		}
		if rows := readLedgerRows(t, repo); len(rows) != 0 {
			t.Fatalf("expected no ledger rows, got %d", len(rows))
		}
	})
}
