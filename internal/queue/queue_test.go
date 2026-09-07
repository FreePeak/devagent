package queue_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/queue"
)

func tmpRepo(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "da-queue-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// fakeClock is the deterministic clock: leases are wall-clock behaviour,
// tests must not sleep.
type fakeClock struct{ at int64 }

func (c *fakeClock) now() int64       { return c.at }
func (c *fakeClock) advance(ms int64) { c.at += ms }

// T0 = Date.parse('2026-09-07T00:00:00.000Z')
const t0 int64 = 1780780800000

func writeQueueJSON(t *testing.T, repoPath, id string, v any) {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	p := filepath.Join(queue.QueueDir(repoPath), id+".json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func enqueue(t *testing.T, repoPath string, in queue.EnqueueInput) *queue.QueuedTask {
	t.Helper()
	task, err := queue.EnqueueTask(repoPath, in)
	if err != nil {
		t.Fatalf("EnqueueTask(%s): %v", in.ID, err)
	}
	return task
}

func TestEnqueueListRead(t *testing.T) {
	t.Run("enqueues and lists tasks sorted by createdAt", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "TASK-1", Title: "First", Goal: "Goal: first"})
		enqueue(t, repo, queue.EnqueueInput{ID: "TASK-2", Title: "Second", Goal: "Goal: second"})
		all := queue.ListTasks(repo, "")
		if len(all) != 2 || all[0].ID != "TASK-1" {
			t.Fatalf("list = %d tasks, first %v", len(all), all[0].ID)
		}
		if got := queue.ReadTask(repo, "TASK-1"); got == nil || got.Title != "First" {
			t.Fatalf("readTask title = %+v", got)
		}
	})

	t.Run("rejects duplicate id", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "DUP", Title: "a", Goal: "Goal: a"})
		_, err := queue.EnqueueTask(repo, queue.EnqueueInput{ID: "DUP", Title: "b", Goal: "Goal: b"})
		if err == nil || !strings.Contains(err.Error(), "already queued") {
			t.Fatalf("want 'already queued' error, got %v", err)
		}
	})

	t.Run("sanitizes weird ids", func(t *testing.T) {
		repo := tmpRepo(t)
		task := enqueue(t, repo, queue.EnqueueInput{ID: "TASK / 1 !!", Title: "x", Goal: "Goal: x"})
		for _, r := range task.ID {
			ok := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
				r == '.' || r == '_' || r == '-'
			if !ok {
				t.Fatalf("id %q has unsanitized char %q", task.ID, r)
			}
		}
	})

	t.Run("rejects fully-unsanitizable id", func(t *testing.T) {
		repo := tmpRepo(t)
		if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{ID: "///", Title: "x", Goal: "Goal: x"}); err == nil ||
			!strings.Contains(err.Error(), `Invalid task id "///"`) {
			t.Fatalf("want invalid-id error, got %v", err)
		}
	})

	t.Run("writes PRD markdown when provided and reads it back", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "PRD-1", Title: "t", Goal: "Goal: t", PrdMarkdown: "# PRD\nok"})
		got, ok := queue.ReadPrd(repo, "PRD-1")
		if !ok || got != "# PRD\nok" {
			t.Fatalf("readPrd = %q ok=%v", got, ok)
		}
		task := queue.ReadTask(repo, "PRD-1")
		if task == nil || task.PrdPath == nil || !strings.Contains(*task.PrdPath, "PRD-1.md") {
			t.Fatalf("prdPath = %+v", task.PrdPath)
		}
	})

	t.Run("writePrd + readPrd standalone", func(t *testing.T) {
		repo := tmpRepo(t)
		queue.EnsureQueueDirs(repo)
		p, err := queue.WritePrd(repo, "X-1", "hello")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(p, "X-1.md") {
			t.Fatalf("writePrd path = %q", p)
		}
		if got, _ := queue.ReadPrd(repo, "X-1"); got != "hello" {
			t.Fatalf("readPrd = %q", got)
		}
		if _, ok := queue.ReadPrd(repo, "missing"); ok {
			t.Fatal("readPrd(missing) should miss")
		}
	})
}

