// Run dedup / latest-wins: the Go port of src/runregistry.ts (sweep lesson).
// One active pipeline per ticket key across processes. Lock files live under
// <home>/locks/<sanitized-ticket>.lock; a stale lock (older than the one-hour
// TTL) is broken rather than blocking forever.
//
// The canonical port lives in internal/ledger next to the run logger that
// shares its storage layout; this thin wrapper keeps the pinned
// pipeline.TryAcquireRun(home, ticketID) shape for the cli wiring, where
// home is the resolved DEVAGENT_HOME ("$DEVAGENT_HOME || ~/.devagent").

package pipeline

import "github.com/FreePeak/devagent/internal/ledger"

// RunLock mirrors runregistry.ts RunLock. Release is safe to call twice.
type RunLock = ledger.RunLock

// TryAcquireRun mirrors tryAcquireRun: acquire the run lock for ticketID, or
// nil when a fresh lock is held elsewhere. A stale or corrupt lock is broken
// (latest-wins) and the payload is {"pid":<pid>,"startedAt":<ms>}.
func TryAcquireRun(home, ticketID string) *RunLock {
	return ledger.TryAcquireRun(home, ticketID, 0)
}
