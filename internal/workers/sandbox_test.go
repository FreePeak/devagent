// Go port of test/sandbox.test.ts: env scrub with the
// DEVAGENT_WORKER_ENV_ALLOWLIST override, seatbelt profile generation,
// network allowlist resolution, and prepareWorkerSpawn behavior.

package workers

import (
	"regexp"
	"strings"
	"testing"
)

// Fake provider-shaped values, assembled at runtime so this file never
// contains a usable credential-shaped literal (SanitizeWorkerEnv only
// matches on variable NAMES — the values are opaque).
const (
	fakeAnthropicKey = "sk-" + "ant-x"
	fakeGithubToken  = "ghp-" + "secret"
	fakeStripeKey    = "sk-" + "live"
	fakeNpmToken     = "npm-" + "secret"
	fakeAwsSecret    = "aws-" + "secret"
	fakeXaiKey       = "xai-" + "secret"
)

func sandboxBaseEnv() map[string]string {
	return map[string]string{
		"PATH":                  "/usr/bin",
		"HOME":                  "/Users/t",
		"ANTHROPIC_API_KEY":     fakeAnthropicKey,
		"GITHUB_TOKEN":          fakeGithubToken,
		"NPM_TOKEN":             fakeNpmToken,
		"AWS_SECRET_ACCESS_KEY": fakeAwsSecret,
		"MY_SERVICE_PASSWORD":   "pw",
		"SLACK_CLIENT_SECRET":   "slack",
		"STRIPE_API_KEY":        fakeStripeKey,
		"XAI_API_KEY":           fakeXaiKey,
	}
}

func TestSanitizeWorkerEnv_SecretShapedStripped(t *testing.T) {
	res := SanitizeWorkerEnv(sandboxBaseEnv(), nil)
	want := []string{"AWS_SECRET_ACCESS_KEY", "MY_SERVICE_PASSWORD", "NPM_TOKEN", "SLACK_CLIENT_SECRET", "STRIPE_API_KEY"}
	assertSetEqual(t, res.Stripped, want)
	if res.Env["GITHUB_TOKEN"] != fakeGithubToken {
		// GITHUB_TOKEN is allowlisted (worker-side gh/curl api.github.com
		// calls — research crawls exhausted the anonymous 60 req/h IP
		// budget, 2026-09-02).
		t.Fatalf("GITHUB_TOKEN = %q", res.Env["GITHUB_TOKEN"])
	}
	if _, ok := res.Env["NPM_TOKEN"]; ok {
		t.Fatal("NPM_TOKEN must be stripped")
	}
	if _, ok := res.Env["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Fatal("AWS_SECRET_ACCESS_KEY must be stripped")
	}
}

func TestSanitizeWorkerEnv_KeepsProcessBasicsAndLLMAuth(t *testing.T) {
	res := SanitizeWorkerEnv(sandboxBaseEnv(), nil)
	if res.Env["PATH"] != "/usr/bin" || res.Env["HOME"] != "/Users/t" {
		t.Fatalf("env basics = %v", res.Env)
	}
	if res.Env["ANTHROPIC_API_KEY"] != fakeAnthropicKey {
		t.Fatal("ANTHROPIC_API_KEY must be kept")
	}
	// XAI_API_KEY is allowlisted for the grok worker (FR-GROK-01): the
	// /_API_KEY$/ scrubber would otherwise strip the CLI's only env auth.
	if res.Env["XAI_API_KEY"] != fakeXaiKey {
		t.Fatal("XAI_API_KEY must be kept")
	}
	for _, n := range res.Stripped {
		if n == "ANTHROPIC_API_KEY" || n == "XAI_API_KEY" {
			t.Fatalf("allowlisted var stripped: %q", n)
		}
	}
}

func TestSanitizeWorkerEnv_DoesNotMutateCallerEnv(t *testing.T) {
	base := sandboxBaseEnv()
	SanitizeWorkerEnv(base, nil)
	if base["GITHUB_TOKEN"] != fakeGithubToken {
		t.Fatal("caller env mutated")
	}
}

