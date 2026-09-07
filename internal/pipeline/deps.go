// deps.go ports src/deps.ts: buildDeps (the PipelineDeps wiring consumed by
// RunPipeline), the publish stage glue, the PR body builder, and the
// runGateG4 async-review wiring. collectChangedSourceFiles is ported locally
// (the rest of async-review.ts ships in internal/orchestrator). LeanKG
// provider and reaper live in leankg.go/reaper.go (same package).
//
// Byte-parity: log lines, error strings, and the PR body mirror the
// TypeScript originals exactly; tests pin them.
//
// Package pipeline is the Go port of DevAgent's ticket→PR pipeline
// command-support layer (FR-GO-07 remainder, issue #223): the pipeline state
// machine, task dispatch, PRD backlog reconciliation, the consume loop,
// the LeanKG client, the stale-worker reaper, implementStage/buildDeps,
// fleet, create, the orchestrator planner, remote task, and the run
// registry. Byte-parity with src/*.ts is pinned by tests.

package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/git"
	"github.com/FreePeak/devagent/internal/integrations"
	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/FreePeak/devagent/internal/spawn"
)

// StageConfig mirrors deps.ts StageConfig: the run-level knobs the CLI
// passes into implementStage, extended with the lessons/knowledge-context
// fields the TS implementation reads.
type StageConfig struct {
	RepoPath          string
	MaxLoops          int
	TimeoutMs         int
	Worker            WorkerName
	AutoPr            bool
	AutoMerge         *bool // nil = unset (TS autoMerge?: boolean)
	Cleanup           CleanupMode
	DropOrcaWorkspace bool
	// implementStage extension (cli.ts task path).
	LessonsFile     string
	LessonsMaxChars *float64
	Context         *config.ContextConfig
	Model           string // "" = unset
	Variant         string // "" = unset
}

// BuildDryRunDeps mirrors buildDryRunDeps: a synthetic ticket so plan-only
// runs need no tracker credentials, plus a trivially-passing G3.
func BuildDryRunDeps() PipelineDeps {
	id := "DRY-RUN"
	return PipelineDeps{
		FetchTicket: func(string) (TicketSpec, error) {
			return TicketSpec{
				ID:                 id,
				Title:              "[dry-run] " + id,
				Description:        "Synthetic ticket for plan-only execution; no tracker credentials required.",
				Labels:             []string{},
				AcceptanceCriteria: []string{},
			}, nil
		},
		RunGateG3: func(string, TicketClass) GateCheck {
			return GateCheck{Passed: true, Findings: []gates.Finding{}, Detail: "skipped: dry-run"}
		},
	}
}

