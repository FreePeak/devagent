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
	// pid/startedAt identify THIS acquisition: Release unlinks the file
	// only when it still carries them (issue #316, 2026-09-11: a finished
	// run's deferred Release unlinked TASK.lock after a TTL stale-break had
	// handed it to a newer live run — runs.active lied and the newer run
	// lost its lock).
	pid       int64
	startedAt int64
	released  bool
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
	raw, err := os.ReadFile(l.Path)
	if err != nil {
		return // gone or unreadable: nothing of ours to unlink
	}
	var holder struct {
		Pid       *int64 `json:"pid"`
		StartedAt *int64 `json:"startedAt"`
	}
	if json.Unmarshal(raw, &holder) != nil || holder.Pid == nil || holder.StartedAt == nil ||
		*holder.Pid != l.pid || *holder.StartedAt != l.startedAt {
		return // the file belongs to a later acquisition — never unlink it
	}
	_ = os.Remove(l.Path) // rmSync(path, { force: true })
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
// lock payload is {"pid":<pid>,"startedAt":<ms>} — JSON.stringify key order.
//
// Atomicity: the judge-break-write sequence runs under fenceAcquire, an
// exclusive flock taken for the duration only (unix). Two processes that
// read the same stale lock can no longer both remove-and-write — exactly
// one winner per acquisition — and no contender can unlink a lock another
// process re-acquired mid-judge. A contender losing the flock itself
// refuses: the flock holder either acquires (the refuser must refuse too)
// or the lock was live anyway. The new payload is published with an atomic
// rename so a concurrent reader never sees a half-written lock file.
// Non-unix runs the sequence unfenced (ponytail: no portable flock —
// upgrade path is LockFileEx in a lock_other GOOS split).
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
	payload := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"startedAt":` + strconv.FormatInt(now, 10) + `}`

	return fenceAcquire(path, func() *RunLock {
		if data, err := os.ReadFile(path); err == nil {
			var holder struct {
				Pid       *int64 `json:"pid"`
				StartedAt *int64 `json:"startedAt"`
			}
			if json.Unmarshal(data, &holder) == nil {
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
		// dead holder, pid-less stale, corrupt, unreadable, or absent:
		// take the lock (latest-wins). Write to a temp file and rename so
		// the publish is atomic — a concurrent reader sees either the old
		// payload or ours, never a truncated file.
		tmp, err := os.CreateTemp(locksDir, ".acquire-")
		if err != nil {
			return nil
		}
		tmpName := tmp.Name()
		defer func() { _ = os.Remove(tmpName) }() // no-op once the rename has moved it
		if _, err := tmp.WriteString(payload); err != nil {
			if err := tmp.Close(); err != nil {
				return nil
			}
			return nil
		}
		if err := tmp.Close(); err != nil {
			return nil
		}
		if err := os.Chmod(tmpName, 0o644); err != nil {
			return nil
		}
		if err := os.Rename(tmpName, path); err != nil {
			return nil
		}
		return &RunLock{TicketID: ticketID, Path: path, pid: int64(os.Getpid()), startedAt: now}
	})
}
