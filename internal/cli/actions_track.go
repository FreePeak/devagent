// actions_track.go ports the `track` command body (src/cli.ts:1838-1860) and
// its tracker core src/tracker.ts verbatim: the progress snapshot
// (queue + scout heartbeat + orchestrator board + ledger tail + git/gh
// evidence), the .selfbuild/progress.{md,json} writers, the tracker
// heartbeat, and the Ctrl+C-interruptible loop. Every output literal is
// byte-identical to the TypeScript original — stdout strings, JSON key
// order, 2-space indent, and exit codes included (FR-GO-02, issue #193;
// tracker wiring #207).

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/FreePeak/devagent/internal/scout"
	"github.com/FreePeak/devagent/internal/spawn"
	"github.com/spf13/cobra"
)

// trackerSnapshot mirrors TrackerSnapshot (tracker.ts:20-29). Field order is
// the JSON key order JSON.stringify emits. Slices are always non-nil so they
// serialize as [] like the TS arrays; scout/board are pointers so a missing
// heartbeat/board serializes as null exactly like the TS object | null.
type trackerSnapshot struct {
	GeneratedAt   string              `json:"generatedAt"`
	Queue         queueJSON           `json:"queue"`
	RecentTasks   []trackerRecentTask `json:"recentTasks"`
	Scout         *trackerScout       `json:"scout"`
	Board         *trackerBoard       `json:"board"`
	LedgerTail    []string            `json:"ledgerTail"`
	RecentCommits []string            `json:"recentCommits"`
	OpenPRs       []string            `json:"openPrs"`
}

// trackerRecentTask mirrors Pick<QueuedTask, 'id'|'title'|'status'|
// 'updatedAt'|'lastError'>. LastError is a pointer so an absent/empty
// lastError drops the key the way JSON.stringify drops undefined (the TS
// readers treat an empty lastError as absent everywhere it is consumed).
type trackerRecentTask struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Status    string  `json:"status"`
	UpdatedAt string  `json:"updatedAt"`
	LastError *string `json:"lastError,omitempty"`
}

// trackerScout mirrors the snapshot scout object (tracker.ts:24). Pointers
// model TS optional fields: a heartbeat file missing a key serializes
// without it, exactly like JSON.stringify dropping undefined.
type trackerScout struct {
	Alive      bool    `json:"alive"`
	LastRunAt  *string `json:"lastRunAt,omitempty"`
	LastTaskID *string `json:"lastTaskId,omitempty"`
	LastStatus *string `json:"lastStatus,omitempty"`
	Worker     *string `json:"worker,omitempty"`
}

// trackerBoard mirrors the readBoardSnapshot result (tracker.ts:25).
// BlockedReason is a pointer so a nil reason drops the key while a "" reason
// (TS `??` keeps empty strings) serializes as "blockedReason": "".
type trackerBoard struct {
	Goal          string             `json:"goal"`
	Counts        trackerBoardCounts `json:"counts"`
	BlockedReason *string            `json:"blockedReason,omitempty"`
}

// trackerBoardTaskRaw mirrors the task shape readBoardSnapshot decodes from
// .devagent-project.json. Pointers preserve the undefined-vs-value split the
// TS reader relies on.
type trackerBoardTaskRaw struct {
	Status        *string               `json:"status"`
	FailureDetail *string               `json:"failureDetail"`
	Audit         *trackerBoardAuditRaw `json:"audit"`
}

// trackerBoardAuditRaw mirrors the optional audit.summary field.
type trackerBoardAuditRaw struct {
	Summary *string `json:"summary"`
}

// trackerBoardCounts preserves the TS Record<string, number> insertion
// order: JSON.stringify serializes object keys in first-seen order, and the
// `tasks:` markdown line and snapshot JSON both surface it.
type trackerBoardCounts struct {
	entries []trackerCountEntry
}

type trackerCountEntry struct {
	Key   string
	Value int
}

// add increments the count for status, appending on first sight (the TS
// `counts[t.status] = (counts[t.status] ?? 0) + 1` walk).
func (c *trackerBoardCounts) add(status string) {
	for i := range c.entries {
		if c.entries[i].Key == status {
			c.entries[i].Value++
			return
		}
	}
	c.entries = append(c.entries, trackerCountEntry{Key: status, Value: 1})
}

