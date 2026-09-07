package scout

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/research/scantext"
)

// tmpRepo mirrors test/scout.test.ts tmpRepo(): the minimal repo shape the
// scout prompt reads.
func tmpRepo(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "docs", "PRD.md"), []byte("# PRD\n## 4 Competitive Landscape\nfoo\n## 17 Roadmap\nbar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d, ".selfbuild"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, ".selfbuild", "ledger.jsonl"), []byte("{\"loop\":1,\"status\":\"ok\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, ".selfbuild", "lessons.md"), []byte("# Lessons\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func strPtr(s string) *string { return &s }

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestParseScoutOutputValid(t *testing.T) {
	raw := "---TASK---\nid: FEAT-1\ntitle: Add foo bar endpoint\n\ngoal: Goal: Add GET /foo returning JSON\ncriteria: returns 200; validates schema\n---PRD---\n# Add foo\n\n## Goal\nAdd endpoint.\n"
	p := ParseScoutOutput(raw)
	if p == nil {
		t.Fatal("valid TASK+PRD block must parse")
	}
	if p.ID != "FEAT-1" {
		t.Errorf("id = %q", p.ID)
	}
	if !strings.HasPrefix(p.Goal, "Goal:") {
		t.Errorf("goal %q must start with Goal:", p.Goal)
	}
	if len(p.Criteria) != 2 || p.Criteria[0] != "returns 200" || p.Criteria[1] != "validates schema" {
		t.Errorf("criteria = %v", p.Criteria)
	}
	if !strings.Contains(p.PRDMarkdown, "# Add foo") {
		t.Errorf("prdMarkdown = %q", p.PRDMarkdown)
	}
}

func TestParseScoutOutputRejects(t *testing.T) {
	cases := map[string]string{
		"no markers":      "no markers",
		"missing Goal:":   "---TASK---\nid: X\ntitle: t\ngoal: not starting with keyword\n---PRD---\n# hi",
		"empty PRD":       "---TASK---\nid: X\ntitle: t\ngoal: Goal: x\n---PRD---\n",
		"prd before task": "---PRD---\n# hi\n---TASK---\nid: X\ntitle: t\ngoal: Goal: x\n",
		"missing id":      "---TASK---\ntitle: t\ngoal: Goal: x\n---PRD---\n# hi",
		"missing title":   "---TASK---\nid: X\ngoal: Goal: x\n---PRD---\n# hi",
		"missing goal":    "---TASK---\nid: X\ntitle: t\n---PRD---\n# hi",
	}
	for name, raw := range cases {
		if p := ParseScoutOutput(raw); p != nil {
			t.Errorf("%s: expected nil, got %+v", name, p)
		}
	}
}

// Deterministic fallback id resolution: one id per UTC day, so the enqueue
// exists-dedup catches repeats; a new day gets a fresh slot.
func TestFallbackTaskIdDeterministic(t *testing.T) {
	morning := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	evening := time.Date(2026, 9, 7, 21, 0, 0, 0, time.UTC)
	nextDay := time.Date(2026, 9, 8, 0, 1, 0, 0, time.UTC)
	if got := FallbackTaskId(morning); got != "SCOUT-20260907-fallback" {
		t.Fatalf("FallbackTaskId = %q", got)
	}
	if FallbackTaskId(morning) != FallbackTaskId(evening) {
		t.Fatal("same UTC day must resolve to the same fallback id")
	}
	if FallbackTaskId(morning) == FallbackTaskId(nextDay) {
		t.Fatal("a new day must get a fresh fallback slot")
	}
	// Local zones east of UTC must not shift the UTC day.
	utcPlus := time.FixedZone("UTC+9", 9*3600)
	shifted := time.Date(2026, 9, 8, 8, 0, 0, 0, utcPlus) // = 2026-09-07T23:00Z
	if got := FallbackTaskId(shifted); got != "SCOUT-20260907-fallback" {
		t.Fatalf("FallbackTaskId(local zone) = %q, must key on UTC", got)
	}
}

func TestFallbackTaskShape(t *testing.T) {
	prompt := "You are the DevAgent SCOUT. Repo: /repo.\n" + strings.Repeat("x", 500)
	fb := FallbackTask(prompt, time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	if fb.ID != "SCOUT-20260907-fallback" {
		t.Errorf("id = %q", fb.ID)
	}
	if !strings.HasPrefix(fb.Goal, "Goal:") {
		t.Errorf("goal must pass the ^Goal: gate, got %q", fb.Goal)
	}
	if len(fb.Criteria) != 2 {
		t.Errorf("criteria = %v", fb.Criteria)
	}
	if !strings.Contains(fb.PRDMarkdown, "Prompt excerpt (first 400 chars): ") {
		t.Error("PRD must carry the bounded prompt excerpt")
	}
	excerpt := fb.PRDMarkdown[strings.Index(fb.PRDMarkdown, "chars): ")+len("chars): "):]
	if len(excerpt) != 400 {
		t.Errorf("excerpt length = %d, want 400", len(excerpt))
	}
}

func TestRandomTaskIdShape(t *testing.T) {
	id := RandomTaskId(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	if !strings.HasPrefix(id, "SCOUT-20260907-") || len(id) != len("SCOUT-20260907-")+4 {
		t.Fatalf("RandomTaskId = %q", id)
	}
	ids := map[string]bool{}
	for i := 0; i < 50; i++ {
		ids[RandomTaskId(time.Now())] = true
	}
	if len(ids) < 45 {
		t.Fatalf("random suffix collapsed: %d distinct in 50", len(ids))
	}
}

func TestSanitizeForFile(t *testing.T) {
	cases := map[string]string{
		"FEAT-1":            "FEAT-1",
		"a b/c":             "a-b-c",
		"--double--dashes-": "-double-dashes-",
		"trailing..dots":    "trailing..dots",
	}
	for in, want := range cases {
		if got := SanitizeForFile(in); got != want {
			t.Errorf("SanitizeForFile(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SanitizeForFile(strings.Repeat("x", 100)); len(got) != 80 {
		t.Errorf("long id truncated to %d, want 80", len(got))
	}
}

// Golden replay suite (port of test/scout-golden.test.ts): every fixture on
// disk must have a golden entry and vice versa, and ExtractScoutPayload must
// return the recorded payload (or null) exactly. A future worker
// output-shape change fails here — at replay, not mid-loop.
func TestReplayGoldenFixtures(t *testing.T) {
	goldenRaw, err := os.ReadFile(filepath.Join("testdata", "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]struct {
		Worker   string  `json:"worker"`
		Expected *string `json:"expected"`
	}
	if err := json.Unmarshal(goldenRaw, &golden); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []string
	for _, e := range entries {
		if !e.IsDir() && e.Name() != "golden.json" {
			onDisk = append(onDisk, e.Name())
		}
	}
	if len(onDisk) != len(golden) {
		t.Fatalf("fixture set drifted: %d on disk vs %d golden entries", len(onDisk), len(golden))
	}
	for _, name := range onDisk {
		if _, ok := golden[name]; !ok {
			t.Errorf("fixture %s on disk has no golden entry", name)
		}
	}
	results, err := ReplayScoutFixtures("") // embedded set
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(golden) {
		t.Fatalf("replay ran %d fixtures, want %d", len(results), len(golden))
	}
	for _, r := range results {
		if !r.Pass {
			t.Errorf("%s: expected %s, got %s", r.Name, deref(r.Expected), deref(r.Actual))
		}
	}
	// The on-disk set must replay identically to the embedded set.
	diskResults, err := ReplayScoutFixtures("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(diskResults) != len(results) {
		t.Fatalf("disk replay %d != embedded replay %d", len(diskResults), len(results))
	}
	for i := range results {
		if diskResults[i].Name != results[i].Name || diskResults[i].Pass != results[i].Pass {
			t.Errorf("replay mismatch at %d: %v vs %v", i, results[i], diskResults[i])
		}
	}
}

// Shape change fails at replay: a stale golden expectation (or a changed
// worker output format) must flip the replay verdict, not silently extract.
func TestReplayDetectsShapeDrift(t *testing.T) {
	dir := t.TempDir()
	payload := "preamble\n---TASK---\nid: FEAT-9\ntitle: T\ngoal: Goal: g\ncriteria: c\n---PRD---\n# T\n"
	if err := os.WriteFile(filepath.Join(dir, "stream.ndjson"), []byte(`{"type":"text","text":`+jsonString(payload)+`}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	golden := `{"stream.ndjson":{"worker":"opencode","expected":` + jsonString("DIFFERENT") + `}}`
	if err := os.WriteFile(filepath.Join(dir, "golden.json"), []byte(golden), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := ReplayScoutFixtures(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Pass {
		t.Fatalf("stale expectation must fail the replay: %+v", results)
	}
}

func TestExtractScoutPayloadShapes(t *testing.T) {
	payload := "preamble text\n---TASK---\nid: FEAT-9\ntitle: T\ngoal: Goal: g\ncriteria: c\n---PRD---\n# T\n## Goal\nx\n"
	// The claude object form's expected value is the raw stream itself (the
	// whole document carries ---TASK--- and ---PRD---, so the raw fallback
	// returns it unchanged) — exactly what golden.json pins.
	cases := []struct {
		name   string
		raw    string
		worker string
		want   *string
	}{
		{"opencode ndjson", `{"type":"step_start"}` + "\n" + `{"type":"text","text":` + jsonString(payload) + `}` + "\n" + `{"type":"step_finish"}` + "\n", "opencode", strPtr(payload)},
		{"claude array form", "[\n  {\"type\":\"system\"},\n  {\"type\":\"result\",\"result\":" + jsonString(payload) + "}\n]\n", "claude-code", strPtr(payload)},
		{"single-line array", "[{\"type\":\"system\"},{\"type\":\"result\",\"result\":" + jsonString(payload) + "}]\n", "claude-code", strPtr(payload)},
		{"part key", `{"type":"part","part":` + jsonString(payload) + `}` + "\n", "omp", strPtr(payload)},
		{"raw marker fallback", payload, "omp", strPtr(payload)},
		{"no markers", "{\n  \"type\": \"result\",\n  \"result\": \"all work done, no markers here\"\n}\n", "claude-code", nil},
		{"malformed json", "{\"type\":\"result\",\"result\":\"---TASK---\nid: FEAT-X\n", "claude-code", nil},
	}
	for _, tc := range cases {
		got := ExtractScoutPayload(tc.raw, tc.worker)
		if tc.want == nil {
			if got != nil {
				t.Errorf("%s: want nil, got %q", tc.name, *got)
			}
			continue
		}
		if got == nil || *got != *tc.want {
			t.Errorf("%s: got %v, want %q", tc.name, got, *tc.want)
		}
	}
}

func TestBuildScoutPrompt(t *testing.T) {
	repo := tmpRepo(t)
	cfg := config.DefaultConfig()
	prompt := BuildScoutPrompt(repo, cfg, nil)
	for _, want := range []string{
		"Queue depth: 0 task(s). Recent PRDs: (none).",
		"---TASK---",
		"---PRD---",
		"You are the DevAgent SCOUT. Repo: " + repo,
		"Ledger tail:\n{\"loop\":1,\"status\":\"ok\"}",
		scantext.BuildAdjacentCategoryScanText(),
		"MCP servers",
		"harness tooling",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// The scout prompt reads .selfbuild/lessons.md directly (raw, 2000-char
	// cap), not the ranked digest — a heading-only file still yields a
	// non-empty lessons line, exactly like the TS builder.
	if !strings.Contains(prompt, "Lessons (ratchet, do not re-derive):\n# Lessons") {
		t.Error("lessons file content must ride along in the scout prompt")
	}
	// Knowledge digest splices at the tail (the scout prompt carries no
	// marker): absent context leaves the prompt byte-identical.
	if strings.Contains(prompt, KnowledgeContextHeader) {
		t.Error("no context dir present: knowledge header must be absent")
	}
}

func TestBuildScoutPromptHandlesMissingLedgerAndLessons(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "PRD.md"), []byte("# PRD"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := BuildScoutPrompt(repo, config.DefaultConfig(), nil)
	if !strings.Contains(prompt, "Ledger tail:\n(no ledger yet)") {
		t.Error("missing ledger must degrade to the (no ledger yet) sentinel")
	}
}

func TestHeartbeatRoundTrip(t *testing.T) {
	repo := t.TempDir()
	if hb := ReadHeartbeat(repo); hb != nil {
		t.Fatal("missing heartbeat must read nil")
	}
	worker := "omp"
	status := "ok"
	detail := "enqueued FEAT-1"
	taskID := "FEAT-1"
	interval := 30.0
	if _, err := WriteHeartbeat(repo, HeartbeatPatch{LastStatus: &status, LastDetail: &detail, Worker: &worker, IntervalMinutes: &interval, LastTaskID: &taskID}); err != nil {
		t.Fatal(err)
	}
	hb := ReadHeartbeat(repo)
	if hb == nil {
		t.Fatal("heartbeat must round-trip")
	}
	if hb.LastStatus == nil || *hb.LastStatus != "ok" || hb.LastTaskID == nil || *hb.LastTaskID != "FEAT-1" {
		t.Fatalf("heartbeat = %+v", hb)
	}
	if _, err := time.Parse(time.RFC3339, hb.LastRunAt); err != nil {
		t.Fatalf("lastRunAt %q not ISO", hb.LastRunAt)
	}
	// The on-disk shape matches the Node writer's: 2-space indent, field
	// order lastRunAt first, optional fields omitted when unset.
	raw, _ := os.ReadFile(HeartbeatPath(repo))
	s := string(raw)
	if !strings.HasPrefix(s, "{\n  \"lastRunAt\": \"") || !strings.HasSuffix(s, "\n}\n") {
		t.Fatalf("on-disk heartbeat shape drifted:\n%s", s)
	}
}

func TestReadHeartbeatUnparseableTimestamp(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".devagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HeartbeatPath(repo), []byte("{\"lastRunAt\":\"not-a-date\",\"lastStatus\":\"ok\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hb := ReadHeartbeat(repo); hb != nil {
		t.Fatal("unparseable timestamp must read as missing (NaN age misleads operators)")
	}
	// Corrupt JSON is also nil.
	if err := os.WriteFile(HeartbeatPath(repo), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hb := ReadHeartbeat(repo); hb != nil {
		t.Fatal("corrupt heartbeat must read nil")
	}
}

func TestScoutLockLifecycle(t *testing.T) {
	repo := t.TempDir()
	now := time.Now()
	if !AcquireScoutLock(repo, now) {
		t.Fatal("lock must acquire when free")
	}
	if AcquireScoutLock(repo, now) {
		t.Fatal("second acquire must refuse while holder alive")
	}
	ReleaseScoutLock(repo)
	if !AcquireScoutLock(repo, now) {
		t.Fatal("lock must re-acquire after release")
	}
	ReleaseScoutLock(repo)

	// Corrupt lock file behaves as stale.
	if err := os.MkdirAll(filepath.Join(repo, ".devagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scoutLockPath(repo), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !AcquireScoutLock(repo, now) {
		t.Fatal("corrupt lock must be taken over")
	}
	ReleaseScoutLock(repo)

	// Dead foreign pid is treated as stale.
	if err := os.WriteFile(scoutLockPath(repo), []byte(`{"pid":99999999,"at":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !AcquireScoutLock(repo, now) {
		t.Fatal("dead-holder lock must be taken over")
	}
	ReleaseScoutLock(repo)

	// Release is a no-op when another process holds the lock.
	if err := os.WriteFile(scoutLockPath(repo), []byte(`{"pid":1,"at":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ReleaseScoutLock(repo)
	if _, err := os.Stat(scoutLockPath(repo)); err != nil {
		t.Fatal("foreign lock must survive our release call")
	}
}

func TestLedgerTailSentinels(t *testing.T) {
	repo := t.TempDir()
	if got := ledgerTail(repo); got != "(no ledger yet)" {
		t.Errorf("missing ledger = %q", got)
	}
	dir := filepath.Join(repo, ".selfbuild")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ledgerTail(repo); got != "(ledger empty)" {
		t.Errorf("empty ledger = %q", got)
	}
	rows := ""
	for i := 1; i <= 5; i++ {
		rows += fmt.Sprintf("{\"loop\":%d}\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ledgerTail(repo)
	for _, want := range []string{"{\"loop\":3}", "{\"loop\":4}", "{\"loop\":5}"} {
		if !strings.Contains(got, want) {
			t.Errorf("tail missing %s: %q", want, got)
		}
	}
	if strings.Contains(got, "{\"loop\":2}") {
		t.Errorf("tail must keep only the last 3 rows: %q", got)
	}
}
