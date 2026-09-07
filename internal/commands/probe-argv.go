package commands

import (
	"strings"

	"github.com/FreePeak/devagent/internal/workers/modelid"
)

// BuildProbeArgvFor is the Go port of src/commands/probe-argv.ts: probe argv
// for guided setup (`devagent init`, FR-SIMPLE-01), extracted from cli.ts's
// operator-preflight helper so both surfaces share one definition. Mirrors
// scripts/orchestrate-loop.sh: omp gets the headless hardening flags and
// requires a provider-qualified model id — an unqualified alias is dropped so
// the CLI default applies (same normalization buildOmpArgs does); grok gets
// the headless streaming-json form with the FR-GROK-02 exact-slug/`xai/`
// model predicate; the other workers take a plain prompt.
func BuildProbeArgvFor(worker string, model string) []string {
	if worker == "omp" {
		var ompModel string
		if model != "" && strings.Contains(model, "/") {
			ompModel = model
		}
		argv := []string{"omp", "-p", "--mode", "json", "--no-prewalk", "--no-lsp", "--no-extensions"}
		if ompModel != "" {
			argv = append(argv, "--model", ompModel)
		}
		return argv
	}
	if worker == "grok" {
		// Same normalization buildGrokArgs applies: only exact xAI slugs or
		// `xai/`-qualified ids reach the CLI; anything else falls back to the
		// grok-configured default model.
		grokModel := ""
		if model != "" && modelid.IsGrokModelId(model) {
			grokModel = strings.TrimSpace(model)
		}
		argv := []string{"grok", "-p", "--output-format", "streaming-json"}
		if grokModel != "" {
			argv = append(argv, "--model", grokModel)
		}
		return argv
	}
	return []string{worker, "-p"}
}
