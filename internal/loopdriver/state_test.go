package loopdriver

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestMergeLedgerLinesLaterTSMachineWins(t *testing.T) {
	remote := `{"loop":1,"ts":"2026-09-01T00:00:00Z","status":"ok","goal":"old one"}
{"loop":3,"ts":"2026-09-02T00:00:00Z","status":"failed","goal":"remote three"}
`
	local := `{"loop":1,"ts":"2026-09-03T00:00:00Z","status":"ok","goal":"new one"}
{"loop":2,"ts":"2026-09-02T12:00:00Z","status":"skipped","goal":"local two"}
`
	got := mergeLedgerLines(remote, local)
	want := `{"loop":1,"ts":"2026-09-03T00:00:00Z","status":"ok","goal":"new one"}
{"loop":2,"ts":"2026-09-02T12:00:00Z","status":"skipped","goal":"local two"}
{"loop":3,"ts":"2026-09-02T00:00:00Z","status":"failed","goal":"remote three"}
`
	if got != want {
		t.Fatalf("merge mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestMergeLedgerLinesBlankAndKeylessDropped(t *testing.T) {
	remote := "\n{\"loop\":1,\"ts\":\"2026-09-01T00:00:00Z\",\"status\":\"ok\",\"goal\":\"a\"}\nnot json\n"
	local := ""
	got := mergeLedgerLines(remote, local)
	want := "{\"loop\":1,\"ts\":\"2026-09-01T00:00:00Z\",\"status\":\"ok\",\"goal\":\"a\"}\n"
	if got != want {
		t.Fatalf("merge mismatch:\ngot: %q\nwant: %q", got, want)
	}
}

func TestDedupeLinesKeepsFirstOccurrence(t *testing.T) {
	in := "a\nb\na\nc\nb\n"
	if got, want := dedupeLines(in), "a\nb\nc\n"; got != want {
		t.Fatalf("dedupe: got %q want %q", got, want)
	}
	if got := dedupeLines(""); got != "" {
		t.Fatalf("dedupe empty: got %q", got)
	}
}

func TestStatePullFreshRemote(t *testing.T) {
	repo := initFixtureRepo(t)
	addBareOrigin(t, repo)
	var out bytes.Buffer
	s := newStateSync(repo, &out, time.Now)
	if err := s.Pull(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[state] no remote state yet (selfbuild/state) — starting fresh") {
		t.Fatalf("unexpected pull output: %q", out.String())
	}
}

func TestStatePullMergeAndPush(t *testing.T) {
	repo := initFixtureRepo(t)
	bare := addBareOrigin(t, repo)

	// Simulate a previous driver: seed the remote state branch with one
	// ledger row via a throwaway clone.
	seed := filepath.Join(t.TempDir(), "seed")
	runGit(t, t.TempDir(), "clone", "--quiet", bare, seed)
	writeRepoFile(t, seed, ".selfbuild/ledger.jsonl",
		"{\"loop\":7,\"ts\":\"2026-09-01T00:00:00Z\",\"status\":\"ok\",\"goal\":\"seeded\"}\n")
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-q", "-m", "seed")
	runGit(t, seed, "push", "-q", "origin", "HEAD:refs/heads/selfbuild/state")

	var out bytes.Buffer
	s := newStateSync(repo, &out, time.Now)
	if err := s.Pull(); err != nil {
		t.Fatal(err)
	}
	assertFileContains(t, s.ledgerPath(), "\"loop\":7")
	if !strings.Contains(out.String(), "[state] pulled 1 ledger entries into") {
		t.Fatalf("unexpected pull output: %q", out.String())
	}

	// Local iteration appends row 8; push publishes both.
	f, err := os.OpenFile(s.ledgerPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{\"loop\":8,\"ts\":\"2026-09-02T00:00:00Z\",\"status\":\"ok\",\"goal\":\"local\"}\n")
	_ = f.Close()
	out.Reset()
	if err := s.Push(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[state] pushed 2 ledger entries to selfbuild/state") {
		t.Fatalf("unexpected push output: %q", out.String())
	}

	// The bare repo's branch carries both rows under .selfbuild/.
	runGit(t, repo, "fetch", "--quiet", "origin", StateBranch+":"+RemoteStateRef)
	show := runGit(t, repo, "show", RemoteStateRef+":.selfbuild/ledger.jsonl")
	if !strings.Contains(show, "seeded") || !strings.Contains(show, "local") {
		t.Fatalf("pushed state missing rows: %q", show)
	}
}

func TestStatePushRetryAfterRace(t *testing.T) {
	repo := initFixtureRepo(t)
	bare := addBareOrigin(t, repo)

	// First driver pushes state.
	seed := filepath.Join(t.TempDir(), "seed")
	runGit(t, t.TempDir(), "clone", "--quiet", bare, seed)
	writeRepoFile(t, seed, ".selfbuild/ledger.jsonl",
		"{\"loop\":1,\"ts\":\"2026-09-01T00:00:00Z\",\"status\":\"ok\",\"goal\":\"raced\"}\n")
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-q", "-m", "seed")
	runGit(t, seed, "push", "-q", "origin", "HEAD:refs/heads/selfbuild/state")

	// Our driver pulls that state, then the remote advances (the race)
	// before our push.
	var out bytes.Buffer
	s := newStateSync(repo, &out, time.Now)
	if err := s.Pull(); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(s.ledgerPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("{\"loop\":2,\"ts\":\"2026-09-02T00:00:00Z\",\"status\":\"ok\",\"goal\":\"ours\"}\n")
	_ = f.Close()

	writeRepoFile(t, seed, ".selfbuild/lessons.md", "lesson: race winner\n")
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-q", "-m", "race")
	runGit(t, seed, "push", "-q", "origin", "HEAD:refs/heads/selfbuild/state")

	out.Reset()
	if err := s.Push(); err != nil {
		t.Fatalf("push should survive the race via retry: %v", err)
	}

	// The retry must have folded the raced remote row in: merged ledger has
	// both loops and the lessons ratchet kept the raced lesson line.
	merged, err := os.ReadFile(s.ledgerPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"raced", "ours"} {
		if !strings.Contains(string(merged), want) {
			t.Fatalf("merged ledger missing %q: %s", want, merged)
		}
	}
	lessons, err := os.ReadFile(s.lessonsPath())
	if err != nil || !strings.Contains(string(lessons), "race winner") {
		t.Fatalf("lessons merge lost raced line: %q err=%v", lessons, err)
	}
}

func TestStatePushNothingWhenLedgerMissing(t *testing.T) {
	repo := initFixtureRepo(t)
	addBareOrigin(t, repo)
	var out bytes.Buffer
	s := newStateSync(repo, &out, time.Now)
	if err := s.Push(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[state] nothing to push:") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}
