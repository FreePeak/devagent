// Go port of src/workers/sandbox.ts (FR-GO-05): worker sandbox isolation
// (PRD section 17 Phase 4: deeper sandbox isolation).
//
// Workers execute inside untrusted target repos where prompt injection can
// run arbitrary shell, so agent-CLI spawns must not inherit the
// orchestrator's full environment (GITHUB_TOKEN, cloud credentials, ...).
// Two layers:
//
//  1. Env scrubbing (default on): secret-shaped env vars are stripped from
//     claude-code / opencode spawns. Git, docker, gh and test-runner spawns
//     keep the untouched parent env — they legitimately need credentials.
//  2. Seatbelt confinement (opt-in via DEVAGENT_SANDBOX=seatbelt, darwin
//     only): worker commands run under sandbox-exec with a generated
//     profile that denies writes outside the worktree cwd and temp dirs.
//     Network defaults to allow (workers must reach LLM APIs) and can be
//     tightened with DEVAGENT_SANDBOX_NETWORK=deny, which emits
//     `(deny network*)` in the profile for fully offline worker runs, or
//     with DEVAGENT_SANDBOX_NETWORK=allowlist, which denies all sockets
//     then re-allows exactly the resolved endpoints from
//     DEVAGENT_SANDBOX_NETWORK_ALLOWLIST.
package workers

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// DEFAULT_ENV_ALLOWLIST: env vars the agent CLIs need to authenticate and
// run; never stripped.
var DEFAULT_ENV_ALLOWLIST = []string{
	// process basics
	"PATH", "HOME", "SHELL", "USER", "LOGNAME", "TMPDIR", "TEMP", "TMP",
	"TERM", "TERM_PROGRAM", "LANG", "LC_ALL", "TZ",
	// LLM providers used by the worker CLIs themselves
	"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN",
	"OPENAI_API_KEY", "OPENAI_BASE_URL",
	// grok (xAI): the Grok Build CLI authenticates via browser OAuth or
	// XAI_API_KEY; the /_API_KEY$/ scrubber strips it otherwise (FR-GROK-01).
	"XAI_API_KEY",
	// opencode provider config
	"OPENCODE_API_KEY",
	// pi provider config (pi resolves provider keys from env; omniroute is
	// the configured default provider in this deployment). Other pi
	// providers can be enabled via DEVAGENT_WORKER_ENV_ALLOWLIST.
	"OMNIROUTE_API_KEY",
	// GitHub API access for worker-side gh/curl calls (research competitor
	// scans hit api.github.com; anonymous = 60 req/h shared per IP,
	// exhausted after ~2 crawls — 2026-09-02 rate-limit stall).
	// Authenticated = 5,000/h.
	"GITHUB_TOKEN",
	"GH_TOKEN",
}

// CREDENTIAL_PATTERNS: secret-shaped variable names to strip from worker
// environments. Order does not matter; a var is stripped if any pattern
// matches and it is not on the allowlist.
var CREDENTIAL_PATTERNS = []*regexp.Regexp{
	regexp.MustCompile(`(?i)TOKEN`),
	regexp.MustCompile(`(?i)SECRET`),
	regexp.MustCompile(`(?i)PASS(WD|WORD)`),
	regexp.MustCompile(`_API_KEY$`),
	regexp.MustCompile(`^AWS_`),
	regexp.MustCompile(`^AZURE_`),
	regexp.MustCompile(`^GOOGLE_|^GCP_`),
	regexp.MustCompile(`^DOCKER|^NPM_|^PYPI_`),
	regexp.MustCompile(`^SSH_AUTH_SOCK$`), // agent forwarding leaks host credentials into children
	regexp.MustCompile(`^GIT_.*_CREDENTIALS?$`),
}

// SanitizeWorkerEnvResult mirrors TS SanitizeWorkerEnvResult.
type SanitizeWorkerEnvResult struct {
	// Env is the scrubbed copy of the input environment.
	Env map[string]string
	// Stripped names, for audit logging.
	Stripped []string
}

// sandboxAllowlist builds the effective allowlist: the defaults plus
// DEVAGENT_WORKER_ENV_ALLOWLIST (comma-separated exact names) plus any
// extra entries. Env lookup happens at call time so tests can t.Setenv.
func sandboxAllowlist(extraAllowlist []string) map[string]bool {
	allowed := map[string]bool{}
	for _, name := range DEFAULT_ENV_ALLOWLIST {
		allowed[name] = true
	}
	if raw := os.Getenv("DEVAGENT_WORKER_ENV_ALLOWLIST"); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			trimmed := strings.TrimSpace(name)
			if trimmed != "" {
				allowed[trimmed] = true
			}
		}
	}
	for _, name := range extraAllowlist {
		allowed[name] = true
	}
	return allowed
}

