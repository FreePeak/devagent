package commands

// doctor.go implements FR-VAL-02 (issue #290): `devagent doctor` — one-command
// machine validation. It re-validates at any time what `devagent init`
// (FR-SIMPLE-01) checked at setup time, plus the runtime surfaces init cannot
// see (herdr session, daemon, stale artifacts). Every network/subprocess
// touchpoint is an injected seam so unit tests fault one dependency at a
// time; a healthy box exits 0, any failing check exits 1 with a remediation
// hint. `--json` (rendered by the CLI layer) feeds the TUI/Tauri setup
// screens.
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/herdr"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/resilience"
	"github.com/FreePeak/devagent/internal/version"
)

// doctorProbeTimeoutMs: one bounded provider probe at the loop's preflight
// cadence — omniroute/dev replies land at 25-38s (2026-09-03 live note on
// resilience.PreflightProbeTimeoutMs), so a 30s cap is a coin-flip.
const doctorProbeTimeoutMs = 60_000

// daemonPort is the control-plane default (daemon.Options.Port nil = 7788).
const daemonPort = 7788

// DoctorCheck: one doctor check row.
type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// DoctorResult is the full report (also the --json payload shape).
type DoctorResult struct {
	OK     bool          `json:"ok"`
	Checks []DoctorCheck `json:"checks"`
}

// DoctorOptions carries the running version and the injection seams. A nil
// seam means "production implementation".
type DoctorOptions struct {
	RepoPath string
	// Version is the running CLI version; "" resolves version.Version.
	Version string

	// LatestTag returns the newest release tag ("vX.Y.Z"); nil = git-backed
	// default (ls-remote origin, bounded; local tag-list fallback).
	LatestTag func() (string, error)
	// GitRemote proves the origin remote is reachable; nil = `git ls-remote
	// origin HEAD` with ssh BatchMode + bounded timeout.
	GitRemote func() error
	// GhToken resolves the effective GitHub token and its source; nil =
	// env GITHUB_TOKEN first (env wins — the 2026-09-09 incident class),
	// else `gh auth token`.
	GhToken func() (source string, token string)
	// TokenValid probes one token's validity; nil = GET api.github.com/user.
	TokenValid func(source, token string) (ok bool, detail string)
	// HerdrCli runs herdr CLI commands; nil = herdr.ExecRunner.
	HerdrCli herdr.CliRunner
	// DaemonProbe returns (HTTP status, error) for the daemon liveness
	// endpoint; nil = GET 127.0.0.1:<daemonPort>/healthz (the unauthenticated
	// probe — /status sits behind the bearer guard).
	DaemonProbe func() (int, error)
	// ProviderProbe is the provider preflight probe (init's ProbeFn shape);
	// nil = RunPreflightProbe with doctorProbeTimeoutMs.
	ProviderProbe ProbeFn
	// Now pins the clock for the stale-artifact TTL math; nil = wall clock.
	Now func() time.Time
}

