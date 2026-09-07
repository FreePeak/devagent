// Remote execution transport: the Go port of src/remote.ts — prompt-driven
// task runs delegated to a shared host over SSH so worker capacity is pooled
// across repos instead of per-workspace. The local machine is a thin client:
// it verifies the host is usable, ships the prompt over an SSH command, and
// extracts the PR URL from the remote output.
//
// Scope note (deliberate): no repo mirroring/sync here — the shared host owns
// its checkout. Fan-out and orchestrated runs are not remote-dispatched yet.

package pipeline

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/FreePeak/devagent/internal/ledger"
)

// RemoteTarget mirrors remote.ts RemoteTarget.
type RemoteTarget struct {
	User string // "" = no user
	Host string
	// Path is the absolute repo path on the remote host.
	Path string
	Port int // 0 = unset
}

// ParseRemoteTarget mirrors parseRemoteTarget. Accepted forms:
//
//	user@host:/srv/repos/app
//	host:/srv/repos/app
//	ssh://user@host:2222/srv/repos/app
func ParseRemoteTarget(target string) (RemoteTarget, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return RemoteTarget{}, fmt.Errorf("empty remote target")
	}

	if strings.HasPrefix(trimmed, "ssh://") {
		rest := trimmed[len("ssh://"):]
		var port int
		slash := strings.IndexByte(rest, '/')
		if slash == -1 {
			return RemoteTarget{}, fmt.Errorf("remote target %q needs a repo path after the host", target)
		}
		authority := rest[:slash]
		if colon := strings.LastIndexByte(authority, ':'); colon != -1 {
			p, convErr := strconv.Atoi(authority[colon+1:])
			if convErr != nil || p <= 0 || p > 65535 {
				return RemoteTarget{}, fmt.Errorf("invalid ssh port in %q", target)
			}
			port = p
			authority = authority[:colon]
		}
		var user string
		if at := strings.IndexByte(authority, '@'); at != -1 {
			user = authority[:at]
			authority = authority[at+1:]
		}
		if authority == "" {
			return RemoteTarget{}, fmt.Errorf("missing host in %q", target)
		}
		return RemoteTarget{User: user, Host: authority, Path: rest[slash:], Port: port}, nil
	}

	colon := strings.IndexByte(trimmed, ':')
	if colon == -1 || colon == 0 {
		return RemoteTarget{}, fmt.Errorf("remote target %q expected [user@]host:/path/to/repo", target)
	}
	authority := trimmed[:colon]
	path := trimmed[colon+1:]
	if !strings.HasPrefix(path, "/") {
		return RemoteTarget{}, fmt.Errorf("remote path must be absolute in %q", target)
	}
	var user string
	host := authority
	if at := strings.IndexByte(authority, '@'); at != -1 {
		user = authority[:at]
		host = authority[at+1:]
	}
	if host == "" {
		return RemoteTarget{}, fmt.Errorf("missing host in %q", target)
	}
	return RemoteTarget{User: user, Host: host, Path: path}, nil
}

// ShellQuote mirrors shellQuote: single-quote a fragment for POSIX shells
// (the safe default for ssh).
func ShellQuote(fragment string) string {
	return "'" + strings.ReplaceAll(fragment, "'", `'\''`) + "'"
}

// BuildSshArgs mirrors buildSshArgs: the argv for an ssh invocation running
// one shell command remotely.
func BuildSshArgs(target RemoteTarget, remoteCmd string) []string {
	args := []string{"ssh", "-o", "BatchMode=yes"}
	if target.Port != 0 {
		args = append(args, "-p", strconv.Itoa(target.Port))
	}
	endpoint := target.Host
	if target.User != "" {
		endpoint = target.User + "@" + target.Host
	}
	return append(args, endpoint, remoteCmd)
}

