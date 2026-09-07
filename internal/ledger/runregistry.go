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
	released bool
}

// Release removes the lock file (idempotent, like the TS closure).
func (l *RunLock) Release() {
	if l.released {
		return
	}
	l.released = true
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
// fresh lock is held elsewhere. ttlMs <= 0 uses DefaultLockTTL; a stale or
// corrupt lock is broken (latest-wins). The lock payload is
// {"pid":<pid>,"startedAt":<ms>} — JSON.stringify key order.
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
				StartedAt *int64 `json:"startedAt"`
			}
			if json.Unmarshal(data, &holder) == nil && holder.StartedAt != nil {
				if now-*holder.StartedAt <= ttlMs {
					return nil // someone else holds it fresh
				}
			}
		}
		// stale or corrupt: break the lock (latest-wins)
		_ = os.Remove(path)
	}

	payload := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"startedAt":` + strconv.FormatInt(now, 10) + `}`
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		return nil
	}
	return &RunLock{TicketID: ticketID, Path: path}
}