// SanitizeWorkerEnv strips credential-shaped env vars from a worker
// environment. Default-on for all agent-CLI worker spawns; extend with
// DEVAGENT_WORKER_ENV_ALLOWLIST (comma-separated exact names).
func SanitizeWorkerEnv(baseEnv map[string]string, extraAllowlist []string) SanitizeWorkerEnvResult {
	allowed := sandboxAllowlist(extraAllowlist)
	env := map[string]string{}
	var stripped []string
	for name, value := range baseEnv {
		if !allowed[name] && matchesAny(CREDENTIAL_PATTERNS, name) {
			stripped = append(stripped, name)
			continue
		}
		env[name] = value
	}
	return SanitizeWorkerEnvResult{Env: env, Stripped: stripped}
}

// SanitizeParentWorkerEnv is SanitizeWorkerEnv over the process env — the
// TS call site shape (sanitizeWorkerEnv(process.env)).
func SanitizeParentWorkerEnv(extraAllowlist []string) SanitizeWorkerEnvResult {
	base := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			base[kv[:i]] = kv[i+1:]
		}
	}
	return SanitizeWorkerEnv(base, extraAllowlist)
}

func matchesAny(patterns []*regexp.Regexp, s string) bool {
	for _, p := range patterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}

// SandboxPolicy mirrors TS SandboxPolicy.
type SandboxPolicy struct {
	// WritablePaths: paths workers may write to (the worktree cwd goes here).
	WritablePaths []string
	// Network policy. "allow" (default) leaves socket creation open so
	// workers can reach LLM APIs; "deny" emits `(deny network*)` for fully
	// offline runs; "allowlist" denies all sockets then re-allows exactly
	// the resolved endpoints in NetworkAllowlist (SBPL is
	// last-match-wins).
	Network string
	// NetworkAllowlist: already-resolved `ip:port` endpoint literals for
	// the "allowlist" mode. Hostnames must be resolved before reaching
	// this layer — SBPL cannot DNS.
	NetworkAllowlist []string
}

// DEFAULT_ALLOWLIST_PORT is the default port when a
// DEVAGENT_SANDBOX_NETWORK_ALLOWLIST entry omits one.
const DEFAULT_ALLOWLIST_PORT = 443