func TestQueueClaim(t *testing.T) {
	t.Run("claimTask transitions pending to claimed and bumps attempts", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "C-1", Title: "c", Goal: "Goal: c"})
		claimed := queue.ClaimTask(repo, "C-1", "w1", nil)
		if claimed == nil || claimed.Status != queue.StatusClaimed || *claimed.Attempts != 1 {
			t.Fatalf("claimed = %+v", claimed)
		}
		// second claim on same task fails
		if second := queue.ClaimTask(repo, "C-1", "w2", nil); second != nil {
			t.Fatalf("second claim = %+v, want nil", second)
		}
	})

	t.Run("claimNextPending picks oldest pending", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "A", Title: "a", Goal: "Goal: a"})
		enqueue(t, repo, queue.EnqueueInput{ID: "B", Title: "b", Goal: "Goal: b"})
		c := queue.ClaimNextPending(repo, "w1", nil)
		if c == nil || c.ID != "A" {
			t.Fatalf("claimed = %+v", c)
		}
		if n := len(queue.ListTasks(repo, queue.StatusPending)); n != 1 {
			t.Fatalf("pending count = %d", n)
		}
	})

	t.Run("claimNextPending prefers clean tasks over failure-carrying ones (Q27)", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "CARRY-OLD", Title: "carried old", Goal: "Goal: carried old", FailureClass: "test-gate"})
		enqueue(t, repo, queue.EnqueueInput{ID: "CARRY-NEW", Title: "carried new", Goal: "Goal: carried new", FailureClass: "worker-error"})
		enqueue(t, repo, queue.EnqueueInput{ID: "CLEAN", Title: "clean", Goal: "Goal: clean"})
		// Pin createdAt via direct queue-JSON writes: rapid enqueues can tie on
		// the wall clock, and tier order must be deterministic.
		pinCreatedAt(t, repo, "CARRY-OLD", "2026-09-01T00:00:00.000Z")
		pinCreatedAt(t, repo, "CARRY-NEW", "2026-09-02T00:00:00.000Z")
		pinCreatedAt(t, repo, "CLEAN", "2026-09-03T00:00:00.000Z")
		// carried tasks created first but a clean task exists: CLEAN must win
		if c := queue.ClaimNextPending(repo, "w1", nil); c == nil || c.ID != "CLEAN" {
			t.Fatalf("first claim = %+v, want CLEAN", c)
		}
		// among carried tasks, oldest createdAt first
		if c := queue.ClaimNextPending(repo, "w1", nil); c == nil || c.ID != "CARRY-OLD" {
			t.Fatalf("second claim = %+v, want CARRY-OLD", c)
		}
		if c := queue.ClaimNextPending(repo, "w1", nil); c == nil || c.ID != "CARRY-NEW" {
			t.Fatalf("third claim = %+v, want CARRY-NEW", c)
		}
	})

	t.Run("updateTask preserves id/createdAt (Q27)", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "JW-1", Title: "j", Goal: "Goal: j", FailureClass: "test-gate"})
		// Direct queue-JSON write with a stable createdAt (updateTask locks id|createdAt)
		task := queue.ReadTask(repo, "JW-1")
		task.FailureClass = nil
		task.Status = queue.StatusPending
		task.CreatedAt = "2026-09-01T00:00:00.000Z"
		writeQueueJSON(t, repo, "JW-1", task)
		patched := func() *queue.QueuedTask {
			title := "jj"
			return queue.UpdateTask(repo, "JW-1", &queue.TaskPatch{Title: &title}, -1, nil)
		}()
		if patched == nil || patched.ID != "JW-1" || patched.CreatedAt != "2026-09-01T00:00:00.000Z" {
			t.Fatalf("patched = %+v", patched)
		}
		if patched.FailureClass != nil {
			t.Fatalf("failureClass = %+v, want cleared", patched.FailureClass)
		}
	})

	t.Run("enqueueTask stamps carried failureClass onto the queued task (Q27)", func(t *testing.T) {
		repo := tmpRepo(t)
		task := enqueue(t, repo, queue.EnqueueInput{ID: "STAMP-1", Title: "s", Goal: "Goal: s", FailureClass: "test-gate"})
		if task.FailureClass == nil || *task.FailureClass != "test-gate" {
			t.Fatalf("failureClass = %+v", task.FailureClass)
		}
		if stored := queue.ReadTask(repo, "STAMP-1"); stored == nil || stored.FailureClass == nil || *stored.FailureClass != "test-gate" {
			t.Fatalf("stored failureClass = %+v", stored)
		}
		clean := enqueue(t, repo, queue.EnqueueInput{ID: "STAMP-2", Title: "s2", Goal: "Goal: s2"})
		if clean.FailureClass != nil {
			t.Fatalf("clean failureClass = %+v, want nil", clean.FailureClass)
		}
	})
}

