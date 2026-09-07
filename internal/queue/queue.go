// Package queue is the Go port of src/queue.ts (FR-GO-07, issue #194):
// the queued-task store under .devagent/queue + .devagent/prds with fenced
// claim semantics (FR-VIS-09).

package queue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// QueuedTaskStatus mirrors the TS QueuedTaskStatus union.
type QueuedTaskStatus string

// Status values mirror the TS literals byte-for-byte.
const (
	StatusPending QueuedTaskStatus = "pending"
	StatusClaimed QueuedTaskStatus = "claimed"
	StatusDone    QueuedTaskStatus = "done"
	StatusFailed  QueuedTaskStatus = "failed"
)

// QueuedTask mirrors the TS QueuedTask interface. Optional TS fields are
// pointers so "absent" round-trips as byte-compatible JSON (omitted, not
// null).
type QueuedTask struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Full goal/prompt text passed to devagent task.
	Goal               string           `json:"goal"`
	Description        *string          `json:"description,omitempty"`
	AcceptanceCriteria []string         `json:"acceptanceCriteria"`
	Status             QueuedTaskStatus `json:"status"`
	CreatedAt          string           `json:"createdAt"`
	UpdatedAt          string           `json:"updatedAt"`
	ClaimedBy          *string          `json:"claimedBy,omitempty"`
	ClaimedAt          *string          `json:"claimedAt,omitempty"`
	PrdPath            *string          `json:"prdPath,omitempty"`
	Source             *string          `json:"source,omitempty"`
	// Fencing token for the current claim (FR-VIS-09 extended from the loop
	// driver to queue claims). Every claim — including an expired-lease
	// reclaim — increments it and never reuses a value, so a write that
	// carries an older generation provably belongs to a worker that lost
	// the task.
	LeaseGeneration *int `json:"leaseGeneration,omitempty"`
	// ISO deadline after which a 'claimed' task is reclaimable by another
	// worker.
	LeaseExpiresAt *string `json:"leaseExpiresAt,omitempty"`
	// Worker holding the current lease (stamped per claim beside claimedBy).
	LeaseOwner *string `json:"leaseOwner,omitempty"`
	// Last failure detail for retry visibility.
	LastError *string `json:"lastError,omitempty"`
	Attempts  *int    `json:"attempts,omitempty"`
	// Cross-board retry memory (Q27): executor failure class carried from
	// an archived board for a re-bridged goal. Tasks carrying one claim
	// after all clean tasks (claimNextPending two-tier order).
	FailureClass *string `json:"failureClass,omitempty"`
}

// EnqueueInput mirrors the TS EnqueueInput interface.
type EnqueueInput struct {
	ID                 string
	Title              string
	Goal               string
	Description        string
	AcceptanceCriteria []string
	PrdMarkdown        string
	Source             string
	// Carried executor failure class from a prior archived board for this
	// goal (Q27).
	FailureClass string
}

// ErrInvalidTaskID mirrors the TS throw: `Invalid task id "<id>"`.
type ErrInvalidTaskID struct{ ID string }

func (e ErrInvalidTaskID) Error() string { return fmt.Sprintf("Invalid task id %q", e.ID) }

// ErrAlreadyQueued mirrors the TS throw: `Task <id> already queued`.
type ErrAlreadyQueued struct{ ID string }

func (e ErrAlreadyQueued) Error() string { return "Task " + e.ID + " already queued" }