// splitHostPort splits an `host[:port]` allowlist entry, defaulting the
// port to 443. IPv6 literals use [host]:port or bare host.
func splitHostPort(entry string) (string, int) {
	if ip := net.ParseIP(entry); ip != nil {
		return entry, DEFAULT_ALLOWLIST_PORT
	}
	// TS /^\[(.+)\](?::(\d+))?$/ — bracketed IPv6 with optional port.
	if bracketed := regexp.MustCompile(`^\[(.+)\](?::(\d+))?$`).FindStringSubmatch(entry); bracketed != nil {
		port := DEFAULT_ALLOWLIST_PORT
		if bracketed[2] != "" {
			port = mustAtoi(bracketed[2])
		}
		return bracketed[1], port
	}
	if colon := strings.LastIndexByte(entry, ':'); colon > -1 && isAllDigits(entry[colon+1:]) {
		return entry[:colon], mustAtoi(entry[colon+1:])
	}
	return entry, DEFAULT_ALLOWLIST_PORT
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// ResolveNetworkAllowlist resolves DEVAGENT_SANDBOX_NETWORK_ALLOWLIST
// entries to literal `ip:port` endpoints. Hostnames go through DNS lookup
// (all addresses); entries that are already IPv4/IPv6 literals pass
// through untouched. Error strings match the TS originals byte-for-byte
// (tests pin them).
func ResolveNetworkAllowlist(raw string) ([]string, error) {
	var entries []string
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSpace(e)
		if e != "" {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("DEVAGENT_SANDBOX_NETWORK=allowlist requires a non-empty DEVAGENT_SANDBOX_NETWORK_ALLOWLIST (comma-separated host[:port] entries)")
	}
	var endpoints []string
	for _, entry := range entries {
		host, port := splitHostPort(entry)
		if net.ParseIP(host) != nil {
			endpoints = append(endpoints, net.JoinHostPort(host, strconv.Itoa(port)))
			continue
		}
		addresses, err := net.LookupIP(host)
		if err != nil || len(addresses) == 0 {
			return nil, fmt.Errorf("DEVAGENT_SANDBOX_NETWORK_ALLOWLIST: unresolvable host %q (from entry %q)", host, entry)
		}
		for _, addr := range addresses {
			endpoints = append(endpoints, net.JoinHostPort(addr.String(), strconv.Itoa(port)))
		}
	}
	return endpoints, nil
}

// BuildSeatbeltProfile generates an SBPL profile: everything allowed by
// default except file writes outside the policy's paths, system temp dirs,
// and the agent config home.
func BuildSeatbeltProfile(policy SandboxPolicy) string {
	home, _ := os.UserHomeDir()
	writePaths := append([]string{}, policy.WritablePaths...)
	writePaths = append(writePaths,
		"/private/tmp", "/tmp",
		"/private/var/folders", // macOS per-user temp/cache roots
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".pi"),             // pi config + session dir
		filepath.Join(home, ".local", "share"), // opencode data dir lives here
		filepath.Join(home, ".cache"),
		filepath.Join(home, ".npm"),
	)
	clauses := make([]string, len(writePaths))
	for i, p := range writePaths {
		clauses[i] = fmt.Sprintf("    (subpath %q)", p)
	}
	var networkClauses []string
	switch policy.Network {
	case "allow":
		networkClauses = []string{"(allow network)"}
	case "deny":
		networkClauses = []string{"(deny network*)"}
	case "allowlist":
		networkClauses = append(networkClauses, "(deny network*)")
		for _, endpoint := range policy.NetworkAllowlist {
			networkClauses = append(networkClauses,
				fmt.Sprintf(`(allow network-outbound (remote ip "%s"))`, endpoint))
		}
	}
	lines := []string{
		"(version 1)",
		"(allow default)",
		`(deny file-write*)`,
		"(allow file-write*",
		strings.Join(clauses, "\n"),
		")",
		// named knob: SBPL is last-match-wins, so network clauses must come
		// after (allow default) to take effect; allowlist re-allows must
		// come after the blanket deny.
	}
	lines = append(lines, networkClauses...)
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// PreparedWorkerSpawn mirrors TS PreparedWorkerSpawn.
type PreparedWorkerSpawn struct {
	Cmd  string
	Args []string
	// Opts carries the merged (scrubbed) env; ReplaceEnv is always true so
	// a scrubbed env cannot unset inherited secrets by merging.
	Opts SpawnCliOptions
	// StrippedEnv: names of env vars stripped by scrubbing (audit trail).
	StrippedEnv []string
}

// PrepareWorkerSpawn is the shared preparation for the worker adapters so
// dispatch paths cannot diverge: scrubs the child env (default on) and,
// when DEVAGENT_SANDBOX=seatbelt on darwin, wraps the command with
// sandbox-exec using a generated profile scoped to opts.Dir. Fails loudly
// when confinement is requested but unavailable — silent fail-open would
// be worse than a crashed spawn.
func PrepareWorkerSpawn(cmd string, args []string, opts SpawnCliOptions) (PreparedWorkerSpawn, error) {
	scrubbed := SanitizeParentWorkerEnv(nil)
	// Caller-provided extras (e.g. model config) still win over the
	// scrubbed base.
	mergedEnv := scrubbed.Env
	if opts.Env != nil {
		mergedEnv = map[string]string{}
		for k, v := range scrubbed.Env {
			mergedEnv[k] = v
		}
		for k, v := range opts.Env {
			mergedEnv[k] = v
		}
	}
	finalCmd := cmd
	finalArgs := append([]string{}, args...)

	if os.Getenv("DEVAGENT_SANDBOX") == "seatbelt" {
		if platform() != "darwin" {
			return PreparedWorkerSpawn{}, fmt.Errorf("DEVAGENT_SANDBOX=seatbelt requires darwin (sandbox-exec); got %s", platform())
		}
		seatbeltBin := "/usr/bin/sandbox-exec"
		if _, err := os.Stat(seatbeltBin); err != nil {
			return PreparedWorkerSpawn{}, fmt.Errorf("DEVAGENT_SANDBOX=seatbelt requested but %s is missing", seatbeltBin)
		}
		network := os.Getenv("DEVAGENT_SANDBOX_NETWORK")
		if network == "" {
			network = "allow"
		}
		policy := SandboxPolicy{
			WritablePaths: []string{opts.Dir},
			Network:       "allow",
		}
		if network == "deny" || network == "allowlist" {
			policy.Network = network
		}
		if network == "allowlist" {
			endpoints, err := ResolveNetworkAllowlist(os.Getenv("DEVAGENT_SANDBOX_NETWORK_ALLOWLIST"))
			if err != nil {
				return PreparedWorkerSpawn{}, err
			}
			policy.NetworkAllowlist = endpoints
		}
		profile := BuildSeatbeltProfile(policy)
		dir, err := os.MkdirTemp("", "devagent-sb-")
		if err != nil {
			return PreparedWorkerSpawn{}, err
		}
		profilePath := filepath.Join(dir, "worker.sb")
		if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
			return PreparedWorkerSpawn{}, err
		}
		finalCmd = seatbeltBin
		finalArgs = append([]string{"-f", profilePath}, finalArgs...)
	}

	prepared := opts
	prepared.Dir = opts.Dir
	prepared.Env = mergedEnv
	prepared.ReplaceEnv = true
	return PreparedWorkerSpawn{
		Cmd:         finalCmd,
		Args:        finalArgs,
		Opts:        prepared,
		StrippedEnv: scrubbed.Stripped,
	}, nil
}

// platform mirrors process.platform (node:os.platform). A var indirection
// keeps the seatbelt guard testable on any OS — the TS suite mocks
// node:os.platform for the same reason. Defaults to the real GOOS.
var platform = func() string { return runtime.GOOS }
