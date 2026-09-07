// Package file mirrors src/orchestrator/board-recovery.ts (FR-GO-07, issue #194).
package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/config"
)

// deadStatuses mirrors the TS DEAD_STATUSES: statuses the scheduler can
// never dispatch again (the dispatch-dead pair).
var deadStatuses = []string{"failed", "blocked"}

// BoardTaskLike mirrors the TS BoardTaskLike: board task shape as read back
// from JSON (statuses validated loosely, mirroring the shell's `node -e`
// leniency).
type BoardTaskLike struct {
	Status        any `json:"status"`
	Attempts      any `json:"attempts"`
	TotalAttempts any `json:"totalAttempts"`
}

// boardFile mirrors the TS BoardFile: the raw parsed object (written back
// verbatim after a requeue) plus its validated tasks array — the same maps
// raw["tasks"] holds, so mutations surface on the re-marshal.
//
// Deviation vs the TS: JS JSON.stringify preserves key insertion order; Go's
// map marshal sorts keys. Requeue write-backs keep every field and value but
// reorder object keys alphabetically.
type boardFile struct {
	Raw   map[string]any
	Tasks []map[string]any
}

// BoardCounts mirrors the TS BoardCounts: raw task counts the decision
// table reads.
type BoardCounts struct {
	Total   int `json:"total"`
	Done    int `json:"done"`
	Open    int `json:"open"`  // not done/failed/blocked — everything the loop may still dispatch
	Stuck   int `json:"stuck"` // failed/blocked — the dispatch-dead states
	Pending int `json:"pending"`
}

// RecoveryOptions mirrors the TS RecoveryOptions.
type RecoveryOptions struct {
	// ParkedPolls: parked cycles including this one (the shell's
	// post-increment counter).
	ParkedPolls int
	// RequeueAfter: requeue threshold in parked polls; 0 = never requeue.
	RequeueAfter int
	// PollSecs: loop sleep seconds, quoted in wait/requeue verdict text.
	PollSecs int
	// MaxTotalAttempts: cumulative lifetime dispatch cap (Q17/Q36); a
	// failed/blocked task whose totalAttempts reaches it is refused the
	// requeue reset and stays terminal. 0 = unbounded.
	MaxTotalAttempts int
}

// RecoveryAction mirrors the TS 'wait' | 'requeue' | 'archive' union.
type RecoveryAction string

const (
	RecoveryActionWait    RecoveryAction = "wait"
	RecoveryActionRequeue RecoveryAction = "requeue"
	RecoveryActionArchive RecoveryAction = "archive"
)

// BoardRecoveryVerdict mirrors the TS BoardRecoveryVerdict.
type BoardRecoveryVerdict struct {
	Action RecoveryAction `json:"action"`
	Reason string         `json:"reason"`
}

// Recovery intent kinds, mirroring the TS RecoveryIntent discriminated union.
const (
	IntentArchiveComplete = "archive-complete"
	IntentRequeueNow      = "requeue-now"
	IntentWait            = "wait"
)

// PostRequeueIntentKindArchive / ...KindRequeue mirror the TS
// PostRequeueIntent discriminated union.
const (
	PostRequeueIntentKindArchive = "archive"
	PostRequeueIntentKindRequeue = "requeue"
)

// RecoveryIntent mirrors the TS RecoveryIntent union as a tagged struct
// (Go has no discriminated unions): Kind is 'archive-complete' (archive the
// completed board), 'requeue-now' (threshold reached), or 'wait' with a
// Reason.
type RecoveryIntent struct {
	Kind   string
	Reason string // wait only
}

// PostRequeueIntent mirrors the TS PostRequeueIntent union as a tagged
// struct: Kind is 'archive' with a human-readable Detail, or 'requeue'.
type PostRequeueIntent struct {
	Kind   string
	Detail string // archive only
}

