package commands

// FR-VAL-02 (issue #290): devagent doctor — one-command machine validation
// with human + --json output. Doctor is strictly read-only: it probes and
// reports, never records ledger rows, opens circuits, or pages operators
// (that side-effect surface belongs to the loop's preflight gate). The
// version check is warn-only — an unstamped local build or a release newer
// than the binary must not fail a healthy box (the issue flags them as
// cosmetic), so exit 1 is reserved for genuinely broken dependencies.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/herdr"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/resilience"
	"github.com/FreePeak/devagent/internal/spawn"
	"github.com/FreePeak/devagent/internal/tui"
	"github.com/FreePeak/devagent/internal/version"
)

// doctorProbeTimeoutMs: doctor probes are bounded one-shots (not the loop's
// 3-attempt gate); 30s clears the slowest observed gateway round-trip the
// preflight gate documents (init uses the same cap).
const doctorProbeTimeoutMs = 30_000

// DoctorCheck: one doctor result row (PASS / FAIL, plus warn = pass with a
// cosmetic or self-healing note). JSON shape feeds FR-SIMPLE-01's checklist
// and the Tauri setup screen.
type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// DoctorResult is the doctor output envelope (--json prints this verbatim).
type DoctorResult struct {
	OK       bool          `json:"ok"`
	RepoPath string        `json:"repoPath"`
	Checks   []DoctorCheck `json:"checks"`
}

// GhTokenInfo reports the resolved GitHub token and where it came from,
// without ever carrying the value out of the package (details quote the
// source, never the token).
type GhTokenInfo struct {
	Token  string
	Source string // "env" | "keyring" | "" (nothing resolvable)
}

// daemonState is the verdict the /status probe pre-decides, so the check
// switch never sniffs prose.
type daemonState string

const (
	daemonDown         daemonState = "down"         // nothing listening: informational pass
	daemonOK           daemonState = "ok"           // /status 200 with the bearer token
	daemonUnhealthy    daemonState = "unhealthy"    // listening but wrong/erroneous answer
	daemonUnauthorized daemonState = "unauthorized" // 401: token unreadable/mismatched
)

// DoctorOptions carries the injection seams for hermetic fault fixtures;
// nil seams run the real (bounded) implementations.
type DoctorOptions struct {
	RepoPath string
	// LatestReleaseTag resolves the newest release tag for the repo
	// (check 1). Nil = `gh release list --limit 1`.
	LatestReleaseTag func() (string, error)
	// GhToken resolves the effective GitHub token + source (check 5).
	// Nil = env GITHUB_TOKEN, else `gh auth token`.
	GhToken func() GhTokenInfo
	// GhTokenValid probes token validity (check 5). Nil = `gh api user`.
	GhTokenValid func(token string) (bool, string)
	// GitRemoteProbe checks remote reachability (check 4). Nil = ssh
	// BatchMode probe for ssh remotes, bounded git ls-remote otherwise.
	GitRemoteProbe func(repoPath string) (bool, string)
	// HerdrList runs `herdr --session <s> agent list` (check 6).
	// Nil = herdr.ExecRunner.
	HerdrList func() herdr.CliResult
	// DaemonStatus probes the daemon /status endpoint (check 7); returns
	// (state, detail). Nil = authenticated GET /status with the persisted
	// daemon-token.
	DaemonStatus func() (daemonState, string)
	// Probe is the provider probe seam (check 8). Nil = the loop's bare
	// RunPreflightProbe (single bounded attempt — NOT the gate: doctor must
	// not record circuits, ledger rows, or pages).
	Probe func(cmd string, args []string, dir string) resilience.Probe
	// ProcessAlive is the pid-liveness seam (check 9). Nil = signal 0 probe
	// on unix, loopdriver's off-unix degradation elsewhere.
	ProcessAlive func(pid int) bool
	// Supervision resolves the loop's supervision mode (check 10, issue
	// #321). Nil = DetectSupervision over the user's home (read-only unit
	// scan: no launchctl / systemctl calls).
	Supervision func() SupervisionMode
}

