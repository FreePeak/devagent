// Task mode: the Go port of src/task.ts — the orchestrator-facing one-shot
// `devagent task` halves (defaultTaskId, syntheticTicketFromPrompt, runTask)
// and the task-publishing boundary (publishTaskBranch + TaskPublishDeps).
// The PRD backlog reconciliation half of src/task.ts lives in backlog.go.

package pipeline

import (
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/scout"
)

// TaskRandFunc is the Math.random seam of defaultTaskId: it returns the
// random base36 suffix segment. Tests pin it for determinism.
var TaskRandFunc = func() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	s := strconv.FormatUint(binary.BigEndian.Uint64(b[:]), 36)
	for len(s) < 4 {
		s = "0" + s
	}
	return s[:4]
}

// DefaultTaskID mirrors defaultTaskId (loop 66): the previous constant `TASK`
// made every concurrent run fight over `.devagent-worktrees/TASK` and the
// branch `devagent/TASK` ("already used by worktree at ..."). Epoch36 + a
// random suffix keeps ids unique per invocation while staying sanitize-safe.
// now == nil uses the wall clock (TS Date.now default).
func DefaultTaskID(now func() int64) string {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	return fmt.Sprintf("TASK-%s-%s", strconv.FormatInt(now(), 36), TaskRandFunc())
}

// SyntheticTicketFromPrompt mirrors syntheticTicketFromPrompt: the first line
// becomes the title (capped at 80 chars), the rest becomes the description
// (falling back to the first line when the prompt is single-line). An empty
// taskID defaults to a collision-free DefaultTaskID.
func SyntheticTicketFromPrompt(prompt, taskID string) scout.TicketSpec {
	trimmed := strings.TrimSpace(prompt)
	first := firstLine(trimmed)
	rest := ""
	if i := strings.IndexByte(trimmed, '\n'); i >= 0 {
		rest = trimmed[i+1:]
	}
	description := strings.TrimSpace(rest)
	if description == "" {
		description = first
	}
	id := taskID
	if id == "" {
		id = DefaultTaskID(nil)
	}
	return scout.TicketSpec{
		ID:                 id,
		Title:              truncateRunes(first, 80),
		Description:        description,
		Labels:             []string{"orchestrated"},
		AcceptanceCriteria: []string{},
		URL:                "",
		TrackerInternalID:  id,
	}
}

// TaskOptions mirrors task.ts TaskOptions: any external harness (Orca, CI,
// another DevAgent) can drive DevAgent with a raw prompt and a cwd.
type TaskOptions struct {
	Prompt            string
	RepoPath          string
	AutoPr            bool
	AutoMerge         bool
	MaxLoops          int
	TimeoutMs         int
	Cleanup           CleanupMode // "" = 'auto'
	DropOrcaWorkspace bool
	Log               RunLog
	// TaskID names the synthetic ticket, worktree (.devagent-worktrees/<id>)
	// and branch (devagent/<id>). Concurrent dispatches must not share one
	// id — git refuses to check a branch out twice across worktrees.
	TaskID string
}

// TaskImplResult mirrors the implementStage result shape of task.ts TaskDeps:
// { ok, worker, attempts, worktreePath? }.
type TaskImplResult struct {
	OK           bool
	Worker       WorkerName
	Attempts     int
	WorktreePath string // "" = undefined
}

// PublishImpl mirrors the impl argument of publishTaskBranch:
// { ok, worktreePath? }. WorktreePath "" = undefined (unpublishable).
type PublishImpl struct {
	OK           bool
	WorktreePath string
}

// TaskDeps mirrors task.ts TaskDeps. PublishStage nil = never publishes
// (no PR URL). The seams follow the TS promise shapes: no error channel.
type TaskDeps struct {
	RunPipelineDeps PipelineDeps
	// ImplementStage: worker dispatch identical to deps.ts implementStage;
	// injected by the caller.
	ImplementStage func(cfg TaskOptions, ticket TicketSpec, log RunLog) TaskImplResult
	// PublishStage returns the PR URL, or "" when no PR was opened.
	PublishStage func(cfg TaskOptions, ticket TicketSpec, impl PublishImpl) string
}

// TaskResult mirrors the runTask return shape { ok, prUrl?, note }.
type TaskResult struct {
	OK    bool
	PRURL string // "" = undefined
	Note  string
}

// RunTask mirrors runTask(): minimal pipeline execution for prompt-driven
// tasks (no tracker round-trip). An implementStage failure yields the pinned
// 'implementation failed validation' note without publishing.
func RunTask(opts TaskOptions, deps TaskDeps) TaskResult {
	ticket := SyntheticTicketFromPrompt(opts.Prompt, opts.TaskID)
	if opts.Log != nil {
		opts.Log.Info(ledger.StageTask, "Task starting", []ledger.KV{
			{Key: "title", Value: ticket.Title},
			{Key: "taskId", Value: ticket.ID},
		})
	}

	impl := deps.ImplementStage(opts, ticket, opts.Log)
	if !impl.OK {
		return TaskResult{OK: false, Note: "implementation failed validation"}
	}

	if !opts.AutoPr {
		wt := impl.WorktreePath
		if wt == "" {
			wt = "(repo root)"
		}
		return TaskResult{OK: true, Note: "worktree ready for review: " + wt}
	}
	var prURL string
	if deps.PublishStage != nil {
		prURL = deps.PublishStage(opts, ticket, PublishImpl{OK: impl.OK, WorktreePath: impl.WorktreePath})
	}
	if prURL == "" {
		return TaskResult{OK: true, Note: "no remote credentials; branch preserved locally"}
	}
	return TaskResult{OK: true, PRURL: prURL, Note: "PR opened: " + prURL}
}