func TestSanitizeWorkerEnv_AllowlistOverride(t *testing.T) {
	t.Setenv("DEVAGENT_WORKER_ENV_ALLOWLIST", "STRIPE_API_KEY,CUSTOM_TOKEN")
	base := sandboxBaseEnv()
	base["CUSTOM_TOKEN"] = "needed-by-worker"
	res := SanitizeWorkerEnv(base, nil)
	if res.Env["STRIPE_API_KEY"] != fakeStripeKey {
		t.Fatal("explicitly allowlisted secret-shape name must be kept")
	}
	if res.Env["CUSTOM_TOKEN"] != "needed-by-worker" {
		t.Fatal("CUSTOM_TOKEN must be kept")
	}
	for _, n := range res.Stripped {
		if n == "STRIPE_API_KEY" || n == "CUSTOM_TOKEN" {
			t.Fatalf("allowlisted var stripped: %q", n)
		}
	}
	// other secrets still stripped (GITHUB_TOKEN is baseline-allowlisted)
	if _, ok := res.Env["NPM_TOKEN"]; ok {
		t.Fatal("NPM_TOKEN must still be stripped")
	}
}

func TestBuildSeatbeltProfile_WritePolicy(t *testing.T) {
	profile := BuildSeatbeltProfile(SandboxPolicy{WritablePaths: []string{"/repo/wt"}, Network: "allow"})
	if !contains(profile, "(deny file-write*)") {
		t.Fatal("missing blanket write deny")
	}
	if !contains(profile, `(subpath "/repo/wt")`) {
		t.Fatal("missing cwd subpath")
	}
	if !contains(profile, `(subpath "/private/tmp")`) || !contains(profile, `(subpath "/private/var/folders")`) {
		t.Fatal("missing temp-dir subpaths")
	}
	if !regexp.MustCompile(`\(subpath "[^"]*\/\.claude"\)`).MatchString(profile) {
		t.Fatal("missing agent config home subpath")
	}
}

func TestBuildSeatbeltProfile_NetworkPolicy(t *testing.T) {
	allow := BuildSeatbeltProfile(SandboxPolicy{Network: "allow"})
	if !contains(allow, "(allow network)") {
		t.Fatal("allow policy must emit (allow network)")
	}

	deny := BuildSeatbeltProfile(SandboxPolicy{Network: "deny"})
	if !contains(deny, "(deny network*)") {
		t.Fatal("deny policy must emit (deny network*)")
	}
	// SBPL is last-match-wins: the deny clause must come after (allow default).
	if strings.Index(deny, "(allow default)") >= strings.Index(deny, "(deny network*)") {
		t.Fatal("deny must come after the default allow")
	}
	if contains(deny, "(allow network)") {
		t.Fatal("deny policy must not carry a bare network allow")
	}

	// deny composes with the write allowlist instead of replacing it
	composed := BuildSeatbeltProfile(SandboxPolicy{WritablePaths: []string{"/repo/wt"}, Network: "deny"})
	if !contains(composed, "(deny file-write*)") || !contains(composed, `(subpath "/repo/wt")`) || !contains(composed, "(deny network*)") {
		t.Fatalf("composed profile = %s", composed)
	}

	// allowlist mode: deny all sockets, then re-allow each endpoint after it.
	endpoints := []string{"140.82.121.3:443", "104.16.26.34:443"}
	profile := BuildSeatbeltProfile(SandboxPolicy{Network: "allowlist", NetworkAllowlist: endpoints})
	if !contains(profile, "(deny network*)") {
		t.Fatal("allowlist mode must start with the blanket deny")
	}
	denyIdx := strings.Index(profile, "(deny network*)")
	prev := denyIdx
	for _, e := range endpoints {
		clause := `(allow network-outbound (remote ip "` + e + `"))`
		idx := strings.Index(profile, clause)
		if idx < 0 {
			t.Fatalf("missing re-allow clause for %s", e)
		}
		if idx < prev {
			t.Fatalf("re-allow for %s must come after the previous clause", e)
		}
		prev = idx
	}
	if got := strings.Count(profile, "allow network-outbound"); got != len(endpoints) {
		t.Fatalf("re-allow count = %d, want %d", got, len(endpoints))
	}
	if contains(profile, "(allow network)") {
		t.Fatal("allowlist mode must not carry a bare network allow")
	}
}