// doctorHome mirrors the daemon's devagentHome: DEVAGENT_HOME wins, else
// $HOME/.devagent.
func doctorHome() string {
	if home := os.Getenv("DEVAGENT_HOME"); home != "" {
		return home
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "."
	}
	return filepath.Join(home, ".devagent")
}

// defaultGhToken is the source-aware token resolution: env GITHUB_TOKEN wins
// over the keyring (so an invalid env token is doctor-visible, not masked by
// a healthy `gh auth token` behind it — the 2026-09-09 incident). No
// sync.Once caching: doctor re-resolves so a re-run reflects fixes.
func defaultGhToken() GhTokenInfo {
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		return GhTokenInfo{Token: tok, Source: "env"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return GhTokenInfo{}
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return GhTokenInfo{}
	}
	return GhTokenInfo{Token: tok, Source: "keyring"}
}

// defaultGhTokenValid probes token validity with a bounded `gh api user`.
func defaultGhTokenValid(token string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api", "user", "-H", "Authorization: token "+token)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, ""
	}
	detail := strings.TrimSpace(string(out))
	if len(detail) > 200 {
		detail = detail[:200]
	}
	if detail == "" {
		detail = "gh api user failed"
	}
	return false, detail
}

// defaultGitRemoteProbe: ssh remotes get the bounded BatchMode probe the
// driver's network ops use (BoundedGitSSHCommand semantics: BatchMode fails
// fast instead of prompting, ConnectTimeout=10); https remotes get a bounded
// ls-remote. GitHub's `ssh -T` answers success with exit 1 plus the
// "successfully authenticated" banner, so the banner counts as reachable.
func defaultGitRemoteProbe(repoPath string) (bool, string) {
	url, err := exec.Command("git", "-C", repoPath, "remote", "get-url", "origin").Output()
	if err != nil {
		return false, "no origin remote (git remote add origin …)"
	}
	target := strings.TrimSpace(string(url))
	if user, host, ok := parseSshRemote(target); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=accept-new", user+"@"+host)
		out, _ := cmd.CombinedOutput()
		if err == nil || strings.Contains(string(out), "successfully authenticated") {
			return true, "ssh " + user + "@" + host + " reachable"
		}
		return false, "ssh " + user + "@" + host + " unreachable (BatchMode)"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "ls-remote", "--heads", "origin")
	if err := cmd.Run(); err != nil {
		return false, "git ls-remote origin failed"
	}
	return true, "origin reachable"
}

// parseSshRemote extracts user@host from `git@host:path` or
// `ssh://user@host[:port]/path` remotes; ok=false for everything else.
func parseSshRemote(url string) (user, host string, ok bool) {
	if i := strings.Index(url, "@"); i > 0 && !strings.Contains(url[:i], "://") {
		rest := url[i+1:]
		if j := strings.IndexAny(rest, ":/"); j > 0 {
			return url[:i], rest[:j], true
		}
		return "", "", false
	}
	if strings.HasPrefix(url, "ssh://") {
		rest := strings.TrimPrefix(url, "ssh://")
		user = "git"
		if i := strings.Index(rest, "@"); i >= 0 {
			user, rest = rest[:i], rest[i+1:]
		}
		if j := strings.IndexAny(rest, ":/"); j > 0 {
			return user, rest[:j], true
		}
	}
	return "", "", false
}

// defaultHerdrList runs the herdr agent-list RPC the roster uses.
func defaultHerdrList() herdr.CliResult {
	return (herdr.ExecRunner{}).HerdrCli([]string{"--session", herdr.ResolveSession(""), "agent", "list"}, 10_000)
}