// CountBoard mirrors countBoard: count the board the way the shell's
// board_* helpers did. Malformed rows (missing/numeric status) count toward
// total and open.
func CountBoard(tasks []BoardTaskLike) BoardCounts {
	done := 0
	stuck := 0
	pending := 0
	for _, t := range tasks {
		status := looseString(t.Status)
		if status == "done" {
			done++
		} else if containsString(deadStatuses, status) {
			stuck++
		}
		if status == "pending" {
			pending++
		}
	}
	total := len(tasks)
	return BoardCounts{Total: total, Done: done, Open: total - done - stuck, Stuck: stuck, Pending: pending}
}

// DecideBoardRecovery mirrors decideBoardRecovery: pre-mutation decision —
// completed board, parked wait, or threshold reached. open > 0 / total == 0
// are defensive (the shell guard keeps the gate out of those paths); they
// verdict wait, never an action.
func DecideBoardRecovery(counts BoardCounts, opts RecoveryOptions) RecoveryIntent {
	if counts.Total == 0 {
		return RecoveryIntent{Kind: IntentWait, Reason: "board has no tasks"}
	}
	if counts.Open > 0 {
		return RecoveryIntent{Kind: IntentWait, Reason: fmt.Sprintf("board has %d open task(s)", counts.Open)}
	}
	if counts.Done == counts.Total {
		return RecoveryIntent{Kind: IntentArchiveComplete}
	}
	sleep := fmt.Sprintf("; sleeping %ds", opts.PollSecs)
	if opts.RequeueAfter <= 0 {
		return RecoveryIntent{Kind: IntentWait, Reason: fmt.Sprintf("%d task(s) failed/blocked%s (requeue disabled)", counts.Stuck, sleep)}
	}
	if opts.ParkedPolls < opts.RequeueAfter {
		return RecoveryIntent{Kind: IntentWait, Reason: fmt.Sprintf("%d task(s) failed/blocked (%d/%d)%s", counts.Stuck, opts.ParkedPolls, opts.RequeueAfter, sleep)}
	}
	return RecoveryIntent{Kind: IntentRequeueNow}
}

// DecidePostRequeue mirrors decidePostRequeue: post-requeue decision, over
// the counts recomputed AFTER the reset write. The stuck branch fires when
// the cumulative attempt cap (Q17/Q36) refused the reset for at least one
// dead task; the all-pending branch is the original real one (a board with
// no done task never recomputes readiness, so requeue cannot unstick it).
func DecidePostRequeue(counts BoardCounts) PostRequeueIntent {
	if counts.Stuck > 0 {
		return PostRequeueIntent{Kind: PostRequeueIntentKindArchive, Detail: fmt.Sprintf("board stuck (%d failed/blocked)", counts.Stuck)}
	}
	if counts.Total > 0 && counts.Pending == counts.Total {
		return PostRequeueIntent{Kind: PostRequeueIntentKindArchive, Detail: "board all-pending but undispatchable"}
	}
	return PostRequeueIntent{Kind: PostRequeueIntentKindRequeue}
}

// FormatVerdict mirrors formatVerdict: render a verdict the way the CLI
// prints it — the shell parses the word before the first colon.
func FormatVerdict(verdict BoardRecoveryVerdict) string {
	return fmt.Sprintf("%s: %s", verdict.Action, verdict.Reason)
}

// FormatTimestamp mirrors formatTimestamp: `date +%Y%m%d-%H%M%S` in the
// time's own location — the archive filename stamp the shell used.
func FormatTimestamp(d time.Time) string {
	return d.Format("20060102-150405")
}

// readBoard mirrors the TS readBoard: parse the raw board file, requiring an
// object with an array `tasks`. Elements stay unknown-shaped (possibly nil
// maps): every access is guarded, mirroring the TS `t.status` leniency on
// hand-edited boards.
func readBoard(boardPath string) *boardFile {
	data, err := os.ReadFile(boardPath)
	if err != nil {
		return nil
	}
	var parsed map[string]any
	if json.Unmarshal(data, &parsed) != nil {
		return nil
	}
	rawTasks, ok := parsed["tasks"].([]any)
	if !ok {
		return nil
	}
	tasks := make([]map[string]any, len(rawTasks))
	for i, el := range rawTasks {
		if m, ok := el.(map[string]any); ok {
			tasks[i] = m
		}
	}
	return &boardFile{Raw: parsed, Tasks: tasks}
}

