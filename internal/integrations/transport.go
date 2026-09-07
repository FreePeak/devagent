// Shared transport and runner seams for the integrations package: every
// HTTP call goes through Doer (tests inject recorded responses — zero
// network) and every CLI call through CliRunner (tests inject fakes).

package integrations

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Doer is the HTTP transport seam. Mirrors the injectable fetch/http-client
// seams in the TS originals so contract tests replay recorded responses.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// DefaultDoer is the production transport.
var DefaultDoer Doer = &http.Client{}

// FetchOptions mirrors the TS `{ retries?: number; sleep?: ... }` options
// on the rate-limit-aware fetch clients (Linear, Jira, GitHub Issues).
type FetchOptions struct {
	// Retries is the number of 429 retries after the initial attempt.
	// 0 = the TS default of 3 (opts.retries ?? 3); negative = a single
	// attempt (the TS explicit retries: 0).
	Retries int
	// Sleep, when set, replaces the wall-clock wait between attempts so
	// tests record the backoff decisions instead of sleeping.
	Sleep func(ms int)
	// Doer, when set, replaces the default transport.
	Doer Doer
	// Rand is the jitter source for the no-Retry-After backoff; tests pin
	// a deterministic source. Returns a value in [0, maxExclusive).
	Rand func(maxExclusive int) int
}

func (o FetchOptions) doer() Doer {
	if o.Doer != nil {
		return o.Doer
	}
	return DefaultDoer
}

func (o FetchOptions) retries() int {
	if o.Retries == 0 {
		return 3
	}
	if o.Retries < 0 {
		return 0
	}
	return o.Retries
}

func (o FetchOptions) sleep(ms int) {
	if o.Sleep != nil {
		o.Sleep(ms)
		return
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

func (o FetchOptions) jitter(maxExclusive int) int {
	if o.Rand != nil {
		return o.Rand(maxExclusive)
	}
	return rand.IntN(maxExclusive)
}

// fetchWithRetry mirrors the shared TS retry loop: up to Retries re-quests
// while retryable(status) is true, honoring a positive finite Retry-After
// header exactly, else backing off exponentially with jitter. The final
// response is returned even when still rate-limited (the TS clients return
// the last Response and let callers map it to errors).
func fetchWithRetry(newRequest func() (*http.Request, error), opts FetchOptions, retryable func(status int) bool) (*http.Response, error) {
	doer := opts.doer()
	retries := opts.retries()
	var res *http.Response
	for attempt := 0; ; attempt++ {
		req, err := newRequest()
		if err != nil {
			return nil, err
		}
		res, err = doer.Do(req)
		if err != nil {
			return nil, err
		}
		if !retryable(res.StatusCode) || attempt == retries {
			return res, nil
		}
		delay := backoffDelay(attempt, res.Header.Get("Retry-After"), opts.jitter)
		// The retried response is not consumed by the caller; drain and
		// close so the transport can reuse the connection.
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		opts.sleep(delay)
	}
}

// nonEmptyStrings mirrors TS `.filter(Boolean)` label lists: keep only
// non-empty strings.
func nonEmptyStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// backoffDelay mirrors the TS formula exactly:
//
//	Number.isFinite(retryAfter) && retryAfter > 0
//	  ? retryAfter * 1000
//	  : Math.min(30_000, 1000 * 2 ** attempt) + Math.floor(Math.random() * 500)
func backoffDelay(attempt int, retryAfterHeader string, jitter func(maxExclusive int) int) int {
	if d, err := strconv.ParseFloat(strings.TrimSpace(retryAfterHeader), 64); err == nil && d > 0 {
		return int(d * 1000)
	}
	base := 1000 << attempt // 1000 * 2^attempt
	if base <= 0 || attempt > 20 || base > 30000 {
		base = 30000
	}
	return base + jitter(500)
}

// isOK mirrors fetch Response.ok: status 200-299.
func isOK(status int) bool { return status >= 200 && status < 300 }

// marshalJSON serializes v exactly like TS JSON.stringify for the payload
// shapes in this package: struct field declaration order is preserved and
// HTML characters are not escaped (Go's default encoder escapes <, >, &
// which JSON.stringify does not). Unreachable error: all payloads are
// plain structs.
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic("integrations: marshalJSON: " + err.Error())
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// encodeURIComponent mirrors ECMAScript encodeURIComponent so request URLs
// are byte-identical to the TS originals.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '!' || c == '~' || c == '*' ||
			c == '\'' || c == '(' || c == ')' {
			b.WriteByte(c)
		} else {
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xF])
		}
	}
	return b.String()
}

// unknownString mirrors TS `typeof x === 'string' ? x : undefined` for
// loosely-typed JSON fields.
func unknownString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// jsNumber accepts the JSON number encodings Go can hand us (decoded
// map values are float64; hand-built test maps may use int).
func jsNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// formatJSNumber renders a number the way a TS template literal would for
// integral values ("5"); fractional values keep the shortest form.
func formatJSNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// jsScalarString renders a JSON scalar the way a TS template literal
// would when a GraphQL error message is not a string.
func jsScalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return formatJSNumber(t), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

// lookupPath walks a decoded-JSON object path; nil when any hop is missing
// or not an object.
func lookupPath(v any, path ...string) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	for _, p := range path {
		next, ok := m[p].(map[string]any)
		if !ok {
			return nil
		}
		m = next
	}
	return m
}