// defaultDaemonStatus probes the daemon /status endpoint with the persisted
// bearer token (resolveToken persists it at $DEVAGENT_HOME/daemon-token;
// DEVAGENT_DAEMON_TOKEN overrides). Every error is classified:
// ECONNREFUSED = down (informational pass — daemons start on demand);
// 200 = ok; 401 = unauthorized (a distinct failure — the doctor's token is
// unreadable/mismatched, never generic "unhealthy"); a timeout, reset, or
// any other transport error or HTTP status = unhealthy (a wedged daemon
// must not silently pass — the incident class #290 targets).
func defaultDaemonStatus() (daemonState, string) {
	token := os.Getenv("DEVAGENT_DAEMON_TOKEN")
	if token == "" {
		if data, err := os.ReadFile(filepath.Join(doctorHome(), "daemon-token")); err == nil {
			token = strings.TrimSpace(string(data))
		}
	}
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:7788/status", nil)
	if err != nil {
		return daemonUnhealthy, "daemon /status request could not be built: " + err.Error()
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if errorsIsConnRefused(err) {
			return daemonDown, "daemon not running on :7788 (informational; start with `devagent daemon`)"
		}
		return daemonUnhealthy, "daemon on :7788 did not answer /status: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return daemonOK, "daemon /status healthy (authenticated)"
	case http.StatusUnauthorized:
		return daemonUnauthorized, "daemon running on :7788 but the token was rejected (401) — daemon-token unreadable or mismatched"
	default:
		return daemonUnhealthy, fmt.Sprintf("daemon /status answered HTTP %d", resp.StatusCode)
	}
}

// errorsIsConnRefused unwraps *net.OpError down to ECONNREFUSED.
func errorsIsConnRefused(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Err == syscall.ECONNREFUSED
}

