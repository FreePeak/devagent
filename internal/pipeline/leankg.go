// LeanKG client: Go port of src/leankg.ts (FR-CTX-03/04/05, PRD Q28).
//
// FR-CTX-05: orchestrator-side LeanKG client behind the `kgProvider` seam
// (src/prompt.ts). One call per digest build to the `leankg` CLI
// (`query` / `graph-query`, JSON output) under a hard 1s wall-clock budget.
// On timeout, missing binary, or non-zero exit the KG layer is omitted and
// the degraded mode is surfaced in the run log — the pipeline never blocks
// (FR-CTX-03). No query-fallback ladder lives here: LeanKG's single-tool
// router degrades internally (L3 vectors → L2 fuzzy → L1 exact → L0 cold)
// and stamps every response with `retrieval: {rung, reason}` +
// `freshness`, which this client consumes verbatim in the digest and run
// log. The client is orchestrator-side only and never reaches a worker
// adapter (FR-CTX-04).

package pipeline

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/spawn"
)

// LEANKG_BIN is the default binary name (the freepeak `leankg` server,
// never be-knowledge-graph).
const LEANKG_BIN = "leankg"

// LEANKG_TIMEOUT_MS is the hard wall-clock budget for every KG client call
// (FR-CTX-05).
const LEANKG_TIMEOUT_MS = 1000

// KG_FRESHNESS_FRESH is the one LeanKG freshness value that clears the
// stale-evidence gate.
const KG_FRESHNESS_FRESH = "fresh"

// KgSubcommand mirrors the TS KgSubcommand union: one verb per call — no
// fallback ladder.
type KgSubcommand = string

// KgDegradedReason mirrors the TS union: why the KG layer was omitted from
// the digest.
type KgDegradedReason = string

// LeanKgProvenance mirrors LeanKG's per-response provenance, consumed
// verbatim (never re-derived). Pointer fields keep TS undefined distinct
// from the empty string.
type LeanKgProvenance struct {
	Rung      *string `json:"rung,omitempty"`
	Reason    *string `json:"reason,omitempty"`
	Freshness *string `json:"freshness,omitempty"`
}

// LeanKgCallOptions mirrors LeanKgCallOptions. Zero values resolve like the
// TS defaults: Bin→"leankg", Subcommand→"query", TimeoutMs→1000.
type LeanKgCallOptions struct {
	// RepoPath the query runs against; passed as the spawn cwd.
	RepoPath string
	// Query is the natural-language query text handed to LeanKG's
	// single-tool router.
	Query string
	// Bin is a binary override (tests inject a stub script). "" = leankg.
	Bin string
	// Subcommand is the CLI verb. "" = "query".
	Subcommand KgSubcommand
	// TimeoutMs is the wall-clock budget in ms. 0 = 1000 (FR-CTX-05).
	TimeoutMs int
}

// LeanKgDegraded mirrors the TS degraded descriptor.
type LeanKgDegraded struct {
	Reason KgDegradedReason `json:"reason"`
	Detail string           `json:"detail"`
}

// LeanKgResult mirrors LeanKgResult.
type LeanKgResult struct {
	// Content: rendered KG digest lines (content + provenance); '' when
	// degraded.
	Content string `json:"content"`
	// Provenance extracted from the response (TS: key present only when the
	// provenance object has keys).
	Provenance *LeanKgProvenance `json:"provenance,omitempty"`
	// Degraded is set whenever the KG layer must be omitted from the digest.
	Degraded *LeanKgDegraded `json:"degraded,omitempty"`
}

// LeanKgClientOptions mirrors LeanKgClientOptions: the call options plus
// the run-log surfacing. Nil Log = silent client.
type LeanKgClientOptions struct {
	LeanKgCallOptions
	// Log is the run logger for the degraded / provenance lines.
	Log RunLog
	// Stage is the stage label for emitted run-log lines. "" = "implement".
	Stage ledger.RunStage
}

