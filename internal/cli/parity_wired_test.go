package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExitParityVsNodeWired is the wave-2 wiring acceptance gate (FR-GO-02
// follow-through, tracker #207): one case per wired command, run against
// both the Go binary and the Node CLI on the same fixture invocations. The
// Node dist may be stale — the parent regenerates with `npm run build` in
// the repo root before judging failures.
//
// Hermeticity: DEVAGENT_HOME points at a t.TempDir() so nothing reads the
// operator's ~/.devagent; the cwd is an isolated temp repo; herdr is forced
// absent via DEVAGENT_HERDR_BIN so the sweep/sessions/attach/pane-run cases
// exercise the unavailable-herdr paths deterministically instead of the
// operator's live session.
func TestExitParityVsNodeWired(t *testing.T) {
	if testing.Short() {
		t.Skip("exit-parity builds both CLIs — skipped in -short")
	}
	bin := filepath.Join(t.TempDir(), "devagent-go")
	build := exec.Command("go", "build", "-o", bin, "./cmd/devagent")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Go CLI: %v\n%s", err, out)
	}

	nodeCLI := filepath.Join(repoRoot(t), "dist", "src", "cli.js")
	_, nodeDistErr := os.Stat(nodeCLI)

	// noHerdr points DEVAGENT_HERDR_BIN at a path that never exists — the
	// same seam herdrCli uses in production (DEVAGENT_HERDR_BIN is read per
	// call), so both runtimes take the herdr-unavailable branch.
	const noHerdr = "/nonexistent/devagent-parity-herdr"

	type parityCase struct {
		name      string
		args      []string
		wantCode  int
		stdoutPin string            // substring both sides' stdout must contain ("" = none)
		env       map[string]string // per-case env overrides (inherited env wins otherwise)
		setup     func(t *testing.T, dir string)
		noNode    bool // Go-only contract; the Node side must not run (dispatch/live effects)
	}

	cases := []parityCase{
		{
			name:      "ledger empty json",
			args:      []string{"ledger", "--json"},
			wantCode:  0,
			stdoutPin: "[]",
		},
		{
			name:      "ledger empty card",
			args:      []string{"ledger"},
			wantCode:  0,
			stdoutPin: "No ledger records. Audits append to .devagent/runs/orchestration/events.jsonl.",
		},
		{
			name:      "ledger clusters empty",
			args:      []string{"ledger", "--clusters"},
			wantCode:  0,
			stdoutPin: "No failure clusters. Failed audits with unmet criteria and taskInterrupt executor events cluster here once the ledger has records.",
		},
		{
			name:      "ledger clusters invalid",
			args:      []string{"ledger", "--clusters", "abc"},
			wantCode:  0,
			stdoutPin: "Nothing to show for --clusters abc.",
		},
		{
			name:      "log missing run exits 1",
			args:      []string{"log", "--run", "nope"},
			wantCode:  1,
			stdoutPin: "",
		},
		{
			name:      "record release",
			args:      []string{"record", "release", "--tag", "v9.9.9", "--sha", "abc123"},
			wantCode:  0,
			stdoutPin: "recorded release-created 9.9.9 (v9.9.9 @ abc123) -> .devagent/runs/orchestration/events.jsonl",
		},
		{
			name:      "status json not started",
			args:      []string{"status", "--json"},
			wantCode:  0,
			stdoutPin: `"phase": "not started"`,
		},
		{
			name:     "validate cards no tests",
			args:     []string{"validate"},
			wantCode: 0,
			// An empty temp repo has no test command: G1 skips, G3 skips.
			stdoutPin: "G1 SKIP",
		},
		{
			name:      "validate json no tests",
			args:      []string{"validate", "--json"},
			wantCode:  0,
			stdoutPin: `"ok": true`,
		},
		{
			// The read-only seam: --replay is wired, every other scout mode
			// keeps the exit-3 stub contract. Go-only — the Node side would
			// run a real scout dispatch (here it would fail doc sync and
			// exit 1; the divergence is inherent to partial wiring and is
			// documented in the PR).
			name:     "scout non-replay still stubbed",
			args:     []string{"scout", "--once"},
			wantCode: 3,
			noNode:   true,
		},
		{
			name:     "sync-docs no repo fails closed",
			args:     []string{"sync-docs", "--json"},
			wantCode: 1,
			// No git repo at all: the generic-failure branch (fetch fails).
			stdoutPin: `"ok": false`,
		},
		{
			name:      "scout-status no heartbeat",
			args:      []string{"scout-status"},
			wantCode:  0,
			stdoutPin: "No scout heartbeat yet. Run: devagent scout --once --dry-run",
		},
		{
			name:      "scout-status json",
			args:      []string{"scout-status", "--json"},
			wantCode:  0,
			stdoutPin: `"heartbeat": null`,
		},
		{
			name:      "scout replay golden",
			args:      []string{"scout", "--replay"},
			wantCode:  0,
			stdoutPin: "fixtures match golden",
		},
		{
			name:      "herdr-sweep without herdr",
			args:      []string{"herdr-sweep", "--session", "parity-probe"},
			wantCode:  0,
			stdoutPin: "no stale panes",
			env:       map[string]string{"DEVAGENT_HERDR_BIN": noHerdr},
		},
		{
			name:      "sessions without herdr",
			args:      []string{"sessions"},
			wantCode:  0,
			stdoutPin: `no worker panes in session "devagent"`,
			env:       map[string]string{"DEVAGENT_HERDR_BIN": noHerdr},
		},
		{
			name:      "attach without herdr",
			args:      []string{"attach", "taskXYZ"},
			wantCode:  1,
			stdoutPin: "",
			env:       map[string]string{"DEVAGENT_HERDR_BIN": noHerdr},
		},
		{
			name:      "track one-shot json",
			args:      []string{"track", "--json"},
			wantCode:  0,
			stdoutPin: `"generatedAt"`,
		},
		{
			name:      "dashboard",
			args:      []string{"dashboard"},
			wantCode:  0,
			stdoutPin: "run(s) -> ",
		},
	}

	for i := range cases {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			repo := t.TempDir()
			t.Setenv("DEVAGENT_HOME", home)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if tc.setup != nil {
				tc.setup(t, repo)
			}

			goRes := runIn(t, repo, bin, tc.args)
			if goRes.code != tc.wantCode {
				t.Errorf("Go exit = %d, want %d (stderr: %s)", goRes.code, tc.wantCode, goRes.stderr)
			}
			if tc.stdoutPin != "" && !strings.Contains(goRes.stdout, tc.stdoutPin) {
				t.Errorf("Go stdout missing %q: %q", tc.stdoutPin, goRes.stdout)
			}
			if tc.noNode {
				return
			}
			if nodeDistErr != nil {
				t.Skip("dist/src/cli.js not built — Node parity half skipped")
			}
			nodeRes := runIn(t, repo, "node", append([]string{nodeCLI}, tc.args...))
			if nodeRes.code != tc.wantCode {
				t.Errorf("Node exit = %d, want %d (stderr: %s)", nodeRes.code, tc.wantCode, nodeRes.stderr)
			}
			if tc.stdoutPin != "" && !strings.Contains(nodeRes.stdout, tc.stdoutPin) {
				t.Errorf("Node stdout missing %q: %q", tc.stdoutPin, nodeRes.stdout)
			}
		})
	}
}

