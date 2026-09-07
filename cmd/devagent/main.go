// Command devagent is the Go implementation entrypoint (PRD §22, FR-GO).
//
// FR-GO-02: the command tree exists at full parity (internal/cli) with
// scan-text / config / init / trust agents-md implemented; every other
// command exits 3 with a not-ported pointer to its owning FR-GO issue.
// Production keeps the Node entrypoint until the cutover soak gate
// (FR-GO-15, issue #204).
package main

import (
	"github.com/FreePeak/devagent/internal/cli"
)

func main() {
	cli.Execute()
}
