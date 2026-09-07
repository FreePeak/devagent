// Package version is the single source of truth for the devagent version,
// mirroring src/version.ts in the TypeScript implementation (FR-GO-02 keeps
// them in lockstep until Node retirement, FR-GO-16).
package version

// Version is the CLI version reported by `devagent --version` and embedded
// in ledger metadata. Must equal the DEVAGENT_VERSION in src/version.ts.
const Version = "0.1.0"
