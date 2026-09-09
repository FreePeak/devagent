package queue_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/tui"
)

// plain strips ANSI escapes so chip/box contiguity can be asserted on
// visible text.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return ansiRe.ReplaceAllString(s, "") }

func countsOf(repoPath string) tui.QueueCounts {
	c := queue.TaskCountOf(repoPath)
	return tui.QueueCounts{Total: c.Total, Pending: c.Pending, Claimed: c.Claimed, Done: c.Done, Failed: c.Failed}
}

func TestQueueListHumanCard(t *testing.T) {
	t.Run("empty queue card shows counts and one next action pointing at status", func(t *testing.T) {
		repo := tmpRepo(t)
		card := plain(tui.RenderQueueCard(nil, countsOf(repo), 80))
		lines := strings.Split(card, "\n")
		nextCount := 0
		for _, l := range lines {
			if strings.Contains(l, "next:") {
				nextCount++
			}
		}
		for _, want := range []string{"Queue ─", "pending 0", "next:", "devagent status", "╰"} {
			if !strings.Contains(card, want) {
				t.Fatalf("card missing %q:\n%s", want, card)
			}
		}
		if nextCount != 1 {
			t.Fatalf("next: appears %d times, want 1", nextCount)
		}
	})

	t.Run("pending tasks suggest consume --auto-pr as the next action", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "T1", Title: "research", Goal: "do the thing"})
		enqueue(t, repo, queue.EnqueueInput{ID: "T2", Title: "implement", Goal: "build it"})
		tasks := queue.ListTasks(repo, "")
		var rows []tui.QueueCardTask
		for _, task := range tasks {
			rows = append(rows, tui.QueueCardTask{ID: task.ID, Title: task.Title, Status: string(task.Status)})
		}
		card := plain(tui.RenderQueueCard(rows, countsOf(repo), 80))
		lines := strings.Split(card, "\n")
		nextCount := 0
		for _, l := range lines {
			if strings.Contains(l, "next:") {
				nextCount++
			}
		}
		for _, want := range []string{"pending 2", "T1", "●", "devagent consume --auto-pr"} {
			if !strings.Contains(card, want) {
				t.Fatalf("card missing %q:\n%s", want, card)
			}
		}
		if nextCount != 1 {
			t.Fatalf("next: appears %d times, want 1", nextCount)
		}
	})

	t.Run("--json contract remains a task array (documented via ListTasks shape)", func(t *testing.T) {
		repo := tmpRepo(t)
		enqueue(t, repo, queue.EnqueueInput{ID: "T1", Title: "research", Goal: "do the thing"})
		tasks := queue.ListTasks(repo, "")
		raw, err := json.MarshalIndent(tasks, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		var parsed []map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed[0]["id"] != "T1" {
			t.Fatalf("first id = %v", parsed[0]["id"])
		}
	})
}