func TestResolveNetworkAllowlist(t *testing.T) {
	t.Run("literal IPv4 and IPv6 entries pass through without DNS", func(t *testing.T) {
		eps, err := ResolveNetworkAllowlist("10.0.0.5:8443, fd00::7")
		if err != nil {
			t.Fatal(err)
		}
		// fd00::7 is a bare IPv6 literal; JoinHostPort brackets it —
		// mirroring a valid host:port endpoint for SBPL.
		if len(eps) != 2 || eps[0] != "10.0.0.5:8443" {
			t.Fatalf("endpoints = %v", eps)
		}
	})
	t.Run("bracketed IPv6 with explicit port", func(t *testing.T) {
		eps, err := ResolveNetworkAllowlist("[fd00::7]:8443")
		if err != nil {
			t.Fatal(err)
		}
		if eps[0] != "[fd00::7]:8443" && eps[0] != "fd00::7:8443" {
			t.Fatalf("endpoints = %v", eps)
		}
	})
	t.Run("throws loudly when every entry is blank", func(t *testing.T) {
		_, err := ResolveNetworkAllowlist(" , ,")
		if err == nil || !contains(err.Error(), "DEVAGENT_SANDBOX_NETWORK_ALLOWLIST") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("throws loudly naming an unresolvable host", func(t *testing.T) {
		// .invalid is guaranteed unresolvable per RFC 2606.
		_, err := ResolveNetworkAllowlist("nope.invalid")
		if err == nil || !contains(err.Error(), "nope.invalid") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("resolves a hostname via DNS with default port 443", func(t *testing.T) {
		// Network-dependent resolution on localhost DNS for localhost.
		eps, err := ResolveNetworkAllowlist("localhost")
		if err != nil {
			t.Skipf("dns unavailable: %v", err)
		}
		if len(eps) == 0 {
			t.Fatal("expected at least one endpoint")
		}
		for _, ep := range eps {
			if !hasSuffix(ep, ":443") {
				t.Fatalf("endpoint %q must default to port 443", ep)
			}
		}
	})
}

func TestPrepareWorkerSpawn(t *testing.T) {
	spawnOpts := SpawnCliOptions{Dir: "/repo/worktree", TimeoutMs: 5_000}

	t.Run("scrubs the worker env by default and flags replaceEnv", func(t *testing.T) {
		t.Setenv("NPM_TOKEN", "x")
		prepared, err := PrepareWorkerSpawn("claude", []string{"-p", "hi"}, spawnOpts)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Cmd != "claude" {
			t.Fatalf("cmd = %q", prepared.Cmd)
		}
		if !prepared.Opts.ReplaceEnv {
			t.Fatal("replaceEnv must be flagged")
		}
		if _, ok := prepared.Opts.Env["NPM_TOKEN"]; ok {
			t.Fatal("NPM_TOKEN must be scrubbed")
		}
		found := false
		for _, n := range prepared.StrippedEnv {
			if n == "NPM_TOKEN" {
				found = true
			}
		}
		if !found {
			t.Fatalf("strippedEnv = %v", prepared.StrippedEnv)
		}
	})

	t.Run("lets caller-provided env win over the scrubbed base", func(t *testing.T) {
		parentKey := "parent" + "-key"
		overrideKey := "override" + "-key"
		t.Setenv("OPENAI_API_KEY", parentKey)
		prepared, err := PrepareWorkerSpawn("claude", []string{"-p"}, SpawnCliOptions{
			Dir: spawnOpts.Dir, TimeoutMs: spawnOpts.TimeoutMs,
			Env: map[string]string{"OPENAI_API_KEY": overrideKey},
		})
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Opts.Env["OPENAI_API_KEY"] != overrideKey {
			t.Fatalf("env = %q", prepared.Opts.Env["OPENAI_API_KEY"])
		}
	})

	t.Run("fails loudly when seatbelt is requested on non-darwin", func(t *testing.T) {
		t.Setenv("DEVAGENT_SANDBOX", "seatbelt")
		orig := platform
		platform = func() string { return "linux" }
		defer func() { platform = orig }()
		_, err := PrepareWorkerSpawn("claude", []string{"-p"}, spawnOpts)
		if err == nil || !contains(err.Error(), "darwin") {
			t.Fatalf("err = %v", err)
		}
	})
}

func hasSuffix(s, suf string) bool { return strings.HasSuffix(s, suf) }

func assertSetEqual(t *testing.T, got, want []string) {
	t.Helper()
	m := map[string]int{}
	for _, g := range got {
		m[g]++
	}
	for _, w := range want {
		m[w]--
	}
	for k, v := range m {
		if v != 0 {
			t.Fatalf("set mismatch: %q count %+v (got %v want %v)", k, v, got, want)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("lengths differ: got %v want %v", got, want)
	}
}