// defaultLatestReleaseTag resolves the newest release tag via gh (bounded).
func defaultLatestReleaseTag(repoPath string) (string, error) {
	r := spawn.RunCli("gh", []string{"release", "list", "--limit", "1", "--json", "tagName"}, spawn.Options{Dir: repoPath, TimeoutMs: 10_000})
	if r.ExitCode != 0 {
		return "", fmt.Errorf("gh release list failed: %s", strings.TrimSpace(firstNonEmpty(r.Stderr, r.Stdout)))
	}
	var tags []struct {
		TagName string `json:"tagName"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &tags); err != nil || len(tags) == 0 {
		return "", fmt.Errorf("no releases found")
	}
	return tags[0].TagName, nil
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// RunDoctor executes the nine FR-VAL-02 checks and aggregates the verdict:
// OK is false only when a check actually failed (warn rows never fail).
func RunDoctor(opts DoctorOptions) (DoctorResult, error) {
	repoPath := opts.RepoPath
	result := DoctorResult{RepoPath: repoPath, OK: true, Checks: []DoctorCheck{}}

	// 1. Version stamp — cosmetic, warn-only (advisory + issue: unstamped
	// local builds are flagged, not failed). One row total: unstamped wins
	// over the release comparison, which is meaningless without a stamp.
	{
		ver := version.Version
		if ver == "0.1.0" { // the unstamped build default (version.go)
			result.Checks = append(result.Checks, DoctorCheck{
				Name: "version", OK: true, Warn: true,
				Detail: "local build unstamped (--version reports the default 0.1.0)",
				Hint:   "rebuild with the release stamp (make build / scripts/self-update.sh)",
			})
		} else {
			latest := opts.LatestReleaseTag
			if latest == nil {
				latest = func() (string, error) { return defaultLatestReleaseTag(repoPath) }
			}
			tag, err := latest()
			switch {
			case err != nil:
				result.Checks = append(result.Checks, DoctorCheck{
					Name: "version", OK: true, Warn: true,
					Detail: "could not fetch latest release tag: " + err.Error(),
				})
			case tag == "v"+ver || tag == ver:
				result.Checks = append(result.Checks, DoctorCheck{
					Name: "version", OK: true, Detail: "binary " + ver + " matches latest release " + tag,
				})
			default:
				result.Checks = append(result.Checks, DoctorCheck{
					Name: "version", OK: true, Warn: true,
					Detail: "binary " + ver + " is older than latest release " + tag,
					Hint:   "run scripts/self-update.sh",
				})
			}
		}
	}

	// 2. devagent.json parses; required keys valid.
	{
		cfgPath := ""
		for _, name := range config.CONFIG_FILENAMES {
			if _, err := os.Stat(filepath.Join(repoPath, name)); err == nil {
				cfgPath = filepath.Join(repoPath, name)
				break
			}
		}
		if cfgPath == "" {
			result.Checks = append(result.Checks, DoctorCheck{
				Name: "config", Detail: "no devagent.json in " + repoPath,
				Hint: "run `devagent init` to write one with sane defaults",
			})
		} else if _, err := config.Load(repoPath); err != nil {
			result.Checks = append(result.Checks, DoctorCheck{
				Name: "config", Detail: cfgPath + ": " + err.Error(),
				Hint: "fix the reported key in devagent.json (or re-run `devagent init` to rewrite with sane defaults)",
			})
		} else {
			result.Checks = append(result.Checks, DoctorCheck{
				Name: "config", OK: true, Detail: cfgPath + " parses and validates",
			})
		}
	}

	// 3. DEVAGENT_HOME writable; locks/ and runs/ present.
	{
		home := doctorHome()
		check := DoctorCheck{Name: "home", Detail: home + " writable; locks/ and runs/ present"}
		writable := true
		if err := os.MkdirAll(home, 0o755); err != nil {
			writable = false
			check.Detail = home + " not creatable: " + err.Error()
		} else {
			probe := filepath.Join(home, ".doctor-write-probe")
			if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
				writable = false
				check.Detail = home + " not writable: " + err.Error()
			} else {
				_ = os.Remove(probe)
			}
		}
		if writable {
			var missing []string
			for _, dir := range []string{"locks", "runs"} {
				if _, err := os.Stat(filepath.Join(home, dir)); err != nil {
					missing = append(missing, dir+"/")
				}
			}
			if len(missing) > 0 {
				check.Hint = "mkdir -p " + filepath.Join(home, "locks") + " " + filepath.Join(home, "runs") + " (also created by the first run)"
				check.Detail = home + " writable; missing " + strings.Join(missing, ", ")
			} else {
				check.OK = true
			}
		} else {
			check.Hint = "set DEVAGENT_HOME to a writable directory"
		}
		result.Checks = append(result.Checks, check)
	}

	// 4. Git remote reachable.
	{
		probe := opts.GitRemoteProbe
		if probe == nil {
			probe = defaultGitRemoteProbe
		}
		ok, detail := probe(repoPath)
		check := DoctorCheck{Name: "git-remote", Detail: detail}
		if !ok {
			check.Hint = "check network/VPN and `git remote -v`; ssh remotes must authenticate in BatchMode (add the key to ssh-agent)"
		} else {
			check.OK = true
		}
		result.Checks = append(result.Checks, check)
	}

	// 5. gh auth: report token source and validity — env wins over keyring,
	// so an invalid env token is flagged, not masked.
	{
		resolve := opts.GhToken
		if resolve == nil {
			resolve = defaultGhToken
		}
		valid := opts.GhTokenValid
		if valid == nil {
			valid = defaultGhTokenValid
		}
		info := resolve()
		check := DoctorCheck{Name: "gh-auth"}
		switch info.Source {
		case "":
			check.Detail = "no GitHub token resolved"
			check.Hint = "run `gh auth login` (or export GITHUB_TOKEN)"
		default:
			ok, reason := valid(info.Token)
			check.Detail = "token source: " + info.Source
			if !ok {
				check.Detail += " — INVALID (" + reason + ")"
				if info.Source == "env" {
					check.Hint = "unset or fix GITHUB_TOKEN — an invalid env token shadows a valid keyring login (2026-09-09 incident)"
				} else {
					check.Hint = "run `gh auth login` to refresh the keyring token"
				}
			} else {
				check.OK = true
				check.Detail += " — valid"
			}
		}
		result.Checks = append(result.Checks, check)
	}

	// 6. herdr binary + session agent list.
	{
		list := opts.HerdrList
		if list == nil {
			list = defaultHerdrList
		}
		check := DoctorCheck{Name: "herdr"}
		if _, err := exec.LookPath(herdr.HerdrBin()); err != nil {
			check.Detail = "herdr binary " + herdr.HerdrBin() + " not on PATH"
			check.Hint = "install herdr (or point DEVAGENT_HERDR_BIN at it)"
		} else {
			r := list()
			if r.Code != 0 {
				check.Detail = "herdr --session " + herdr.ResolveSession("") + " agent list failed (exit " + strconv.Itoa(r.Code) + ")"
				check.Hint = "create/start the herdr session (herdr session create --session " + herdr.ResolveSession("") + ") or check DEVAGENT_HERDR_BIN"
			} else {
				check.OK = true
				check.Detail = "herdr session " + herdr.ResolveSession("") + " reachable"
			}
		}
		result.Checks = append(result.Checks, check)
	}

	// 7. Daemon :7788 /status reachable when expected. Down is an
	// informational pass (daemons start on demand); unauthorized and
	// unhealthy both fail.
	{
		status := opts.DaemonStatus
		if status == nil {
			status = defaultDaemonStatus
		}
		state, detail := status()
		check := DoctorCheck{Name: "daemon", Detail: detail}
		switch state {
		case daemonDown, daemonOK:
			check.OK = true
		case daemonUnauthorized:
			check.Hint = "check " + filepath.Join(doctorHome(), "daemon-token") + " / DEVAGENT_DAEMON_TOKEN, or restart the daemon (devagent daemon)"
		default: // daemonUnhealthy
			check.Hint = "restart the daemon (devagent daemon)"
		}
		result.Checks = append(result.Checks, check)
	}

	// 8. Provider preflight probe — the loop's bare probe, single bounded
	// attempt, no gate side effects (doctor is read-only).
	{
		worker := ""
		model := ""
		if cfg, err := config.Load(repoPath); err == nil {
			worker = cfg.Worker
			model = cfg.Model
		}
		check := DoctorCheck{Name: "provider", OK: true}
		if worker != "omp" && worker != "grok" {
			check.Detail = "skipped: no probe shape for worker " + worker
		} else {
			argv := BuildProbeArgvFor(worker, model)
			probeArgs := append([]string{argv[1], "OK"}, argv[2:]...)
			probe := opts.Probe
			if probe == nil {
				probe = func(cmd string, args []string, dir string) resilience.Probe {
					return resilience.RunPreflightProbe(cmd, args, dir, doctorProbeTimeoutMs)
				}
			}
			r := probe(argv[0], probeArgs, repoPath)
			check.OK = r.OK
			if r.OK {
				check.Detail = "provider answered via " + worker
			} else {
				check.Detail = "provider " + worker + " did not answer: " + r.Detail
				check.Hint = "check the provider login for " + worker + " (see `devagent init` guidance)"
			}
		}
		result.Checks = append(result.Checks, check)
	}

	// 9. Stale artifacts. There are exactly two persisted pid registries:
	// <home>/locks/<ticket>.lock ({"pid","startedAt","generation"},
	// ledger/runregistry.go) and <repo>/.selfbuild/loop.lock.d/pid (loopdriver/lock.go) — the
	// daemon keeps no pidfile. TTL-expired run locks self-heal (TryAcquireRun
	// breaks them), so they warn; a dead holder pid or a wedged/crashed
	// loop.lock.d fails. ponytail: bare kill-0 liveness can false-positive
	// on a recycled pid — the TTL cross-check bounds that; upgrade path is
	// a start-time pid identity check.
	{
		check := DoctorCheck{Name: "artifacts", OK: true}
		var failFindings, warnFindings, failHints, warnHints []string
		alive := opts.ProcessAlive
		if alive == nil {
			alive = processAlive
		}
		now := time.Now()
		lockDir := filepath.Join(doctorHome(), "locks")
		if entries, err := os.ReadDir(lockDir); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
					continue
				}
				path := filepath.Join(lockDir, e.Name())
				data, _ := os.ReadFile(path)
				var holder struct {
					Pid       *int   `json:"pid"`
					StartedAt *int64 `json:"startedAt"`
				}
				_ = json.Unmarshal(data, &holder)
				switch {
				case holder.StartedAt != nil && now.Sub(time.UnixMilli(*holder.StartedAt)) > time.Duration(ledger.DefaultLockTTL)*time.Millisecond:
					warnFindings = append(warnFindings, path+" (older than TTL — self-heals on the next acquire)")
					warnHints = append(warnHints, "rm "+path)
				case holder.Pid != nil && !alive(*holder.Pid):
					failFindings = append(failFindings, fmt.Sprintf("%s (holder pid %d is gone)", path, *holder.Pid))
					failHints = append(failHints, "rm "+path)
				}
			}
		}
		// Driver lock: a dir whose pid is dead (or missing) is a
		// wedged/crashed driver leftover blocking the next run.
		lockD := filepath.Join(repoPath, ".selfbuild", "loop.lock.d")
		if _, err := os.Stat(lockD); err == nil {
			pidData, _ := os.ReadFile(filepath.Join(lockD, "pid"))
			pidStr := strings.TrimSpace(string(pidData))
			if pid, err := strconv.Atoi(pidStr); err == nil && alive(pid) {
				check.Detail = "active driver pid " + pidStr + "; "
			} else {
				failFindings = append(failFindings, lockD+" (pid "+pidStr+" is gone or unreadable)")
				failHints = append(failHints, "rm -rf "+lockD)
			}
		}
		if len(warnFindings) > 0 {
			check.Warn = true
			check.Detail += strings.Join(warnFindings, "; ") + "; "
			check.Hint = strings.Join(uniqueStrings(warnHints), "; ")
		}
		if len(failFindings) > 0 {
			check.OK = false
			check.Detail += strings.Join(failFindings, "; ")
			check.Hint = strings.Join(uniqueStrings(append(failHints, warnHints...)), "; ")
		} else {
			check.Detail += "no stale locks"
		}
		result.Checks = append(result.Checks, check)
	}

	// 10. Loop supervision mode (issue #321). Nothing in-tree supervises
	// the loop (make loop-start is plain nohup); the exposure is external
	// supervisor config — a restart=always / KeepAlive=true unit turned the
	// 2026-09-10 intentional exit-0 halt into 133 hollow overnight
	// restarts. Warn on restart-on-success units (naming the unit path);
	// on-failure and unsupervised both pass. Read-only: unit files are
	// scanned, never launchctl/systemctl.
	{
		probe := opts.Supervision
		if probe == nil {
			probe = func() SupervisionMode {
				home, _ := os.UserHomeDir()
				return DetectSupervision(home)
			}
		}
		m := probe()
		check := DoctorCheck{Name: "supervision"}
		switch {
		case m.Unit == "":
			check.OK = true
			check.Detail = firstNonEmpty(m.Detail, "unsupervised (nohup) — intentional halts leave the loop stopped")
		case m.Policy == "on-failure":
			check.OK = true
			check.Detail = m.Source + " unit " + m.Unit + " restarts on failure only — intentional exit-0 halts stay stopped"
		case m.Policy == "none":
			check.OK = true
			check.Detail = m.Source + " unit " + m.Unit + " has no restart policy — intentional halts stay stopped"
		default: // always (or an unrecognized restart-on-success shape)
			check.OK = true
			check.Warn = true
			check.Detail = m.Source + " unit " + m.Unit + " restarts on success — every intentional exit-0 halt becomes a hollow restart (the 2026-09-10 incident: 133 overnight restarts, zero work)"
			check.Hint = "switch the unit to restart=on-failure / KeepAlive {SuccessfulExit=false} (template: launchagents/com.devagent.selfbuild-loop.plist.template — opt-in, not installed by agents-install)"
		}
		result.Checks = append(result.Checks, check)
	}

	for _, c := range result.Checks {
		if !c.OK {
			result.OK = false
			break
		}
	}
	return result, nil
}

// uniqueStrings preserves order while dropping duplicates.
func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// RenderDoctorReport prints the human checklist: one PASS/FAIL row per check
// plus the remediation hint under every failure.
func RenderDoctorReport(r DoctorResult, render func(string)) {
	render("devagent doctor — " + r.RepoPath)
	render("")
	for _, c := range r.Checks {
		state := map[bool]string{true: "ok", false: "failed"}[c.OK]
		if c.Warn {
			state = "warn"
		}
		render("  " + tui.StatusGlyph(state) + " " + tui.Bold + c.Name + tui.Reset + "  " + tui.DimText(c.Detail))
		if !c.OK && c.Hint != "" {
			render("      " + tui.WarnText("fix: ") + tui.DimText(c.Hint))
		}
	}
	render("")
	if r.OK {
		render(tui.SuccessText("All checks passed."))
	} else {
		render(tui.FailText("doctor: one or more checks failed — fix the hints above, then re-run."))
	}
}