// RunDoctor executes the nine checks in the issue's order. A config load
// failure fails the config check but never aborts the run — later checks
// degrade to the default config so the report stays complete.
func RunDoctor(opts DoctorOptions) DoctorResult {
	repo := opts.RepoPath
	if repo == "" {
		repo, _ = os.Getwd()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	cfg, cfgErr := config.Load(repo)

	ver := opts.Version
	if ver == "" {
		ver = version.Version
	}

	var checks []DoctorCheck
	add := func(c DoctorCheck) { checks = append(checks, c) }

	add(doctorVersionCheck(opts, ver))
	add(doctorConfigCheck(repo, cfg, cfgErr))
	add(doctorHomeCheck())
	add(doctorGitRemoteCheck(opts))
	add(doctorGhAuthCheck(opts))
	add(doctorHerdrCheck(opts, cfg))
	add(doctorDaemonCheck(opts))
	add(doctorProviderCheck(opts, repo, cfg))
	add(doctorArtifactsCheck(opts, repo, now))

	res := DoctorResult{OK: true, Checks: checks}
	for _, c := range checks {
		if !c.OK {
			res.OK = false
			break
		}
	}
	return res
}

// semverTagRe pins vMAJOR.MINOR.PATCH release tags (nextversion's shape).
var semverTagRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

func doctorVersionCheck(opts DoctorOptions, ver string) DoctorCheck {
	latest := opts.LatestTag
	if latest == nil {
		latest = func() (string, error) { return doctorLatestTag(opts.RepoPath) }
	}
	tag, err := latest()
	if err != nil {
		return DoctorCheck{Name: "version", OK: false, Detail: "could not list release tags: " + err.Error(),
			Hint: "check network / git remote; the version check is cosmetic otherwise"}
	}
	if tag == "" {
		return DoctorCheck{Name: "version", OK: true, Detail: "no v* release tags found; nothing to compare"}
	}
	if strings.TrimPrefix(tag, "v") == ver {
		return DoctorCheck{Name: "version", OK: true, Detail: "up to date (" + tag + ")"}
	}
	detail := fmt.Sprintf("running %s; latest release %s", ver, tag)
	hint := "rebuild with the release stamp or install the latest release (scripts/release, README install)"
	if ver == version.Version {
		detail += " (unstamped local build)"
	}
	return DoctorCheck{Name: "version", OK: false, Detail: detail, Hint: hint}
}

// semverLess orders vMAJOR.MINOR.PATCH tags numerically.
func semverLess(a, b [3]int) bool {
	for i := range 3 {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// doctorLatestTag mirrors scripts/release/nextversion lastTag: the remote tag
// list is authoritative (a local snapshot misses a tag pushed after checkout),
// the local tag list is the fallback for no-origin checkouts, non-semver tags
// drop, and the winner is the numerically greatest vMAJOR.MINOR.PATCH.
func doctorLatestTag(repo string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	git := exec.CommandContext(ctx, "git", "-C", repo, "ls-remote", "--tags", "origin", "refs/tags/v*")
	out, err := git.Output()
	lines := []string{}
	if err == nil {
		lines = strings.Split(string(out), "\n")
	}
	if strings.TrimSpace(strings.Join(lines, "")) == "" {
		local, lerr := exec.CommandContext(ctx, "git", "-C", repo, "tag", "--list", "v*").Output()
		if lerr != nil {
			if err != nil {
				return "", err
			}
			return "", nil
		}
		for _, t := range strings.Split(string(local), "\n") {
			if t = strings.TrimSpace(t); t != "" {
				lines = append(lines, "refs/tags/"+t)
			}
		}
	}
	best := ""
	var bestNum [3]int
	for _, line := range lines {
		_, rest, ok := strings.Cut(strings.TrimSpace(line), "refs/tags/")
		if !ok || !semverTagRe.MatchString(rest) {
			continue
		}
		if n := semverNum(rest); best == "" || semverLess(bestNum, n) {
			best, bestNum = rest, n
		}
	}
	return best, nil
}

func semverNum(tag string) [3]int {
	var out [3]int
	for i, part := range strings.Split(strings.TrimPrefix(tag, "v"), ".") {
		n, _ := strconv.Atoi(part)
		out[i] = n
	}
	return out
}

// ---------------------------------------------------------------------------
// 2. config
// ---------------------------------------------------------------------------

func doctorConfigCheck(repo string, cfg config.Config, cfgErr error) DoctorCheck {
	if cfgErr != nil {
		return DoctorCheck{Name: "config", OK: false, Detail: cfgErr.Error(),
			Hint: "fix devagent.json, or re-run `devagent init` for sane defaults"}
	}
	_, statErr := os.Stat(filepath.Join(repo, "devagent.json"))
	if statErr != nil {
		if _, statErr = os.Stat(filepath.Join(repo, ".devagent.json")); statErr != nil {
			return DoctorCheck{Name: "config", OK: true, Detail: "no devagent.json (defaults apply)"}
		}
	}
	model := cfg.Model
	if model == "" {
		model = "(default)"
	}
	return DoctorCheck{Name: "config", OK: true, Detail: "devagent.json valid (worker=" + cfg.Worker + " model=" + model + ")"}
}

// ---------------------------------------------------------------------------
// 3. DEVAGENT_HOME
// ---------------------------------------------------------------------------

// doctorHomeDir resolves DEVAGENT_HOME the way every consumer does:
// $DEVAGENT_HOME, else $HOME/.devagent.
func doctorHomeDir() string {
	if h := os.Getenv("DEVAGENT_HOME"); h != "" {
		return h
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "."
	}
	return filepath.Join(home, ".devagent")
}

func doctorHomeCheck() DoctorCheck {
	home := doctorHomeDir()
	var missing []string
	if !writableDir(home) {
		missing = append(missing, home+" (not writable)")
	}
	for _, sub := range []string{"locks", "runs"} {
		if !writableDir(filepath.Join(home, sub)) {
			missing = append(missing, filepath.Join(home, sub)+" (missing)")
		}
	}
	if len(missing) > 0 {
		return DoctorCheck{Name: "home", OK: false,
			Detail: "DEVAGENT_HOME " + home + " problems: " + strings.Join(missing, "; "),
			Hint:   "mkdir -p " + filepath.Join(home, "locks") + " " + filepath.Join(home, "runs")}
	}
	return DoctorCheck{Name: "home", OK: true, Detail: home + " writable; locks/ and runs/ present"}
}

// writableDir: dir exists and accepts a file create/delete.
func writableDir(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	probe, err := os.CreateTemp(dir, ".doctor-write-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return true
}

// ---------------------------------------------------------------------------
// 4. git remote
// ---------------------------------------------------------------------------

func doctorGitRemoteCheck(opts DoctorOptions) DoctorCheck {
	remote := opts.GitRemote
	if remote == nil {
		remote = func() error { return doctorGitRemoteReachable(opts.RepoPath) }
	}
	if err := remote(); err != nil {
		return DoctorCheck{Name: "git-remote", OK: false, Detail: "origin unreachable: " + err.Error(),
			Hint: "check VPN / ssh keys (`ssh -T git@github.com`); the loop pushes and fetches origin"}
	}
	return DoctorCheck{Name: "git-remote", OK: true, Detail: "origin reachable"}
}

// doctorGitRemoteReachable runs `git ls-remote origin HEAD` with the state
// sync's bounded ssh: keep an ambient GIT_SSH_COMMAND, otherwise pin
// `ssh -o BatchMode=yes -o ConnectTimeout=10` so a wedged ssh child can never
// hang doctor.
func doctorGitRemoteReachable(repo string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	urlCheck := exec.CommandContext(ctx, "git", "-C", repo, "remote", "get-url", "origin")
	if err := urlCheck.Run(); err != nil {
		return fmt.Errorf("no 'origin' remote configured")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "ls-remote", "origin", "HEAD")
	cmd.Env = append(os.Environ(), gitSSHEnv()...)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func gitSSHEnv() []string {
	if os.Getenv("GIT_SSH_COMMAND") != "" {
		return nil
	}
	return []string{"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=10"}
}

// ---------------------------------------------------------------------------
// 5. gh auth
// ---------------------------------------------------------------------------

func doctorGhAuthCheck(opts DoctorOptions) DoctorCheck {
	resolve := opts.GhToken
	if resolve == nil {
		resolve = doctorResolveGhToken
	}
	source, token := resolve()
	if token == "" {
		return DoctorCheck{Name: "gh-auth", OK: false,
			Detail: "no GitHub token (env GITHUB_TOKEN unset, `gh auth token` empty)",
			Hint:   "run `gh auth login` or export GITHUB_TOKEN"}
	}
	valid := opts.TokenValid
	if valid == nil {
		valid = doctorTokenValid
	}
	ok, detail := valid(source, token)
	if ok {
		return DoctorCheck{Name: "gh-auth", OK: true, Detail: "token valid (source: " + source + ")"}
	}
	hint := "run `gh auth login` to refresh the keyring credential"
	if strings.HasPrefix(source, "env") {
		// The 2026-09-09 incident class: an invalid env token silently
		// shadows a valid keyring credential. Name it, never mask it.
		hint = "env GITHUB_TOKEN wins over `gh auth token` — unset or fix the env value"
	}
	return DoctorCheck{Name: "gh-auth", OK: false,
		Detail: "invalid token (source: " + source + "): " + detail, Hint: hint}
}

// doctorResolveGhToken mirrors config.LoadCredentials' precedence: env
// GITHUB_TOKEN wins, else `gh auth token` (bounded).
func doctorResolveGhToken() (string, string) {
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		return "env GITHUB_TOKEN", tok
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "keyring (gh auth token)", ""
	}
	return "keyring (gh auth token)", strings.TrimSpace(string(out))
}

// doctorTokenValid probes api.github.com with the token (never logs it).
func doctorTokenValid(source, token string) (bool, string) {
	_ = source
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, ""
	case http.StatusUnauthorized:
		return false, "rejected (HTTP 401)"
	default:
		// ponytail: a 403 rate-limit burst reads as invalid; the upgrade
		// path is retrying once on 403 before failing the check.
		return false, "verification probe returned HTTP " + strconv.Itoa(resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 6. herdr session
// ---------------------------------------------------------------------------

func doctorHerdrCheck(opts DoctorOptions, cfg config.Config) DoctorCheck {
	bin := herdr.HerdrBin()
	_, lookErr := exec.LookPath(bin)
	if lookErr != nil {
		expected := config.HerdrEnabled(cfg) || os.Getenv("DEVAGENT_HERDR_SESSION") != ""
		if expected {
			return DoctorCheck{Name: "herdr", OK: false, Detail: "herdr binary " + bin + " not found",
				Hint: "install herdr, or disable it (herdr.enabled=false / DEVAGENT_HERDR=0)"}
		}
		return DoctorCheck{Name: "herdr", OK: true, Detail: "not installed (optional; not enabled in config)"}
	}
	cli := opts.HerdrCli
	if cli == nil {
		cli = herdr.ExecRunner{}
	}
	session := config.HerdrSessionName(cfg)
	res := cli.HerdrCli([]string{"--session", session, "agent", "list"}, 10_000)
	if res.Code == 0 {
		return DoctorCheck{Name: "herdr", OK: true, Detail: "session " + session + " reachable"}
	}
	detail := fmt.Sprintf("herdr session %s not reachable (exit %d)", session, res.Code)
	if res.Stderr != "" {
		detail += ": " + bound(res.Stderr, 200)
	}
	return DoctorCheck{Name: "herdr", OK: false, Detail: detail,
		Hint: "start the herdr server (`herdr --session " + session + "`) or disable herdr in config"}
}

// bound trims an excerpt like resilience.boundDetail.
func bound(text string, n int) string {
	s := strings.Join(strings.Fields(text), " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------------------------------------------------------------------------
// 7. daemon
// ---------------------------------------------------------------------------

func doctorDaemonCheck(opts DoctorOptions) DoctorCheck {
	probe := opts.DaemonProbe
	if probe == nil {
		probe = func() (int, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet,
				fmt.Sprintf("http://127.0.0.1:%d/healthz", daemonPort), nil)
			if err != nil {
				return 0, err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, err
			}
			defer func() { _ = resp.Body.Close() }()
			return resp.StatusCode, nil
		}
	}
	status, err := probe()
	switch {
	case err == nil && status == http.StatusOK:
		return DoctorCheck{Name: "daemon", OK: true,
			Detail: fmt.Sprintf("daemon healthy on :%d (/healthz; /status needs the bearer token)", daemonPort)}
	case err != nil && isConnRefused(err):
		return DoctorCheck{Name: "daemon", OK: true,
			Detail: "not running (optional; start with `devagent daemon`)"}
	default:
		detail := fmt.Sprintf("daemon on :%d listening but unhealthy", daemonPort)
		if err != nil {
			detail += ": " + err.Error()
		} else {
			detail += fmt.Sprintf(" (HTTP %d)", status)
		}
		return DoctorCheck{Name: "daemon", OK: false, Detail: detail,
			Hint: "restart the daemon: `devagent daemon`"}
	}
}

func isConnRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection refused")
}

// ---------------------------------------------------------------------------
// 8. provider preflight
// ---------------------------------------------------------------------------

func doctorProviderCheck(opts DoctorOptions, repo string, cfg config.Config) DoctorCheck {
	argv := BuildProbeArgvFor(cfg.Worker, cfg.Model)
	probe := opts.ProviderProbe
	if probe == nil {
		probe = func(cmd string, args []string, dir string) resilience.Probe {
			return resilience.RunPreflightProbe(cmd, args, dir, doctorProbeTimeoutMs)
		}
	}
	r := probe(argv[0], append([]string{argv[1], "OK"}, argv[2:]...), repo)
	if r.OK {
		return DoctorCheck{Name: "provider", OK: true,
			Detail: "provider answered via " + argv[0] + " (same probe as the loop's preflight)"}
	}
	detail := "provider did not answer"
	if r.Detail != "" {
		detail += ": " + bound(r.Detail, 200)
	}
	return DoctorCheck{Name: "provider", OK: false, Detail: detail,
		Hint: "run `devagent preflight --role selfbuild` for the gated verdict, or check provider status"}
}

// ---------------------------------------------------------------------------
// 9. stale artifacts
// ---------------------------------------------------------------------------

func doctorArtifactsCheck(opts DoctorOptions, repo string, now func() time.Time) DoctorCheck {
	var problems []string

	// Stale run locks: <home>/locks/*.lock older than the registry TTL.
	home := doctorHomeDir()
	entries, err := os.ReadDir(filepath.Join(home, "locks"))
	if err == nil {
		stale := 0
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
				continue
			}
			data, rerr := os.ReadFile(filepath.Join(home, "locks", e.Name()))
			if rerr != nil {
				continue
			}
			var holder struct {
				StartedAt *int64 `json:"startedAt"`
			}
			if json.Unmarshal(data, &holder) != nil || holder.StartedAt == nil {
				continue // corrupt: TryAcquireRun breaks these on next use
			}
			if now().UnixMilli()-*holder.StartedAt > ledger.DefaultLockTTL {
				stale++
			}
		}
		if stale > 0 {
			problems = append(problems, fmt.Sprintf("%d stale run lock(s) older than the TTL", stale))
		}
	}

	// loop.lock pid mismatch: a dead holder pid means the lock dir is stale
	// debris (acquireLock would clear it on retry, but a live run with a
	// mismatched/dead pid is exactly the 2026-09-10 wedged-driver class).
	pidPath := filepath.Join(repo, ".selfbuild", "loop.lock.d", "pid")
	if data, err := os.ReadFile(pidPath); err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil {
			if !processAlive(pid) {
				problems = append(problems, fmt.Sprintf("stale loop.lock held by dead pid %d", pid))
			}
		}
	}

	if len(problems) > 0 {
		return DoctorCheck{Name: "artifacts", OK: false, Detail: strings.Join(problems, "; "),
			Hint: "rm -rf .selfbuild/loop.lock.d and stale <home>/locks/*.lock; both self-heal on the next loop start"}
	}
	return DoctorCheck{Name: "artifacts", OK: true, Detail: "no stale locks, no dead loop.lock pid"}
}

// RenderDoctorReport prints the human checklist (the same glyph language as
// the init report); the CLI layer renders the --json shape from DoctorResult.
func RenderDoctorReport(res DoctorResult, render func(string)) {
	render("DevAgent doctor — machine validation")
	render("")
	for _, c := range res.Checks {
		glyph := map[bool]string{true: "ok", false: "failed"}[c.OK]
		line := "  " + glyph + " " + c.Name + "  " + c.Detail
		if !c.OK && c.Hint != "" {
			line += "\n      hint: " + c.Hint
		}
		render(line)
	}
	render("")
	if res.OK {
		render("All checks passed.")
	} else {
		failed := 0
		for _, c := range res.Checks {
			if !c.OK {
				failed++
			}
		}
		unit := "checks"
		if failed == 1 {
			unit = "check"
		}
		render(fmt.Sprintf("%d %s failed — fix the hints above and re-run `devagent doctor`.", failed, unit))
	}
}
