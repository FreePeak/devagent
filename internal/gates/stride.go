package gates

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Rubric severities (lowercase) mirror StrideSeverity.
const (
	StrideSeverityLow    = "low"
	StrideSeverityMedium = "medium"
	StrideSeverityHigh   = "high"
)

// strideFinding mirrors src/validation/stride-gate.ts StrideFinding.
type strideFinding struct {
	ID             string `json:"id"`
	Category       string `json:"category"`
	Severity       string `json:"severity"`
	Title          string `json:"title"`
	Evidence       string `json:"evidence"`
	File           string `json:"file"`
	Line           *int   `json:"line,omitempty"`
	Recommendation string `json:"recommendation"`
}

// parsedDiffHunk mirrors src/validation/stride-gate.ts ParsedDiffHunk: added
// is the newline-joined '+' lines, removed the '-' lines; line is the
// 1-indexed new-file line where the hunk starts, when known.
type parsedDiffHunk struct {
	File    string
	Line    *int
	Added   string
	Removed string
}

// strideGateResult mirrors src/validation/stride-gate.ts StrideGateResult.
type strideGateResult struct {
	Gate        string          `json:"gate"`
	Passed      bool            `json:"passed"`
	Findings    []strideFinding `json:"findings"`
	SeverityMax string          `json:"severityMax"`
	Detail      string          `json:"detail"`
}

var strideNames = map[string]string{
	"S": "Spoofing",
	"T": "Tampering",
	"R": "Repudiation",
	"I": "Information disclosure",
	"D": "Denial of service",
	"E": "Elevation of privilege",
}

// strideRule mirrors src/validation/stride-gate.ts StrideRule. The regexp is
// guaranteed non-nil unless `match` carries the full test (RE2 lacks
// negative lookaheads, so one TS literal needs a hand-rolled matcher).
type strideRule struct {
	category  string
	severity  string
	title     string
	recommend string
	// pattern is the compiled TS literal. When the TS literal needs a
	// negative lookahead (unsupported by RE2), pattern is nil and match
	// implements the full check instead.
	pattern   *regexp.Regexp
	match     func(line string) bool
	onRemoved bool
}

