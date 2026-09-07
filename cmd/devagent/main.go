// Command devagent is the Go implementation entrypoint (PRD §22, FR-GO).
//
// G0 scaffold only: this binary reports its version and otherwise refuses to
// run, so it can never be mistaken for a partial port. Command parity arrives
// with FR-GO-02 (issue #193); production keeps the Node entrypoint until the
// cutover soak gate (FR-GO-15, issue #204).
package main

import (
	"fmt"
	"os"

	"github.com/FreePeak/devagent/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "devagent (Go scaffold): no commands yet — see PRD §22 and issue #193; the Node CLI remains the production entrypoint")
	os.Exit(2)
}
