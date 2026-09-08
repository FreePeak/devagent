package version

import (
	"regexp"
	"testing"
)

// semverRe pins the shape of Version (MAJOR.MINOR.PATCH). Release tooling
// (scripts/release/next-version.mjs) and the git v* tags emit this shape;
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