// pinCreatedAt rewrites the task file with a fixed createdAt, like the TS
// tests' direct JSON write.
func pinCreatedAt(t *testing.T, repoPath, id, at string) {
	t.Helper()
	task := queue.ReadTask(repoPath, id)
	if task == nil {
		t.Fatalf("task %s missing", id)
	}
	task.CreatedAt = at
	writeQueueJSON(t, repoPath, id, task)
}

func TestLeaseAndFencing(t *testing.T) {
	t.Run("double claim: exactly one worker wins the lease", func(t *testing.T) {
		repo := tmpRepo(t)
		clock := fakeClock{at: t0}
		enqueue(t, repo, queue.EnqueueInput{ID: "L-1", Title: "l", Goal: "Goal: l"})
		opts := &queue.ClaimOptions{Now: clock.now, LeaseMs: 60_000}
		first := queue.ClaimTask(repo, "L-1", "w1", opts)
		second := queue.ClaimTask(repo, "L-1", "w2", opts)
		if first == nil || first.Status != queue.StatusClaimed || *first.LeaseGeneration != 1 || *first.LeaseOwner != "w1" {
			t.Fatalf("first = %+v", first)
		}
		if second != nil {
			t.Fatalf("second = %+v, want nil", second)
		}
		stored := queue.ReadTask(repo, "L-1")
		if *stored.LeaseOwner != "w1" || *stored.ClaimedBy != "w1" || *stored.Attempts != 1 {
			t.Fatalf("stored = %+v", stored)
		}
		// the claim lock is released with the claim, not left behind
		if _, err := os.Stat(filepath.Join(queue.QueueDir(repo), "L-1.claim.lock")); !os.IsNotExist(err) {
			t.Fatal("claim lock left behind")
		}
	})

	t.Run("a live claim lock makes the loser give up without touching the task or the lock", func(t *testing.T) {
		repo := tmpRepo(t)
		clock := fakeClock{at: t0}
		enqueue(t, repo, queue.EnqueueInput{ID: "L-2", Title: "l", Goal: "Goal: l"})
		// Another claimant mid-read-modify-write (link() already won the name).
		lock := filepath.Join(queue.QueueDir(repo), "L-2.claim.lock")
		payload, _ := json.Marshal(map[string]any{"workerId": "other", "pid": 999_999, "acquiredAtMs": clock.now()})
		payload = append(payload, '\n')
		if err := os.WriteFile(lock, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := queue.ClaimTask(repo, "L-2", "w1", &queue.ClaimOptions{Now: clock.now}); got != nil {
			t.Fatalf("claim = %+v, want nil", got)
		}
		if stored := queue.ReadTask(repo, "L-2"); stored.Status != queue.StatusPending {
			t.Fatalf("status = %s", stored.Status)
		}
		if _, err := os.Stat(lock); err != nil {
			t.Fatal("live lock was removed")
		}
		// no .tmp. droppings
		entries, _ := os.ReadDir(queue.QueueDir(repo))
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp.") {
				t.Fatalf("tmp file left behind: %s", e.Name())
			}
		}
	})

	t.Run("a wedged claim lock past the stale window is broken and the claim proceeds", func(t *testing.T) {
		repo := tmpRepo(t)
		clock := fakeClock{at: t0}
		enqueue(t, repo, queue.EnqueueInput{ID: "L-3", Title: "l", Goal: "Goal: l"})
		lock := filepath.Join(queue.QueueDir(repo), "L-3.claim.lock")
		// Holder died between link() and unlink(): lock is 60s old, window is 30s.
		payload, _ := json.Marshal(map[string]any{"workerId": "dead", "pid": 1, "acquiredAtMs": clock.now() - 60_000})
		payload = append(payload, '\n')
		if err := os.WriteFile(lock, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := queue.ClaimTask(repo, "L-3", "w1", &queue.ClaimOptions{Now: clock.now}); got == nil {
			t.Fatal("claim refused on wedged lock")
		}
		if stored := queue.ReadTask(repo, "L-3"); stored == nil || stored.LeaseOwner == nil || *stored.LeaseOwner != "w1" {
			t.Fatalf("leaseOwner = %+v", stored)
		}
	})

	t.Run("expired lease is reclaimable and bumps the generation (never reused)", func(t *testing.T) {
		repo := tmpRepo(t)
		clock := fakeClock{at: t0}
		enqueue(t, repo, queue.EnqueueInput{ID: "L-4", Title: "l", Goal: "Goal: l"})
		opts := &queue.ClaimOptions{Now: clock.now, LeaseMs: 1000}
		first := queue.ClaimTask(repo, "L-4", "w1", opts)
		if queue.LeaseIsExpired(first, clock.now()) {
			t.Fatal("fresh lease reported expired")
		}
		// still inside the lease: nobody else gets in
		if second := queue.ClaimTask(repo, "L-4", "w2", opts); second != nil {
			t.Fatalf("reclaimed inside lease = %+v", second)
		}
		clock.advance(1000)
		reclaimed := queue.ClaimTask(repo, "L-4", "w2", opts)
		if reclaimed == nil || *reclaimed.LeaseGeneration != *first.LeaseGeneration+1 || *reclaimed.LeaseOwner != "w2" || *reclaimed.Attempts != 2 {
			t.Fatalf("reclaimed = %+v", reclaimed)
		}
		if queue.LeaseIsExpired(reclaimed, clock.now()+999) {
			t.Fatal("fresh reclaim reported expired")
		}
	})

	t.Run("claimNextPending reclaims an expired lease instead of wedging on it", func(t *testing.T) {
		repo := tmpRepo(t)
		clock := fakeClock{at: t0}
		enqueue(t, repo, queue.EnqueueInput{ID: "L-5", Title: "l", Goal: "Goal: l"})
		opts := &queue.ClaimOptions{Now: clock.now, LeaseMs: 1000}
		if first := queue.ClaimNextPending(repo, "w1", opts); first == nil || *first.LeaseGeneration != 1 {
			t.Fatalf("first = %+v", first)
		}
		if second := queue.ClaimNextPending(repo, "w2", opts); second != nil {
			t.Fatalf("inside-lease claim = %+v", second)
		}
		clock.advance(1000)
		reclaimed := queue.ClaimNextPending(repo, "w2", opts)
		if reclaimed == nil || *reclaimed.LeaseGeneration != 2 || *reclaimed.LeaseOwner != "w2" {
			t.Fatalf("reclaimed = %+v", reclaimed)
		}
	})

	t.Run("stale-generation writes are refused: complete, fail, and requeue", func(t *testing.T) {
		repo := tmpRepo(t)
		clock := fakeClock{at: t0}
		enqueue(t, repo, queue.EnqueueInput{ID: "L-6", Title: "l", Goal: "Goal: l"})
		opts := &queue.ClaimOptions{Now: clock.now, LeaseMs: 1000}
		queue.ClaimTask(repo, "L-6", "w1", opts)
		clock.advance(1000)
		queue.ClaimTask(repo, "L-6", "w2", opts) // generation 2
		// w1 woke up late and still holds generation 1
		if got := queue.CompleteTask(repo, "L-6", 1, nil); got != nil {
			t.Fatalf("stale complete = %+v", got)
		}
		if got := queue.FailTask(repo, "L-6", 1, "boom", nil); got != nil {
			t.Fatalf("stale fail = %+v", got)
		}
		if got := queue.RequeueTask(repo, "L-6", 1, "oops", nil); got != nil {
			t.Fatalf("stale requeue = %+v", got)
		}
		if got := queue.SetTaskStatus(repo, "L-6", queue.StatusDone, "", 1, nil); got != nil {
			t.Fatalf("stale setTaskStatus = %+v", got)
		}
		held := queue.ReadTask(repo, "L-6")
		if held.Status != queue.StatusClaimed || *held.LeaseOwner != "w2" || *held.LeaseGeneration != 2 || held.LastError != nil {
			t.Fatalf("held = %+v", held)
		}
		// the current token writes
		if done := queue.CompleteTask(repo, "L-6", 2, nil); done == nil || done.Status != queue.StatusDone {
			t.Fatalf("current-token complete = %+v", done)
		}
	})

	t.Run("requeue releases the lease and kills the releasing worker token", func(t *testing.T) {
		repo := tmpRepo(t)
		clock := fakeClock{at: t0}
		enqueue(t, repo, queue.EnqueueInput{ID: "L-7", Title: "l", Goal: "Goal: l"})
		opts := &queue.ClaimOptions{Now: clock.now}
		queue.ClaimTask(repo, "L-7", "w1", opts)
		requeued := queue.RequeueTask(repo, "L-7", 1, "transient infra", nil)
		if requeued == nil || requeued.Status != queue.StatusPending || *requeued.LeaseGeneration != 2 ||
			requeued.LeaseOwner != nil || !strings.Contains(*requeued.LastError, "transient infra") {
			t.Fatalf("requeued = %+v", requeued)
		}
		// a late completion from the worker that released it is refused
		if late := queue.CompleteTask(repo, "L-7", 1, nil); late != nil {
			t.Fatalf("late complete = %+v", late)
		}
		if stored := queue.ReadTask(repo, "L-7"); stored.Status != queue.StatusPending {
			t.Fatalf("status = %s", stored.Status)
		}
		// and the next claim continues the sequence rather than reusing 1
		next := queue.ClaimTask(repo, "L-7", "w2", opts)
		if next == nil || *next.LeaseGeneration != 3 {
			t.Fatalf("next claim = %+v", next)
		}
	})

	t.Run("failTask records the detail and completeTask clears it under the current token", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "L-8", Title: "l", Goal: "Goal: l"})
		claimed := queue.ClaimTask(repo, "L-8", "w1", nil)
		gen := *claimed.LeaseGeneration
		if failed := queue.FailTask(repo, "L-8", int64(gen), "gate red", nil); failed == nil || *failed.LastError != "gate red" {
			t.Fatalf("failed = %+v", failed)
		}
		if stored := queue.ReadTask(repo, "L-8"); stored.Status != queue.StatusFailed {
			t.Fatalf("status = %s", stored.Status)
		}
		// terminal failed is not claimable: it must be requeued by an
		// administrative write first
		if retried := queue.ClaimTask(repo, "L-8", "w2", nil); retried != nil {
			t.Fatalf("failed task claimable: %+v", retried)
		}
		queue.SetTaskStatus(repo, "L-8", queue.StatusPending, "", -1, nil)
		again := queue.ClaimTask(repo, "L-8", "w2", nil)
		if again == nil || *again.LeaseGeneration != gen+1 {
			t.Fatalf("again = %+v", again)
		}
		if done := queue.CompleteTask(repo, "L-8", int64(*again.LeaseGeneration), nil); done == nil || done.LastError != nil {
			t.Fatalf("done = %+v", done)
		}
	})

	t.Run("a legacy claimed task with no lease fields is reclaimable, not wedged", func(t *testing.T) {
		repo := tmpRepo(t)
		clock := fakeClock{at: t0}
		enqueue(t, repo, queue.EnqueueInput{ID: "L-9", Title: "l", Goal: "Goal: l"})
		queue.ClaimTask(repo, "L-9", "w1", &queue.ClaimOptions{Now: clock.now})
		// Rewrite the record as a pre-lease build left it: claimed, no lease.
		legacy := queue.ReadTask(repo, "L-9")
		legacy.LeaseGeneration = nil
		legacy.LeaseOwner = nil
		legacy.LeaseExpiresAt = nil
		writeQueueJSON(t, repo, "L-9", legacy)
		reclaimed := queue.ClaimTask(repo, "L-9", "w2", &queue.ClaimOptions{Now: clock.now})
		if reclaimed == nil || *reclaimed.LeaseGeneration != 1 || *reclaimed.LeaseOwner != "w2" {
			t.Fatalf("reclaimed = %+v", reclaimed)
		}
	})
}

