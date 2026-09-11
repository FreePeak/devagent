// Tests for task.go, remote.go and runregistry.go — the Go port of
// test/task.test.ts, test/task-publish.test.ts (real git fixtures + fake
// remote boundary) and test/run-lock.test.ts. tbl-prefixed helpers are
// shared with backlog_test.go.

package pipeline

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/git"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/queue"
)

// tblCapturingLog records Info/Warn messages for byte-parity pins.
type tblCapturingLog struct {
	infos []string
	warns []string
}

func (l *tblCapturingLog) Info(_ ledger.RunStage, message string, _ []ledger.KV) {
	l.infos = append(l.infos, message)
}

func (l *tblCapturingLog) Warn(_ ledger.RunStage, message string, _ []ledger.KV) {
	l.warns = append(l.warns, message)
}

func (l *tblCapturingLog) Error(_ ledger.RunStage, message string, _ []ledger.KV) {
	l.warns = append(l.warns, message)
}

// tblGit runs one git command, failing the test on error; returns trimmed output.
func tblGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestTaskDefaultTaskID(t *testing.T) {
	shape := regexp.MustCompile(`^TASK-[a-z0-9]+-[a-z0-9]{2,6}$`)
	a := DefaultTaskID(nil)
	b := DefaultTaskID(nil)
	if !shape.MatchString(a) || !shape.MatchString(b) {
		t.Fatalf("ids %q / %q must match TASK-<epoch36>-<rand>", a, b)
	}
	if a == b {
		t.Fatalf("ids must be unique per invocation, got %q twice", a)
	}

	// Injected clock drives the epoch segment.
	zero := DefaultTaskID(func() int64 { return 0 })
	if !regexp.MustCompile(`^TASK-0-[a-z0-9]{2,6}$`).MatchString(zero) {
		t.Fatalf("epoch 0 id = %q", zero)
	}

	// Injected rand drives the suffix segment (TaskRandFunc seam).
	oldRand := TaskRandFunc
	TaskRandFunc = func() string { return "ab01" }
	defer func() { TaskRandFunc = oldRand }()
	if got := DefaultTaskID(func() int64 { return 42 }); got != "TASK-16-ab01" {
		t.Fatalf("id = %q, want TASK-16-ab01 (42 base36 = 16)", got)
	}
}

func TestTaskSyntheticTicket(t *testing.T) {
	tk := SyntheticTicketFromPrompt("Add rate limiting\n\nUse a token bucket per client IP.", "")
	if tk.Title != "Add rate limiting" || tk.Description != "Use a token bucket per client IP." {
		t.Fatalf("title/description = %q / %q", tk.Title, tk.Description)
	}
	if len(tk.Labels) != 1 || tk.Labels[0] != "orchestrated" {
		t.Fatalf("labels = %v, want [orchestrated]", tk.Labels)
	}
	if len(tk.AcceptanceCriteria) != 0 || tk.URL != "" {
		t.Fatalf("ticket = %+v", tk)
	}

	single := SyntheticTicketFromPrompt("Only one line", "")
	if single.Title != "Only one line" || single.Description != "Only one line" {
		t.Fatalf("single-line ticket = %+v", single)
	}

	capped := SyntheticTicketFromPrompt(strings.Repeat("x", 200), "")
	if len(capped.Title) != 80 {
		t.Fatalf("title len = %d, want 80", len(capped.Title))
	}

	explicit := SyntheticTicketFromPrompt("do thing", "loop-66")
	if explicit.ID != "loop-66" || explicit.TrackerInternalID != "loop-66" {
		t.Fatalf("explicit id ticket = %+v", explicit)
	}

	first := SyntheticTicketFromPrompt("one", "")
	second := SyntheticTicketFromPrompt("two", "")
	if first.ID == "TASK" || first.ID == second.ID {
		t.Fatalf("default ids must be collision-free: %q vs %q", first.ID, second.ID)
	}
}

