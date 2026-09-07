package cli

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/FreePeak/devagent/internal/gates"
)

// timeNowMs mirrors Date.now().
func timeNowMs() int64 { return time.Now().UnixMilli() }

// marshalIndent mirrors JSON.stringify(x, null, 2) (no trailing newline;
// callers add it like console.log does).
func marshalIndent(v any) string {
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "null"
	}
	return string(blob)
}

// findingsJSON marshals gate findings verbatim for tui.ValidateJSON; an
// empty/nil findings list serializes as [] like the TS gate result.
func findingsJSON(findings []gates.Finding) string {
	if findings == nil {
		findings = []gates.Finding{}
	}
	blob, err := json.Marshal(findings)
	if err != nil {
		return "[]"
	}
	return string(blob)
}

// strconvFormatNumber mirrors JS String(number) for the finite values the
// CLI prints (shortest round-trip decimal form).
func strconvFormatNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