// TestWiredStubContract pins the exit-3 stub surface after wiring: every
// command still in the notPortedIssue map exits 3 with the pointer message.
func TestWiredStubContract(t *testing.T) {
	stillStubbed := []string{
		"backlog-check", "selfbuild-gate", "board-recovery", "prd-audit",
		"task", "project", "mcp", "preflight",
		"page-degrade-breach", "guard", "guard-status", "automerge", "autosweep",
		"pr-hygiene", "daemon", "tui", "lessons", "queue list",
		"queue show", "queue bridge", "consume", "reap-stale",
	}
	bin := filepath.Join(t.TempDir(), "devagent-go")
	build := exec.Command("go", "build", "-o", bin, "./cmd/devagent")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Go CLI: %v\n%s", err, out)
	}
	t.Setenv("DEVAGENT_HOME", t.TempDir())
	for _, dotted := range stillStubbed {
		res := runIn(t, t.TempDir(), bin, strings.Split(dotted, " "))
		if res.code != 3 {
			t.Errorf("%s exit = %d, want 3 (stderr: %s)", dotted, res.code, res.stderr)
		}
		if !strings.Contains(res.stderr, "not yet ported to Go") {
			t.Errorf("%s stub message missing: %q", dotted, res.stderr)
		}
	}
	// pane-run needs its required flags before it can reach the stub body.
	res := runIn(t, t.TempDir(), bin, []string{"pane-run", "x"})
	if res.code != 1 {
		t.Errorf("pane-run missing flags exit = %d, want 1", res.code)
	}
}

// runIn executes a command with cwd set, capturing the three streams.
func runIn(t *testing.T, dir, bin string, args []string) struct {
	code   int
	stdout string
	stderr string
} {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	return struct {
		code   int
		stdout string
		stderr string
	}{code, stdout.String(), stderr.String()}
}
