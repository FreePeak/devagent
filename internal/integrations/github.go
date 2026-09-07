// Package integrations is the Go port of src/integrations/* and the
// transport-agnostic core of src/server/webhook.ts (FR-GO-09): the Linear
// GraphQL thin client, the Jira REST v3 adapter, the GitHub gh/git
// publisher (hardened spawn env so publish stages never die with "spawn
// git ENOENT" under launchd/scrubbed contexts; a stderr rate-limit signal
// retries once after a fixed 60s pause), the GitLab REST publisher, the
// GitHub Issues adapter, the Orca workspace helpers, and the HMAC-SHA256
// webhook receiver with delivery-ID dedup.
//
// Byte-parity contract (PRD §22): error strings, JSON field names, and
// request payload shapes match the TypeScript originals exactly. All HTTP
// is seamed through the Doer interface so tests inject recorded responses
// — zero network in tests.
package integrations

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/spawn"
)

// GitHubRunner is the CLI seam; production runs real binaries through
// internal/spawn.RunCli (hardened env), tests inject fakes — the Go
// equivalent of the TS execFile mock.
type GitHubRunner func(cmd string, args []string, opts spawn.Options) spawn.Result

// GitHubOptions carries the test seams for the gh/git publisher.
type GitHubOptions struct {
	// Runner replaces the spawn-based CLI runner (tests inject fakes);
	// nil = default.
	Runner GitHubRunner
	// Sleep replaces the 60s rate-limit wait (tests record delays); nil =
	// wall-clock sleep.
	Sleep func(ms int)
}

func (o GitHubOptions) runner() GitHubRunner {
	if o.Runner != nil {
		return o.Runner
	}
	return func(cmd string, args []string, opts spawn.Options) spawn.Result {
		return spawn.RunCli(cmd, args, opts)
	}
}

// CliError carries spawn output on failure so callers can re-describe the
// error with their own context (mirrors the TS err.stdout/err.stderr
// attachment on the run() helper).
type CliError struct {
	Message string
	Stdout  string
	Stderr  string
}

func (e *CliError) Error() string { return e.Message }

// runCli mirrors the private run(): `<cmd> <args[0]> failed:
// <stderr.trim() || exit N>` on non-zero exit.
func runCli(runner GitHubRunner, cmd string, args []string, cwd string) (string, string, error) {
	r := runner(cmd, args, spawn.Options{Dir: cwd, TimeoutMs: 120000})
	if r.ExitCode != 0 {
		detail := strings.TrimSpace(r.Stderr)
		if detail == "" {
			detail = fmt.Sprintf("exit %d", r.ExitCode)
		}
		return r.Stdout, r.Stderr, &CliError{
			Message: fmt.Sprintf("%s %s failed: %s", cmd, args[0], detail),
			Stdout:  r.Stdout,
			Stderr:  r.Stderr,
		}
	}
	return r.Stdout, r.Stderr, nil
}

// describeError mirrors the TS helper: `<context>: <stderr>` when stderr
// trims non-empty, `<context>` otherwise.
func describeError(context string, stderr string) error {
	if trimmed := strings.TrimSpace(stderr); trimmed != "" {
		return fmt.Errorf("%s: %s", context, trimmed)
	}
	return fmt.Errorf("%s", context)
}

// rateLimitPattern matches GitHub's stderr rate-limit signals.
var rateLimitPattern = regexp.MustCompile(`(?i)rate limit|secondary rate|abuse detection`)

// WithRateLimitRetry mirrors withRateLimitRetry: retry once after a pause
// when GitHub signals a (secondary) rate limit. gh/git expose limits as
// stderr text rather than headers, so the wait is fixed at 60s — long
// enough for the standard secondary window. Non-rate-limit errors and a
// second failure propagate unchanged.
func WithRateLimitRetry[T any](fn func() (T, error), sleep func(ms int)) (T, error) {
	out, err := fn()
	if err == nil {
		return out, nil
	}
	if !rateLimitPattern.MatchString(err.Error()) {
		var zero T
		return zero, err
	}
	if sleep != nil {
		sleep(60000)
	} else {
		time.Sleep(60 * time.Second)
	}
	return fn()
}