// BuildDeps mirrors buildDeps(creds, cfg, log) (src/deps.ts:55): production
// deps wired to the real tracker, gate, git, and gh integrations.
func BuildDeps(creds config.Credentials, cfg StageConfig, log RunLog) PipelineDeps {
	return PipelineDeps{
		FetchTicket: func(ticketID string) (TicketSpec, error) {
			if integrations.GitHubIssueRef.MatchString(ticketID) {
				if creds.GithubToken == "" {
					return TicketSpec{}, fmt.Errorf("GitHub issue ref %s requires GITHUB_TOKEN to be set", ticketID)
				}
				spec, err := integrations.FetchGitHubTicket(ticketID, creds.GithubToken, integrations.FetchOptions{})
				return impToIntegrationTicketSpec(spec), err
			}
			if os.Getenv("JIRA_DOMAIN") != "" && os.Getenv("JIRA_EMAIL") != "" && os.Getenv("JIRA_API_TOKEN") != "" {
				spec, err := integrations.FetchJiraTicket(ticketID, integrations.JiraCredentials{
					Domain:   os.Getenv("JIRA_DOMAIN"),
					Email:    os.Getenv("JIRA_EMAIL"),
					APIToken: os.Getenv("JIRA_API_TOKEN"),
				}, integrations.FetchOptions{})
				return impToIntegrationTicketSpec(spec), err
			}
			spec, err := integrations.LinearFetchTicket(ticketID, creds.LinearAPIKey, integrations.FetchOptions{})
			return impToIntegrationTicketSpec(spec), err
		},
		PostTicketComment: func(trackerInternalID, comment string) error {
			return integrations.LinearPostTicketComment(trackerInternalID, comment, creds.LinearAPIKey, integrations.FetchOptions{})
		},
		RunGateG0: func(ticket TicketSpec, classification TicketClass) ReadinessResult {
			r := gates.EvaluateReadiness(gates.ReadinessTicket{
				ID:                 ticket.ID,
				Title:              ticket.Title,
				Description:        ticket.Description,
				Labels:             ticket.Labels,
				AcceptanceCriteria: ticket.AcceptanceCriteria,
			}, classification)
			return ReadinessResult{
				Gate: r.Gate, Passed: r.Passed, Skipped: r.Skipped,
				Score: r.Score, Threshold: r.Threshold, Findings: r.Findings, Detail: r.Detail,
			}
		},
		RunGateG3: func(repoPath string, classification TicketClass) GateCheck {
			r := gates.RunMigrationStaticGate(gates.GateContext{RepoPath: repoPath, Classification: classification})
			return GateCheck{Passed: r.Passed, Findings: r.Findings, Detail: r.Detail}
		},
		// Forward worker/model/variant from the pipeline's RunConfig —
		// BuildDeps' own StageConfig param doesn't carry model/variant.
		ImplementStage: func(c RunConfig, plan ImplementationPlan, lg RunLog) (ImplementResult, error) {
			runCfg := cfg
			if c.Worker != "" {
				runCfg.Worker = c.Worker
			}
			runCfg.Model = c.Model
			runCfg.Variant = c.Variant
			return ImplementStage(runCfg, plan, lg)
		},
		RunGateG2: func(worktreePath string, timeoutMs int) (G2G4Result, error) {
			r := gates.RunMigrationApplyGate(gates.SpawnRunner{}, worktreePath, timeoutMs)
			return G2G4Result{Passed: r.Passed, Skipped: r.Skipped, Detail: r.Detail}, nil
		},
		RunGateG1: func(worktreePath string, timeoutMs int) (G2G4Result, error) {
			r, err := gates.RunTestGate(gates.SpawnRunner{}, worktreePath, timeoutMs)
			if err != nil {
				return G2G4Result{}, err
			}
			return G2G4Result{Passed: r.Passed, Skipped: r.Skipped, Detail: r.Detail}, nil
		},
		RunGateG4: func(worktreePath string) (G2G4Result, error) {
			return impRunGateG4(worktreePath)
		},
		PublishStage: func(c RunConfig, plan ImplementationPlan, impl ImplementResult) (string, error) {
			return impPublishStage(creds, cfg, c, plan, impl, log)
		},
	}
}

// impRunGateG4 mirrors runGateG4 (src/deps.ts:82-97): collect the files the
// branch changed versus its merge-base with the base branch, run the async
// hazard analysis, and block on high-severity findings.
func impRunGateG4(worktreePath string) (G2G4Result, error) {
	repoCfg, err := config.Load(worktreePath)
	if err != nil {
		return G2G4Result{}, err
	}
	base := repoCfg.GithubBaseBranch
	runGit := func(args []string) (int, string) {
		r := spawn.RunCli("git", args, spawn.Options{Dir: worktreePath, TimeoutMs: 30000})
		return r.ExitCode, r.Stdout
	}
	files := impCollectChangedSourceFiles(worktreePath, base, runGit)
	findings := orchestrator.AnalyzeAsyncHazards(files)
	blocking := false
	for _, f := range findings {
		if f.Severity == "high" {
			blocking = true
			break
		}
	}
	detail := fmt.Sprintf("%d changed file(s), %d finding(s)", len(files), len(findings))
	if blocking {
		detail += " (blocking high-severity)"
	}
	return G2G4Result{Passed: !blocking, Detail: detail}, nil
}

// impSourceExtRe mirrors SOURCE_EXT (src/validation/async-review.ts).
var impSourceExtRe = regexp.MustCompile(`(?i)\.(ts|js|mts|mjs|tsx|jsx)$`)

