package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// This file is the Go port of src/runregistry.ts: run dedup / latest-wins.
// One active pipeline per ticket key across processes. Lock files live under
// <home>/locks/<sanitized-ticket>.lock; a stale lock (older than ttl) is
// broken rather than blocking forever.

// RunLock is the acquired lock (TS RunLock). Release is safe to call twice.
type RunLock struct {
	TicketID string
	Path     string
	// Generation is this lease's fencing token: 1 for a fresh lock, and
	// prevGeneration+1 on every break-and-reacquire — a token is never
	// reused. A holder compares it against the on-disk payload before an
	// irreversible action (StillHeld) so a run whose lock was broken and
	// re-acquired by a newer incarnation can detect it lost the lease.
	Generation int64
	// pid/startedAt identify THIS acquisition: Release unlinks the file
	// only when it still carries them (issue #316, 2026-09-11: a finished
	// run's deferred Release unlinked TASK.lock after a TTL stale-break had
	// handed it to a newer live run — runs.active lied and the newer run
	// lost its lock).
	pid       int64
	startedAt int64
	released  bool
}

// holdsPayload reports whether the lock file still carries THIS
// acquisition's identity (pid, startedAt, generation).
func (l *RunLock) holdsPayload() bool {
	raw, err := os.ReadFile(l.Path)
	if err != nil {
		return false // gone or unreadable: nothing of ours there
	}
	var holder struct {
		Pid        *int64 `json:"pid"`
		StartedAt  *int64 `json:"startedAt"`
		Generation *int64 `json:"generation"`
	}
	if json.Unmarshal(raw, &holder) != nil || holder.Pid == nil || holder.StartedAt == nil ||
		*holder.Pid != l.pid || *holder.StartedAt != l.startedAt ||
		holder.Generation == nil || *holder.Generation != l.Generation {
		return false // the file belongs to a later acquisition — never touch it
	}
	return true
}

// StillHeld is the fencing check for lease holders: true only while the
// lock file still carries this acquisition's payload. A run whose lease
// was broken and re-acquired by a newer incarnation (stale-break winner),
// released, or whose file vanished must refuse irreversible work (the
// selfbuild task publish boundary consults this before pushing a PR).
func (l *RunLock) StillHeld() bool {
	return !l.released && l.holdsPayload()
}

// Release removes the lock file — but only when the file still holds THIS
// acquisition's payload. A lock another process broke and re-acquired
// (stale-break winner) must survive our deferred release. Idempotent, like
// the TS closure.
func (l *RunLock) Release() {
	if l.released {
		return
	}
	l.released = true
	if l.holdsPayload() {
		_ = os.Remove(l.Path) // rmSync(path, { force: true })
	}
}

// nowMillis is the wall clock in milliseconds (TS Date.now()).
func nowMillis() int64 { return time.Now().UnixMilli() }

// NowFunc lets tests pin the clock (TS opts.now, default Date.now).
var NowFunc = nowMillis

// DefaultLockTTL is the TS default ttlMs: one hour.
const DefaultLockTTL = 60 * 60 * 1000

var sanitizeRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// SanitizeKey mirrors sanitizeKey: every character outside [A-Za-z0-9._-]
// becomes '_'.
func SanitizeKey(ticketID string) string {
	return sanitizeRe.ReplaceAllString(ticketID, "_")
}

// TryAcquireRun acquires the run lock for ticketID, or returns nil when a
// fresh lock is held elsewhere. ttlMs <= 0 uses DefaultLockTTL. A lock whose
// holder pid is DEAD breaks immediately regardless of TTL (issue #316
// class, verified live 2026-09-11: a breaker-killed incarnation left
// TASK.lock with pid 6762 gone; the 1h TTL then refused every task dispatch
// for up to an hour — one orphaned lock bricks the whole selfbuild loop);
// a LIVE holder is never broken, TTL notwithstanding (issue #316
// amendment: loop task runs routinely outlive the 1h TTL, so the old
// stale-break let a newcomer steal a long run's lock); only a lock with no
// usable pid falls back to the TTL check (corrupt/legacy payloads). The
// lock payload is {"pid":<pid>,"startedAt":<ms>,"generation":<n>} (key
// order matches the Go struct field order; generation is the lease's
// fencing token — see RunLock.Generation).
func TryAcquireRun(homeDir, ticketID string, ttlMs int64) *RunLock {
	locksDir := filepath.Join(homeDir, "locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		return nil
	}
	path := filepath.Join(locksDir, SanitizeKey(ticketID)+".lock")
	now := NowFunc()
	if ttlMs <= 0 {
		ttlMs = DefaultLockTTL
	}

	prevGen := int64(0)
	if _, err := os.Stat(path); err == nil {
		if data, err := os.ReadFile(path); err == nil {
			var holder struct {
				Pid        *int64 `json:"pid"`
				StartedAt  *int64 `json:"startedAt"`
				Generation *int64 `json:"generation"`
			}
			if json.Unmarshal(data, &holder) == nil {
				if holder.Generation != nil && *holder.Generation > 0 {
					prevGen = *holder.Generation
				}
				if holder.Pid != nil && *holder.Pid > 0 {
					// pid known: liveness decides, TTL never relabels it
					// (mirrors countActiveRuns). On the non-unix fallback
					// any pid reads as alive, so only the pid-less path
					// still breaks on TTL there.
					if processAlive(int(*holder.Pid)) {
						return nil // live holder: never stolen, TTL notwithstanding
					}
				} else if holder.StartedAt != nil && now-*holder.StartedAt <= ttlMs {
					return nil // no usable pid: TTL backstops corrupt/legacy payloads
				}
			}
		}
		// dead holder, pid-less stale, or corrupt: break the lock
		// (latest-wins) and bump the lease generation — a token is never
		// reused, so the broken holder's late fenced writes die here.
		_ = os.Remove(path)
	}
	generation := prevGen + 1

	payload := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"startedAt":` + strconv.FormatInt(now, 10) +
		`,"generation":` + strconv.FormatInt(generation, 10) + `}`
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		return nil
	}
	return &RunLock{TicketID: ticketID, Path: path, Generation: generation, pid: int64(os.Getpid()), startedAt: now}
}