// RequeueResult mirrors the TS RequeueResult: resets performed vs refused by
// the cumulative cap.
type RequeueResult struct {
	// Reset: dead tasks reset to pending with a fresh per-round attempts budget.
	Reset int `json:"reset"`
	// Capped: dead tasks at/above the cumulative cap — refused, left terminal (Q17/Q36).
	Capped int `json:"capped"`
}

// RequeueParked mirrors requeueParked (the port of requeue_parked()):
// failed/blocked → pending with the attempts budget reset; writes only when
// something changed (2-space indent, trailing newline, tmp+rename so a crash
// mid-write cannot corrupt the board). Above the cumulative cap (Q17/Q36,
// 0 = unbounded) a dead task is refused the fresh budget and stays
// failed/blocked; totalAttempts itself is never reset. Missing or
// non-numeric lifetime history counts as zero.
func RequeueParked(boardPath string, board *boardFile, maxTotalAttempts int) RequeueResult {
	reset := 0
	capped := 0
	for _, t := range board.Tasks {
		if t == nil || !containsString(deadStatuses, looseString(t["status"])) {
			continue
		}
		total, finite := numberIsFinite(t["totalAttempts"])
		if maxTotalAttempts > 0 && finite && total >= float64(maxTotalAttempts) {
			capped++
			continue
		}
		t["status"] = "pending"
		t["attempts"] = 0
		reset++
	}
	if reset > 0 {
		writeJSONAtomic(boardPath, board.Raw)
	}
	return RequeueResult{Reset: reset, Capped: capped}
}

// ArchiveRetentionKeep mirrors the TS ARCHIVE_RETENTION_KEEP: default bound
// for .devagent/archive/ retention (Q16) — the newest N stamped archives are
// kept.
const ArchiveRetentionKeep = 20

// archiveName mirrors the TS ARCHIVE_NAME regexp: archive filenames carry a
// `YYYYMMDD-HHMMSS` stamp; only these are pruned.
var archiveName = regexp.MustCompile(`^(?:board|board-stuck)-(\d{8}-\d{6})\.json$`)

// PruneArchive mirrors pruneArchive: bounded retention prune of
// .devagent/archive/ (Q16) — keep the newest `keep` stamped archives, delete
// the rest, ordered by the filename stamp (deterministic under an injected
// clock). keep <= 0 is unbounded (never prune). Best-effort: a failed unlink
// is swallowed. Returns the removed names.
func PruneArchive(repoPath string, keep int) []string {
	if keep <= 0 {
		return []string{}
	}
	dir := filepath.Join(repoPath, ".devagent", "archive")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{} // no archive dir yet — nothing to prune
	}
	type stamped struct {
		name  string
		stamp string
	}
	var stamps []stamped
	for _, e := range entries {
		if m := archiveName.FindStringSubmatch(e.Name()); m != nil {
			stamps = append(stamps, stamped{name: e.Name(), stamp: m[1]})
		}
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].stamp > stamps[j].stamp }) // newest first
	removed := []string{}
	for _, s := range stamps[min(keep, len(stamps)):] {
		if err := os.Remove(filepath.Join(dir, s.name)); err == nil {
			removed = append(removed, s.name)
		}
	}
	return removed
}

