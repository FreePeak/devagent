// state_glue.go holds the read-only state accessors several CLI actions need
// but no wave-1 package owns yet: the task queue (src/queue.ts) and the
// durable orchestration board (src/orchestrator/store.ts loadBoard). Both are
// plain file reads over repo state — the ports below mirror the Node readers
// exactly (same dir layout, same skip-on-corrupt discipline, same sort).
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/FreePeak/devagent/internal/tui"
)

// queueTask is the QueuedTask slice the CLI surfaces (src/queue.ts
// QueuedTask). Only the fields any human/machine surface reads are decoded.
type queueTask struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	LastError string `json:"lastError"`
}

// queueDir mirrors queue.ts queueDir().
// TODO(FR-GO-04 #194): replace with the queue package once it lands.
func queueDir(repoPath string) string { return filepath.Join(repoPath, ".devagent", "queue") }

// listQueueTasks mirrors listTasks(repoPath) with no status filter: every
// parseable task JSON in the queue dir, sorted by createdAt ascending (TS
// `a.createdAt.localeCompare(b.createdAt)` — ISO-8601 stamps compare
// identically under byte order). Unreadable/corrupt files are skipped.
func listQueueTasks(repoPath string) []queueTask {
	dir := queueDir(repoPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	// readdirSync order is what TS iterates before sorting; sorting by
	// createdAt alone leaves same-stamp ties in dir order, so read the dir
	// listing sorted (os.ReadDir is already name-sorted, matching the
	// stable-sort tie behavior of a sorted readdir on macOS/Linux).
	tasks := make([]queueTask, 0, len(names))
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var t queueTask
		if err := json.Unmarshal(raw, &t); err != nil {
			continue // readTaskFile catch -> null -> skipped
		}
		tasks = append(tasks, t)
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].CreatedAt < tasks[j].CreatedAt })
	return tasks
}

// queueCounts mirrors taskCount(): {total, pending, claimed, done, failed}.
// Divergence note: a task file carrying an unknown `status` string adds an
// extra key to the TS record; the surfaces below only ever read the five
// known buckets, so the extra key is dropped (documented, not silent — the
// Node writer only produces these four statuses).
func queueCounts(repoPath string) tui.QueueCounts {
	all := listQueueTasks(repoPath)
	c := tui.QueueCounts{Total: len(all)}
	for _, t := range all {
		switch t.Status {
		case "pending":
			c.Pending++
		case "claimed":
			c.Claimed++
		case "done":
			c.Done++
		case "failed":
			c.Failed++
		}
	}
	return c
}

// loadProjectBoard mirrors store.ts loadBoard(): the durable
// <repo>/.devagent-project.json, validated the same way (tasks must be an
// array, goal a string; anything else — including a corrupt file — reads as
// "no board"). Returned as the tui.StatusBoard slice the status view needs.
func loadProjectBoard(repoPath string) *tui.StatusBoard {
	file := filepath.Join(repoPath, ".devagent-project.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var doc struct {
		Goal  *string `json:"goal"`
		Tasks []struct {
			ID            string  `json:"id"`
			Title         string  `json:"title"`
			Status        string  `json:"status"`
			FailureDetail *string `json:"failureDetail"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	if doc.Goal == nil {
		return nil // typeof raw.goal !== 'string'
	}
	if !json.Valid(raw) {
		return nil
	}
	// Array-ness of tasks is enforced by the decode above: a non-array
	// `tasks` fails unmarshal, exactly like the TS Array.isArray guard.
	tasks := make([]tui.StatusTask, 0, len(doc.Tasks))
	for _, t := range doc.Tasks {
		ref := ""
		if t.FailureDetail != nil {
			ref = *t.FailureDetail
		}
		tasks = append(tasks, tui.StatusTask{ID: t.ID, Title: t.Title, Status: t.Status, FailureDetail: ref})
	}
	return &tui.StatusBoard{Goal: *doc.Goal, Tasks: tasks}
}
