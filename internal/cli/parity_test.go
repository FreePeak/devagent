package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type surfaceEntry struct {
	Flags       []string `json:"flags"`
	Subcommands []string `json:"subcommands"`
}

// TestSurfaceParity is the FR-GO-02 acceptance gate: the cobra tree must
// expose every command/subcommand and flag the Node CLI exposes, and
// nothing beyond them. The fixture is generated from the live Node CLI
// (scripts/go/extract-commands.mjs after `npm run build`); a surface change
// on either side fails here.
func TestSurfaceParity(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "commands.json"))
	if err != nil {
		t.Fatalf("fixture missing — regenerate with scripts/go/extract-commands.mjs: %v", err)
	}
	var want map[string]surfaceEntry
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}

	got := map[string]surfaceEntry{}
	collect(NewRoot(), "", got)

	// The fixture records every command as a dotted key; subcommands of a
	// parent are the keys one segment deeper (the extractor's recorded
	// subcommands array is always empty — derive from keys instead).
	expected := map[string]surfaceEntry{}
	for path, entry := range want {
		expected[path] = surfaceEntry{Flags: entry.Flags}
	}
	for path := range want {
		for other := range want {
			rest, ok := strings.CutPrefix(other, path+" ")
			if ok && !strings.Contains(rest, " ") {
				e := expected[path]
				e.Subcommands = append(e.Subcommands, rest)
				expected[path] = e
			}
		}
		sort.Strings(expected[path].Subcommands)
	}

	for path, w := range expected {
		g, ok := got[path]
		if !ok {
			t.Errorf("missing command %q in Go tree", path)
			continue
		}
		if !equalStrings(g.Flags, w.Flags) {
			t.Errorf("flags for %q:\n got: %v\nwant: %v", path, g.Flags, w.Flags)
		}
		if !equalStrings(g.Subcommands, w.Subcommands) {
			t.Errorf("subcommands for %q:\n got: %v\nwant: %v", path, g.Subcommands, w.Subcommands)
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Errorf("extra command %q in Go tree (Node surface does not have it)", path)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func collect(cmd *cobra.Command, prefix string, out map[string]surfaceEntry) {
	for _, c := range cmd.Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		path := c.Name()
		if prefix != "" {
			path = prefix + " " + c.Name()
		}
		var flags []string
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if len(f.Name) > 1 && !f.Hidden {
				flags = append(flags, "--"+f.Name)
			}
		})
		var subs []string
		for _, sc := range c.Commands() {
			if sc.Name() != "help" && sc.Name() != "completion" {
				subs = append(subs, sc.Name())
			}
		}
		sort.Strings(flags)
		sort.Strings(subs)
		out[path] = surfaceEntry{Flags: flags, Subcommands: subs}
		collect(c, path, out)
	}
}

// TestExitParityVsNode runs identical invocations against the Go binary and
// the Node CLI and compares exit codes (and stdout where the output is
// byte-stable: --version, scan-text). The Node half skips when dist/ is
// absent so the Go suite stays runnable without a Node build.
func TestExitParityVsNode(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "devagent-go")
	build := exec.Command("go", "build", "-o", bin, "./cmd/devagent")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Go CLI: %v\n%s", err, out)
	}

	nodeCLI := "../../dist/src/cli.js"
	if _, err := os.Stat(nodeCLI); err != nil {
		t.Skip("dist/src/cli.js not built — Node parity half skipped")
	}

	cases := []struct {
		name     string
		args     []string
		wantCode int
		// wantStdout pins exact stdout on both sides (nil = don't compare).
		wantStdout []string
	}{
		{"version", []string{"--version"}, 0, []string{"0.1.0"}},
		{"scan-text", []string{"scan-text"}, 0, nil}, // compared node-vs-go below
		{"missing required run flag", []string{"run"}, 1, nil},
		{"missing required orchestrate flag", []string{"orchestrate"}, 1, nil},
		{"unknown command", []string{"definitely-not-a-command"}, 1, nil},
		{"init help", []string{"init", "--help"}, 0, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			goRes := runCLI(t, bin, tc.args)
			if goRes.code != tc.wantCode {
				t.Errorf("Go exit = %d, want %d (stderr: %s)", goRes.code, tc.wantCode, goRes.stderr)
			}
			nodeRes := runCLI(t, "node", append([]string{nodeCLI}, tc.args...))
			if nodeRes.code != tc.wantCode {
				t.Errorf("Node exit = %d, want %d (stderr: %s)", nodeRes.code, tc.wantCode, nodeRes.stderr)
			}
			for _, pin := range tc.wantStdout {
				if !strings.Contains(goRes.stdout, pin) {
					t.Errorf("Go stdout missing %q: %q", pin, goRes.stdout)
				}
				if !strings.Contains(nodeRes.stdout, pin) {
					t.Errorf("Node stdout missing %q: %q", pin, nodeRes.stdout)
				}
			}
			if tc.name == "scan-text" && goRes.stdout != nodeRes.stdout {
				t.Errorf("scan-text output drifted:\nGo:   %q\nNode: %q", goRes.stdout, nodeRes.stdout)
			}
		})
	}

	// Stub contract: an implemented-surface command that is not yet ported
	// (`mcp`, FR-GO) exits 3 in Go (Node runs real behavior — only the Go
	// side is asserted). `run`/`fleet`/`orchestrate` carry required flags,
	// so they fail at parse time (exit 1) before reaching the stub body.
	res := runCLI(t, bin, []string{"mcp"})
	if res.code != 3 {
		t.Errorf("stub exit = %d, want 3 (stderr: %s)", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "not yet ported to Go") {
		t.Errorf("stub message missing: %q", res.stderr)
	}

	// trust agents-md writes the same trust record shape on both sides.
	dir := t.TempDir()
	res = runCLI(t, bin, []string{"trust", "agents-md", "--repo", dir})
	if res.code != 0 {
		t.Fatalf("trust agents-md exit = %d (stderr: %s)", res.code, res.stderr)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".devagent", "trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec["agentsMd"] != true {
		t.Fatalf("trust record wrong: %s", raw)
	}
}

func runCLI(t *testing.T, bin string, args []string) struct {
	code   int
	stdout string
	stderr string
} {
	t.Helper()
	cmd := exec.Command(bin, args...)
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

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}
