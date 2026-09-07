package gates

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Gate G5 executor adapter (PRD section 11): wraps the STRIDE rubric in
// stride.go for the consume/autoMerge path. Reuses the existing rules
// verbatim — no rubric logic lives here; this file only maps
// categories/severities to the gate-executor contract and applies the
// CRITICAL promotion for committed credential literals.
//
// Per-path allowlist (PRD Q25): a PR may commit
// .devagent/stride-allowlist.json ({"paths": [...glob patterns...]}) so
// findings whose file matches an allowed path are suppressed — fixture
// credentials in test files no longer stall autoMerge. The allowlist is read
// from the PR branch itself (see the consume wiring); an absent, unreadable,
// or malformed allowlist is ignored (fail closed: the gate keeps blocking).

// Gate-executor severities mirror src/gates/stride.ts StrideGateSeverity.
const (
	StrideGateSeverityHigh     = "HIGH"
	StrideGateSeverityCritical = "CRITICAL"
	StrideGateSeverityMedium   = "MEDIUM"
	StrideGateSeverityLow      = "LOW"
)

// Gate-executor categories mirror src/gates/stride.ts StrideCategory (the
// spelled-out names used in evidence artifacts).
const (
	StrideCatSpoofing            = "Spoofing"
	StrideCatTampering           = "Tampering"
	StrideCatRepudiation         = "Repudiation"
	StrideCatInformationDisclose = "InformationDisclosure"
	StrideCatDenialOfService     = "DenialOfService"
	StrideCatElevationOfPriv     = "ElevationOfPrivilege"
)

// StrideAllowlistPath is the repo-relative location of the committed
// per-path allowlist (PRD Q25).
const StrideAllowlistPath = ".devagent/stride-allowlist.json"

// StrideGateFinding mirrors src/gates/stride.ts StrideGateFinding. File has
// no omitempty: the TS adapter always sets it (the rubric fills hunk.file,
// possibly ""), so the key is always present.
type StrideGateFinding struct {
	Category string `json:"category"`
	Severity string `json:"severity"`
	Evidence string `json:"evidence"`
	File     string `json:"file"`
	Line     *int   `json:"line,omitempty"`
}

// StrideGateEvaluation mirrors src/gates/stride.ts StrideGateEvaluation.
// SeverityMax is "" when there are no findings (the TS null).
type StrideGateEvaluation struct {
	Findings      []StrideGateFinding `json:"findings"`
	SeverityMax   string              `json:"severityMax"`
	ContextDigest string              `json:"contextDigest,omitempty"`
	// HasDigest reports whether contextDigest was set (TS emits the key only
	// when defined).
	HasDigest bool `json:"-"`
}

// strideCategoryNames maps rubric letters to gate-executor category names.
var strideCategoryNames = map[string]string{
	"S": StrideCatSpoofing,
	"T": StrideCatTampering,
	"R": StrideCatRepudiation,
	"I": StrideCatInformationDisclose,
	"D": StrideCatDenialOfService,
	"E": StrideCatElevationOfPriv,
}

var strideGateSeverityNames = map[string]string{
	"high":   StrideGateSeverityHigh,
	"medium": StrideGateSeverityMedium,
	"low":    StrideGateSeverityLow,
}

var strideGateSeverityRank = map[string]int{
	StrideGateSeverityLow:      1,
	StrideGateSeverityMedium:   2,
	StrideGateSeverityHigh:     3,
	StrideGateSeverityCritical: 4,
}

// credentialLiteral mirrors the TS CREDENTIAL_LITERAL: committed credential
// literal promotes an otherwise HIGH finding to CRITICAL.
var credentialLiteral = regexp.MustCompile(`(?i)(?:api[_-]?key|secret|password|token)\s*[:=]\s*["'][^"']{8,}`)

// StrideInput mirrors the evaluateStride input object. Diff being empty means
// "no diff" (TS null/undefined/empty); ContextDigestSet mirrors TS
// `contextDigest !== undefined`.
type StrideInput struct {
	Diff             string
	ContextDigest    string
	ContextDigestSet bool
	AllowlistPaths   []string
}