// SanitizeID mirrors the TS sanitizeId: map every char outside
// [A-Za-z0-9._-] to '-', collapse runs, trim leading/trailing '-'.
func SanitizeID(id string) (string, error) {
	// Map every char outside [A-Za-z0-9._-] to '-', then collapse '-+' runs
	// to a single '-' (the TS chained .replace does exactly this).
	var b strings.Builder
	for _, r := range id {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := b.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if s == "" {
		return "", ErrInvalidTaskID{ID: id}
	}
	return s, nil
}

// QueueDir mirrors the TS queueDir.
func QueueDir(repoPath string) string {
	return filepath.Join(repoPath, ".devagent", "queue")
}

// PrdsDir mirrors the TS prdsDir.
func PrdsDir(repoPath string) string {
	return filepath.Join(repoPath, ".devagent", "prds")
}

// EnsureQueueDirs mirrors the TS ensureQueueDirs.
func EnsureQueueDirs(repoPath string) {
	_ = os.MkdirAll(QueueDir(repoPath), 0o755)
	_ = os.MkdirAll(PrdsDir(repoPath), 0o755)
}

func taskPath(repoPath, id string) string {
	s, err := SanitizeID(id)
	if err != nil {
		panic(err) // mirrors the TS throw inside the path helper
	}
	return filepath.Join(QueueDir(repoPath), s+".json")
}

func prdPath(repoPath, id string) string {
	s, err := SanitizeID(id)
	if err != nil {
		panic(err)
	}
	return filepath.Join(PrdsDir(repoPath), s+".md")
}

// NowIso mirrors the TS nowIso: millisecond-precision UTC ISO-8601.
func NowIso() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func isoAt(millis int64) string {
	return time.UnixMilli(millis).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func readTaskFile(path string) *QueuedTask {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var t QueuedTask
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil
	}
	return &t
}

// writeTaskFile mirrors the TS writeTaskFile: JSON with 2-space indent plus
// a trailing newline, via a tmp+rename atomic write (falling back to a
// direct write when rename fails, e.g. across devices).
func writeTaskFile(path string, task *QueuedTask) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return
	}
	data = append(data, '\n')
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if werr := os.WriteFile(tmp, data, 0o644); werr != nil {
		return
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		_ = os.WriteFile(path, data, 0o644)
		_ = os.Remove(tmp)
	}
}

// DefaultLeaseMs mirrors the TS DEFAULT_LEASE_MS (2h, sized to the longest
// legitimate single claim: scripts/selfbuild-loop.sh caps one task at
// SELFBUILD_TASK_TIMEOUT = 7200s, so a healthy in-flight claim is never
// reclaimed under its owner while a crashed holder's lease still lapses
// inside one iteration).
const DefaultLeaseMs = 2 * 60 * 60 * 1000

// DefaultClaimLockStaleMs mirrors the TS DEFAULT_CLAIM_LOCK_STALE_MS: a
// claim lock is held only for the read-modify-write of one task file, so a
// lock older than this belongs to a process that died mid-claim.
const DefaultClaimLockStaleMs = 30_000

// ClaimOptions mirrors the TS ClaimOptions.
type ClaimOptions struct {
	// Lease lifetime before the claim becomes reclaimable (default
	// DefaultLeaseMs). 0 uses the default.
	LeaseMs int64
	// Injectable clock (ms since epoch) so lease behaviour is testable
	// without sleeping. nil uses time.Now.
	Now func() int64
	// Age past which a wedged claim lock is broken (default
	// DefaultClaimLockStaleMs). 0 uses the default.
	LockStaleMs int64
}

// FencedWriteOptions mirrors the TS FencedWriteOptions.
type FencedWriteOptions struct {
	Now         func() int64
	LockStaleMs int64
}

func (o *ClaimOptions) now() func() int64 {
	if o != nil && o.Now != nil {
		return o.Now
	}
	return func() int64 { return time.Now().UnixMilli() }
}

func (o *FencedWriteOptions) now() func() int64 {
	if o != nil && o.Now != nil {
		return o.Now
	}
	return func() int64 { return time.Now().UnixMilli() }
}

func (o *ClaimOptions) leaseMs() int64 {
	if o != nil && o.LeaseMs > 0 {
		return o.LeaseMs
	}
	return DefaultLeaseMs
}

func (o *ClaimOptions) lockStaleMs() int64 {
	if o != nil && o.LockStaleMs > 0 {
		return o.LockStaleMs
	}
	return DefaultClaimLockStaleMs
}

func (o *FencedWriteOptions) lockStaleMs() int64 {
	if o != nil && o.LockStaleMs > 0 {
		return o.LockStaleMs
	}
	return DefaultClaimLockStaleMs
}

func claimLockPath(repoPath, id string) string {
	s, _ := SanitizeID(id)
	return filepath.Join(QueueDir(repoPath), s+".claim.lock")
}

// LeaseIsExpired mirrors the TS leaseIsExpired: true when a claimed task's
// lease has lapsed and another worker may take it. A 'claimed' task with no
// leaseExpiresAt predates leasing, so it is treated as lapsed — otherwise
// legacy claims would wedge the queue head forever.
func LeaseIsExpired(task *QueuedTask, now int64) bool {
	if task.LeaseExpiresAt == nil || *task.LeaseExpiresAt == "" {
		return true
	}
	at, err := parseIso(*task.LeaseExpiresAt)
	if err != nil {
		return true
	}
	return at <= now
}