// MarshalJSON emits the counts object in insertion order. A missing status
// key (TS undefined) counts under the literal key "undefined", mirroring
// `counts[undefined]`. Keys use jsonStringify so no HTML escaping sneaks in
// (json.Marshal alone would escape < > &, which JSON.stringify does not).
func (c trackerBoardCounts) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, e := range c.entries {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonStringify(e.Key))
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(e.Value))
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// trackerHeartbeat mirrors TrackerHeartbeat (tracker.ts:179-183); field
// order is the JSON key order of writeTrackerHeartbeat.
type trackerHeartbeat struct {
	LastRunAt  string `json:"lastRunAt"`
	LastStatus string `json:"lastStatus"`
	LastDetail string `json:"lastDetail"`
}

// trackOnceResult mirrors TrackOnceResult (tracker.ts:200-207). Snapshot and
// the two progress paths are pointers so a failed cycle serializes/prints
// without them (TS `snapshot?: TrackerSnapshot` → undefined).
type trackOnceResult struct {
	Ok               bool
	Detail           string
	Snapshot         *trackerSnapshot
	ProgressMdPath   *string
	ProgressJsonPath *string
	HeartbeatPath    string
}

// trackCommand ports the `track` command body (src/cli.ts:1838-1860). Flag
// registration happens in root.go via the frozen surface; the TS
// `if (!opts.interval)` gate reduces to interval == 0 (Number coercion of
// undefined is NaN, falsy like 0). A failed one-shot sets the exit code to 1
// with the exact TS summary text — no cobra error wrapping.
func trackCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "track",
		Short: "Progress-tracker agent: snapshot queue+scout+ledger+git+PRs -> .selfbuild/progress.md",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			interval, _ := cmd.Flags().GetInt("interval")
			jsonOut, _ := cmd.Flags().GetBool("json")
			if interval == 0 {
				r := trackOnce(repo, 0)
				if jsonOut && r.Snapshot != nil {
					fmt.Println(marshalIndent(r.Snapshot))
				} else {
					mdPath := "(n/a)"
					if r.ProgressMdPath != nil {
						mdPath = *r.ProgressMdPath
					}
					ok := "ok"
					if !r.Ok {
						ok = "FAILED"
					}
					fmt.Printf("%s: %s\nprogress: %s\nheartbeat: %s\n", ok, r.Detail, mdPath, r.HeartbeatPath)
				}
				if !r.Ok {
					os.Exit(1)
				}
				return nil
			}
			fmt.Printf("Tracker loop started: every %dm in %s (Ctrl+C to stop)\n", interval, repo)
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			trackLoop(ctx, repo, interval, func(r trackOnceResult) {
				fmt.Printf("[track] %s\n", r.Detail)
			})
			return nil
		},
	}
}

// trackOnce mirrors trackOnce() (tracker.ts:210-227): gather → write
// .selfbuild/progress.{md,json} → heartbeat. limit 0 is the unset TS
// `opts.limit` and defaults to 10. A failed write reports the raw error in
// the heartbeat lastDetail (unprefixed) and the "track failed: " prefix in
// the returned detail — the two strings differ in the TS original too.
func trackOnce(repoPath string, limit int) trackOnceResult {
	hbPath := trackerHeartbeatPath(repoPath)
	mdPath := filepath.Join(repoPath, ".selfbuild", "progress.md")
	jsonPath := filepath.Join(repoPath, ".selfbuild", "progress.json")
	snap := collectProgressAsync(repoPath, limit)
	var err error
	if err = os.MkdirAll(filepath.Join(repoPath, ".selfbuild"), 0o755); err == nil {
		if err = os.WriteFile(mdPath, []byte(renderProgressMarkdown(snap)), 0o644); err == nil {
			err = os.WriteFile(jsonPath, []byte(marshalIndent(snap)+"\n"), 0o644)
		}
	}
	if err == nil {
		state := "down"
		if snap.Scout != nil && snap.Scout.Alive {
			state = "alive"
		}
		detail := fmt.Sprintf("queue %d (%d pending, %d failed), scout %s, %d open PR(s)",
			snap.Queue.Total, snap.Queue.Pending, snap.Queue.Failed, state, len(snap.OpenPRs))
		writeTrackerHeartbeat(repoPath, trackerHeartbeat{LastRunAt: trackerIsoNow(), LastStatus: "ok", LastDetail: detail})
		return trackOnceResult{
			Ok:               true,
			Detail:           detail,
			Snapshot:         &snap,
			ProgressMdPath:   &mdPath,
			ProgressJsonPath: &jsonPath,
			HeartbeatPath:    hbPath,
		}
	}
	writeTrackerHeartbeat(repoPath, trackerHeartbeat{LastRunAt: trackerIsoNow(), LastStatus: "failed", LastDetail: err.Error()})
	return trackOnceResult{Ok: false, Detail: "track failed: " + err.Error(), HeartbeatPath: hbPath}
}