func TestStatusUpdatesAndPrune(t *testing.T) {
	t.Run("setTaskStatus failed records lastError, done clears it", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "S-1", Title: "s", Goal: "Goal: s"})
		queue.ClaimTask(repo, "S-1", "w1", nil)
		queue.SetTaskStatus(repo, "S-1", queue.StatusFailed, "boom", -1, nil)
		if stored := queue.ReadTask(repo, "S-1"); stored == nil || !strings.Contains(*stored.LastError, "boom") {
			t.Fatalf("lastError = %+v", stored)
		}
		queue.SetTaskStatus(repo, "S-1", queue.StatusDone, "", -1, nil)
		stored := queue.ReadTask(repo, "S-1")
		if stored.Status != queue.StatusDone || stored.LastError != nil {
			t.Fatalf("done record = %+v", stored)
		}
	})

	t.Run("pruneDone removes done tasks", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "P-1", Title: "p", Goal: "Goal: p"})
		queue.ClaimTask(repo, "P-1", "w1", nil)
		queue.SetTaskStatus(repo, "P-1", queue.StatusDone, "", -1, nil)
		if n := queue.PruneDone(repo, 0); n != 1 {
			t.Fatalf("pruned = %d", n)
		}
		if n := len(queue.ListTasks(repo, "")); n != 0 {
			t.Fatalf("remaining = %d", n)
		}
	})

	t.Run("taskCount reflects totals per status", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "T-1", Title: "a", Goal: "Goal: a"})
		enqueue(t, repo, queue.EnqueueInput{ID: "T-2", Title: "b", Goal: "Goal: b"})
		queue.ClaimTask(repo, "T-1", "w1", nil)
		c := queue.TaskCountOf(repo)
		if c.Total != 2 || c.Pending != 1 || c.Claimed != 1 {
			t.Fatalf("counts = %+v", c)
		}
	})

	t.Run("updateTask patches fields", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "U-1", Title: "u", Goal: "Goal: u"})
		titleU := "uu"
		queue.UpdateTask(repo, "U-1", &queue.TaskPatch{Title: &titleU}, -1, nil)
		if got := queue.ReadTask(repo, "U-1"); got == nil || got.Title != "uu" {
			t.Fatalf("title = %+v", got)
		}
	})

	t.Run("returns empty/null gracefully when dirs missing", func(t *testing.T) {
		repo := tmpRepo(t)
		if got := queue.ListTasks(repo, ""); len(got) != 0 {
			t.Fatalf("list = %v", got)
		}
		if got := queue.ReadTask(repo, "nope"); got != nil {
			t.Fatalf("read = %v", got)
		}
		if got := queue.ClaimNextPending(repo, "w1", nil); got != nil {
			t.Fatalf("claimNext = %v", got)
		}
	})

	t.Run("task JSON is 2-space indented with a trailing newline", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "FMT-1", Title: "f", Goal: "Goal: f"})
		raw, err := os.ReadFile(filepath.Join(queue.QueueDir(repo), "FMT-1.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(string(raw), "}\n") || !strings.Contains(string(raw), "\n  \"id\"") {
			t.Fatalf("unexpected task-file formatting: %q", string(raw[:min(120, len(raw))]))
		}
	})
}

func TestSanitizeIDEdgeCases(t *testing.T) {
	cases := []struct{ in, want string }{
		{"TASK / 1 !!", "TASK-1"},
		{"a--b", "a-b"},
		{"--x--", "x"},
		{"a.b_c-d", "a.b_c-d"},
		{"///", ""},
	}
	for _, tc := range cases {
		got, err := queue.SanitizeID(tc.in)
		if tc.want == "" {
			if err == nil {
				t.Fatalf("SanitizeID(%q): want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("SanitizeID(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("SanitizeID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