// parseIso parses a JS-style ISO timestamp. Time zone: Go's time package
// accepts the trailing Z / +hh:mm forms directly.
func parseIso(s string) (int64, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, err
	}
	return t.UnixMilli(), nil
}

// claimableNow mirrors the TS claimableNow: pending, or claimed past its
// lease.
func claimableNow(task *QueuedTask, now int64) bool {
	if task.Status == StatusPending {
		return true
	}
	return task.Status == StatusClaimed && LeaseIsExpired(task, now)
}

// acquireClaimLock mirrors the TS acquireClaimLock: acquire the per-task
// claim lock atomically with link() (os.Link), so exactly one caller wins
// even when several race at once — and unlike flock it is portable to
// macOS. The holder writes its identity into the link target first, so a
// wedged lock is diagnosable. Returns the lock path on success, "" when
// another claimant holds it.
func acquireClaimLock(repoPath, id, workerID string, now, staleMs int64) string {
	lock := claimLockPath(repoPath, id)
	tmp := fmt.Sprintf("%s.tmp.%d.%d", lock, os.Getpid(), now)
	for attempt := 0; attempt < 2; attempt++ {
		payload, _ := json.Marshal(map[string]any{
			"workerId":     workerID,
			"pid":          os.Getpid(),
			"acquiredAtMs": now,
		})
		payload = append(payload, '\n')
		if werr := os.WriteFile(tmp, payload, 0o644); werr != nil {
			return ""
		}
		err := os.Link(tmp, lock)
		_ = os.Remove(tmp)
		if err == nil {
			return lock
		}
		// EEXIST is the only contention signal; any other errno (missing
		// dir, EPERM on a foreign filesystem) is ours to abort on, not to
		// retry.
		if !os.IsExist(err) {
			return ""
		}
		if attempt == 0 && claimLockIsStale(lock, now, staleMs) {
			_ = os.Remove(lock)
			continue
		}
		return ""
	}
	return ""
}

// claimLockIsStale mirrors the TS claimLockIsStale: a lock with no readable
// timestamp is corrupt or legacy — break it (latest-wins).
func claimLockIsStale(lock string, now, staleMs int64) bool {
	raw, err := os.ReadFile(lock)
	if err != nil {
		return true
	}
	var obj struct {
		AcquiredAtMs *int64 `json:"acquiredAtMs"`
	}
	if json.Unmarshal(raw, &obj) != nil || obj.AcquiredAtMs == nil {
		return true
	}
	return now-*obj.AcquiredAtMs > staleMs
}

func releaseClaimLock(lock string) {
	_ = os.Remove(lock)
}

// EnqueueTask mirrors the TS enqueueTask: enqueue a new task; errors if the
// id already exists. Writes PRD markdown when provided.
func EnqueueTask(repoPath string, input EnqueueInput) (*QueuedTask, error) {
	EnsureQueueDirs(repoPath)
	id, err := SanitizeID(input.ID)
	if err != nil {
		return nil, err
	}
	path := taskPath(repoPath, id)
	if _, statErr := os.Stat(path); statErr == nil {
		return nil, ErrAlreadyQueued{ID: id}
	}
	ts := NowIso()
	task := QueuedTask{
		ID:                 id,
		Title:              truncate(input.Title, 120),
		Goal:               input.Goal,
		AcceptanceCriteria: orEmpty(input.AcceptanceCriteria),
		Status:             StatusPending,
		CreatedAt:          ts,
		UpdatedAt:          ts,
		Attempts:           intPtr(0),
	}
	if input.Description != "" {
		task.Description = strPtr(input.Description)
	}
	if input.Source != "" {
		task.Source = strPtr(input.Source)
	} else {
		task.Source = strPtr("scout")
	}
	if input.FailureClass != "" {
		task.FailureClass = strPtr(input.FailureClass)
	}
	if input.PrdMarkdown != "" {
		p := prdPath(repoPath, id)
		if werr := os.WriteFile(p, []byte(input.PrdMarkdown), 0o644); werr != nil {
			return nil, werr
		}
		task.PrdPath = strPtr(p)
	}
	writeTaskFile(path, &task)
	return &task, nil
}

