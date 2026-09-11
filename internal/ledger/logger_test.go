package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Logger + run-registry tests (ports of the TS src/logger.ts and
// src/runregistry.ts contracts).

func TestRunLoggerShapeAndRedaction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	l, err := NewRunLogger(home)
	if err != nil {
		t.Fatal(err)
	}
	if l.RunID() == "" || len(l.RunID()) != 36 || strings.Count(l.RunID(), "-") != 4 {
		t.Fatalf("runId not a UUID: %q", l.RunID())
	}
	if l.Path() != filepath.Join(home, "runs", l.RunID()+".jsonl") {
		t.Fatalf("path mismatch: %s", l.Path())
	}
	l.Log(StageImplement, LevelWarn, "step done", []KV{{Key: "n", Value: 2}, {Key: "api_key", Value: "super"}, {Key: "authToken", Value: "x"}, {Key: "PASSWORD", Value: "y"}, {Key: "client-credential", Value: "z"}})
	data, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		TS      string         `json:"ts"`
		RunID   string         `json:"runId"`
		Stage   string         `json:"stage"`
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("log line not valid JSON: %v", err)
	}
	if entry.Stage != "implement" || entry.Level != "warn" || entry.Message != "step done" || entry.RunID != l.RunID() {
		t.Fatalf("entry mismatch: %+v", entry)
	}
	if entry.Data["n"] != float64(2) {
		t.Fatalf("numeric data must survive: %v", entry.Data)
	}
	for _, k := range []string{"api_key", "authToken", "PASSWORD", "client-credential"} {
		if entry.Data[k] != "[REDACTED]" {
			t.Fatalf("key %q not redacted: %v", k, entry.Data)
		}
	}
}

func TestRunLoggerOmitsDataAndMkdirs(t *testing.T) {
	// data? is omitted entirely when not passed; nested runs dirs are created
	// on construction (FR-OPS-01).
	home := t.TempDir()
	l, err := NewRunLogger(filepath.Join(home, "deep", "nested"))
	if err != nil {
		t.Fatal(err)
	}
	l.Info(StageAudit, "no payload", nil)
	raw, _ := os.ReadFile(l.Path())
	line := strings.TrimRight(string(raw), "\n")
	if strings.Contains(line, `"data"`) {
		t.Fatalf("nil data must be omitted: %s", line)
	}
	if !strings.Contains(line, `"level":"info"`) {
		t.Fatalf("info level missing: %s", line)
	}
}

func TestRunLoggerDEVAGENTHOMEFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVAGENT_HOME", "")
	t.Setenv("HOME", home)
	l, err := NewRunLogger("")
	if err != nil {
		t.Fatal(err)
	}
	if l.Path() != filepath.Join(home, ".devagent", "runs", l.RunID()+".jsonl") {
		t.Fatalf("default home fallback mismatch: %s", l.Path())
	}
}

func TestSanitizeKey(t *testing.T) {
	if got := SanitizeKey("TASK-mtn9f85c-83s7"); got != "TASK-mtn9f85c-83s7" {
		t.Fatalf("safe key mutated: %q", got)
	}
	if got := SanitizeKey("release/0.1.0"); got != "release_0.1.0" {
		t.Fatalf("slash not replaced: %q", got)
	}
	if got := SanitizeKey("a b:c/d e"); got != "a_b_c_d_e" {
		t.Fatalf("sanitize mismatch: %q", got)
	}
}