// impCollectChangedSourceFiles mirrors collectChangedSourceFiles
// (src/validation/async-review.ts): source files changed on this branch vs
// its merge-base with the base branch. runGit is injectable for tests
// (production passes spawnCli-backed git).
func impCollectChangedSourceFiles(repoPath, baseBranch string, runGit func(args []string) (int, string)) []orchestrator.AsyncSourceFile {
	var files []orchestrator.AsyncSourceFile
	mbCode, mbOut := runGit([]string{"merge-base", baseBranch, "HEAD"})
	if mbCode != 0 {
		return files
	}
	diffCode, diffOut := runGit([]string{"diff", "--name-only", strings.TrimSpace(mbOut), "HEAD"})
	if diffCode != 0 {
		return files
	}
	for _, rel := range strings.Split(diffOut, "\n") {
		rel = strings.TrimSpace(rel)
		if rel == "" || !impSourceExtRe.MatchString(rel) {
			continue
		}
		content, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		files = append(files, orchestrator.AsyncSourceFile{Path: rel, Content: string(content)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

// impPublishStage mirrors the publishStage closure (src/deps.ts:122-202):
// commit the worker's leftovers, push the run branch, open the PR (or GitLab
// MR), and fire-and-forget the auto-merge watcher.
func impPublishStage(creds config.Credentials, cfg StageConfig, c RunConfig, plan ImplementationPlan, impl ImplementResult, log RunLog) (string, error) {
	// With cleanup=auto a successful implement already snapshotted and removed
	// its worktree; the run branch survives in the main repo, so publish there.
	wtAlive := impl.WorktreePath != "" && impPathExists(impl.WorktreePath)
	if impl.WorktreePath == "" {
		return "", nil
	}
	gitCwd := c.RepoPath
	if wtAlive {
		gitCwd = impl.WorktreePath
		title := plan.Ticket.Title
		if r := []rune(title); len(r) > 72 {
			title = string(r[:72])
		}
		if _, err := git.CommitAllChanges(impl.WorktreePath, fmt.Sprintf("devagent(%s): %s", plan.Ticket.ID, title)); err != nil {
			return "", err
		}
	}
	branch := fmt.Sprintf("devagent/%s", plan.Ticket.ID)
	repoCfg, err := config.Load(gitCwd)
	if err != nil {
		return "", err
	}
	baseBranch := repoCfg.GithubBaseBranch

	// GitLab path: GITLAB_* env wins when no GitHub credentials present.
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	gitlabProject := os.Getenv("GITLAB_PROJECT_ID")
	if gitlabToken != "" && gitlabProject != "" && creds.GithubToken == "" {
		if err := integrations.PushBranch(gitCwd, branch, integrations.GitHubOptions{}); err != nil {
			return "", err
		}
		baseURL := os.Getenv("GITLAB_BASE_URL")
		if baseURL == "" {
			baseURL = "https://gitlab.com"
		}
		return integrations.CreateMergeRequest(context.TODO(), integrations.GitlabCredentials{
			BaseURL:   baseURL,
			ProjectID: gitlabProject,
			Token:     gitlabToken,
		}, integrations.CreateMrOptions{
			SourceBranch: branch,
			TargetBranch: baseBranch,
			Title:        fmt.Sprintf("[%s] %s", plan.Ticket.ID, plan.Ticket.Title),
			Description:  impBuildPrBody(plan, nil),
		}, nil)
	}
	if creds.GithubToken == "" {
		return "", nil
	}

	// Divergence guard (Orca lesson): warn when base moved ahead of our fork
	// point — the PR will be flagged out-of-date upstream.
	probe := func(args ...string) spawn.Result {
		return spawn.RunCli("git", args, spawn.Options{Dir: gitCwd, TimeoutMs: 30000})
	}
	mb := probe("merge-base", "HEAD", "origin/"+baseBranch)
	ob := probe("rev-parse", "origin/"+baseBranch)
	if mb.ExitCode == 0 && ob.ExitCode == 0 && strings.TrimSpace(mb.Stdout) != strings.TrimSpace(ob.Stdout) {
		log.Warn("publish", fmt.Sprintf("%s has advanced since this run started; PR may be out-of-date", baseBranch), nil)
	}

	// The branch exists only locally until pushed (worktree or main repo).
	if err := integrations.PushBranch(gitCwd, branch, integrations.GitHubOptions{}); err != nil {
		return "", err
	}
	// Evidence is best-effort; never block publishing on it.
	changedFiles, err := git.ListChangedFiles(gitCwd, baseBranch, "")
	if err != nil {
		changedFiles = nil
	}
	prURL, err := integrations.CreatePr(integrations.CreatePrOptions{
		RepoPath: cfg.RepoPath,
		Branch:   branch,
		Title:    fmt.Sprintf("[%s] %s", plan.Ticket.ID, plan.Ticket.Title),
		Body:     impBuildPrBody(plan, changedFiles),
	}, integrations.GitHubOptions{})
	if err != nil {
		return "", err
	}
	merge := repoCfg.AutoMerge
	if cfg.AutoMerge != nil {
		merge = cfg.AutoMerge
	}
	if merge != nil && *merge {
		if m := impPrURLPattern.FindStringSubmatch(prURL); m != nil {
			if n, convErr := strconv.Atoi(m[1]); convErr == nil {
				// Fire-and-forget: auto-merge waits on CI and must not block
				// the run report.
				go func() {
					outcome := orchestrator.AutoReviewAndMergeOne(cfg.RepoPath, n, orchestrator.AutoReviewAndMergeOneOpts{
						AutoReviewAndMergeOptions: orchestrator.AutoReviewAndMergeOptions{BaseBranch: baseBranch},
					}, nil)
					detail := impRuneTruncate(outcome.Detail, 120)
					log.Info("publish", fmt.Sprintf("auto-merge PR #%d: %s (%s)", n, outcome.Action, detail), nil)
				}()
			}
		}
	}
	return prURL, nil
}

// impPrURLPattern extracts the PR number from a GitHub PR URL (auto-merge hook).
var impPrURLPattern = regexp.MustCompile(`pull/(\d+)/`)

// impPathExists mirrors existsSync.
func impPathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// impBuildPrBody mirrors buildPrBody (src/deps.ts:490): PR body with plan,
// changed-file evidence, and acceptance criteria (FR-DELIVER-01).
func impBuildPrBody(plan ImplementationPlan, changedFiles []string) string {
	t := plan.Ticket
	head := fmt.Sprintf("Closes %s", t.ID)
	if t.URL != "" {
		head += fmt.Sprintf(" (%s)", t.URL)
	}
	head += "."
	lines := []string{
		head,
		"",
		"## Summary",
		fmt.Sprintf("Automated implementation classified as **%s** by DevAgent.", plan.Classification),
		"",
		"## Plan",
	}
	for i, task := range plan.Tasks {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, task))
	}
	lines = append(lines, "")
	if len(changedFiles) > 0 {
		lines = append(lines, "## Files changed")
		for _, f := range changedFiles {
			lines = append(lines, fmt.Sprintf("- `%s`", f))
		}
		lines = append(lines, "")
	}
	lines = append(lines,
		"## Validation",
		"- G3 static migration analysis: passed",
		"- Test suite: see CI run on this branch",
		"",
		"## Acceptance criteria",
	)
	if len(t.AcceptanceCriteria) > 0 {
		for _, c := range t.AcceptanceCriteria {
			lines = append(lines, fmt.Sprintf("- [ ] %s", c))
		}
	} else {
		lines = append(lines, "- (see ticket)")
	}
	return strings.Join(lines, "\n")
}

// DropOrcaWorkspaceIfRequested ports dropEnclosingOrcaWorkspaceIfRequested
// (src/deps.ts:476): opt-in post-run disposal of the enclosing Orca
// workspace (--drop-orca-workspace). When repoPath is an Orca-managed
// worktree, remove card+dir through orca-cli so the app stays consistent.
// Best-effort; never fails the run.
func DropOrcaWorkspaceIfRequested(cfg StageConfig, log RunLog) {
	if !cfg.DropOrcaWorkspace {
		return
	}
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		id := integrations.FindOrcaWorktreeByPath(cfg.RepoPath, nil)
		if id == "" {
			return nil // not Orca-managed: nothing to drop
		}
		dropped := integrations.DropOrcaWorkspace(id, cfg.RepoPath, nil)
		if dropped {
			log.Info("implement", fmt.Sprintf("Orca workspace dropped: %s", id), nil)
		} else {
			log.Warn("implement", fmt.Sprintf("Failed to drop Orca workspace %s (kept)", id), nil)
		}
		return nil
	}()
	if err != nil {
		log.Warn("implement", fmt.Sprintf("Orca workspace drop skipped: %s", err.Error()), nil)
	}
}

// impToIntegrationTicketSpec converts the integrations.TicketSpec returned
// by the tracker fetchers to the pipeline TicketSpec (scout.TicketSpec).
func impToIntegrationTicketSpec(spec integrations.TicketSpec) TicketSpec {
	return TicketSpec{
		ID:                 spec.ID,
		Title:              spec.Title,
		Description:        spec.Description,
		Labels:             spec.Labels,
		AcceptanceCriteria: spec.AcceptanceCriteria,
		URL:                spec.URL,
		TrackerInternalID:  spec.TrackerInternalID,
	}
}