// LeanKgProvider mirrors the TS `(() => string) & { last?: LeanKgResult }`:
// the `kgProvider` seam plus the last raw result. The last result lives in
// a shared cell so value copies (Go structs) observe the latest Fn call,
// mirroring the TS provider object's mutable `last` property (PRD Q28:
// the merge path reads the provenance the run actually consumed instead
// of re-querying).
type LeanKgProvider struct {
	// Fn performs exactly one leankg call (no fallback ladder) and returns
	// the digest text ('' when degraded).
	Fn func() string
	// last is the shared cell behind Last(); nil only on zero values.
	last *leanKgLastCell
}

// leanKgLastCell is the mutable last-result cell.
type leanKgLastCell struct{ res *LeanKgResult }

// Last returns the raw result of the most recent Fn call (nil before the
// first call, or on a zero-value provider).
func (p LeanKgProvider) Last() *LeanKgResult {
	if p.last == nil {
		return nil
	}
	return p.last.res
}

// ProvenanceLine renders `retrieval: {rung, reason}` + `freshness` verbatim
// as one line.
func ProvenanceLine(p LeanKgProvenance) string {
	var parts []string
	if p.Rung != nil || p.Reason != nil {
		rung := "unknown"
		if p.Rung != nil {
			rung = *p.Rung
		}
		reason := ""
		if p.Reason != nil {
			reason = fmt.Sprintf(" (%s)", *p.Reason)
		}
		parts = append(parts, fmt.Sprintf("retrieval: %s%s", rung, reason))
	}
	if p.Freshness != nil {
		parts = append(parts, fmt.Sprintf("freshness: %s", *p.Freshness))
	}
	return strings.Join(parts, " | ")
}

// resultLine mirrors the TS resultLine: one digest line per result entry;
// unknown shapes stay verbatim JSON.
func resultLine(e any) string {
	switch v := e.(type) {
	case string:
		return v
	case map[string]any:
		// TS `.map(...).find(Boolean)` scans: first truthy string wins, and
		// label is computed before desc.
		var label, desc string
		for _, key := range []string{"name", "title", "file", "id"} {
			if s, ok := v[key].(string); ok && s != "" {
				label = s
				break
			}
		}
		for _, key := range []string{"description", "summary", "detail"} {
			if s, ok := v[key].(string); ok && s != "" {
				desc = s
				break
			}
		}
		if label != "" {
			if desc != "" {
				return fmt.Sprintf("%s: %s", label, desc)
			}
			return label
		}
	}
	return jsonValueString(e)
}

// jsonValueString mirrors JSON.stringify for the fallback shapes (compact
// JSON, no HTML escaping).
func jsonValueString(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(raw)
}

// strOrNum accepts the string-or-number fields of the response (TS:
// `typeof x === 'string' || typeof x === 'number'` then String(x)). The
// decoder uses json.Number so number rendering matches the original
// literal.
func strOrNum(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), true
	}
	return "", false
}