// PushBranch mirrors pushBranch: push a branch to origin so
// `gh pr create -H` can reference it. Uses an explicit refspec so
// local-only branches publish cleanly.
func PushBranch(repoPath, branch string, gh GitHubOptions) error {
	_, err := WithRateLimitRetry(func() (struct{}, error) {
		r := gh.runner()("git", []string{"push", "-u", "origin", branch + ":" + branch},
			spawn.Options{Dir: repoPath, TimeoutMs: 120000})
		if r.ExitCode != 0 {
			stderr := r.Stderr
			if stderr == "" {
				stderr = fmt.Sprintf("exit %d", r.ExitCode)
			}
			return struct{}{}, describeError(fmt.Sprintf("git push %s failed", branch), stderr)
		}
		return struct{}{}, nil
	}, gh.Sleep)
	return err
}

// CreatePrOptions mirrors CreatePrOptions.
type CreatePrOptions struct {
	// RepoPath is an absolute or relative path to the git repository.
	RepoPath string
	Branch   string
	Title    string
	Body     string
	// BaseBranch opens the PR against this base; empty = gh's default.
	BaseBranch string
}

// CreatePr mirrors createPr: `gh pr create -t <title> -b <body>
// [-B <base>] -H <branch>` inside the repository, resolving the PR URL
// parsed from stdout (the last non-empty line).
func CreatePr(opts CreatePrOptions, gh GitHubOptions) (string, error) {
	args := []string{"pr", "create", "-t", opts.Title, "-b", opts.Body}
	if opts.BaseBranch != "" {
		args = append(args, "-B", opts.BaseBranch)
	}
	args = append(args, "-H", opts.Branch)

	type runResult struct{ stdout, stderr string }
	res, err := WithRateLimitRetry(func() (runResult, error) {
		stdout, stderr, err := runCli(gh.runner(), "gh", args, opts.RepoPath)
		return runResult{stdout, stderr}, err
	}, gh.Sleep)
	if err != nil {
		// Mirrors the TS catch: run failures are re-described with the
		// branch context plus the stderr (`e.stderr ?? e.message` — the
		// empty string wins over the message, describeError drops empties).
		var ce *CliError
		if errors.As(err, &ce) {
			return "", describeError(fmt.Sprintf("gh pr create failed for branch %q", opts.Branch), ce.Stderr)
		}
		return "", err
	}

	// `gh pr create` prints the PR URL as the last non-empty stdout line.
	url := lastNonEmptyLine(res.stdout)
	if url == "" {
		if trimmed := strings.TrimSpace(res.stderr); trimmed != "" {
			return "", fmt.Errorf("gh pr create produced no PR URL (stderr: %s)", trimmed)
		}
		return "", fmt.Errorf("gh pr create produced no PR URL")
	}
	return url, nil
}

// lastNonEmptyLine mirrors the TS stdout.split(/\r?\n/).map(trim)
// .filter(non-empty).pop() URL extraction.
func lastNonEmptyLine(stdout string) string {
	lines := strings.Split(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if trimmed := strings.TrimSpace(lines[i]); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// BranchExists mirrors branchExists: `git rev-parse --verify
// refs/heads/<branch>`; false on any failure.
func BranchExists(repoPath, branch string, gh GitHubOptions) bool {
	_, _, err := runCli(gh.runner(), "git", []string{"rev-parse", "--verify", "refs/heads/" + branch}, repoPath)
	return err == nil
}

// AutoMergePr mirrors autoMergePr: `gh pr merge <ref> --auto
// <--squash|--merge|--rebase>`. Returns the trimmed gh output or the
// stderr-annotated error. Empty strategy defaults to squash.
func AutoMergePr(repoPath, prRef, strategy string, gh GitHubOptions) (string, error) {
	flag := "--squash"
	switch strategy {
	case "merge":
		flag = "--merge"
	case "rebase":
		flag = "--rebase"
	case "squash", "":
	}
	out, err := WithRateLimitRetry(func() (string, error) {
		stdout, _, err := runCli(gh.runner(), "gh", []string{"pr", "merge", prRef, "--auto", flag}, repoPath)
		return stdout, err
	}, gh.Sleep)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
