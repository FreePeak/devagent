package spawn

import (
	"maps"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildEnvScrubBlocklist(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "should-not-leak")
	t.Setenv("CLAUDECODE", "1")
	env := BuildEnv(Options{})
	if _, ok := env["ANTHROPIC_MODEL"]; ok {
		t.Error("ANTHROPIC_MODEL survived the blocklist scrub")
	}
	if _, ok := env["CLAUDECODE"]; ok {
		t.Error("CLAUDECODE survived the blocklist scrub")
	}
}

func TestBuildEnvMergeOverBase(t *testing.T) {
	t.Setenv("BASE_VAR", "base")
	env := BuildEnv(Options{Env: map[string]string{"EXTRA": "x"}})
	if env["BASE_VAR"] != "base" || env["EXTRA"] != "x" {
		t.Fatalf("merge semantics broken: %v", env)
	}
}

func TestBuildEnvReplaceEnv(t *testing.T) {
	t.Setenv("BASE_VAR", "base")
	env := BuildEnv(Options{ReplaceEnv: true, Env: map[string]string{"ONLY": "x"}})
	if _, ok := env["BASE_VAR"]; ok {
		t.Error("ReplaceEnv leaked a base var")
	}
	if env["ONLY"] != "x" {
		t.Error("ReplaceEnv dropped the replacement env")
	}
}

func TestBuildEnvPathFallback(t *testing.T) {
	env := BuildEnv(Options{ReplaceEnv: true, Env: map[string]string{}})
	path := env["PATH"]
	for _, seg := range []string{"/usr/bin", "/bin"} {
		if !strings.Contains(path, seg) {
			t.Errorf("PATH %q missing %s", path, seg)
		}
	}
}

func TestBuildEnvPathNoDuplicate(t *testing.T) {
	full := strings.Join(FALLBACK_PATH_SEGMENTS, ":")
	env := BuildEnv(Options{ReplaceEnv: true, Env: map[string]string{"PATH": full}})
	if env["PATH"] != full {
		t.Errorf("complete PATH was modified: %q", env["PATH"])
	}
}

func TestBuildEnvPwdSync(t *testing.T) {
	dir := t.TempDir()
	env := BuildEnv(Options{Dir: dir})
	if env["PWD"] != dir {
		t.Errorf("PWD = %q, want %q", env["PWD"], dir)
	}
	if _, ok := env["OLDPWD"]; ok {
		t.Error("OLDPWD not deleted")
	}
}

func TestBuildEnvKeepsNonEnvState(t *testing.T) {
	t.Setenv("KEEP_ME", "yes")
	env := BuildEnv(Options{})
	if env["KEEP_ME"] != "yes" {
		t.Error("BuildEnv dropped a non-blocklisted var")
	}
}

func TestRunCliCaptureAndExit(t *testing.T) {
	r := RunCli("sh", []string{"-c", "echo out; echo err >&2; exit 3"}, Options{})
	if r.ExitCode != 3 || r.Stdout != "out\n" || r.Stderr != "err\n" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestRunCliTimeout(t *testing.T) {
	r := RunCli("sh", []string{"-c", "sleep 5"}, Options{TimeoutMs: 100})
	if !r.TimedOut || r.ExitCode != -1 {
		t.Fatalf("want timeout result, got %+v", r)
	}
}

func TestRunCliSpawnFailure(t *testing.T) {
	r := RunCli("definitely-not-a-real-binary-xyz", []string{}, Options{})
	if r.ExitCode != -1 || r.TimedOut {
		t.Fatalf("spawn failure should be exit -1 not timedOut: %+v", r)
	}
}

func TestEnvBlocklistNotInResultEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "x")
	opts := Options{ReplaceEnv: false}
	env := BuildEnv(opts)
	if _, ok := env["ANTHROPIC_MODEL"]; ok {
		t.Error("blocklist var present in built env")
	}
	_ = os.Environ
	_ = maps.Clone[map[string]string]
}

func TestRunCliUntilEarlyCompletion(t *testing.T) {
	// The marker arrives mid-stream while the child is still sleeping; the
	// predicate must fire on the accumulated stdout and return before the
	// child's own exit, with our-kill semantics (ExitCode -1, TimedOut
	// false) and the partial output preserved.
	start := time.Now()
	var seen []string
	r := RunCliUntil("sh", []string{"-c", "printf part1; sleep 0.2; printf part2; sleep 30"}, Options{TimeoutMs: 20_000},
		func(stdout string) bool {
			seen = append(seen, stdout)
			return strings.Contains(stdout, "part1part2")
		})
	if r.ExitCode != -1 || r.TimedOut {
		t.Fatalf("early completion should be exit -1 not timedOut: %+v", r)
	}
	if r.Stdout != "part1part2" {
		t.Fatalf("partial stdout not preserved: %q", r.Stdout)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("early completion did not return promptly: %s", elapsed)
	}
	// The predicate sees ACCUMULATED stdout: the last observation fires only
	// if earlier prefixes were retained, not raw per-chunk reads.
	if len(seen) == 0 || !strings.HasSuffix(seen[len(seen)-1], "part1part2") {
		t.Fatalf("predicate did not receive accumulated stdout: %q", seen)
	}
}

func TestRunCliUntilNilPredicate(t *testing.T) {
	r := RunCliUntil("sh", []string{"-c", "echo out; echo err >&2; exit 3"}, Options{}, nil)
	if r.ExitCode != 3 || r.Stdout != "out\n" || r.Stderr != "err\n" || r.TimedOut {
		t.Fatalf("nil predicate should behave like RunCli: %+v", r)
	}
}

func TestRunCliUntilTimeout(t *testing.T) {
	r := RunCliUntil("sh", []string{"-c", "echo partial; sleep 5"}, Options{TimeoutMs: 100},
		func(string) bool { return false })
	if !r.TimedOut || r.ExitCode != -1 || r.Stdout != "partial\n" {
		t.Fatalf("want timeout with partial stdout, got %+v", r)
	}
}

func TestRunCliUntilSpawnFailure(t *testing.T) {
	r := RunCliUntil("definitely-not-a-real-binary-xyz", []string{}, Options{}, nil)
	if r.ExitCode != -1 || r.TimedOut {
		t.Fatalf("spawn failure should be exit -1 not timedOut: %+v", r)
	}
}
