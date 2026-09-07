// Package tracker is the Go port of src/tracker.ts: the progress-tracker
// role of the self-build factory. One long-lived agent observes the other
// two (scout writes the queue, builder consumes it) and publishes a durable
// progress snapshot. Read-only over the repo; writes only
// .devagent/tracker.heartbeat.json + .selfbuild/progress.{md,json}.
package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/scout"
	"github.com/FreePeak/devagent/internal/spawn"
)

// TrackerOptions mirrors TrackerOptions. Limit is the max entries per
// section in the markdown snapshot (nil = 10, like the TS default param).
type TrackerOptions struct {
	RepoPath string
	Limit    *int
}

// TrackerTask mirrors the Pick<QueuedTask, ...> shape of recentTasks.
type TrackerTask struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Status    string  `json:"status"`
	UpdatedAt string  `json:"updatedAt"`
	LastError *string `json:"lastError,omitempty"`
}

// TrackerScout mirrors the snapshot's scout object. Optional fields stay
// pointers so JSON.stringify's key-dropping behaviour is preserved.
type TrackerScout struct {
	Alive      bool    `json:"alive"`
	LastRunAt  string  `json:"lastRunAt,omitempty"`
	LastTaskID *string `json:"lastTaskId,omitempty"`
	LastStatus *string `json:"lastStatus,omitempty"`
	Worker     *string `json:"worker,omitempty"`
}

// TrackerBoard mirrors the snapshot's board object.
type TrackerBoard struct {
	Goal          string      `json:"goal"`
	Counts        BoardCounts `json:"counts"`
	BlockedReason *string     `json:"blockedReason,omitempty"`
}

// TrackerSnapshot mirrors TrackerSnapshot. Scout and Board are pointers
// without omitempty: the TS object always carries both keys, value null
// when absent.
type TrackerSnapshot struct {
	GeneratedAt   string          `json:"generatedAt"`
	Queue         queue.TaskCount `json:"queue"`
	RecentTasks   []TrackerTask   `json:"recentTasks"`
	Scout         *TrackerScout   `json:"scout"`
	Board         *TrackerBoard   `json:"board"`
	LedgerTail    []string        `json:"ledgerTail"`
	RecentCommits []string        `json:"recentCommits"`
	OpenPrs       []string        `json:"openPrs"`
}

// CliRunOptions mirrors the runner opts object ({ cwd, timeoutMs }).
type CliRunOptions struct {
	Cwd       string
	TimeoutMs int
}

// CliRunResult mirrors the runner result object.
type CliRunResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
}

// CliRunner mirrors the TS CliRunner injection seam (git/gh probes).
type CliRunner func(cmd string, args []string, opts CliRunOptions) CliRunResult

// DefaultCliRunner delegates to internal/spawn.RunCli, the port of
// spawnCli from src/workers/spawn-utils.ts.
func DefaultCliRunner(cmd string, args []string, opts CliRunOptions) CliRunResult {
	r := spawn.RunCli(cmd, args, spawn.Options{Dir: opts.Cwd, TimeoutMs: opts.TimeoutMs})
	return CliRunResult{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr, TimedOut: r.TimedOut}
}

func runnerOrDefault(runner CliRunner) CliRunner {
	if runner != nil {
		return runner
	}
	return DefaultCliRunner
}

// BoardCountEntry is one key/count pair in insertion order.
type BoardCountEntry struct {
	Key   string
	Count int
}

// BoardCounts preserves the JS object insertion order of the board's status
// counts — a plain Go map would marshal keys sorted, breaking byte parity
// of the rendered `tasks:` line and progress.json.
type BoardCounts struct {
	keys []string
	vals map[string]int
}

// Add increments the count for key, remembering first-seen order.
func (c *BoardCounts) Add(key string) {
	if c.vals == nil {
		c.vals = make(map[string]int)
	}
	if _, seen := c.vals[key]; !seen {
		c.keys = append(c.keys, key)
	}
	c.vals[key]++
}

// Get returns the count for key (0 when absent).
func (c BoardCounts) Get(key string) int { return c.vals[key] }

func (c BoardCounts) len() int { return len(c.keys) }

// Entries returns the key/count pairs in first-seen insertion order.
func (c BoardCounts) Entries() []BoardCountEntry {
	entries := make([]BoardCountEntry, 0, len(c.keys))
	for _, k := range c.keys {
		entries = append(entries, BoardCountEntry{Key: k, Count: c.vals[k]})
	}
	return entries
}