// ListTasks mirrors the TS listTasks: list tasks, optionally filtered by
// status. Sorted by createdAt ascending (string compare, like the TS
// localeCompare on ISO timestamps).
func ListTasks(repoPath string, filter QueuedTaskStatus) []*QueuedTask {
	dir := QueueDir(repoPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var tasks []*QueuedTask
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		t := readTaskFile(filepath.Join(dir, name))
		if t == nil {
			continue
		}
		if filter != "" && t.Status != filter {
			continue
		}
		tasks = append(tasks, t)
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		return tasks[i].CreatedAt < tasks[j].CreatedAt
	})
	return tasks
}

// ReadTask mirrors the TS readTask.
func ReadTask(repoPath, id string) *QueuedTask {
	s, err := SanitizeID(id)
	if err != nil {
		return nil
	}
	return readTaskFile(filepath.Join(QueueDir(repoPath), s+".json"))
}

// TaskPatch mirrors the TS updateTask patch object: Partial<Omit<QueuedTask,
// 'id' | 'createdAt'>>. Nil fields leave the stored value untouched; use the
// Clear* bools to force a field to undefined (the TS patch delete).
type TaskPatch struct {
	Title               *string
	Goal                *string
	Description         *string
	DescriptionClear    bool
	AcceptanceCriteria  []string
	Status              *QueuedTaskStatus
	UpdatedAt           *string
	ClaimedBy           *string
	ClaimedByClear      bool
	ClaimedAt           *string
	ClaimedAtClear      bool
	PrdPath             *string
	PrdPathClear        bool
	Source              *string
	SourceClear         bool
	LeaseGeneration     *int
	LeaseOwner          *string
	LeaseOwnerClear     bool
	LeaseExpiresAt      *string
	LeaseExpiresAtClear bool
	LastError           *string
	LastErrorClear      bool
	Attempts            *int
	FailureClass        *string
	FailureClassClear   bool
}

func applyPatch(cur *QueuedTask, p *TaskPatch) {
	if p == nil {
		return
	}
	if p.Title != nil {
		cur.Title = *p.Title
	}
	if p.Goal != nil {
		cur.Goal = *p.Goal
	}
	if p.DescriptionClear {
		cur.Description = nil
	} else if p.Description != nil {
		cur.Description = p.Description
	}
	if p.AcceptanceCriteria != nil {
		cur.AcceptanceCriteria = p.AcceptanceCriteria
	}
	if p.Status != nil {
		cur.Status = *p.Status
	}
	if p.UpdatedAt != nil {
		cur.UpdatedAt = *p.UpdatedAt
	}
	if p.ClaimedByClear {
		cur.ClaimedBy = nil
	} else if p.ClaimedBy != nil {
		cur.ClaimedBy = p.ClaimedBy
	}
	if p.ClaimedAtClear {
		cur.ClaimedAt = nil
	} else if p.ClaimedAt != nil {
		cur.ClaimedAt = p.ClaimedAt
	}
	if p.PrdPathClear {
		cur.PrdPath = nil
	} else if p.PrdPath != nil {
		cur.PrdPath = p.PrdPath
	}
	if p.SourceClear {
		cur.Source = nil
	} else if p.Source != nil {
		cur.Source = p.Source
	}
	if p.LeaseGeneration != nil {
		cur.LeaseGeneration = p.LeaseGeneration
	}
	if p.LeaseOwnerClear {
		cur.LeaseOwner = nil
	} else if p.LeaseOwner != nil {
		cur.LeaseOwner = p.LeaseOwner
	}
	if p.LeaseExpiresAtClear {
		cur.LeaseExpiresAt = nil
	} else if p.LeaseExpiresAt != nil {
		cur.LeaseExpiresAt = p.LeaseExpiresAt
	}
	if p.LastErrorClear {
		cur.LastError = nil
	} else if p.LastError != nil {
		cur.LastError = p.LastError
	}
	if p.Attempts != nil {
		cur.Attempts = p.Attempts
	}
	if p.FailureClassClear {
		cur.FailureClass = nil
	} else if p.FailureClass != nil {
		cur.FailureClass = p.FailureClass
	}
}