// compileStrideRubric mirrors the TS RUBRIC array: static rubric, iterated
// in order; first match wins per line. Severity note: console.log of request
// objects (req/req.body/req.params) is high, while console.log of a
// sensitive *variable name* (password/token/secret) is medium — the latter
// is advisory so it does not block the gate. All flags are (?i) except the
// private-key rule, which is case-sensitive like its TS literal.
func compileStrideRubric() []strideRule {
	return []strideRule{
		// ---- Spoofing ------------------------------------------------------
		{category: "S", severity: StrideSeverityHigh, title: "Hardcoded API key or secret",
			recommend: "Load the credential from an environment variable instead of committing it.",
			pattern:   icase(`api[_-]?key\s*[:=]`)},
		{category: "S", severity: StrideSeverityHigh, title: "Hardcoded bearer token",
			recommend: "Read the token from config/env; never inline it in source.",
			pattern:   icase(`bearer\s+[A-Za-z0-9._-]{20,}`)},
		{category: "S", severity: StrideSeverityHigh, title: "Private key material committed",
			recommend: "Store key material outside the repository and reference it by path.",
			pattern:   regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
		{category: "S", severity: StrideSeverityMedium, title: "Authentication check skipped or bypassed",
			recommend: "Keep auth middleware short-circuiting to an error, never an unconditional next().",
			pattern:   icase(`(?:skip|bypass)\s+auth|(?:auth|jwt|session)[^\n]{0,40}\bnext\s*\(\s*\)`)},

		// ---- Tampering -----------------------------------------------------
		{category: "T", severity: StrideSeverityHigh, title: "SQL query built by string interpolation",
			recommend: "Use parameterized queries or an ORM binding instead of interpolating input.",
			pattern:   icase("`?\\s*(SELECT|INSERT|UPDATE|DELETE)\\s+.*\\$\\{")},
		{category: "T", severity: StrideSeverityHigh, title: "Command execution fed by request data",
			recommend: "Avoid shelling out with user input; use execFile with an argument array and validation.",
			pattern:   icase(`\b(?:exec|execSync|spawn(?:Sync)?)\s*\([^)]*(?:req\.|body|params)`)},
		{category: "T", severity: StrideSeverityHigh, title: "Unsafe deserialization into eval/Function",
			recommend: "Never pass parsed request data to eval or the Function constructor.",
			pattern:   icase(`\b(?:eval|new\s+Function)\s*\([^)]*(?:JSON\.parse|req\.|body)`)},

		// ---- Repudiation ---------------------------------------------------
		{category: "R", severity: StrideSeverityMedium, title: "Audit or log call commented out",
			recommend: "Restore the audit/log call or route it through the project logger.",
			pattern:   icase(`^\s*(?:\/\/|\/\*).*\b(log|audit)\b`)},

		// ---- Information disclosure ---------------------------------------
		{category: "I", severity: StrideSeverityHigh, title: "Request data logged to console",
			recommend: "Drop the console.log or redact request payloads before logging.",
			pattern:   icase(`console\.log\s*\([^)]*\breq\b`)},
		{category: "I", severity: StrideSeverityMedium, title: "Sensitive variable logged to console",
			recommend: "Remove the log or redact the sensitive value.",
			pattern:   icase(`console\.log\s*\([^)]*(?:password|passwd|token|secret)`)},
		{category: "I", severity: StrideSeverityMedium, title: "Raw error stack or message returned to client",
			recommend: "Return a generic error to the client and log the details server-side.",
			pattern:   icase(`\bres\.(?:status|json|send)\s*\([^)]*err(?:or)?\.(?:stack|message)|err(?:or)?\.stack`)},

		// ---- Denial of service --------------------------------------------
		{category: "D", severity: StrideSeverityMedium, title: "Unbounded timer or infinite loop",
			recommend: "Bound the loop/timer with a clear exit condition or cancellation.",
			pattern:   icase(`\bsetInterval\s*\(|\bsetTimeout\s*\(|while\s*\(\s*true\s*\)`)},
		{category: "D", severity: StrideSeverityMedium, title: "Blocking file read of a user-controlled path",
			recommend: "Validate/sandbox the path and prefer async fs APIs.",
			pattern:   icase(`\breadFileSync\s*\([^)]*(?:req\.|input|body|params)`)},
		{category: "D", severity: StrideSeverityHigh, title: "Outbound HTTP call without timeout",
			recommend: "Set an explicit timeout or wire an AbortController before awaiting the call.",
			match:     matchFetchNoTimeout},

		// ---- Elevation of privilege ---------------------------------------
		{category: "E", severity: StrideSeverityHigh, title: "Process environment or global state assignment",
			recommend: "Keep configuration read-only at startup; never assign into globalThis from a handler.",
			pattern:   icase(`^\s*(?:globalThis\.\w+\s*=[^=]|Object\.assign\s*\(\s*global|process\.env\.\w+\s*=[^=])|\brequire\s*\(\s*["']fs["']\s*\)[^;\n]*(?:req\.|body|input|params)`)},
		{category: "E", severity: StrideSeverityMedium, title: "Role or permission check removed",
			recommend: "Re-add the authorization check or document the delegated guard.",
			pattern:   icase(`\b(role|permission|isAdmin|authorize)\b`),
			onRemoved: true},
	}
}

// matchFetchNoTimeout implements the TS D-high literal, whose negative
// lookahead RE2 cannot compile:
// /\b(?:fetch|axios(?:\.\w+)?|https?\.request|https?\.get)\s*\([^)]*(?:req\.|body|params|input)(?![^)]*(?:timeout|signal|AbortController))/i
// Strategy: find each call-token match and check the lookahead predicate on
// the remainder of the enclosing paren group, like the backtracking engine
// would for these single-line inputs.
var fetchNoTimeoutHead = icase(`\b(?:fetch|axios(?:\.\w+)?|https?\.request|https?\.get)\s*\([^)]*(?:req\.|body|params|input)`)

var fetchTail = icase(`(?:timeout|signal|AbortController)`)

func matchFetchNoTimeout(line string) bool {
	loc := fetchNoTimeoutHead.FindStringIndex(line)
	if loc == nil {
		return false
	}
	// Negative lookahead at the end of the head match: the rest of the
	// (...) group must NOT contain timeout/signal/AbortController.
	rest := line[loc[1]:]
	if close := strings.IndexByte(rest, ')'); close >= 0 {
		rest = rest[:close]
	}
	return !fetchTail.MatchString(rest)
}

var strideRubric = compileStrideRubric()

// icase compiles a case-insensitive regexp, mirroring a TS /.../i literal.
func icase(expr string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)` + expr)
}

var strideSeverityRank = map[string]int{
	StrideSeverityLow:    1,
	StrideSeverityMedium: 2,
	StrideSeverityHigh:   3,
}

var strideSeverityTag = map[string]string{
	StrideSeverityHigh:   "H",
	StrideSeverityMedium: "M",
	StrideSeverityLow:    "L",
}

// formatStrideDetail mirrors formatDetail(): deterministic markdown block for
// the string detail channel, sorted severity-first (H before M before L),
// then by category letter. localeCompare sorts letters ascending; Go byte
// comparison matches for the A-Z letters this sorting sees.
func formatStrideDetail(passed bool, findings []strideFinding) string {
	if len(findings) == 0 {
		if passed {
			return "STRIDE pass: no diff to analyze"
		}
		return "STRIDE gate failed: no findings"
	}
	sorted := make([]strideFinding, len(findings))
	copy(sorted, findings)
	sort.SliceStable(sorted, func(i, j int) bool {
		if strideSeverityRank[sorted[i].Severity] != strideSeverityRank[sorted[j].Severity] {
			return strideSeverityRank[sorted[i].Severity] > strideSeverityRank[sorted[j].Severity]
		}
		return sorted[i].Category < sorted[j].Category
	})
	lines := make([]string, 0, len(sorted))
	for _, f := range sorted {
		line := fmt.Sprintf("- [%s] %s — %s", strideSeverityTag[f.Severity], strideNames[f.Category], f.File)
		if f.Line != nil {
			line += fmt.Sprintf(":%d", *f.Line)
		}
		line += fmt.Sprintf(" — %s (%s)", f.Title, f.Evidence)
		lines = append(lines, line)
	}
	header := "## STRIDE G5 findings"
	if passed {
		header = "STRIDE pass (advisory):\n## STRIDE G5 findings"
	}
	return strings.Join(append([]string{header}, lines...), "\n")
}

// RunStrideGate mirrors runStrideGate(): run the STRIDE rubric over parsed
// diff hunks. worktree is accepted for future file-content enrichment but
// unused in this revision. Empty input is a pass with zero findings (first
// run may have no diff yet) — never panics on untrusted input.
func RunStrideGate(parsed []parsedDiffHunk, worktree string) strideGateResult {
	_ = worktree
	if len(parsed) == 0 {
		return strideGateResult{
			Gate:        "G5-stride",
			Passed:      true,
			Findings:    []strideFinding{},
			SeverityMax: "",
			Detail:      "STRIDE pass: no diff to analyze",
		}
	}

	var findings []strideFinding
	matchCounter := 0
	for _, hunk := range parsed {
		// Iterate lines, then rules: the first matching rule wins per line,
		// and every matching line yields its own finding (duplicates never
		// collapse).
		addedLines := []string{}
		if hunk.Added != "" {
			addedLines = strings.Split(hunk.Added, "\n")
		}
		removedLines := []string{}
		if hunk.Removed != "" {
			removedLines = strings.Split(hunk.Removed, "\n")
		}
		for _, line := range addedLines {
			rule := firstStrideRule(line, false)
			if rule == nil {
				continue
			}
			matchCounter++
			findings = append(findings, newStrideFinding(rule, hunk, line, matchCounter))
		}
		for _, line := range removedLines {
			rule := firstStrideRule(line, true)
			if rule == nil {
				continue
			}
			matchCounter++
			findings = append(findings, newStrideFinding(rule, hunk, line, matchCounter))
		}
	}

	severityMax := ""
	for _, f := range findings {
		if severityMax == "" || strideSeverityRank[f.Severity] > strideSeverityRank[severityMax] {
			severityMax = f.Severity
		}
	}

	passed := severityMax != StrideSeverityHigh
	if findings == nil {
		findings = []strideFinding{}
	}
	return strideGateResult{
		Gate:        "G5-stride",
		Passed:      passed,
		Findings:    findings,
		SeverityMax: severityMax,
		Detail:      formatStrideDetail(passed, findings),
	}
}

// firstStrideRule mirrors RUBRIC.find(): the first matching rule wins per
// line. onRemoved rules match only against removed lines (and vice versa).
func firstStrideRule(line string, onRemoved bool) *strideRule {
	for i := range strideRubric {
		r := &strideRubric[i]
		if r.onRemoved != onRemoved {
			continue
		}
		if r.pattern != nil {
			if r.pattern.MatchString(line) {
				return r
			}
			continue
		}
		if r.match != nil && r.match(line) {
			return r
		}
	}
	return nil
}

func newStrideFinding(rule *strideRule, hunk parsedDiffHunk, line string, matchCounter int) strideFinding {
	lineNo := 0
	if hunk.Line != nil {
		lineNo = *hunk.Line
	}
	id := fmt.Sprintf("%s-%s-%d-%d", rule.category, hunk.File, lineNo, matchCounter)
	f := strideFinding{
		ID:             id,
		Category:       rule.category,
		Severity:       rule.severity,
		Title:          rule.title,
		Evidence:       strings.TrimSpace(line),
		File:           hunk.File,
		Recommendation: rule.recommend,
	}
	if hunk.Line != nil {
		l := *hunk.Line
		f.Line = &l
	}
	return f
}

var (
	unifiedFileHeader = regexp.MustCompile(`^\+\+\+ b/(.*)$`)
	unifiedHunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
)

// ParseUnifiedDiff mirrors parseUnifiedDiff(): minimal unified-diff parser
// producing hunks. Tracks the current file from `+++` headers and hunk starts
// from `@@ -a,b +c,d @@`.
func ParseUnifiedDiff(diffText string) []parsedDiffHunk {
	hunks := []parsedDiffHunk{}
	file := ""
	var current *parsedDiffHunk

	for _, raw := range strings.Split(diffText, "\n") {
		if m := unifiedFileHeader.FindStringSubmatch(raw); m != nil && m[1] != "" {
			file = m[1]
			continue
		}
		if m := unifiedHunkHeader.FindStringSubmatch(raw); m != nil {
			line := atoiMust(m[1])
			hunks = append(hunks, parsedDiffHunk{File: file, Line: &line, Added: "", Removed: ""})
			current = &hunks[len(hunks)-1]
			continue
		}
		if current == nil {
			continue
		}
		if strings.HasPrefix(raw, "+") {
			current.Added += raw[1:] + "\n"
		} else if strings.HasPrefix(raw, "-") {
			current.Removed += raw[1:] + "\n"
		}
	}
	return hunks
}

func atoiMust(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