// trackLoop mirrors trackLoop() (tracker.ts:229-245): run one cycle, report
// it, then sleep intervalMinutes (woken early by ctx cancellation — the
// AbortController signal). limit stays 0 because the TS loop calls
// trackOnce({repoPath}) with no limit.
func trackLoop(ctx context.Context, repoPath string, intervalMinutes int, onCycle func(trackOnceResult)) {
	for ctx.Err() == nil {
		r := trackOnce(repoPath, 0)
		if onCycle != nil {
			onCycle(r)
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(intervalMinutes) * time.Minute):
		}
	}
}

// collectProgress mirrors collectProgress() (tracker.ts:133-159): the
// synchronous snapshot minus the runner-backed git/gh sections, which stay
// empty here exactly as in the TS base (filled by collectProgressAsync).
func collectProgress(repoPath string, limit int) trackerSnapshot {
	if limit <= 0 {
		limit = 10
	}
	tasks := listQueueTasks(repoPath)
	start := len(tasks) - limit
	if start < 0 {
		start = 0
	}
	recent := []trackerRecentTask{}
	for i := len(tasks) - 1; i >= start; i-- {
		rt := trackerRecentTask{
			ID:        tasks[i].ID,
			Title:     tasks[i].Title,
			Status:    tasks[i].Status,
			UpdatedAt: tasks[i].UpdatedAt,
		}
		if tasks[i].LastError != "" {
			rt.LastError = &tasks[i].LastError
		}
		recent = append(recent, rt)
	}
	hb := scout.ReadHeartbeat(repoPath)
	var sc *trackerScout
	if hb != nil {
		// Date.parse(NaN) would make the TS comparison false; parseISOMs
		// returns 0 for the same case, and a 1970 stamp is never within 6h.
		lastRunMs := parseISOMs(hb.LastRunAt)
		alive := lastRunMs != 0 && timeNowMs()-lastRunMs < 6*3_600_000
		sc = &trackerScout{
			Alive:      alive,
			LastRunAt:  &hb.LastRunAt,
			LastTaskID: hb.LastTaskID,
			LastStatus: hb.LastStatus,
			Worker:     hb.Worker,
		}
	}
	counts := queueCounts(repoPath)
	return trackerSnapshot{
		GeneratedAt:   trackerIsoNow(),
		Queue:         queueJSON{Total: counts.Total, Pending: counts.Pending, Claimed: counts.Claimed, Done: counts.Done, Failed: counts.Failed},
		RecentTasks:   recent,
		Scout:         sc,
		Board:         trackerReadBoard(repoPath),
		LedgerTail:    trackerLedgerTail(repoPath, limit),
		RecentCommits: []string{},
		OpenPRs:       []string{},
	}
}

// collectProgressAsync mirrors collectProgressAsync() (tracker.ts:162-173):
// the base snapshot plus the git log and gh pr list evidence gathered
// through the spawn runner. The TS Promise.all has no observable ordering
// effect, so the two calls run sequentially.
func collectProgressAsync(repoPath string, limit int) trackerSnapshot {
	snap := collectProgress(repoPath, limit)
	snap.RecentCommits = trackerRecentCommits(repoPath, limit)
	snap.OpenPRs = trackerOpenPrs(repoPath, limit)
	return snap
}