// splitNonEmptyLines mirrors the TS `split('\n').map(l => l.trimEnd())
// .filter(Boolean)` chain (the filter runs on the trimmed line).
func splitNonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRightFunc(l, unicode.IsSpace)
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// cnsTruncateRunes mirrors TS String.prototype.slice(0, n) over the short
// stderr slices: cut without splitting multi-byte runes.
func cnsTruncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// QueryLeanKg mirrors queryLeanKg — the single client call. Spawns
// `leankg <subcommand> <query> --json` with a hard wall-clock budget
// (SIGKILL on expiry), parses the JSON response, and renders the digest
// lines with the response's own provenance stamp appended verbatim. Never
// throws: every failure mode returns Content:” plus a Degraded descriptor
// the caller surfaces in the run log.
func QueryLeanKg(opts LeanKgCallOptions) LeanKgResult {
	bin := opts.Bin
	if bin == "" {
		bin = LEANKG_BIN
	}
	sub := opts.Subcommand
	if sub == "" {
		sub = "query"
	}
	budgetMs := opts.TimeoutMs
	if budgetMs <= 0 {
		budgetMs = LEANKG_TIMEOUT_MS
	}

	resp := spawn.RunCli(bin, []string{sub, opts.Query, "--json"}, spawn.Options{Dir: opts.RepoPath, TimeoutMs: budgetMs})
	if resp.TimedOut {
		return LeanKgResult{Content: "", Degraded: &LeanKgDegraded{
			Reason: "timeout",
			Detail: fmt.Sprintf("%s %s exceeded %dms budget (killed)", bin, sub, budgetMs),
		}}
	}
	if resp.ExitCode == -1 {
		// Spawn failure: ENOENT dominates (missing binary); any other spawn
		// error maps like the TS non-status branch.
		return LeanKgResult{Content: "", Degraded: &LeanKgDegraded{
			Reason: "missing-binary",
			Detail: fmt.Sprintf("%s binary not found", bin),
		}}
	}
	if resp.ExitCode != 0 {
		stderr := strings.TrimSpace(resp.Stderr)
		if stderr != "" {
			stderr = cnsTruncateRunes(stderr, 200)
		}
		detail := fmt.Sprintf("%s %s exited %d", bin, sub, resp.ExitCode)
		if stderr != "" {
			detail += fmt.Sprintf(": %s", stderr)
		}
		return LeanKgResult{Content: "", Degraded: &LeanKgDegraded{Reason: "exit", Detail: detail}}
	}

	raw := strings.TrimSpace(resp.Stdout)
	if raw == "" {
		return LeanKgResult{Content: "", Degraded: &LeanKgDegraded{
			Reason: "parse",
			Detail: fmt.Sprintf("%s %s returned empty output", bin, sub),
		}}
	}

	// JSON.parse parity: strict about trailing content, and a valid
	// non-object reply degrades to the raw-line body (TS `parsed && typeof
	// parsed === 'object' ? parsed : {}`).
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return LeanKgResult{Content: "", Degraded: &LeanKgDegraded{
			Reason: "parse",
			Detail: fmt.Sprintf("%s %s response is not valid JSON", bin, sub),
		}}
	}
	if _, err := dec.Token(); err != io.EOF {
		return LeanKgResult{Content: "", Degraded: &LeanKgDegraded{
			Reason: "parse",
			Detail: fmt.Sprintf("%s %s response is not valid JSON", bin, sub),
		}}
	}
	res, _ := parsed.(map[string]any)

	prov := LeanKgProvenance{}
	if retrieval, ok := res["retrieval"].(map[string]any); ok {
		if v, ok := strOrNum(retrieval["rung"]); ok {
			s := v
			prov.Rung = &s
		}
		if v, ok := strOrNum(retrieval["reason"]); ok {
			s := v
			prov.Reason = &s
		}
	}
	if v, ok := strOrNum(res["freshness"]); ok {
		s := v
		prov.Freshness = &s
	}

	// Body: prefer the router's own text fields; fall back to the raw
	// response. This is shape consumption of ONE reply, not a re-query
	// ladder.
	var body []string
	if answer, ok := res["answer"].(string); ok && strings.TrimSpace(answer) != "" {
		body = splitNonEmptyLines(answer)
	} else if results, ok := res["results"].([]any); ok {
		for _, r := range results {
			l := resultLine(r)
			if strings.TrimSpace(l) != "" {
				body = append(body, l)
			}
		}
	} else {
		body = splitNonEmptyLines(raw)
	}

	stamp := ProvenanceLine(prov)
	if stamp != "" {
		body = append(body, stamp)
	}
	out := LeanKgResult{Content: strings.Join(body, "\n")}
	if prov.Rung != nil || prov.Reason != nil || prov.Freshness != nil {
		p := prov
		out.Provenance = &p
	}
	return out
}

