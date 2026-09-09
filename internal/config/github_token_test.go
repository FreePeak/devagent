package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// resetGithubTokenCacheForTest clears the process-level gh fallback cache so
// each test exercises a fresh resolution; the sync.Once otherwise persists
// across tests in the same test binary.
func resetGithubTokenCacheForTest() {
	githubTokenOnce.Once = sync.Once{}
	githubTokenOnce.value = ""
}

// installFakeGh writes a fake `gh` executable into dir and returns the path
// of its invocation counter (one "x" appended per exec). The script prints
// token on stdout and exits with exit.
func installFakeGh(t *testing.T, dir, token string, exit int) string {
	t.Helper()
	counter := filepath.Join(dir, "gh.invocations")
	script := fmt.Sprintf("#!/bin/sh\nprintf x >> %q\nprintf '%%s\\n' %q\nexit %d\n", counter, token, exit)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return counter
}

// stubGhAuthToken replaces the gh resolver seam with a function returning
// token (or empty) and counting invocations in *int. Restores the real
// implementation on cleanup and resets the process-level cache (issue #262:
// no real exec under test, so the 5s timeout cannot flake under load).
func stubGhAuthToken(t *testing.T, token string, calls *int) {
	t.Helper()
	old := ghAuthToken
	ghAuthToken = func() string {
		*calls++
		return token
	}
	t.Cleanup(func() { ghAuthToken = old })
	resetGithubTokenCacheForTest()
}

// unsetGithubTokenForTest makes GITHUB_TOKEN genuinely absent (not just
// empty) for the test and restores the original state afterwards.
func unsetGithubTokenForTest(t *testing.T) {
	t.Helper()
	old, had := os.LookupEnv("GITHUB_TOKEN")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("GITHUB_TOKEN", old)
		} else {
			_ = os.Unsetenv("GITHUB_TOKEN")
		}
	})
	_ = os.Unsetenv("GITHUB_TOKEN")
}

func TestResolveGithubTokenEnvWins(t *testing.T) {
	calls := 0
	stubGhAuthToken(t, "gh-must-not-run", &calls)
	t.Setenv("GITHUB_TOKEN", "env-token")

	if got := LoadCredentials().GithubToken; got != "env-token" {
		t.Fatalf("GithubToken = %q, want env value", got)
	}
	if calls != 0 {
		t.Fatalf("gh resolver invoked %d times despite env GITHUB_TOKEN, want 0", calls)
	}
}

func TestResolveGithubTokenGhFallback(t *testing.T) {
	calls := 0
	stubGhAuthToken(t, "  gh-keyring-token  ", &calls) // padded: resolution must trim
	unsetGithubTokenForTest(t)

	if got := LoadCredentials().GithubToken; got != "gh-keyring-token" {
		t.Fatalf("GithubToken = %q, want trimmed gh output", got)
	}
}

func TestResolveGithubTokenGhMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no gh anywhere on PATH
	unsetGithubTokenForTest(t)
	resetGithubTokenCacheForTest()

	if got := LoadCredentials().GithubToken; got != "" {
		t.Fatalf("GithubToken = %q, want empty when gh is absent", got)
	}
}

func TestResolveGithubTokenGhFailure(t *testing.T) {
	dir := t.TempDir()
	installFakeGh(t, dir, "partial-garbage", 1)
	t.Setenv("PATH", dir)
	unsetGithubTokenForTest(t)
	resetGithubTokenCacheForTest()

	if got := LoadCredentials().GithubToken; got != "" {
		t.Fatalf("GithubToken = %q, want empty on gh non-zero exit", got)
	}
}

func TestResolveGithubTokenEmptyEnvFallsThrough(t *testing.T) {
	calls := 0
	stubGhAuthToken(t, "gh-empty-env", &calls)
	t.Setenv("GITHUB_TOKEN", "") // empty counts as unset

	if got := LoadCredentials().GithubToken; got != "gh-empty-env" {
		t.Fatalf("GithubToken = %q, want gh fallback for empty env", got)
	}
}

func TestResolveGithubTokenCachedPerProcess(t *testing.T) {
	calls := 0
	stubGhAuthToken(t, "gh-cached", &calls)
	unsetGithubTokenForTest(t)

	if got := LoadCredentials().GithubToken; got != "gh-cached" {
		t.Fatalf("first call = %q, want gh token", got)
	}
	if got := LoadCredentials().GithubToken; got != "gh-cached" {
		t.Fatalf("second call = %q, want cached gh token", got)
	}
	if calls != 1 {
		t.Fatalf("gh resolver invoked %d times, want exactly one exec", calls)
	}

	// A later-set env GITHUB_TOKEN still wins over the primed cache.
	t.Setenv("GITHUB_TOKEN", "later-env")
	if got := LoadCredentials().GithubToken; got != "later-env" {
		t.Fatalf("GithubToken = %q, want later env value over cache", got)
	}
	if calls != 1 {
		t.Fatalf("env-wins check re-executed gh: %d calls", calls)
	}
}
