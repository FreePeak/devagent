package ledger

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// This file renders the `devagent ledger --clusters` surface (the ledger
// half of src/commands/fr-simple-wire.ts) so the CLI wiring is a pure
// flag->call mapping. Output must be byte-identical to the Node CLI for the
// same fixture input, text and --json alike.

// RenderClustersText renders the --clusters body lines. top comes from
// `--clusters [n]` (default 5); rawOpt is the raw option string for the
// invalid-value message (`--clusters abc`). Mirrors the Node logic:
//   - non-positive/non-numeric top  -> "Nothing to show for --clusters <raw>."
//   - both views empty              -> the "No failure clusters." pointer line
//   - else ranked failure clusters, then ranked failure classes.
func RenderClustersText(clusters []FailureCluster, classes []FailureClassCluster, top int, rawOpt string) []string {
	if top <= 0 {
		return []string{"Nothing to show for --clusters " + rawOpt + "."}
	}
	if len(clusters) == 0 && len(classes) == 0 {
		return []string{
			"No failure clusters. Failed audits with unmet criteria and taskInterrupt executor events cluster here once the ledger has records.",
		}
	}
	var lines []string
	if len(clusters) > 0 {
		shown := min(top, len(clusters))
		lines = append(lines, "failure clusters (top "+strconv.Itoa(shown)+" of "+strconv.Itoa(len(clusters))+"):")
		for _, c := range clusters[:shown] {
			lines = append(lines, `- "`+c.Criterion+`" — `+strconv.Itoa(c.Occurrences)+
				` occurrence(s) across `+strconv.Itoa(len(c.Tasks))+` task(s) (`+
				strconv.Itoa(c.OpenTasks)+` still open): `+strings.Join(c.Tasks, ", "))
		}
	}
	if len(classes) > 0 {
		shown := min(top, len(classes))
		lines = append(lines, "failure classes (top "+strconv.Itoa(shown)+" of "+strconv.Itoa(len(classes))+"):")
		for _, c := range classes[:shown] {
			line := `- "` + c.FailureClass + `" — ` + strconv.Itoa(c.Occurrences) +
				` interrupt(s) across ` + strconv.Itoa(len(c.Tasks)) + ` task(s): ` + strings.Join(c.Tasks, ", ")
			if c.Exemplar != "" {
				line += ` | exemplar: "` + truncateUTF16(c.Exemplar, 120) + `"`
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// clustersPayload mirrors the --json shape:
// JSON.stringify({ clusters: [...], failureClasses: [...] }, null, 2), with
// the top-N slice applied. Returns the indented JSON with trailing newline
// (console.log's newline).
func ClustersJSON(clusters []FailureCluster, classes []FailureClassCluster, limit int) ([]byte, error) {
	if limit <= 0 {
		limit = 5
	}
	if clusters == nil {
		clusters = []FailureCluster{}
	}
	if classes == nil {
		classes = []FailureClassCluster{}
	}
	payload := struct {
		Clusters       []FailureCluster      `json:"clusters"`
		FailureClasses []FailureClassCluster `json:"failureClasses"`
	}{
		Clusters:       clusters[:min(limit, len(clusters))],
		FailureClasses: classes[:min(limit, len(classes))],
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // JSON.stringify does not escape HTML characters
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
