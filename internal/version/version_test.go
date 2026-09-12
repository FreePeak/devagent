package version

import (
	"regexp"
	"testing"
)

// semverRe pins the shape of Version (MAJOR.MINOR.PATCH). Release tooling
// (scripts/release/nextversion) and the git v* tags emit this shape;
// a drift means someone hand-edited the constant.
var semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// TestVersionIsSemver checks the self-contained contract: the constant is
// non-empty and a bare semantic version, so --version output and ledger
// metadata stay parseable without any external file to read.
func TestVersionIsSemver(t *testing.T) {
	if Version == "" {
		t.Fatal("Version is empty")
	}
	if !semverRe.MatchString(Version) {
		t.Fatalf("Version = %q, want MAJOR.MINOR.PATCH", Version)
	}
}

// TestRevision pins the stale-binary sentinel contract: under go test the
// binary carries no vcs.revision build setting (probed 2026-09-12), so the
// unstamped path returns RevisionUnknown, repeat calls agree (memoized),
// and an override — the test-only injection surface — wins over whatever
// was memoized.
func TestRevision(t *testing.T) {
	old := RevisionOverride
	t.Cleanup(func() { RevisionOverride = old })

	RevisionOverride = ""
	if got := Revision(); got != RevisionUnknown {
		t.Fatalf("Revision() = %q under go test, want %q", got, RevisionUnknown)
	}
	if a, b := Revision(), Revision(); a != b {
		t.Fatalf("Revision not memoized: %q vs %q", a, b)
	}
	RevisionOverride = "deadbeef"
	if got := Revision(); got != "deadbeef" {
		t.Fatalf("Revision() = %q, want the override deadbeef", got)
	}
}