// EvaluateStride mirrors evaluateStride(): run the STRIDE rubric over a
// unified diff. Empty diffs pass with zero findings; the function never
// panics (diffs are untrusted input). contextDigest is provenance-only and
// passed through verbatim (PRD Q12). allowlistPaths suppresses findings whose
// file matches one of the glob patterns before severityMax is computed
// (PRD Q25); malformed/absent allowlists are ignored by callers, so an
// empty list is a no-op here.
func EvaluateStride(input StrideInput) (evaluation StrideGateEvaluation) {
	// TS wraps the whole evaluation in try/catch ("never throws: an
	// evaluation failure must not block or crash the merge path").
	defer func() {
		if r := recover(); r != nil {
			e := StrideGateEvaluation{Findings: []StrideGateFinding{}, SeverityMax: ""}
			if input.ContextDigestSet {
				e.ContextDigest = input.ContextDigest
				e.HasDigest = true
			}
			evaluation = e
		}
	}()
	digest := func() StrideGateEvaluation {
		e := StrideGateEvaluation{Findings: []StrideGateFinding{}, SeverityMax: ""}
		if input.ContextDigestSet {
			e.ContextDigest = input.ContextDigest
			e.HasDigest = true
		}
		return e
	}

	if input.Diff == "" {
		return digest()
	}

	hunks := ParseUnifiedDiff(input.Diff)
	gate := RunStrideGate(hunks, "")

	allowlist := input.AllowlistPaths
	findings := []StrideGateFinding{}
	for _, f := range gate.Findings {
		if len(allowlist) > 0 && f.File != "" && PathMatchesAllowlist(f.File, allowlist) {
			continue
		}
		severity := strideGateSeverityNames[f.Severity]
		if severity == "" {
			severity = StrideGateSeverityLow
		}
		if f.Severity == StrideSeverityHigh && credentialLiteral.MatchString(f.Evidence) {
			severity = StrideGateSeverityCritical
		}
		gf := StrideGateFinding{
			Category: strideCategoryNames[f.Category],
			Severity: severity,
			Evidence: f.Evidence,
			File:     f.File,
			Line:     f.Line,
		}
		if gf.Category == "" {
			gf.Category = StrideCatSpoofing
		}
		findings = append(findings, gf)
	}

	severityMax := ""
	for _, f := range findings {
		if severityMax == "" || strideGateSeverityRank[f.Severity] > strideGateSeverityRank[severityMax] {
			severityMax = f.Severity
		}
	}

	e := StrideGateEvaluation{Findings: findings, SeverityMax: severityMax}
	if input.ContextDigestSet {
		e.ContextDigest = input.ContextDigest
		e.HasDigest = true
	}
	return e
}

// ParseStrideAllowlist mirrors parseStrideAllowlist(): parse the raw text of
// a committed allowlist file into a pattern list. Returns ok=false when the
// file is malformed JSON, not an object, or lacks a string-array `paths`
// field — callers must treat that as "no allowlist" (fail closed).
func ParseStrideAllowlist(text string) ([]string, bool) {
	var parsed struct {
		Paths []any `json:"paths"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil, false
	}
	// Not an object with a paths field (or a non-string member) fails closed.
	if parsed.Paths == nil {
		return nil, false
	}
	paths := make([]string, 0, len(parsed.Paths))
	for _, p := range parsed.Paths {
		s, ok := p.(string)
		if !ok {
			return nil, false
		}
		paths = append(paths, s)
	}
	return paths, true
}

// PathMatchesAllowlist mirrors pathMatchesAllowlist(): minimal glob match for
// allowlist patterns (no dependencies, deterministic): `**` matches any
// characters including `/`, `*` matches within one path segment, `?` matches
// one non-separator character. A pattern containing no `/` matches the
// file's basename too (so `*.json` covers `test/fixtures/x.json`).
func PathMatchesAllowlist(path string, patterns []string) bool {
	for _, pattern := range patterns {
		// A pattern containing no `/` (including the empty pattern) also
		// matches as `**/<pattern>`, i.e. against any basename.
		regexes := []string{compileAllowGlob(pattern)}
		if !strings.Contains(pattern, "/") {
			regexes = append(regexes, compileAllowGlob("**/"+pattern))
		}
		for _, source := range regexes {
			if match, err := regexp.MatchString(source, path); err == nil && match {
				return true
			}
		}
	}
	return false
}

// compileAllowGlob mirrors the TS toRegex(): translate the glob into an
// anchored regexp source. Escapes regex metacharacters outside glob tokens.
func compileAllowGlob(pattern string) string {
	var b strings.Builder
	b.WriteString("^")
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch ch {
		case '*':
			if i+1 < len(runes) && runes[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	return b.String()
}