// RunRemoteTaskOptions mirrors remote.ts RunRemoteTaskOptions.
type RunRemoteTaskOptions struct {
	Target string
	Prompt string
	// TaskID is forwarded as `--id` so the remote side uses the
	// caller-chosen worktree/branch instead of its own default (loop 66).
	TaskID    string
	Worker    string
	TimeoutMs int
	// Log warn is consumed for the preflight failure; nil is tolerated.
	Log RunLog
}

// RemoteRunResult mirrors remote.ts RemoteRunResult { ok, prUrl?, note }.
type RemoteRunResult struct {
	OK    bool
	PRURL string // "" = undefined
	Note  string
}

// RemoteRunOutcome mirrors the TS runner's { exitCode, stdout } pair.
type RemoteRunOutcome struct {
	ExitCode int
	Stdout   string
}

// RemoteDeps mirrors remote.ts RemoteDeps: the injected runner so tests
// never touch a real network.
type RemoteDeps struct {
	Run func(argv []string, timeoutMs int) RemoteRunOutcome
}

var remotePRURLRe = regexp.MustCompile(`https://github\.com/[^\s)"']+/pull/\d+`)

// ExtractPrURL mirrors extractPrUrl: the first GitHub PR URL in stdout.
func ExtractPrURL(stdout string) string {
	return remotePRURLRe.FindString(stdout)
}

// RunRemoteTask mirrors runRemoteTask: delegate a `devagent task` run to a
// remote host — preflight (devagent installed on PATH and the target path is
// a git repo, loop-59 hang lesson: fail fast instead of burning the full
// timeout), then dispatch `devagent task --prompt <prompt> --auto-pr`. The
// remote side owns credentials and workers; we only report its outcome.
func RunRemoteTask(opts RunRemoteTaskOptions, deps RemoteDeps) RemoteRunResult {
	target, err := ParseRemoteTarget(opts.Target)
	if err != nil {
		return RemoteRunResult{OK: false, Note: err.Error()}
	}

	preflightCmd := "command -v devagent >/dev/null && test -d " + ShellQuote(target.Path) +
		" && git -C " + ShellQuote(target.Path) + " rev-parse --git-dir >/dev/null"
	preflightTimeout := opts.TimeoutMs
	if preflightTimeout > 15000 {
		preflightTimeout = 15000
	}
	preflight := deps.Run(BuildSshArgs(target, preflightCmd), preflightTimeout)
	if preflight.ExitCode != 0 {
		if opts.Log != nil {
			opts.Log.Warn(ledger.StageTask, fmt.Sprintf("remote preflight failed on %s", target.Host), []ledger.KV{
				{Key: "exitCode", Value: preflight.ExitCode},
			})
		}
		return RemoteRunResult{
			OK:   false,
			Note: fmt.Sprintf("remote preflight failed on %s: need devagent on PATH and a git repo at %s", target.Host, target.Path),
		}
	}

	parts := []string{
		"cd " + ShellQuote(target.Path),
		"devagent task " + ShellQuote(opts.Prompt) + " --auto-pr",
	}
	if opts.TaskID != "" {
		parts = append(parts, "--id "+ShellQuote(opts.TaskID))
	}
	if opts.Worker != "" {
		parts = append(parts, "--worker "+ShellQuote(opts.Worker))
	}
	dispatch := deps.Run(BuildSshArgs(target, strings.Join(parts, " && ")), opts.TimeoutMs)
	prURL := ExtractPrURL(dispatch.Stdout)
	if dispatch.ExitCode != 0 {
		note := fmt.Sprintf("remote task failed on %s (exit %d)", target.Host, dispatch.ExitCode)
		if prURL != "" {
			note += fmt.Sprintf(", PR opened anyway: %s", prURL)
		}
		return RemoteRunResult{OK: false, PRURL: prURL, Note: note}
	}
	if prURL == "" {
		return RemoteRunResult{OK: true, Note: "remote task finished without a PR URL"}
	}
	return RemoteRunResult{OK: true, PRURL: prURL, Note: "remote PR opened: " + prURL}
}
