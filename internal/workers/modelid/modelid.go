// Package modelid is the Go port of src/workers/model-id.ts (Q32): one
// registry declaring each adapter's accepted `config.model` id shape, so
// dispatch preflight rejects unsupported ids in seconds instead of letting
// adapters burn attempts mid-board. Error strings are byte-identical to the
// TypeScript originals — tests pin them and callers embed them in ledger
// rows.
package modelid

import (
	"regexp"
	"strings"
)

// GROK_EXACT_SLUG matches exact xAI model slugs ("grok-4.6", dated pins like
// "grok-4.6-2026-08-14"). Driver tier aliases ("coding") and bare config
// aliases are not grok ids — the loop-58 `--model coding` burn is the
// precedent for rejecting them at the gate.
var GROK_EXACT_SLUG = regexp.MustCompile(`^grok-[a-z0-9][a-z0-9.-]*$`)

// IsGrokModelId reports whether the model is an exact grok slug or
// `xai/`-qualified.
func IsGrokModelId(model string) bool {
	raw := strings.TrimSpace(model)
	if raw == "" {
		return false
	}
	return strings.HasPrefix(raw, "xai/") || GROK_EXACT_SLUG.MatchString(raw)
}

// providerQualifiedReason mirrors providerQualifiedReason() in the TS
// registry: adapters that require provider-qualified ids ("provider/model",
// fuzzy matched; multi-segment providers like omniroute/bai/glm-5.3-flash are
// real ids). Returns "" when acceptable.
func providerQualifiedReason(worker, model string) string {
	raw := strings.TrimSpace(model)
	if raw == "" {
		return "" // unset = adapter default
	}
	if strings.Contains(raw, "/") {
		return "" // provider-qualified: pass through
	}
	return "worker \"" + worker + "\" requires a provider-qualified model id (\"provider/model\", e.g. omniroute/bai/glm-5.3-flash); got \"" + raw + "\" (driver tier aliases like \"coding\" are not valid " + worker + " ids)"
}

func grokModelIdReason(model string) string {
	raw := strings.TrimSpace(model)
	if raw == "" {
		return ""
	}
	if IsGrokModelId(raw) {
		return ""
	}
	return "worker \"grok\" requires an exact xAI model slug (\"grok-4.6\", \"grok-build-0.1\", dated pins) or an \"xai/\"-qualified id; got \"" + raw + "\" (driver tier aliases like \"coding\" are not valid grok ids)"
}

type predicate func(model string) string

// MODEL_ID_VALIDATORS is the per-adapter registry (Q33 isProgress precedent).
// Passthrough adapters own their id normalization at argv build time and
// deliberately accept anything; an unknown worker falls back to passthrough
// so a registry miss can never block a dispatch.
var MODEL_ID_VALIDATORS = map[string]predicate{
	"omp":         func(m string) string { return providerQualifiedReason("omp", m) },
	"pi":          func(m string) string { return providerQualifiedReason("pi", m) },
	"grok":        grokModelIdReason,
	"claude-code": func(string) string { return "" },
	"opencode":    func(string) string { return "" },
}

// ValidateModelId returns "" when the model is acceptable (unset/empty always
// is — the worker CLI falls back to its own configured default); otherwise a
// one-line reason suitable for failure detail / log output.
func ValidateModelId(worker, model string) string {
	if validate, ok := MODEL_ID_VALIDATORS[worker]; ok {
		return validate(model)
	}
	return "" // unknown worker: passthrough
}

// HasDeclaredModelIdShape reports whether the registry declares an explicit
// shape for this worker.
func HasDeclaredModelIdShape(worker string) bool {
	_, ok := MODEL_ID_VALIDATORS[worker]
	return ok
}