// UpdateTask mirrors the TS updateTask: update a task's fields; returns the
// updated task or nil if missing.
//
// Pass ExpectedGeneration >= 0 to fence the write to the worker that
// currently holds the claim: the patch lands only while it matches the
// stored leaseGeneration, and a stale token is refused (nil, no write).
// Without it the write stays unfenced — correct for out-of-band
// administration of a task nobody has claimed (the scout's PRD backfill,
// the bridge retiring a pending goal), never for claim-lifecycle writes.
func UpdateTask(repoPath, id string, patch *TaskPatch, expectedGeneration int, opts *FencedWriteOptions) *QueuedTask {
	if expectedGeneration >= 0 {
		return fencedUpdate(repoPath, id, int64(expectedGeneration), patch, opts)
	}
	path := taskPath(repoPath, sanitize(id))
	cur := readTaskFile(path)
	if cur == nil {
		return nil
	}
	applyPatch(cur, patch)
	cur.ID = id
	cur.UpdatedAt = NowIso()
	writeTaskFile(path, cur)
	return cur
}

// SetTaskStatus mirrors the TS setTaskStatus.
func SetTaskStatus(repoPath, id string, status QueuedTaskStatus, detail string, expectedGeneration int, opts *FencedWriteOptions) *QueuedTask {
	patch := TaskPatch{Status: &status}
	if status == StatusFailed && detail != "" {
		patch.LastError = strPtr(truncate(detail, 2000))
	}
	if status == StatusDone {
		patch.LastErrorClear = true
	}
	return UpdateTask(repoPath, id, &patch, expectedGeneration, opts)
}

// fencedUpdate mirrors the TS fencedUpdate: read-modify-write a task under
// the claim lock, refusing the write unless generation is the task's
// current fencing token. Returns nil when the task is missing, another
// claimant holds the lock, or the token is stale — a stale token means the
// lease moved to a different worker, so this writer must not touch the
// record.
func fencedUpdate(repoPath, id string, generation int64, patch *TaskPatch, opts *FencedWriteOptions) *QueuedTask {
	nowFn := opts.now()
	path := taskPath(repoPath, sanitize(id))
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	lock := acquireClaimLock(repoPath, id, fmt.Sprintf("fenced-%d", os.Getpid()), nowFn(), opts.lockStaleMs())
	if lock == "" {
		return nil
	}
	defer releaseClaimLock(lock)
	cur := readTaskFile(path)
	if cur == nil {
		return nil
	}
	gen := 0
	if cur.LeaseGeneration != nil {
		gen = *cur.LeaseGeneration
	}
	if int64(gen) != generation {
		return nil
	}
	applyPatch(cur, patch)
	cur.UpdatedAt = isoAt(nowFn())
	writeTaskFile(path, cur)
	return cur
}

// CompleteTask mirrors the TS completeTask: fenced completion, refused once
// the lease is no longer this worker's.
func CompleteTask(repoPath, id string, generation int64, opts *FencedWriteOptions) *QueuedTask {
	done := StatusDone
	lastErrClear := true
	patch := TaskPatch{Status: &done, LastErrorClear: lastErrClear}
	return fencedUpdate(repoPath, id, generation, &patch, opts)
}

// FailTask mirrors the TS failTask: fenced terminal failure.
func FailTask(repoPath, id string, generation int64, detail string, opts *FencedWriteOptions) *QueuedTask {
	failed := StatusFailed
	patch := TaskPatch{Status: &failed}
	if detail != "" {
		patch.LastError = strPtr(truncate(detail, 2000))
	}
	return fencedUpdate(repoPath, id, generation, &patch, opts)
}

// RequeueTask mirrors the TS requeueTask: fenced requeue back to pending.
// Releases the lease by BUMPING the generation (never reusing a token), so
// the releasing worker's own writes are dead from the moment it hands the
// task back and a late completion cannot resurrect it.
func RequeueTask(repoPath, id string, generation int64, detail string, opts *FencedWriteOptions) *QueuedTask {
	pending := StatusPending
	nextGen := int(generation + 1)
	patch := TaskPatch{
		Status:              &pending,
		LeaseGeneration:     &nextGen,
		LeaseOwnerClear:     true,
		LeaseExpiresAtClear: true,
	}
	if detail != "" {
		patch.LastError = strPtr(truncate(detail, 2000))
	}
	return fencedUpdate(repoPath, id, generation, &patch, opts)
}