// trackerHeartbeatPath mirrors tracker.ts heartbeatPath().
func trackerHeartbeatPath(repoPath string) string {
	return filepath.Join(repoPath, ".devagent", "tracker.heartbeat.json")
}

// writeTrackerHeartbeat mirrors writeTrackerHeartbeat() (tracker.ts:195-198):
// mkdir .devagent, write the 2-space JSON + newline. Best-effort — the TS
// callers only reach a write failure on an unwritable repo, where the cycle
// itself has already failed.
func writeTrackerHeartbeat(repoPath string, hb trackerHeartbeat) {
	if err := os.MkdirAll(filepath.Join(repoPath, ".devagent"), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(trackerHeartbeatPath(repoPath), []byte(marshalIndent(hb)+"\n"), 0o644)
}

// renderProgressMarkdown mirrors renderProgressMarkdown() (tracker.ts:93-131)
// line for line, including the single trailing newline (the final "" element
// joined with \n) and the em-dash separators.
func renderProgressMarkdown(snap trackerSnapshot) string {
	lines := []string{
		"# DevAgent Self-Build Progress",
		"",
		"Generated: " + snap.GeneratedAt,
		"",
		"## Queue",
		fmt.Sprintf("total %d — pending:%d claimed:%d done:%d failed:%d",
			snap.Queue.Total, snap.Queue.Pending, snap.Queue.Claimed, snap.Queue.Done, snap.Queue.Failed),
	}
	for _, t := range snap.RecentTasks {
		line := fmt.Sprintf("- [%s] %s: %s", t.Status, t.ID, t.Title)
		if t.LastError != nil && *t.LastError != "" {
			line += " — " + trackerSliceRunes(*t.LastError, 80)
		}
		lines = append(lines, line)
	}
	lines = append(lines, "")
	if snap.Board != nil {
		lines = append(lines, "## Orchestrator board")
		lines = append(lines, "goal: "+trackerSliceRunes(snap.Board.Goal, 120))
		parts := []string{}
		for _, e := range snap.Board.Counts.entries {
			parts = append(parts, fmt.Sprintf("%s:%d", e.Key, e.Value))
		}
		lines = append(lines, "tasks: "+strings.Join(parts, " "))
		if snap.Board.BlockedReason != nil && *snap.Board.BlockedReason != "" {
			lines = append(lines, "blocked: "+trackerSliceRunes(*snap.Board.BlockedReason, 200))
		}
		lines = append(lines, "")
	}
	lines = append(lines, "## Scout (PRD writer)")
	if snap.Scout != nil {
		state := "stale (>6h)"
		if snap.Scout.Alive {
			state = "alive"
		}
		lastTask := "none"
		if snap.Scout.LastTaskID != nil {
			lastTask = *snap.Scout.LastTaskID
		}
		lines = append(lines, fmt.Sprintf("%s: worker=%s lastRunAt=%s lastTask=%s (%s)",
			state, jsOptStr(snap.Scout.Worker), jsOptStr(snap.Scout.LastRunAt), lastTask, jsOptStr(snap.Scout.LastStatus)))
	} else {
		lines = append(lines, "no heartbeat yet")
	}
	lines = append(lines, "")
	lines = append(lines, "## Builder ledger tail")
	if len(snap.LedgerTail) > 0 {
		for _, l := range snap.LedgerTail {
			lines = append(lines, "- "+l)
		}
	} else {
		lines = append(lines, "- (empty)")
	}
	lines = append(lines, "")
	lines = append(lines, "## Recent commits")
	if len(snap.RecentCommits) > 0 {
		for _, c := range snap.RecentCommits {
			lines = append(lines, "- "+c)
		}
	} else {
		lines = append(lines, "- (none)")
	}
	lines = append(lines, "")
	lines = append(lines, "## Open PRs")
	if len(snap.OpenPRs) > 0 {
		for _, p := range snap.OpenPRs {
			lines = append(lines, "- "+p)
		}
	} else {
		lines = append(lines, "- (none or gh unavailable)")
	}
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// trackerLedgerTail mirrors readLedgerTail() (tracker.ts:39-47): the last n
// non-empty lines of .selfbuild/ledger.jsonl; missing/unreadable file → [].
func trackerLedgerTail(repoPath string, n int) []string {
	raw, err := os.ReadFile(filepath.Join(repoPath, ".selfbuild", "ledger.jsonl"))
	if err != nil {
		return []string{}
	}
	return sliceFromEnd(trackerNonEmptyLines(string(raw)), n)
}

// trackerReadBoard mirrors readBoardSnapshot() (tracker.ts:49-64) over
// .devagent-project.json. Every TS failure mode maps to a nil board: missing
// file, corrupt JSON, absent goal/tasks, or a null task element (the TS
// member access on null throws into the catch).
func trackerReadBoard(repoPath string) *trackerBoard {
	raw, err := os.ReadFile(filepath.Join(repoPath, ".devagent-project.json"))
	if err != nil {
		return nil
	}
	var parsed struct {
		Goal  *string                 `json:"goal"`
		Tasks *[]*trackerBoardTaskRaw `json:"tasks"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return nil
	}
	if parsed.Goal == nil || parsed.Tasks == nil {
		return nil
	}
	var counts trackerBoardCounts
	var blocked *trackerBoardTaskRaw
	for _, t := range *parsed.Tasks {
		if t == nil {
			return nil
		}
		status := "undefined"
		if t.Status != nil {
			status = *t.Status
		}
		counts.add(status)
		if blocked == nil && t.Status != nil && (*t.Status == "blocked" || *t.Status == "ask") {
			blocked = t
		}
	}
	board := &trackerBoard{Goal: *parsed.Goal, Counts: counts}
	if blocked != nil {
		// blocked?.failureDetail ?? blocked?.audit?.summary — nil only when
		// both are absent; a "" failureDetail is kept.
		if blocked.FailureDetail != nil {
			board.BlockedReason = blocked.FailureDetail
		} else if blocked.Audit != nil {
			board.BlockedReason = blocked.Audit.Summary
		}
	}
	return board
}

// trackerRecentCommits mirrors recentCommits() (tracker.ts:66-74): git log
// --oneline -n, best-effort — any failure or timeout yields [].
func trackerRecentCommits(repoPath string, n int) []string {
	r := spawn.RunCli("git", []string{"log", "--oneline", fmt.Sprintf("-%d", n)}, spawn.Options{Dir: repoPath, TimeoutMs: 10_000})
	if r.TimedOut || r.ExitCode != 0 {
		return []string{}
	}
	return trackerNonEmptyLines(r.Stdout)
}

// trackerOpenPrs mirrors openPrs() (tracker.ts:77-91): gh pr list as JSON,
// best-effort — failure, timeout, no JSON array in stdout, or a parse error
// yields []. Rows render as `#N [state] title — url`.
func trackerOpenPrs(repoPath string, n int) []string {
	r := spawn.RunCli("gh", []string{"pr", "list", "--limit", strconv.Itoa(n), "--json", "number,title,url,state"}, spawn.Options{Dir: repoPath, TimeoutMs: 15_000})
	if r.TimedOut || r.ExitCode != 0 {
		return []string{}
	}
	start := strings.Index(r.Stdout, "[")
	if start < 0 {
		return []string{}
	}
	var parsed []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"url"`
		State  string `json:"state"`
	}
	if json.Unmarshal([]byte(r.Stdout[start:]), &parsed) != nil {
		return []string{}
	}
	out := []string{}
	for _, p := range parsed {
		out = append(out, fmt.Sprintf("#%d [%s] %s — %s", p.Number, p.State, p.Title, p.URL))
	}
	return out
}

// trackerNonEmptyLines mirrors `.trim().split('\n').filter(Boolean)`:
// whitespace-only middle lines are truthy in JS and are kept.
func trackerNonEmptyLines(s string) []string {
	out := []string{}
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// trackerSliceRunes is String#slice(0, n): the caps here (goal 120,
// blockedReason 200, lastError 80) match JS UTF-16 code units for BMP text.
func trackerSliceRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// trackerIsoNow mirrors new Date().toISOString(): UTC with exactly three
// fractional digits and a literal trailing Z (a lone Z is not a Go timezone
// token, so it formats verbatim).
func trackerIsoNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