// CreateLeanKgProvider mirrors createLeanKgProvider — adapts the client to
// the synchronous `kgProvider` seam. Each Fn invocation performs exactly
// one leankg call (no fallback ladder) and surfaces the outcome in the run
// log: a warn line with the degraded reason when the layer is omitted, an
// info line carrying the response's `retrieval`/`freshness` provenance
// verbatim when it is not. Never throws.
func CreateLeanKgProvider(opts LeanKgClientOptions) LeanKgProvider {
	stage := opts.Stage
	if stage == "" {
		stage = ledger.StageImplement
	}
	last := &leanKgLastCell{}
	prov := LeanKgProvider{last: last}
	prov.Fn = func() string {
		res := QueryLeanKg(opts.LeanKgCallOptions)
		r := res
		last.res = &r
		if res.Degraded != nil {
			if opts.Log != nil {
				opts.Log.Warn(stage,
					fmt.Sprintf("kg=leankg degraded (%s): %s — KG layer omitted from digest", res.Degraded.Reason, res.Degraded.Detail),
					[]ledger.KV{{Key: "kg", Value: "leankg"}, {Key: "reason", Value: res.Degraded.Reason}},
				)
			}
			return ""
		}
		stamp := ""
		if res.Provenance != nil {
			stamp = ProvenanceLine(*res.Provenance)
		}
		if opts.Log != nil {
			data := []ledger.KV{{Key: "kg", Value: "leankg"}}
			if res.Provenance != nil {
				if res.Provenance.Rung != nil {
					data = append(data, ledger.KV{Key: "rung", Value: *res.Provenance.Rung})
				}
				if res.Provenance.Reason != nil {
					data = append(data, ledger.KV{Key: "reason", Value: *res.Provenance.Reason})
				}
				if res.Provenance.Freshness != nil {
					data = append(data, ledger.KV{Key: "freshness", Value: *res.Provenance.Freshness})
				}
			}
			msg := "kg=leankg digest attached"
			if stamp != "" {
				msg += ": " + stamp
			}
			opts.Log.Info(stage, msg, data)
		}
		return res.Content
	}
	return prov
}

// KgEvidence mirrors the TS KgEvidence — verbatim KG provenance excerpt of
// one digest build (PRD Q28). Freshness is nil when absent (TS undefined),
// so "freshness": "" never masquerades as a stamp.
type KgEvidence struct {
	// Excerpt: `retrieval: … | freshness: …`, rendered verbatim by
	// ProvenanceLine.
	Excerpt string `json:"excerpt"`
	// Freshness is LeanKG's own freshness stamp, never re-derived.
	Freshness *string `json:"freshness,omitempty"`
}

// CaptureKgEvidence mirrors captureKgEvidence: capture the run's verbatim
// provenance excerpt from a provider that has already been called (by the
// knowledge-context build). Returns nil when the KG layer was absent,
// degraded, or answered without provenance — the caller then persists
// nothing.
func CaptureKgEvidence(provider LeanKgProvider) *KgEvidence {
	last := provider.Last()
	if last == nil || last.Provenance == nil {
		return nil
	}
	prov := *last.Provenance
	excerpt := ProvenanceLine(prov)
	if excerpt == "" {
		return nil
	}
	ev := KgEvidence{Excerpt: excerpt}
	if prov.Freshness != nil {
		f := *prov.Freshness
		ev.Freshness = &f
	}
	return &ev
}

// IsFreshKgEvidence mirrors isFreshKgEvidence (FR-CTX-05 freshness gate):
// only a `fresh` stamp may persist across runs. `stale`, `possibly_stale`,
// `cold`, and an absent stamp are all omitted so a later digest never
// learns from evidence LeanKG itself distrusts.
func IsFreshKgEvidence(evidence *KgEvidence) bool {
	return evidence != nil && evidence.Freshness != nil && *evidence.Freshness == KG_FRESHNESS_FRESH
}