// BoardArchivedAlert mirrors src/resilience/operator-alert.ts
// BoardArchivedAlert (Q16 board-archived payload).
//
// TODO(FR-GO-05 #190): replace with the shared internal/resilience
// operator-alert port when it lands (not in this sub-wave's ownership list).
type BoardArchivedAlert struct {
	Event  string `json:"event"`
	Ts     string `json:"ts"`
	Repo   string `json:"repo"`
	Prefix string `json:"prefix"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// OperatorNotifier mirrors src/resilience/operator-alert.ts OperatorNotifier:
// injection seam for outbound paging transports.
type OperatorNotifier func(url string, alert BoardArchivedAlert) error

// postOperatorAlert is the default transport: a best-effort JSON POST.
func postOperatorAlert(url string, alert BoardArchivedAlert) error {
	body, err := json.Marshal(alert)
	if err != nil {
		return err
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ArchiveBoardOptions mirrors the TS ArchiveBoardOptions: archiveBoard's
// best-effort Q16 side effects (tests inject them).
type ArchiveBoardOptions struct {
	// URL: operator webhook URL; empty = no paging (opt-in, like Q41).
	URL string
	// Notify: injection seam for tests; defaults to postOperatorAlert.
	Notify OperatorNotifier
	// Now: injectable clock for the alert ts (tests); zero = time.Now.
	Now time.Time
	// Reason: human-readable why, carried onto the board-archived alert;
	// empty = "<prefix> board archived".
	Reason string
	// Keep: retention bound; nil = ArchiveRetentionKeep.
	Keep *int
}

// ArchiveBoard mirrors archiveBoard: move the board into .devagent/archive/
// under `prefix-<stamp>.json`, then fire the best-effort `board-archived`
// operator alert and prune the archive to the retention bound (Q16). Returns
// the repo-relative path. Paging and retention never fail the archive; a
// failed rename does (the CLI maps it to exit 2).
func ArchiveBoard(repoPath, boardPath, prefix, stamp string, opts ArchiveBoardOptions) (string, error) {
	dir := filepath.Join(repoPath, ".devagent", "archive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	rel := filepath.Join(".devagent", "archive", fmt.Sprintf("%s-%s.json", prefix, stamp))
	if err := os.Rename(boardPath, filepath.Join(repoPath, rel)); err != nil {
		return "", err
	}
	if opts.URL != "" {
		notify := opts.Notify
		if notify == nil {
			notify = postOperatorAlert
		}
		now := opts.Now
		if now.IsZero() {
			now = time.Now()
		}
		reason := opts.Reason
		if reason == "" {
			reason = fmt.Sprintf("%s board archived", prefix)
		}
		// paging is observability, never a failure signal for the loop (Q16)
		_ = notify(opts.URL, BoardArchivedAlert{
			Event:  "board-archived",
			Ts:     now.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			Repo:   repoPath,
			Prefix: prefix,
			Path:   rel,
			Reason: reason,
		})
	}
	keep := ArchiveRetentionKeep
	if opts.Keep != nil {
		keep = *opts.Keep
	}
	PruneArchive(repoPath, keep)
	return rel, nil
}

// pruneMergedWorktrees mirrors the TS safe-gated merged-worktree prune (the
// shell's cleanup_merged_worktrees): only an executable repo script, never
// fatal.
func pruneMergedWorktrees(repoPath string) {
	script := filepath.Join(repoPath, "scripts", "git-cleanup-merged.sh")
	info, err := os.Stat(script)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return // missing or not executable: nothing to do
	}
	// cleanup is opportunistic; a failed prune must not break the cycle
	_ = exec.Command(script, "--root", repoPath, "--apply").Run()
}

// RunBoardRecoveryOptions mirrors the TS RunBoardRecoveryOptions.
type RunBoardRecoveryOptions struct {
	RecoveryOptions
	// Now: injectable clock for the archive stamp (tests); zero = time.Now.
	Now time.Time
	// Notify: injection seam for tests: outbound operator paging transport (Q16).
	Notify OperatorNotifier
}

// RunBoardRecovery mirrors runBoardRecovery: one recovery cycle — read the
// board, decide, perform the action, return the verdict. Mutates the board
// file (requeue) and moves it (archive) exactly where the shell did. Returns
// an error only when the board vanishes between the decision and the action
// — the CLI maps that to exit 2 (unresolved → the shell falls back to wait).
func RunBoardRecovery(repoPath string, opts RunBoardRecoveryOptions) (BoardRecoveryVerdict, error) {
	boardPath := filepath.Join(repoPath, BoardFile)
	board := readBoard(boardPath)
	if board == nil {
		return BoardRecoveryVerdict{Action: RecoveryActionWait, Reason: "board unreadable"}, nil
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	stamp := FormatTimestamp(now)
	sleep := fmt.Sprintf("; sleeping %ds", opts.PollSecs)
	// Q16: resolve the paging webhook + retention bound once. A broken config
	// must not turn an archive into a cycle failure, so paging stays opt-in
	// and the retention bound falls back to ArchiveRetentionKeep.
	var url string
	var keep *int
	if cfg, err := config.Load(repoPath); err == nil && cfg.Resilience != nil {
		url = cfg.Resilience.DegradeWebhookURL
		if cfg.Resilience.ArchiveKeep != nil {
			k := int(*cfg.Resilience.ArchiveKeep)
			keep = &k
		}
	}

	counts := CountBoard(boardLikes(board.Tasks))
	intent := DecideBoardRecovery(counts, opts.RecoveryOptions)
	if intent.Kind == IntentWait {
		return BoardRecoveryVerdict{Action: RecoveryActionWait, Reason: intent.Reason}, nil
	}
	if intent.Kind == IntentArchiveComplete {
		rel, err := ArchiveBoard(repoPath, boardPath, "board", stamp, ArchiveBoardOptions{
			URL:    url,
			Keep:   keep,
			Notify: opts.Notify,
			Now:    now,
			Reason: fmt.Sprintf("board complete (%d done)", counts.Done),
		})
		if err != nil {
			return BoardRecoveryVerdict{}, err
		}
		pruneMergedWorktrees(repoPath)
		return BoardRecoveryVerdict{Action: RecoveryActionWait, Reason: fmt.Sprintf("board complete (%d done); archived to %s", counts.Done, rel)}, nil
	}

	rq := RequeueParked(boardPath, board, opts.MaxTotalAttempts)
	post := DecidePostRequeue(CountBoard(boardLikes(board.Tasks)))
	if post.Kind == PostRequeueIntentKindRequeue {
		return BoardRecoveryVerdict{Action: RecoveryActionRequeue, Reason: fmt.Sprintf("reset %d parked task(s) to pending%s", rq.Reset, sleep)}, nil
	}
	// capped > 0 guarantees stuck > 0 here, so refusals always land on this
	// archive verdict: the tasks stay terminal and the loop re-bridges.
	capNote := ""
	if rq.Capped > 0 {
		capNote = fmt.Sprintf("%d task(s) over cumulative attempt cap %d; ", rq.Capped, opts.MaxTotalAttempts)
	}
	rel, err := ArchiveBoard(repoPath, boardPath, "board-stuck", stamp, ArchiveBoardOptions{
		URL:    url,
		Keep:   keep,
		Notify: opts.Notify,
		Now:    now,
		Reason: capNote + post.Detail,
	})
	if err != nil {
		return BoardRecoveryVerdict{}, err
	}
	return BoardRecoveryVerdict{Action: RecoveryActionArchive, Reason: fmt.Sprintf("reset %d parked task(s) to pending; %s%s; archived to %s", rq.Reset, capNote, post.Detail, rel)}, nil
}

// boardLikes adapts raw board task maps to the loose BoardTaskLike shape.
func boardLikes(tasks []map[string]any) []BoardTaskLike {
	likes := make([]BoardTaskLike, len(tasks))
	for i, t := range tasks {
		if t == nil {
			continue
		}
		likes[i] = BoardTaskLike{Status: t["status"], Attempts: t["attempts"], TotalAttempts: t["totalAttempts"]}
	}
	return likes
}

// looseString mirrors the TS String(x) coercion used for loose status checks.
func looseString(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case string:
		return x
	default:
		return fmt.Sprintf("%v", x)
	}
}

// numberIsFinite mirrors Number(x) + Number.isFinite for the totalAttempts
// check. (Distinctly named from autopr.go's jsNumber, which returns the bare
// Number(x) value without the finite probe.)
func numberIsFinite(v any) (float64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false // Number(undefined) is NaN
	case float64:
		return x, true
	case int:
		return float64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, true // Number('') is 0
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// containsString is a tiny membership check.
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
