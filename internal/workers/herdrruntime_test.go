// Tests for the herdr-runtime routing decision + loud once-per-site
// fallback (FR-VIS-01) and the FR-GROK-04 cache-key env channel.
package workers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBinIn writes an executable into dir (which the caller puts on PATH)
// and returns its path.
func fakeBinIn(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestShouldUseHerdr_Precedence(t *testing.T) {
	os.Unsetenv("DEVAGENT_HERDR")
	os.Unsetenv("DEVAGENT_VISIBILITY")
	SpawnVisibilityConfig = nil

	// Explicit wins over everything.
	f := false
	tr := true
	if ShouldUseHerdr(&f) {
		t.Fatal("explicit false must force direct spawn")
	}
	if !ShouldUseHerdr(&tr) {
		t.Fatal("explicit true must route to panes")
	}

	// DEVAGENT_HERDR legacy env-wide override stays ahead of visibility.
	for raw, want := range map[string]bool{"1": true, "0": false, "false": false} {
		t.Setenv("DEVAGENT_HERDR", raw)
		if got := ShouldUseHerdr(nil); got != want {
			t.Fatalf("DEVAGENT_HERDR=%q → %v, want %v", raw, got, want)
		}
	}
	os.Unsetenv("DEVAGENT_HERDR")

	// DEVAGENT_VISIBILITY: headless forces direct spawn.
	t.Setenv("DEVAGENT_VISIBILITY", "headless")
	if ShouldUseHerdr(nil) {
		t.Fatal("DEVAGENT_VISIBILITY=headless must force direct spawn")
	}
	t.Setenv("DEVAGENT_VISIBILITY", "visible")
	if !ShouldUseHerdr(nil) {
		t.Fatal("DEVAGENT_VISIBILITY=visible must route to panes")
	}

	// Default (no env): visible — FR-VIS-01 flips the historical default so
	// worker launches are observable unless the operator opts out.
	os.Unsetenv("DEVAGENT_VISIBILITY")
	if !ShouldUseHerdr(nil) {
		t.Fatal("default visibility must route to panes (FR-VIS-01)")
	}
}

// The loud fallback: no herdr runner registered → the launch still runs
// (direct child), and the warning fires exactly once per spawn site.
func TestRunWorkerCli_LoudFallbackOncePerSite(t *testing.T) {
	t.Setenv("DEVAGENT_VISIBILITY", "visible")

	var warnings []string
	SetFallbackSink(func(site, message string) { warnings = append(warnings, site+": "+message) })
	defer SetFallbackSink(nil)

	budget := 5_000
	opts := RunWorkerCliOptions{SpawnCliOptions: SpawnCliOptions{TimeoutMs: budget}}

	// Absolute binary paths keep this test independent of PATH resolution
	// (the SpawnCli PATH-fallback itself is covered by spawnstream tests).
	site1 := fakeBin(t, "fake-site-worker-1", "echo ok")
	res := RunWorkerCli(site1, nil, opts)
	if res.ExitCode != 0 {
		t.Fatalf("fallback must still run the worker: %+v", res)
	}
	if len(warnings) != 1 || !contains(warnings[0], "directly") {
		t.Fatalf("expected exactly one loud fallback warning, got %v", warnings)
	}

	// Second launch on the same site: no duplicate warning (once per site).
	res = RunWorkerCli(site1, nil, opts)
	if res.ExitCode != 0 {
		t.Fatalf("second launch failed: %+v", res)
	}
	if len(warnings) != 1 {
		t.Fatalf("fallback must warn once per site, got %d: %v", len(warnings), warnings)
	}

	// A different site warns separately.
	site2 := fakeBin(t, "fake-site-worker-2", "echo ok")
	_ = RunWorkerCli(site2, nil, opts)
	if len(warnings) != 2 {
		t.Fatalf("second site must warn separately, got %v", warnings)
	}
}

// FR-GROK-04 Seam E: the per-task cache key rides the prepareWorkerSpawn
// env channel (never argv) and wins over any inherited value. Pinned at
// the seam the child process receives its env from (the prepared spawn
// options); child-execution itself is covered by the fake-bin spawn tests.
func TestGrokAdapter_CacheKeyThroughEnvChannel(t *testing.T) {
	t.Setenv(GrokPromptCacheKeyEnv, "stale-key")

	var preparedEnv map[string]string
	var argv []string
	adapter := &GrokAdapter{}
	adapter.Prepare = func(cmd string, args []string, opts SpawnCliOptions) (PreparedWorkerSpawn, error) {
		// Real sandbox scrub + merge, exactly like production.
		return PrepareWorkerSpawn(cmd, args, opts)
	}
	adapter.Run = func(cmd string, args []string, opts RunWorkerCliOptions) SpawnCliResult {
		preparedEnv = opts.Env
		argv = args
		// The child receives opts.Env verbatim (ReplaceEnv=true).
		return SpawnCliResult{ExitCode: 0, Stdout: `{"type":"end","sessionId":"gk-1","stopReason":"end_turn"}`}
	}

	res := adapter.Spawn(WorkerSpawnOptions{
		Prompt:         "echo the cache key",
		Cwd:            t.TempDir(),
		TimeoutMs:      15_000,
		Env:            map[string]string{"DUMP": "/tmp/dump"},
		WatchdogLedger: &WatchdogLedgerContext{RepoPath: "/r", TaskId: "TASK-1", Attempt: 1, Worker: "grok"},
	})
	if res.ExitCode != 0 || res.SessionId != "gk-1" {
		t.Fatalf("result = %+v", res)
	}
	if preparedEnv[GrokPromptCacheKeyEnv] != "devagent-TASK-1" {
		t.Fatalf("per-task key must win over the inherited value: %q", preparedEnv[GrokPromptCacheKeyEnv])
	}
	if contains(strings.Join(argv, " "), "devagent-TASK-1") {
		t.Fatalf("the key must never ride argv: %v", argv)
	}
	if preparedEnv["DUMP"] != "/tmp/dump" {
		t.Fatalf("caller env extras must survive the merge: %v", preparedEnv["DUMP"])
	}
	// The scrubbed parent PATH must reach the child (worker finds its CLI).
	if _, ok := preparedEnv["PATH"]; !ok {
		t.Fatal("scrubbed env must keep PATH (worker CLI discovery)")
	}
}
