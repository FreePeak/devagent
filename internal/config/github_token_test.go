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
	dir := t.TempDir()
	counter := installFakeGh(t, dir, "gh-must-not-run", 0)
	t.Setenv("PATH", dir)
	t.Setenv("GITHUB_TOKEN", "env-token")
	resetGithubTokenCacheForTest()

	if got := LoadCredentials().GithubToken; got != "env-token" {
		t.Fatalf("GithubToken = %q, want env value", got)
	}
	if _, err := os.Stat(counter); !os.IsNotExist(err) {
		t.Fatalf("gh was invoked despite env GITHUB_TOKEN (counter: %v)", err)
	}
}

func TestResolveGithubTokenGhFallback(t *testing.T) {
	dir := t.TempDir()
	installFakeGh(t, dir, "  gh-keyring-token  ", 0) // padded: resolution must trim
	t.Setenv("PATH", dir)
	unsetGithubTokenForTest(t)
	resetGithubTokenCacheForTest()

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
	dir := t.TempDir()
	installFakeGh(t, dir, "gh-empty-env", 0)
	t.Setenv("PATH", dir)
	t.Setenv("GITHUB_TOKEN", "") // empty counts as unset
	resetGithubTokenCacheForTest()

	if got := LoadCredentials().GithubToken; got != "gh-empty-env" {
		t.Fatalf("GithubToken = %q, want gh fallback for empty env", got)
	}
}

func TestResolveGithubTokenCachedPerProcess(t *testing.T) {
	dir := t.TempDir()
	counter := installFakeGh(t, dir, "gh-cached", 0)
	t.Setenv("PATH", dir)
	unsetGithubTokenForTest(t)
	resetGithubTokenCacheForTest()

	if got := LoadCredentials().GithubToken; got != "gh-cached" {
		t.Fatalf("first call = %q, want gh token", got)
	}
	if got := LoadCredentials().GithubToken; got != "gh-cached" {
		t.Fatalf("second call = %q, want cached gh token", got)
	}
	if data, err := os.ReadFile(counter); err != nil || string(data) != "x" {
		t.Fatalf("gh invocation count wrong: data=%q err=%v, want exactly one exec", data, err)
	}

	// A later-set env GITHUB_TOKEN still wins over the primed cache.
	t.Setenv("GITHUB_TOKEN", "later-env")
	if got := LoadCredentials().GithubToken; got != "later-env" {
		t.Fatalf("GithubToken = %q, want later env value over cache", got)
	}
	if data, err := os.ReadFile(counter); err != nil || string(data) != "x" {
		t.Fatalf("env-wins check re-executed gh: data=%q err=%v", data, err)
	}
}
