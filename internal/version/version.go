// Package version is the single source of truth for the devagent version.
package version

import (
	"runtime/debug"
	"sync"
)

// Version is the CLI version reported by `devagent --version` and embedded
// in ledger metadata. A var (not a const) so -ldflags -X can stamp it at
// build time; the unstamped default stays 0.1.0.
var Version = "0.1.0"

// RevisionUnknown is what Revision returns when the binary was built
// without a VCS stamp (go-test binaries, builds from a tarball): it cannot
// prove which commit it drives, so consumers must not treat it as clean.
const RevisionUnknown = "unknown"

// RevisionOverride injects the build revision in tests — go-test binaries
// carry no vcs.revision build setting, so the real stamp is unreachable
// there. A non-empty value wins over the memoized build info; production
// leaves it empty (same stampable-var shape as Version).
var RevisionOverride string

var (
	revisionOnce sync.Once
	revisionMemo string
)

// Revision returns the VCS revision the binary was built from
// (debug.ReadBuildInfo's vcs.revision), memoized on first call, or
// RevisionUnknown when the build carries no stamp.
func Revision() string {
	revisionOnce.Do(func() {
		rev := RevisionUnknown
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && s.Value != "" {
					rev = s.Value
					break
				}
			}
		}
		revisionMemo = rev
	})
	if RevisionOverride != "" {
		return RevisionOverride
	}
	return revisionMemo
}