// TaskPublishPrRequest mirrors the createPr argument of TaskPublishDeps.
type TaskPublishPrRequest struct {
	RepoPath string
	Branch   string
	Title    string
	Body     string
}

// TaskPublishDeps mirrors task.ts TaskPublishDeps: the remote boundary of
// task publishing, injected so the logic stays testable over real git
// fixtures. In listChangedFiles an empty ref means undefined (HEAD).
type TaskPublishDeps struct {
	CommitAllChanges func(worktreePath, message string) (bool, error)
	CurrentBranch    func(worktreePath string) (string, error)
	ListChangedFiles func(worktreePath, baseBranch, ref string) ([]string, error)
	PushBranch       func(repoPath, branch string) error
	CreatePr         func(o TaskPublishPrRequest) (string, error)
}

// TaskPublishOptions mirrors task.ts TaskPublishOptions.
type TaskPublishOptions struct {
	RepoPath   string
	Prompt     string
	BaseBranch string
	Log        RunLog
}

// PublishTaskBranch mirrors publishTaskBranch (dogfood loops 7-9 lesson):
//   - commits whatever the worker left uncommitted — agents routinely edit
//     without committing, and an uncommitted change silently ships as an
//     empty PR;
//   - pushes the branch the worktree ACTUALLY has checked out;
//   - refuses to open a PR when the diff vs base is empty;
//   - when cleanup=auto already removed the worktree, publishes from the
//     surviving run branch in the main repo (snapshot already landed there).
func PublishTaskBranch(opts TaskPublishOptions, impl PublishImpl, io TaskPublishDeps) (string, error) {
	if impl.WorktreePath == "" {
		return "", nil
	}

	title := truncateRunes(firstLine(opts.Prompt), 80)
	// cleanup=auto removes a successful run's worktree after snapshotting its
	// uncommitted output onto the run branch. Publishing must then happen from
	// the main repo against that branch: git add in a removed cwd exits -1
	// with empty stderr (loop 57-58).
	wtAlive := fileExists(impl.WorktreePath)
	branch := ""
	if wtAlive {
		b, err := io.CurrentBranch(impl.WorktreePath)
		if err != nil {
			return "", err
		}
		branch = b
	} else {
		branch = "devagent/" + filepath.Base(impl.WorktreePath)
	}
	if wtAlive {
		if _, err := io.CommitAllChanges(impl.WorktreePath, "devagent(task): "+title); err != nil {
			return "", err
		}
	}
	diffRepo := opts.RepoPath
	ref := ""
	if wtAlive {
		diffRepo = impl.WorktreePath
	} else {
		ref = branch
	}
	changed, err := io.ListChangedFiles(diffRepo, opts.BaseBranch, ref)
	if err != nil {
		return "", err
	}
	if len(changed) == 0 {
		if opts.Log != nil {
			opts.Log.Warn(ledger.StageTask, "nothing changed vs base; skipping PR", []ledger.KV{
				{Key: "branch", Value: branch},
				{Key: "baseBranch", Value: opts.BaseBranch},
			})
		}
		return "", nil
	}

	if err := io.PushBranch(opts.RepoPath, branch); err != nil {
		return "", err
	}
	// PRD-per-PR policy (2026-09-07): docs/PRD.md is the living state record of
	// the repo, so every automated PR lands with its state update. The section
	// rides in the PR body (reviewer-facing), while the dispatch prompt carries
	// the same requirement worker-facing.
	body := strings.Join([]string{
		"Automated task via `devagent task`.",
		"",
		"## Prompt",
		opts.Prompt,
		"",
		"## PRD state update (repo policy)",
		"- [ ] docs/PRD.md sections touched by this change are updated to the post-PR state",
		"- [ ] the *Last updated* footer reflects this change",
	}, "\n")
	return io.CreatePr(TaskPublishPrRequest{
		RepoPath: opts.RepoPath,
		Branch:   branch,
		Title:    title,
		Body:     body,
	})
}

// DispatchClaimOwner is the queue claim owner the control API stamps on the
// row it dispatches (`POST /dispatch`, issue #315). The selfbuild loop claims
// under `selfbuild-loop-<pid>`, so a daemon-owned claim is never confused
// with a loop claim.
const DispatchClaimOwner = "daemon"

// FinishDispatchClaim releases the control API's claim on a dispatched row
// once the run it spawned ends: `done` on a clean run, `failed` with the
// detail otherwise. The write is fenced on the generation read back from the
// row, so a row another worker reclaimed meanwhile is never overwritten, and
// a row this process does not own (any other claimedBy) is left alone —
// `devagent task --id` is also driven with ids that have no daemon row (the
// CI fixer, remote forwarding), and those must not touch the queue.
//
// The release lives in the CHILD (`devagent task`), not the daemon: the
// detached child outlives a daemon restart (setDetach), so a daemon-side
// release would silently drop the completion, leave the row claimed until its
// lease lapses, and the selfbuild loop would then re-run a finished goal.
func FinishDispatchClaim(repoPath, taskID string, ok bool, detail string) {
	if taskID == "" {
		return
	}
	row := queue.ReadTask(repoPath, taskID)
	if row == nil || row.ClaimedBy == nil || *row.ClaimedBy != DispatchClaimOwner {
		return
	}
	gen := int64(0)
	if row.LeaseGeneration != nil {
		gen = int64(*row.LeaseGeneration)
	}
	if ok {
		_ = queue.CompleteTask(repoPath, taskID, gen, nil)
		return
	}
	_ = queue.FailTask(repoPath, taskID, gen, detail, nil)
}
