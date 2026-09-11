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
	// pid/startedAt are this acquisition's identity as written to the lock
	// payload: Release only unlinks a lock that still carries them.
	pid       int
	startedAt int64
	released  bool
}

// Release removes the lock file, but only while this acquisition still owns
// it (issue #316): a lock broken as stale and re-acquired by another run
// (latest-wins) must survive this process's Release — the old unconditional
// unlink deleted a LIVE newcomer's lock at 09:51:16 and left two running
// tasks invisible to countActiveRuns. Idempotent, like the TS closure.
func (l *RunLock) Release() {
	if l.released {
		return
	}
	l.released = true
	if !l.owns() {
		return
	}
	_ = os.Remove(l.Path) // rmSync(path, { force: true })
}

// owns reports whether the on-disk payload is still this acquisition's
// (same pid AND startedAt). An unreadable, corrupt or replaced lock is not
// ours to delete.
func (l *RunLock) owns() bool {
	data, err := os.ReadFile(l.Path)
	if err != nil {
		return false
	}
	var holder struct {
		Pid       *int64 `json:"pid"`
		StartedAt *int64 `json:"startedAt"`
	}
	if json.Unmarshal(data, &holder) != nil || holder.Pid == nil || holder.StartedAt == nil {
		return false
	}
	return int(*holder.Pid) == l.pid && *holder.StartedAt == l.startedAt
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
// otherwise a stale (TTL-expired) or corrupt lock is broken (latest-wins).
// The lock payload is {"pid":<pid>,"startedAt":<ms>} — JSON.stringify key order.
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

	if _, err := os.Stat(path); err == nil {
		if data, err := os.ReadFile(path); err == nil {
			var holder struct {
				Pid       *int64 `json:"pid"`
				StartedAt *int64 `json:"startedAt"`
			}
			if json.Unmarshal(data, &holder) == nil {
				holderDead := holder.Pid != nil && *holder.Pid > 0 && !processAlive(int(*holder.Pid))
				holderFresh := holder.StartedAt != nil && now-*holder.StartedAt <= ttlMs
				if holderFresh && !holderDead {
					return nil // someone else holds it fresh and alive
				}
			}
		}
		// dead holder, stale, or corrupt: break the lock (latest-wins)
		_ = os.Remove(path)
	}

	payload := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"startedAt":` + strconv.FormatInt(now, 10) + `}`
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		return nil
	}
	return &RunLock{TicketID: ticketID, Path: path, pid: os.Getpid(), startedAt: now}
}