// MarshalJSON emits the counts object in insertion order.
func (c BoardCounts) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range c.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := json.Marshal(c.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON reads the counts object back, preserving key order.
func (c *BoardCounts) UnmarshalJSON(data []byte) error {
	c.keys = nil
	c.vals = nil
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil { // JSON null: stays empty, like a missing TS object
		return nil
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("board counts: expected JSON object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("board counts: expected string key")
		}
		var count int
		if err := dec.Decode(&count); err != nil {
			return err
		}
		if c.vals == nil {
			c.vals = make(map[string]int)
		}
		if _, seen := c.vals[key]; !seen {
			c.keys = append(c.keys, key)
		}
		c.vals[key] = count
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
}

// tailSlice mirrors JS arr.slice(-n), including the -0 === 0 quirk (n = 0
// yields the whole array) and clamping beyond both ends.
func tailSlice[T any](items []T, n int) []T {
	start := len(items) - n
	if n <= 0 {
		start = -n
	}
	if start < 0 {
		start = 0
	}
	if start > len(items) {
		start = len(items)
	}
	return items[start:]
}

// sliceStr mirrors TS String.prototype.slice(0, n) (rune-based; the only
// divergence from V8's UTF-16 units is a dropped astral rune, which cannot
// round-trip through Go strings anyway).
func sliceStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// jsStr mirrors TS template interpolation of a possibly-undefined value.
func jsStr(s *string) string {
	if s == nil {
		return "undefined"
	}
	return *s
}

// jsNone mirrors the TS `x ?? 'none'` fallback in the scout line.
func jsNone(s *string) string {
	if s == nil {
		return "none"
	}
	return *s
}

func readLedgerTail(repoPath string, n int) []string {
	p := filepath.Join(repoPath, ".selfbuild", "ledger.jsonl")
	raw, err := os.ReadFile(p)
	if err != nil {
		return []string{}
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	filtered := make([]string, 0, len(lines))
	for _, l := range lines {
		if l != "" {
			filtered = append(filtered, l)
		}
	}
	return tailSlice(filtered, n)
}

func readBoardSnapshot(repoPath string) *TrackerBoard {
	raw, err := os.ReadFile(filepath.Join(repoPath, ".devagent-project.json"))
	if err != nil {
		return nil
	}
	var boardMod struct {
		Goal  *string `json:"goal"`
		Tasks *[]struct {
			Status        string  `json:"status"`
			FailureDetail *string `json:"failureDetail"`
			Audit         *struct {
				Summary *string `json:"summary"`
			} `json:"audit"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &boardMod); err != nil {
		return nil
	}
	// TS: if (!boardMod?.tasks || typeof boardMod.goal !== 'string') return null
	if boardMod.Tasks == nil || boardMod.Goal == nil {
		return nil
	}
	var counts BoardCounts
	for _, t := range *boardMod.Tasks {
		counts.Add(t.Status)
	}
	board := &TrackerBoard{Goal: *boardMod.Goal, Counts: counts}
	for _, t := range *boardMod.Tasks {
		if t.Status == "blocked" || t.Status == "ask" {
			if t.FailureDetail != nil {
				board.BlockedReason = t.FailureDetail
			} else if t.Audit != nil {
				board.BlockedReason = t.Audit.Summary
			}
			break
		}
	}
	return board
}

func recentCommits(repoPath string, n int, runner CliRunner) []string {
	r := runner("git", []string{"log", "--oneline", fmt.Sprintf("-%d", n)}, CliRunOptions{Cwd: repoPath, TimeoutMs: 10_000})
	if r.TimedOut || r.ExitCode != 0 {
		return []string{}
	}
	lines := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// openPrs lists open PRs via gh; best-effort — empty list when gh is
// missing or there is no remote.
func openPrs(repoPath string, n int, runner CliRunner) []string {
	r := runner("gh", []string{"pr", "list", "--limit", strconv.Itoa(n), "--json", "number,title,url,state"}, CliRunOptions{Cwd: repoPath, TimeoutMs: 15_000})
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
	if err := json.Unmarshal([]byte(r.Stdout[start:]), &parsed); err != nil {
		return []string{}
	}
	out := make([]string, 0, len(parsed))
	for _, p := range parsed {
		out = append(out, fmt.Sprintf("#%d [%s] %s — %s", p.Number, p.State, p.Title, p.URL))
	}
	return out
}

// RenderProgressMarkdown renders the snapshot exactly like the TS
// renderProgressMarkdown (lines joined with \n, trailing newline).
func RenderProgressMarkdown(snap TrackerSnapshot) string {
	lines := make([]string, 0, 32)
	lines = append(lines, "# DevAgent Self-Build Progress", "")
	lines = append(lines, "Generated: "+snap.GeneratedAt, "")
	lines = append(lines, "## Queue")
	lines = append(lines, fmt.Sprintf("total %d — pending:%d claimed:%d done:%d failed:%d",
		snap.Queue.Total, snap.Queue.Pending, snap.Queue.Claimed, snap.Queue.Done, snap.Queue.Failed))
	for _, t := range snap.RecentTasks {
		line := fmt.Sprintf("- [%s] %s: %s", t.Status, t.ID, t.Title)
		if t.LastError != nil && *t.LastError != "" {
			line += " — " + sliceStr(*t.LastError, 80)
		}
		lines = append(lines, line)
	}
	lines = append(lines, "")
	if snap.Board != nil {
		lines = append(lines, "## Orchestrator board")
		lines = append(lines, "goal: "+sliceStr(snap.Board.Goal, 120))
		parts := make([]string, 0, snap.Board.Counts.len())
		for _, e := range snap.Board.Counts.Entries() {
			parts = append(parts, fmt.Sprintf("%s:%d", e.Key, e.Count))
		}
		lines = append(lines, "tasks: "+strings.Join(parts, " "))
		if snap.Board.BlockedReason != nil && *snap.Board.BlockedReason != "" {
			lines = append(lines, "blocked: "+sliceStr(*snap.Board.BlockedReason, 200))
		}
		lines = append(lines, "")
	}
	lines = append(lines, "## Scout (PRD writer)")
	if snap.Scout != nil {
		state := "stale (>6h)"
		if snap.Scout.Alive {
			state = "alive"
		}
		lines = append(lines, fmt.Sprintf("%s: worker=%s lastRunAt=%s lastTask=%s (%s)",
			state, jsStr(snap.Scout.Worker), snap.Scout.LastRunAt, jsNone(snap.Scout.LastTaskID), jsStr(snap.Scout.LastStatus)))
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
	if len(snap.OpenPrs) > 0 {
		for _, p := range snap.OpenPrs {
			lines = append(lines, "- "+p)
		}
	} else {
		lines = append(lines, "- (none or gh unavailable)")
	}
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// parseHeartbeatTime mirrors internal/scout's parseJSDate (Date.parse for
// the ISO shapes the heartbeat file carries); unparseable input yields the
// zero time, which callers treat as NaN.
func parseHeartbeatTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func scoutAlive(lastRunAt string, now time.Time) bool {
	t := parseHeartbeatTime(lastRunAt)
	if t.IsZero() {
		return false
	}
	return now.Sub(t) < 6*time.Hour
}

// CollectProgress mirrors collectProgress: the synchronous snapshot
// (queue + scout + board + ledger; git/gh fields stay empty).
func CollectProgress(opts TrackerOptions, runner CliRunner) TrackerSnapshot {
	repoPath := opts.RepoPath
	limit := 10
	if opts.Limit != nil {
		limit = *opts.Limit
	}
	_ = runner // unused in the sync path; kept for TS signature parity
	listed := queue.ListTasks(repoPath, "")
	picked := tailSlice(listed, limit)
	recentTasks := make([]TrackerTask, 0, len(picked))
	for i := len(picked) - 1; i >= 0; i-- {
		t := picked[i]
		recentTasks = append(recentTasks, TrackerTask{
			ID:        t.ID,
			Title:     t.Title,
			Status:    string(t.Status),
			UpdatedAt: t.UpdatedAt,
			LastError: t.LastError,
		})
	}
	var sc *TrackerScout
	if hb := scout.ReadHeartbeat(repoPath); hb != nil {
		sc = &TrackerScout{
			Alive:      scoutAlive(hb.LastRunAt, time.Now()),
			LastRunAt:  hb.LastRunAt,
			LastTaskID: hb.LastTaskID,
			LastStatus: hb.LastStatus,
			Worker:     hb.Worker,
		}
	}
	return TrackerSnapshot{
		GeneratedAt:   ledger.NowISO(),
		Queue:         queue.TaskCountOf(repoPath),
		RecentTasks:   recentTasks,
		Scout:         sc,
		Board:         readBoardSnapshot(repoPath),
		LedgerTail:    readLedgerTail(repoPath, limit),
		RecentCommits: []string{},
		OpenPrs:       []string{},
	}
}

// CollectProgressAsync mirrors collectProgressAsync: the sync snapshot plus
// git + gh evidence gathered through the injectable runner (Promise.all).
func CollectProgressAsync(opts TrackerOptions, runner CliRunner) TrackerSnapshot {
	base := CollectProgress(opts, runner)
	limit := 10
	if opts.Limit != nil {
		limit = *opts.Limit
	}
	r := runnerOrDefault(runner)
	var commits, prs []string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		commits = recentCommits(opts.RepoPath, limit, r)
	}()
	go func() {
		defer wg.Done()
		prs = openPrs(opts.RepoPath, limit, r)
	}()
	wg.Wait()
	base.RecentCommits = commits
	base.OpenPrs = prs
	return base
}

func heartbeatPath(repoPath string) string {
	return filepath.Join(repoPath, ".devagent", "tracker.heartbeat.json")
}

// TrackerHeartbeat mirrors TrackerHeartbeat: the tracker's own heartbeat,
// separate from the scout's.
type TrackerHeartbeat struct {
	LastRunAt  string `json:"lastRunAt"`
	LastStatus string `json:"lastStatus"`
	LastDetail string `json:"lastDetail"`
}

// ReadTrackerHeartbeat mirrors readTrackerHeartbeat: nil on missing file or
// corrupt JSON (no field validation, unlike the scout's reader).
func ReadTrackerHeartbeat(repoPath string) *TrackerHeartbeat {
	raw, err := os.ReadFile(heartbeatPath(repoPath))
	if err != nil {
		return nil
	}
	var hb TrackerHeartbeat
	if err := json.Unmarshal(raw, &hb); err != nil {
		return nil
	}
	return &hb
}

// marshalIndentJSON renders JSON.stringify(v, null, 2) + '\n': two-space
// indent and no HTML escaping (Go's default escapes <, >, &; V8 does not).
func marshalIndentJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeTrackerHeartbeat mirrors writeTrackerHeartbeat.
func writeTrackerHeartbeat(repoPath string, hb TrackerHeartbeat) error {
	if err := os.MkdirAll(filepath.Join(repoPath, ".devagent"), 0o755); err != nil {
		return err
	}
	raw, err := marshalIndentJSON(hb)
	if err != nil {
		return err
	}
	return os.WriteFile(heartbeatPath(repoPath), raw, 0o644)
}

// TrackOnceResult mirrors TrackOnceResult.
type TrackOnceResult struct {
	OK               bool
	Detail           string
	Snapshot         *TrackerSnapshot
	ProgressMdPath   string
	ProgressJsonPath string
	HeartbeatPath    string
}

func scoutState(snap TrackerSnapshot) string {
	if snap.Scout != nil && snap.Scout.Alive {
		return "alive"
	}
	return "down"
}

// TrackOnce mirrors trackOnce: one tracker cycle — gather, write
// .selfbuild/progress.{md,json} + heartbeat, report.
func TrackOnce(opts TrackerOptions, runner CliRunner) TrackOnceResult {
	repoPath := opts.RepoPath
	hbPath := heartbeatPath(repoPath)
	fail := func(err error) TrackOnceResult {
		_ = writeTrackerHeartbeat(repoPath, TrackerHeartbeat{
			LastRunAt:  ledger.NowISO(),
			LastStatus: "failed",
			LastDetail: err.Error(),
		})
		return TrackOnceResult{OK: false, Detail: "track failed: " + err.Error(), HeartbeatPath: hbPath}
	}
	snap := CollectProgressAsync(opts, runner)
	mdPath := filepath.Join(repoPath, ".selfbuild", "progress.md")
	jsonPath := filepath.Join(repoPath, ".selfbuild", "progress.json")
	if err := os.MkdirAll(filepath.Join(repoPath, ".selfbuild"), 0o755); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(mdPath, []byte(RenderProgressMarkdown(snap)), 0o644); err != nil {
		return fail(err)
	}
	jsonRaw, err := marshalIndentJSON(snap)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(jsonPath, jsonRaw, 0o644); err != nil {
		return fail(err)
	}
	detail := fmt.Sprintf("queue %d (%d pending, %d failed), scout %s, %d open PR(s)",
		snap.Queue.Total, snap.Queue.Pending, snap.Queue.Failed, scoutState(snap), len(snap.OpenPrs))
	if err := writeTrackerHeartbeat(repoPath, TrackerHeartbeat{
		LastRunAt:  ledger.NowISO(),
		LastStatus: "ok",
		LastDetail: detail,
	}); err != nil {
		return fail(err)
	}
	return TrackOnceResult{
		OK:               true,
		Detail:           detail,
		Snapshot:         &snap,
		ProgressMdPath:   mdPath,
		ProgressJsonPath: jsonPath,
		HeartbeatPath:    hbPath,
	}
}

// TrackLoopOptions mirrors trackLoop's opts object minus the signal.
type TrackLoopOptions struct {
	RepoPath        string
	IntervalMinutes int
}

// TrackLoop mirrors trackLoop: run TrackOnce every IntervalMinutes until
// ctx is cancelled (the Go replacement for the TS AbortSignal). onCycle, if
// non-nil, is invoked after each cycle. Like the TS, the cycle runs with
// the default limit (no limit override is passed through).
func TrackLoop(ctx context.Context, opts TrackLoopOptions, onCycle func(TrackOnceResult)) {
	for {
		if ctx != nil && ctx.Err() != nil {
			return
		}
		r := TrackOnce(TrackerOptions{RepoPath: opts.RepoPath}, nil)
		if onCycle != nil {
			onCycle(r)
		}
		if ctx != nil && ctx.Err() != nil {
			return
		}
		if ctx == nil {
			time.Sleep(time.Duration(opts.IntervalMinutes) * time.Minute)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(opts.IntervalMinutes) * time.Minute):
		}
	}
}