// ClaimTask mirrors the TS claimTask: claim a task under the atomic link()
// claim lock and stamp a fresh fencing token. Claimable when pending or
// when a previous claim's lease has lapsed — a reclaim bumps the
// generation, so the dead holder's writes are refused from then on.
// Returns nil when the task is missing, leased to a live worker, or
// another claimant currently holds the lock.
func ClaimTask(repoPath, id, workerID string, opts *ClaimOptions) *QueuedTask {
	nowFn := opts.now()
	path := taskPath(repoPath, sanitize(id))
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	lock := acquireClaimLock(repoPath, id, workerID, nowFn(), opts.lockStaleMs())
	if lock == "" {
		return nil
	}
	defer releaseClaimLock(lock)
	cur := readTaskFile(path)
	if cur == nil {
		return nil
	}
	at := nowFn()
	if !claimableNow(cur, at) {
		return nil
	}
	ts := isoAt(at)
	gen := 0
	if cur.LeaseGeneration != nil {
		gen = *cur.LeaseGeneration
	}
	generation := gen + 1
	claimed := StatusClaimed
	leaseExpires := isoAt(at + opts.leaseMs())
	patch := TaskPatch{
		Status:          &claimed,
		ClaimedBy:       strPtr(workerID),
		ClaimedAt:       strPtr(ts),
		LeaseOwner:      strPtr(workerID),
		LeaseGeneration: &generation,
		LeaseExpiresAt:  &leaseExpires,
	}
	cur.UpdatedAt = ts
	applyPatch(cur, &patch)
	cur.Attempts = intPtr(derefInt(cur.Attempts) + 1)
	writeTaskFile(path, cur)
	return cur
}

// ClaimNextPending mirrors the TS claimNextPending: claim the next
// claimable task; returns nil when none is. Two-tier order (Q27 cross-board
// retry memory): tasks without a carried failureClass claim before tasks
// with one, each tier in createdAt order.
func ClaimNextPending(repoPath, workerID string, opts *ClaimOptions) *QueuedTask {
	nowFn := opts.now()
	all := ListTasks(repoPath, "")
	var clean, carried []*QueuedTask
	for _, t := range all {
		if !claimableNow(t, nowFn()) {
			continue
		}
		if t.FailureClass == nil || *t.FailureClass == "" {
			clean = append(clean, t)
		} else {
			carried = append(carried, t)
		}
	}
	for _, t := range append(append([]*QueuedTask{}, clean...), carried...) {
		if claimed := ClaimTask(repoPath, t.ID, workerID, opts); claimed != nil {
			return claimed
		}
	}
	return nil
}

// WritePrd mirrors the TS writePrd.
func WritePrd(repoPath, id, markdown string) (string, error) {
	EnsureQueueDirs(repoPath)
	p := prdPath(repoPath, sanitize(id))
	if err := os.WriteFile(p, []byte(markdown), 0o644); err != nil {
		return "", err
	}
	existing := ReadTask(repoPath, id)
	if existing != nil && existing.PrdPath == nil {
		UpdateTask(repoPath, id, &TaskPatch{PrdPath: strPtr(p)}, -1, nil)
	}
	return p, nil
}

// ReadPrd mirrors the TS readPrd.
func ReadPrd(repoPath, id string) (string, bool) {
	p := prdPath(repoPath, sanitize(id))
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// PruneDone mirrors the TS pruneDone: remove done tasks older than
// cutoffMs (0 = all done). Returns count removed.
func PruneDone(repoPath string, cutoffMs int64) int {
	tasks := ListTasks(repoPath, StatusDone)
	removed := 0
	now := time.Now().UnixMilli()
	for _, t := range tasks {
		at, err := parseIso(t.UpdatedAt)
		if err != nil {
			continue
		}
		if now-at >= cutoffMs {
			if rmErr := os.Remove(taskPath(repoPath, t.ID)); rmErr == nil {
				removed++
			}
		}
	}
	return removed
}

// TaskCount mirrors the TS taskCount.
type TaskCount struct {
	Pending int `json:"pending"`
	Claimed int `json:"claimed"`
	Done    int `json:"done"`
	Failed  int `json:"failed"`
	Total   int `json:"total"`
}

func TaskCountOf(repoPath string) TaskCount {
	all := ListTasks(repoPath, "")
	c := TaskCount{Total: len(all)}
	for _, t := range all {
		switch t.Status {
		case StatusPending:
			c.Pending++
		case StatusClaimed:
			c.Claimed++
		case StatusDone:
			c.Done++
		case StatusFailed:
			c.Failed++
		}
	}
	return c
}

func sanitize(id string) string {
	s, err := SanitizeID(id)
	if err != nil {
		panic(err)
	}
	return s
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func truncate(s string, n int) string {
	// JS String.prototype.slice on UTF-16 units; for BMP text this is a
	// rune slice. Keep the byte-prefix behavior of the original helper
	// deterministic: count runes, then slice.
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