func tblFakeTaskDeps(ok bool, prURL string, seenTicketID *string) TaskDeps {
	return TaskDeps{
		RunPipelineDeps: PipelineDeps{},
		ImplementStage: func(_ TaskOptions, ticket TicketSpec, _ RunLog) TaskImplResult {
			if seenTicketID != nil {
				*seenTicketID = ticket.ID
			}
			wt := ""
			if ok {
				wt = "/wt"
			}
			return TaskImplResult{OK: ok, Worker: "claude-code", Attempts: 1, WorktreePath: wt}
		},
		PublishStage: func(TaskOptions, TicketSpec, PublishImpl) string { return prURL },
	}
}

func TestTaskRunTask(t *testing.T) {
	log := &tblCapturingLog{}

	// Threads opts.taskId into the dispatched ticket.
	seen := ""
	RunTask(TaskOptions{Prompt: "do thing", RepoPath: ".", AutoPr: false, MaxLoops: 1, TimeoutMs: 1000, Log: log, TaskID: "loop-66-x"}, tblFakeTaskDeps(true, "", &seen))
	if seen != "loop-66-x" {
		t.Fatalf("dispatched ticket id = %q, want loop-66-x", seen)
	}
	if len(log.infos) != 1 || log.infos[0] != "Task starting" {
		t.Fatalf("info log = %q, want [Task starting]", log.infos)
	}

	// Failure: no publish, pinned note.
	r := RunTask(TaskOptions{Prompt: "do thing", RepoPath: ".", AutoPr: true, MaxLoops: 1, TimeoutMs: 1000, Log: log}, tblFakeTaskDeps(false, "", nil))
	if r.OK || r.PRURL != "" || r.Note != "implementation failed validation" {
		t.Fatalf("failure result = %+v", r)
	}

	// No --auto-pr: publisher never consulted, worktree reported.
	r = RunTask(TaskOptions{Prompt: "do thing", RepoPath: ".", AutoPr: false, MaxLoops: 1, TimeoutMs: 1000, Log: log}, tblFakeTaskDeps(true, "https://example/pr/1", nil))
	if !r.OK || r.PRURL != "" || r.Note != "worktree ready for review: /wt" {
		t.Fatalf("no-autoPr result = %+v", r)
	}
	noWt := RunTask(TaskOptions{Prompt: "do thing", RepoPath: ".", AutoPr: false, MaxLoops: 1, TimeoutMs: 1000, Log: log}, TaskDeps{
		RunPipelineDeps: PipelineDeps{},
		ImplementStage: func(TaskOptions, TicketSpec, RunLog) TaskImplResult {
			return TaskImplResult{OK: true, Worker: "claude-code", Attempts: 1}
		},
	})
	if noWt.Note != "worktree ready for review: (repo root)" {
		t.Fatalf("no-worktree note = %q", noWt.Note)
	}

	// --auto-pr with a PR url.
	r = RunTask(TaskOptions{Prompt: "do thing", RepoPath: ".", AutoPr: true, MaxLoops: 1, TimeoutMs: 1000, Log: log}, tblFakeTaskDeps(true, "https://example/pr/2", nil))
	if !r.OK || r.PRURL != "https://example/pr/2" || r.Note != "PR opened: https://example/pr/2" {
		t.Fatalf("auto-pr result = %+v", r)
	}

	// Missing credentials: publishStage yields no URL.
	r = RunTask(TaskOptions{Prompt: "do thing", RepoPath: ".", AutoPr: true, MaxLoops: 1, TimeoutMs: 1000, Log: log}, tblFakeTaskDeps(true, "", nil))
	if !r.OK || r.PRURL != "" || r.Note != "no remote credentials; branch preserved locally" {
		t.Fatalf("no-credentials result = %+v", r)
	}
}

type tblPublishCapture struct {
	pushed string
	pr     TaskPublishPrRequest
}

