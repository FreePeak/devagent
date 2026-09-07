package loopdriver

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/FreePeak/devagent/internal/queue"
)

// workerID mirrors the bash queue-claim worker id.
func workerID() string { return fmt.Sprintf("selfbuild-loop-%d", os.Getpid()) }

// claimedTask is the port of selfbuild-queue-claim.mjs's success payload.
type claimedTask struct {
	ID       string
	Goal     string
	LeaseGen int64 // valid only when HasLease
	HasLease bool
}

// claimQueueTask ports the Phase-2a queue-first claim: ClaimNextPending
// under the selfbuild-loop-<pid> worker id; an empty goal/title is failed
// in place and reads as "no claim"; the goal is normalized to a
// "Goal: "-prefixed string.
func claimQueueTask(repoPath string) *claimedTask {
	t := queue.ClaimNextPending(repoPath, workerID(), nil)
	if t == nil {
		return nil
	}
	goal := strings.TrimSpace(t.Goal)
	if goal == "" {
		goal = strings.TrimSpace(t.Title)
	}
	if goal == "" {
		// Mirror the mjs: a claimable task with no usable text is failed
		// fenced (or unfenced when no lease) and the driver continues.
		gen := int64(0)
		if t.LeaseGeneration != nil {
			gen = int64(*t.LeaseGeneration)
		}
		_ = queue.FailTask(repoPath, t.ID, gen, "empty goal and title", nil)
		return nil
	}
	if !strings.HasPrefix(goal, "Goal: ") {
		goal = "Goal: " + goal
	}
	c := &claimedTask{ID: t.ID, Goal: goal}
	if t.LeaseGeneration != nil {
		c.LeaseGen = int64(*t.LeaseGeneration)
		c.HasLease = true
	}
	return c
}

// markQueueTaskDone ports selfbuild-queue-done.mjs: fenced done/failed
// writes when a lease generation is known, unfenced SetTaskStatus otherwise.
// Returns the refusal message on a stale-lease refusal (the mjs printed it
// to stderr and exited 1; the driver discarded it) so tests can assert the
// fencing contract.
func markQueueTaskDone(repoPath, taskID, status, detail string, lease *claimedTask) (refused string) {
	if lease != nil && lease.HasLease {
		var written *queue.QueuedTask
		if status == "done" {
			written = queue.CompleteTask(repoPath, taskID, lease.LeaseGen, nil)
		} else {
			written = queue.FailTask(repoPath, taskID, lease.LeaseGen, detail, nil)
		}
		if written == nil {
			return fmt.Sprintf("%s -> %s REFUSED (stale lease generation %s)", taskID, status, strconv.FormatInt(lease.LeaseGen, 10))
		}
		return ""
	}
	_ = queue.SetTaskStatus(repoPath, taskID, queue.QueuedTaskStatus(status), detail, -1, nil)
	return ""
}