func TestTryAcquireRunDedupAndStaleBreak(t *testing.T) {
	home := t.TempDir()
	base := time.Now().UnixMilli()
	defer func() { NowFunc = nowMillis }()

	NowFunc = func() int64 { return base }
	l1 := TryAcquireRun(home, "ticket/one", 0)
	if l1 == nil {
		t.Fatal("fresh acquire must succeed")
	}
	if l2 := TryAcquireRun(home, "ticket/one", 0); l2 != nil {
		t.Fatal("second acquire while fresh must fail")
	}
	if l3 := TryAcquireRun(home, "ticket/two", 0); l3 == nil {
		t.Fatal("different ticket must not collide")
	}

	// Same ticket, sanitized identically: latest-wins on the same lock file.
	if l4 := TryAcquireRun(home, "ticket:one", 0); l4 != nil {
		t.Fatal("sanitized alias must hit the same fresh lock")
	}

	// Issue #316 amendment: a TTL-expired lock whose holder is ALIVE must
	// never be stale-broken — loop task runs routinely outlive the 1h TTL,
	// and breaking the lock let a newcomer steal a long run's lock. The
	// holder here is this very process, verifiably alive.
	NowFunc = func() int64 { return base + DefaultLockTTL + 1 }
	if l5 := TryAcquireRun(home, "ticket/one", 0); l5 != nil {
		t.Fatal("live holder's expired lock must not be broken")
	}

	// With no usable pid the TTL stays the breaker: a pid-less (legacy or
	// corrupt) expired payload is broken and re-acquired.
	if err := os.WriteFile(l1.Path, []byte(`{"startedAt":`+strconv.FormatInt(base, 10)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	l5 := TryAcquireRun(home, "ticket/one", 0)
	if l5 == nil {
		t.Fatal("pid-less expired lock must be broken")
	}
	// Issue #316: the broken predecessor's deferred Release must not delete
	// the new holder's live lock (observed live: a finished run unlinked
	// TASK.lock while it already belonged to a newer run).
	l1.Release()
	if _, err := os.Stat(l5.Path); err != nil {
		t.Fatal("predecessor release must keep the later holder's lock: " + err.Error())
	}
	l5.Release() // idempotent second release below
	l5.Release()
	if l6 := TryAcquireRun(home, "ticket/one", 0); l6 == nil {
		t.Fatal("released lock must be re-acquirable")
	}
}

func TestTryAcquireRunCorruptLockBroken(t *testing.T) {
	home := t.TempDir()
	locks := filepath.Join(home, "locks")
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locks, "t.lock"), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if l := TryAcquireRun(home, "t", 0); l == nil {
		t.Fatal("corrupt lock must be broken, not block forever")
	}
	raw, _ := os.ReadFile(filepath.Join(locks, "t.lock"))
	var holder struct {
		PID       int64 `json:"pid"`
		StartedAt int64 `json:"startedAt"`
	}
	if err := json.Unmarshal(raw, &holder); err != nil {
		t.Fatalf("lock payload not the TS shape: %s", raw)
	}
	if holder.PID <= 0 || holder.StartedAt == 0 {
		t.Fatalf("lock payload incomplete: %s", raw)
	}
}

// TestTryAcquireRunFencingGeneration pins the lease-generation fencing
// contract: a fresh lock carries generation 1, every break-and-reacquire
// bumps the broken holder's token (a token is never reused), and a legacy
// payload without a generation field restarts the sequence at 1.
func TestTryAcquireRunFencingGeneration(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "locks", "TASK.lock")

	l1 := TryAcquireRun(home, "TASK", 0)
	if l1 == nil || l1.Generation != 1 {
		t.Fatalf("fresh acquire must carry generation 1, got %+v", l1)
	}
	var onDisk struct {
		Generation int64 `json:"generation"`
	}
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &onDisk); err != nil || onDisk.Generation != 1 {
		t.Fatalf("lock payload must carry the fencing token: %s", raw)
	}
	l1.Release()

	// A dead holder's lock with generation 7 must be broken into a
	// generation-8 acquisition — the dead holder's token is retired.
	if err := os.WriteFile(path, []byte(`{"pid":`+strconv.Itoa(deadPid(t))+`,"startedAt":`+strconv.FormatInt(time.Now().UnixMilli(), 10)+`,"generation":7}`), 0o644); err != nil {
		t.Fatal(err)
	}
	l2 := TryAcquireRun(home, "TASK", 0)
	if l2 == nil || l2.Generation != 8 {
		t.Fatalf("break-and-reacquire must bump generation 7 to 8, got %+v", l2)
	}
	l2.Release()

	// Legacy/corrupt payload with no generation field: token restarts at 1.
	if err := os.WriteFile(path, []byte(`{"startedAt":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	l3 := TryAcquireRun(home, "TASK", 0)
	if l3 == nil || l3.Generation != 1 {
		t.Fatalf("legacy payload must restart the token at 1, got %+v", l3)
	}
}

// TestRunLockStillHeldFencing pins the fencing check a lease holder
// consults before irreversible work: true while the file carries this
// acquisition's payload, false once a newer acquisition replaced it or the
// lock was released — and a stolen lease's Release must leave the winner's
// lock untouched.
func TestRunLockStillHeldFencing(t *testing.T) {
	home := t.TempDir()
	l1 := TryAcquireRun(home, "TASK", 0)
	if l1 == nil {
		t.Fatal("fresh acquire must succeed")
	}
	if !l1.StillHeld() {
		t.Fatal("holder must see its own live lease as held")
	}

	// A newer incarnation's acquisition replaces the file (the stale-break
	// winner writes its own generation-2 payload).
	stolen := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"startedAt":` + strconv.FormatInt(time.Now().UnixMilli(), 10) + `,"generation":2}`
	if err := os.WriteFile(l1.Path, []byte(stolen), 0o644); err != nil {
		t.Fatal(err)
	}
	if l1.StillHeld() {
		t.Fatal("stolen lease must read as lost")
	}
	l1.Release() // the old holder's deferred release
	raw, err := os.ReadFile(l1.Path)
	if err != nil {
		t.Fatalf("predecessor release must keep the winner's lock: %v", err)
	}
	var winner struct {
		Generation int64 `json:"generation"`
	}
	if err := json.Unmarshal(raw, &winner); err != nil || winner.Generation != 2 {
		t.Fatalf("winner's lock must survive the stale holder's release: %s", raw)
	}

	// After its own Release the fence reads lost as well.
	l2 := TryAcquireRun(home, "TASK2", 0)
	if l2 == nil || !l2.StillHeld() {
		t.Fatalf("fresh acquire must read held, got %+v", l2)
	}
	l2.Release()
	if l2.StillHeld() {
		t.Fatal("released lease must read as lost")
	}
}