// tblPublishDeps wires the REAL git seam over the fixture with a fake remote
// boundary — mirrors the production wiring and test/task-publish.test.ts.
func tblPublishDeps(capture *tblPublishCapture) TaskPublishDeps {
	return TaskPublishDeps{
		CommitAllChanges: git.CommitAllChanges,
		CurrentBranch:    git.CurrentBranch,
		ListChangedFiles: git.ListChangedFiles,
		PushBranch: func(_ string, branch string) error {
			capture.pushed = branch
			return nil
		},
		CreatePr: func(o TaskPublishPrRequest) (string, error) {
			capture.pr = o
			return "https://example/pr/" + o.Branch, nil
		},
	}
}

func tblPublishFixture(t *testing.T) (repo, wt string) {
	t.Helper()
	repo = t.TempDir()
	tblGit(t, repo, "init", "-b", "main")
	tblGit(t, repo, "config", "user.email", "t@t")
	tblGit(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tblGit(t, repo, "add", "-A")
	tblGit(t, repo, "commit", "-m", "init")
	wt = filepath.Join(repo, ".devagent-worktrees", "TASK")
	tblGit(t, repo, "worktree", "add", "-b", "devagent/TASK", wt)
	return repo, wt
}

func tblPublishOpts(repo string, log *tblCapturingLog) TaskPublishOptions {
	return TaskPublishOptions{RepoPath: repo, Prompt: "Fix the thing", BaseBranch: "main", Log: log}
}

func TestTaskPublishTaskBranch(t *testing.T) {
	t.Run("commits uncommitted worker output and pushes the ACTUAL worktree branch", func(t *testing.T) {
		repo, wt := tblPublishFixture(t)
		// Worker edited files but never committed — the dogfood failure mode.
		if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		capture := &tblPublishCapture{}
		log := &tblCapturingLog{}

		url, err := PublishTaskBranch(tblPublishOpts(repo, log), PublishImpl{OK: true, WorktreePath: wt}, tblPublishDeps(capture))
		if err != nil {
			t.Fatal(err)
		}
		if url == "" || !strings.HasPrefix(url, "https://example/pr/") {
			t.Fatalf("url = %q", url)
		}
		// Ground truth branch name, NOT an invented devagent/task-<runId> refspec.
		if capture.pushed != "devagent/TASK" {
			t.Fatalf("pushed = %q, want devagent/TASK", capture.pushed)
		}
		if capture.pr.Branch != "devagent/TASK" {
			t.Fatalf("pr branch = %q, want devagent/TASK", capture.pr.Branch)
		}
		if capture.pr.Title != "Fix the thing" {
			t.Fatalf("pr title = %q", capture.pr.Title)
		}
		// Nothing left uncommitted in the worktree, and the pushed branch
		// actually contains the new file.
		if status := tblGit(t, wt, "status", "--porcelain"); status != "" {
			t.Fatalf("worktree dirty after publish: %q", status)
		}
		if shown := tblGit(t, wt, "show", "devagent/TASK:feature.txt"); shown != "hello" {
			t.Fatalf("branch content = %q, want hello", shown)
		}
		// PRD-per-PR policy block rides verbatim in the PR body.
		wantBody := strings.Join([]string{
			"Automated task via `devagent task`.",
			"",
			"## Prompt",
			"Fix the thing",
			"",
			"## PRD state update (repo policy)",
			"- [ ] docs/PRD.md sections touched by this change are updated to the post-PR state",
			"- [ ] the *Last updated* footer reflects this change",
		}, "\n")
		if capture.pr.Body != wantBody {
			t.Fatalf("pr body = %q, want %q", capture.pr.Body, wantBody)
		}
		if capture.pr.RepoPath != repo {
			t.Fatalf("pr repoPath = %q, want %q", capture.pr.RepoPath, repo)
		}
	})

	t.Run("skips publishing entirely when the diff vs base is empty", func(t *testing.T) {
		repo, wt := tblPublishFixture(t)
		capture := &tblPublishCapture{}
		log := &tblCapturingLog{}

		url, err := PublishTaskBranch(tblPublishOpts(repo, log), PublishImpl{OK: true, WorktreePath: wt}, tblPublishDeps(capture))
		if err != nil {
			t.Fatal(err)
		}
		if url != "" || capture.pushed != "" || capture.pr.Branch != "" {
			t.Fatalf("empty diff must not publish: url=%q capture=%+v", url, capture)
		}
		if len(log.warns) != 1 || log.warns[0] != "nothing changed vs base; skipping PR" {
			t.Fatalf("warns = %q", log.warns)
		}
	})

	t.Run("publishes even when the tree is clean but the branch carries commits", func(t *testing.T) {
		repo, wt := tblPublishFixture(t)
		if err := os.WriteFile(filepath.Join(wt, "committed.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := git.CommitAllChanges(wt, "worker already committed"); err != nil {
			t.Fatal(err)
		}
		capture := &tblPublishCapture{}

		url, err := PublishTaskBranch(tblPublishOpts(repo, &tblCapturingLog{}), PublishImpl{OK: true, WorktreePath: wt}, tblPublishDeps(capture))
		if err != nil {
			t.Fatal(err)
		}
		if url == "" {
			t.Fatalf("url = %q, want a PR", url)
		}
		if capture.pushed != "devagent/TASK" {
			t.Fatalf("pushed = %q", capture.pushed)
		}
	})

	t.Run("reports missing worktree as unpublishable without error", func(t *testing.T) {
		capture := &tblPublishCapture{}
		url, err := PublishTaskBranch(tblPublishOpts(".", &tblCapturingLog{}), PublishImpl{OK: true}, tblPublishDeps(capture))
		if err != nil {
			t.Fatal(err)
		}
		if url != "" || capture.pushed != "" || capture.pr.Branch != "" {
			t.Fatalf("missing worktree must be a no-op: url=%q capture=%+v", url, capture)
		}
	})

	// Regression: loop 57-58. cleanup=auto removed the worktree after
	// auto-cleanup snapshotted the changes onto devagent/TASK. Publishing must
	// then run from the main repo against that branch, not fail with
	// "git add -A exited -1" from a nonexistent cwd.
	t.Run("publishes from the run branch after auto-cleanup removed the worktree", func(t *testing.T) {
		repo, wt := tblPublishFixture(t)
		if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("fix\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := git.CommitAllChanges(wt, "worker changes"); err != nil {
			t.Fatal(err)
		}
		tblGit(t, repo, "worktree", "remove", "--force", wt)
		tblGit(t, repo, "worktree", "prune")
		capture := &tblPublishCapture{}

		url, err := PublishTaskBranch(tblPublishOpts(repo, &tblCapturingLog{}), PublishImpl{OK: true, WorktreePath: wt}, tblPublishDeps(capture))
		if err != nil {
			t.Fatal(err)
		}
		if url == "" {
			t.Fatalf("url = %q, want a PR", url)
		}
		if capture.pushed != "devagent/TASK" || capture.pr.Branch != "devagent/TASK" {
			t.Fatalf("pushed=%q pr.branch=%q, want devagent/TASK", capture.pushed, capture.pr.Branch)
		}
	})
}

func TestTaskParseRemoteTarget(t *testing.T) {
	scp := []struct {
		in   string
		want RemoteTarget
	}{
		{"user@host:/srv/repos/app", RemoteTarget{User: "user", Host: "host", Path: "/srv/repos/app"}},
		{"host:/srv/repos/app", RemoteTarget{Host: "host", Path: "/srv/repos/app"}},
	}
	for _, c := range scp {
		got, err := ParseRemoteTarget(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("ParseRemoteTarget(%q) = %+v, %v; want %+v, nil", c.in, got, err, c.want)
		}
	}

	sshURL, err := ParseRemoteTarget("ssh://user@host:2222/srv/repos/app")
	if err != nil || sshURL != (RemoteTarget{User: "user", Host: "host", Path: "/srv/repos/app", Port: 2222}) {
		t.Fatalf("ssh URL parse = %+v, %v", sshURL, err)
	}
	noPort, err := ParseRemoteTarget("ssh://host/srv/repos/app")
	if err != nil || noPort != (RemoteTarget{Host: "host", Path: "/srv/repos/app"}) {
		t.Fatalf("ssh no-port parse = %+v, %v", noPort, err)
	}

	errCases := []struct{ in, want string }{
		{"", "empty remote target"},
		{"ssh://host", `remote target "ssh://host" needs a repo path after the host`},
		{"ssh://host:0/srv", `invalid ssh port in "ssh://host:0/srv"`},
		{"ssh://host:70000/srv", `invalid ssh port in "ssh://host:70000/srv"`},
		{"ssh://:2222/srv", `missing host in "ssh://:2222/srv"`},
		{"no-colon-at-all", `remote target "no-colon-at-all" expected [user@]host:/path/to/repo`},
		{":/srv/app", `remote target ":/srv/app" expected [user@]host:/path/to/repo`},
		{"host:relative/path", `remote path must be absolute in "host:relative/path"`},
		{"user@:/srv/app", `missing host in "user@:/srv/app"`},
	}
	for _, c := range errCases {
		_, err := ParseRemoteTarget(c.in)
		if err == nil || err.Error() != c.want {
			t.Fatalf("ParseRemoteTarget(%q) error = %v, want %q", c.in, err, c.want)
		}
	}
}

func TestTaskShellQuoteAndSshArgs(t *testing.T) {
	if got := ShellQuote("it's"); got != `'it'\''s'` {
		t.Fatalf("ShellQuote = %q", got)
	}
	plain := BuildSshArgs(RemoteTarget{Host: "h"}, "cmd")
	if strings.Join(plain, " ") != "ssh -o BatchMode=yes h cmd" {
		t.Fatalf("args = %q", plain)
	}
	withUser := BuildSshArgs(RemoteTarget{User: "u", Host: "h", Port: 2222}, "cmd")
	if strings.Join(withUser, " ") != "ssh -o BatchMode=yes -p 2222 u@h cmd" {
		t.Fatalf("args = %q", withUser)
	}
}

func TestTaskRunRemoteTask(t *testing.T) {
	const target = "deploy@build.example:/srv/repo"
	opts := RunRemoteTaskOptions{
		Target:    target,
		Prompt:    "do it",
		TaskID:    "TASK-1",
		Worker:    "claude-code",
		TimeoutMs: 60000,
		Log:       &tblCapturingLog{},
	}

	t.Run("parse failure short-circuits without spawning", func(t *testing.T) {
		r := RunRemoteTask(RunRemoteTaskOptions{Target: "host:relative"}, RemoteDeps{
			Run: func([]string, int) RemoteRunOutcome {
				t.Fatal("runner must not be called on parse failure")
				return RemoteRunOutcome{}
			},
		})
		if r.OK || r.Note != `remote path must be absolute in "host:relative"` {
			t.Fatalf("result = %+v", r)
		}
	})

	t.Run("preflight failure fails fast with the pinned note and a warn", func(t *testing.T) {
		log := &tblCapturingLog{}
		calls := 0
		r := RunRemoteTask(RunRemoteTaskOptions{Target: target, Prompt: "p", TimeoutMs: 60000, Log: log}, RemoteDeps{
			Run: func(argv []string, timeoutMs int) RemoteRunOutcome {
				calls++
				if calls != 1 {
					t.Fatal("dispatch must not run after failed preflight")
				}
				// Preflight is capped at 15s even when the timeout is larger.
				if timeoutMs != 15000 {
					t.Errorf("preflight timeout = %d, want 15000", timeoutMs)
				}
				if argv[0] != "ssh" || argv[1] != "-o" || argv[2] != "BatchMode=yes" || argv[3] != "deploy@build.example" {
					t.Errorf("preflight argv = %q", argv)
				}
				if !strings.Contains(argv[4], "command -v devagent >/dev/null && test -d '/srv/repo' && git -C '/srv/repo' rev-parse --git-dir >/dev/null") {
					t.Errorf("preflight command = %q", argv[4])
				}
				return RemoteRunOutcome{ExitCode: 127}
			},
		})
		if r.OK || r.Note != "remote preflight failed on build.example: need devagent on PATH and a git repo at /srv/repo" {
			t.Fatalf("result = %+v", r)
		}
		if len(log.warns) != 1 || log.warns[0] != "remote preflight failed on build.example" {
			t.Fatalf("warns = %q", log.warns)
		}
	})

	t.Run("dispatch extracts the PR URL and reports it", func(t *testing.T) {
		calls := 0
		var dispatchCmd string
		var dispatchTimeout int
		r := RunRemoteTask(opts, RemoteDeps{
			Run: func(argv []string, timeoutMs int) RemoteRunOutcome {
				calls++
				if calls == 1 {
					return RemoteRunOutcome{ExitCode: 0}
				}
				dispatchCmd = argv[len(argv)-1]
				dispatchTimeout = timeoutMs
				return RemoteRunOutcome{ExitCode: 0, Stdout: "...\nPR opened: https://github.com/o/r/pull/7\n"}
			},
		})
		if calls != 2 {
			t.Fatalf("calls = %d, want 2", calls)
		}
		if dispatchTimeout != 60000 {
			t.Errorf("dispatch timeout = %d, want 60000", dispatchTimeout)
		}
		// Byte-parity: TS pushes --id/--worker as separate &&-joined parts.
		wantCmd := "cd '/srv/repo' && devagent task 'do it' --auto-pr && --id 'TASK-1' && --worker 'claude-code'"
		if dispatchCmd != wantCmd {
			t.Errorf("dispatch cmd = %q, want %q", dispatchCmd, wantCmd)
		}
		if !r.OK || r.PRURL != "https://github.com/o/r/pull/7" || r.Note != "remote PR opened: https://github.com/o/r/pull/7" {
			t.Fatalf("result = %+v", r)
		}
	})

	t.Run("non-zero dispatch exit with a PR reports the fallback note", func(t *testing.T) {
		r := RunRemoteTask(opts, RemoteDeps{
			Run: func(argv []string, timeoutMs int) RemoteRunOutcome {
				if timeoutMs == 15000 {
					return RemoteRunOutcome{ExitCode: 0} // preflight passes
				}
				return RemoteRunOutcome{ExitCode: 2, Stdout: "https://github.com/o/r/pull/9"}
			},
		})
		if r.OK || r.PRURL != "https://github.com/o/r/pull/9" {
			t.Fatalf("result = %+v", r)
		}
		if r.Note != "remote task failed on build.example (exit 2), PR opened anyway: https://github.com/o/r/pull/9" {
			t.Fatalf("note = %q", r.Note)
		}
	})

	t.Run("success without a PR URL", func(t *testing.T) {
		r := RunRemoteTask(opts, RemoteDeps{
			Run: func([]string, int) RemoteRunOutcome {
				return RemoteRunOutcome{ExitCode: 0, Stdout: "worktree ready for review: /wt"}
			},
		})
		if !r.OK || r.PRURL != "" || r.Note != "remote task finished without a PR URL" {
			t.Fatalf("result = %+v", r)
		}
	})
}

func TestTaskTryAcquireRun(t *testing.T) {
	home := t.TempDir()
	l1 := TryAcquireRun(home, "TASK-abc-123")
	if l1 == nil {
		t.Fatal("fresh acquire must succeed")
	}
	if filepath.Base(l1.Path) != "TASK-abc-123.lock" || l1.TicketID != "TASK-abc-123" {
		t.Fatalf("lock = %+v", l1)
	}
	if l2 := TryAcquireRun(home, "TASK-abc-123"); l2 != nil {
		t.Fatal("second acquire while fresh must fail")
	}
	// The on-disk payload mirrors the TS JSON.stringify shape.
	raw, err := os.ReadFile(l1.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\{"pid":\d+,"startedAt":\d+\}$`).MatchString(strings.TrimSpace(string(raw))) {
		t.Fatalf("lock payload = %q", raw)
	}
	l1.Release()
	if l3 := TryAcquireRun(home, "TASK-abc-123"); l3 == nil {
		t.Fatal("released lock must be re-acquirable")
	}
}

func TestFinishDispatchClaim(t *testing.T) {
	repo := t.TempDir()
	enqueue := func(id string) {
		t.Helper()
		if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{
			ID: id, Title: "t", Goal: "g", Source: DispatchClaimOwner,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Clean run: the control API's claim resolves to done.
	enqueue("TASK-ok")
	if queue.ClaimTask(repo, "TASK-ok", DispatchClaimOwner, nil) == nil {
		t.Fatal("claim TASK-ok failed")
	}
	FinishDispatchClaim(repo, "TASK-ok", true, "")
	if row := queue.ReadTask(repo, "TASK-ok"); row == nil || row.Status != queue.StatusDone {
		t.Fatalf("TASK-ok = %+v, want done", row)
	}

	// Failed run: failed, with the detail kept for retry visibility.
	enqueue("TASK-bad")
	if queue.ClaimTask(repo, "TASK-bad", DispatchClaimOwner, nil) == nil {
		t.Fatal("claim TASK-bad failed")
	}
	FinishDispatchClaim(repo, "TASK-bad", false, "implementation failed validation")
	row := queue.ReadTask(repo, "TASK-bad")
	if row.Status != queue.StatusFailed || row.LastError == nil ||
		!strings.Contains(*row.LastError, "failed validation") {
		t.Fatalf("TASK-bad = %+v, want failed with detail", row)
	}

	// A row another worker owns is never touched: `devagent task --id` also
	// runs for the CI fixer and remote forwarding, whose ids have no daemon
	// row (or a row claimed by someone else).
	enqueue("TASK-other")
	if queue.ClaimTask(repo, "TASK-other", "selfbuild-loop-9", nil) == nil {
		t.Fatal("claim TASK-other failed")
	}
	FinishDispatchClaim(repo, "TASK-other", true, "")
	if got := queue.ReadTask(repo, "TASK-other"); got.Status != queue.StatusClaimed {
		t.Fatalf("foreign claim overwritten: %q", got.Status)
	}

	// A lease reclaimed while the run was in flight bumps the generation; the
	// stale release must be refused, not resurrect the row.
	enqueue("TASK-stale")
	t0 := time.Now().UnixMilli()
	short := &queue.ClaimOptions{LeaseMs: 1000, Now: func() int64 { return t0 }}
	if queue.ClaimTask(repo, "TASK-stale", DispatchClaimOwner, short) == nil {
		t.Fatal("claim TASK-stale failed")
	}
	later := &queue.ClaimOptions{Now: func() int64 { return t0 + 2000 }}
	if queue.ClaimTask(repo, "TASK-stale", "selfbuild-loop-9", later) == nil {
		t.Fatal("reclaim TASK-stale failed")
	}
	FinishDispatchClaim(repo, "TASK-stale", true, "")
	if got := queue.ReadTask(repo, "TASK-stale"); got.Status != queue.StatusClaimed ||
		*got.ClaimedBy != "selfbuild-loop-9" {
		t.Fatalf("reclaimed row overwritten: %+v", got)
	}

	// No id, no row: no-op.
	FinishDispatchClaim(repo, "", true, "")
	FinishDispatchClaim(repo, "TASK-absent", true, "")
}
