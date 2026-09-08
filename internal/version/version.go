// Package version is the single source of truth for the devagent version.
package version

// Version is the CLI version reported by `devagent --version` and embedded
// in ledger metadata. A var (not a const) so -ldflags -X can stamp it at
// build time; the unstamped default stays 0.1.0.
var Version = "0.1.0"
