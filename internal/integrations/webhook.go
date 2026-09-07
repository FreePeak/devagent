// Webhook receiver core (FR-TICKET-04): the Go port of the transport-
// agnostic part of src/server/webhook.ts. Raw-body collection, HMAC-SHA256
// signature verification, and delivery-ID dedup. Responds 2xx within the
// provider deadline (Linear: 5s) — handlers run async (respond-fast /
// process-late).

package integrations

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"strings"
	"sync"
)

// MaxBodyBytes caps webhook bodies at 1 MiB.
const MaxBodyBytes = 1 << 20

// WebhookRequest is the raw delivery input: the header multimap (node
// lowercases all names; lookup here is case-insensitive either way) plus
// the exact request body bytes.
type WebhookRequest struct {
	Headers map[string][]string
	RawBody []byte
}

// VerifiedWebhook is a signature-verified delivery.
type VerifiedWebhook struct {
	// DeliveryID is the provider delivery id for dedup
	// (Linear-Delivery / X-GitHub-Delivery).
	DeliveryID string
	// Event is Linear's Linear-Event value ("unknown" when the header is
	// absent).
	Event string
	// GithubEvent is GitHub's X-GitHub-Event when present (issues, push,
	// ...); "" otherwise.
	GithubEvent string
	// Payload is the decoded JSON body.
	Payload any
}

// SignatureError marks webhook verification failures (mapped to HTTP 401).
type SignatureError struct{ Message string }

func (e *SignatureError) Error() string { return e.Message }

// webhookHeader mirrors the TS header() helper: the first value of a
// possibly-repeated header, found case-insensitively; ok=false when absent.
func webhookHeader(h map[string][]string, name string) (string, bool) {
	if v, ok := h[name]; ok && len(v) > 0 {
		return v[0], true
	}
	for k, v := range h {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0], true
		}
	}
	return "", false
}

// VerifyAndParse mirrors verifyAndParse: delivery id, signature presence,
// constant-time HMAC-SHA256 comparison (GitHub's optional `sha256=` prefix
// stripped), then JSON decode. Every failure is a *SignatureError.
func VerifyAndParse(req WebhookRequest, signingSecret string) (VerifiedWebhook, error) {
	deliveryID, ok := webhookHeader(req.Headers, "linear-delivery")
	if !ok {
		deliveryID, ok = webhookHeader(req.Headers, "x-github-delivery")
	}
	if !ok || deliveryID == "" {
		return VerifiedWebhook{}, &SignatureError{"missing delivery id"}
	}

	provided, ok := webhookHeader(req.Headers, "linear-signature")
	if !ok {
		provided, ok = webhookHeader(req.Headers, "x-hub-signature-256")
	}
	if !ok || provided == "" {
		return VerifiedWebhook{}, &SignatureError{"missing signature"}
	}

	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write(req.RawBody)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !TimingSafeEqualHex(expected, StripSignaturePrefix(provided)) {
		return VerifiedWebhook{}, &SignatureError{"signature mismatch"}
	}

	var payload any
	if err := json.Unmarshal(req.RawBody, &payload); err != nil {
		return VerifiedWebhook{}, &SignatureError{"invalid JSON body"}
	}

	verified := VerifiedWebhook{DeliveryID: deliveryID, Payload: payload, Event: "unknown"}
	if event, ok := webhookHeader(req.Headers, "linear-event"); ok {
		verified.Event = event
	}
	if gh, ok := webhookHeader(req.Headers, "x-github-event"); ok {
		verified.GithubEvent = gh
	}
	return verified, nil
}

// StripSignaturePrefix mirrors stripPrefix: drop GitHub's `sha256=`.
func StripSignaturePrefix(sig string) string {
	return strings.TrimPrefix(sig, "sha256=")
}

// TimingSafeEqualHex mirrors timingSafeEqualHex: compare two hex digests
// without short-circuiting on the first differing byte.
func TimingSafeEqualHex(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// DeliveryDedup mirrors DeliveryDedup: in-memory dedup window; a real
// deployment swaps this for the run store. At capacity it drops the oldest
// ~10% rather than growing unbounded (eviction follows insertion order,
// like the JS Set).
type DeliveryDedup struct {
	mu         sync.Mutex
	seen       map[string]struct{}
	order      []string
	maxEntries int
}

// NewDeliveryDedup builds the window; maxEntries <= 0 uses the TS default
// of 10_000.
func NewDeliveryDedup(maxEntries int) *DeliveryDedup {
	if maxEntries <= 0 {
		maxEntries = 10_000
	}
	return &DeliveryDedup{
		seen:       make(map[string]struct{}, maxEntries),
		order:      make([]string, 0, maxEntries),
		maxEntries: maxEntries,
	}
}

// IsFirst mirrors isFirst: true on first sight of a delivery id.
func (d *DeliveryDedup) IsFirst(deliveryID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.seen[deliveryID]; dup {
		return false
	}
	if len(d.order) >= d.maxEntries {
		// drop oldest ~10% rather than growing unbounded
		drop := d.maxEntries / 10
		for _, k := range d.order[:drop] {
			delete(d.seen, k)
		}
		d.order = append(d.order[:0], d.order[drop:]...)
	}
	d.seen[deliveryID] = struct{}{}
	d.order = append(d.order, deliveryID)
	return true
}

// WebhookOptions mirrors the handleWebhook opts object.
type WebhookOptions struct {
	SigningSecret string
	Dedup         *DeliveryDedup
	// OnEvent runs after the 2xx response has been written (process-late).
	OnEvent func(VerifiedWebhook)
}

// HandleWebhook mirrors handleWebhook as a net/http adapter: collect the
// raw body (413 over the 1 MiB cap), verify, dedup, respond-fast, then
// process-late. Signature failures answer 401 with the reason; duplicate
// deliveries answer 200 "duplicate"; accepted ones 200 "accepted".
func HandleWebhook(w http.ResponseWriter, r *http.Request, opts WebhookOptions) {
	raw, tooLarge := readBodyCapped(r)
	if tooLarge {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}

	verified, err := VerifyAndParse(WebhookRequest{Headers: r.Header, RawBody: raw}, opts.SigningSecret)
	if err != nil {
		var se *SignatureError
		if ok := asSignatureError(err, &se); ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, se.Message)
		} else {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, err.Error())
		}
		return
	}

	if verified.DeliveryID == "" || (opts.Dedup != nil && !opts.Dedup.IsFirst(verified.DeliveryID)) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "duplicate")
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "accepted") // respond inside the provider deadline...
	go opts.OnEvent(verified)        // ...then process async
}

// asSignatureError is errors.As without importing errors here twice.
func asSignatureError(err error, target **SignatureError) bool {
	for err != nil {
		if se, ok := err.(*SignatureError); ok {
			*target = se
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// readBodyCapped collects the raw body; tooLarge mirrors the TS 413 +
// destroy path when the body exceeds MaxBodyBytes.
func readBodyCapped(r *http.Request) (raw []byte, tooLarge bool) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil && n <= MaxBodyBytes {
		// read failure mid-body: treat like the TS destroy — no accept
		return nil, true
	}
	if n > MaxBodyBytes {
		return nil, true
	}
	return buf.Bytes(), false
}

// AgentSessionDispatch is a dispatchable ticket identifier from a Linear
// AgentSessionEvent payload. All fields are treated as untrusted source
// data (PRD risk R5).
type AgentSessionDispatch struct {
	IssueIdentifier string
	IssueID         string
	// CommentBody is the triggering comment text; "" when absent
	// (TS null).
	CommentBody string
}

// ParseAgentSessionEvent mirrors parseAgentSessionEvent; nil when the
// payload is not a dispatchable AgentSessionEvent.
func ParseAgentSessionEvent(payload any) *AgentSessionDispatch {
	p, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	typ, _ := p["type"].(string)
	if typ != "AgentSessionEvent" {
		return nil
	}
	issue := lookupPath(p, "agentSession", "issue")
	if issue == nil {
		return nil
	}
	identifier, idOK := issue["identifier"].(string)
	id, id2OK := issue["id"].(string)
	if !idOK || !id2OK {
		return nil
	}
	dispatch := &AgentSessionDispatch{IssueIdentifier: identifier, IssueID: id}
	if comment, ok := p["comment"].(map[string]any); ok {
		if body, ok := comment["body"].(string); ok {
			dispatch.CommentBody = body
		}
	}
	return dispatch
}

// GithubIssueDispatch is a dispatchable ticket from a GitHub issues webhook
// event (event header "issues", action opened/labeled).
type GithubIssueDispatch struct {
	// IssueIdentifier is the "owner/repo#123" composite key.
	IssueIdentifier string
	Title           string
}

// ParseGithubIssueEvent mirrors parseGithubIssueEvent. Untrusted-data
// discipline: only well-typed fields are read. PRs also fire "issues"
// events — those are skipped (they have their own pipeline).
func ParseGithubIssueEvent(eventHeader string, payload any) *GithubIssueDispatch {
	if eventHeader != "issues" {
		return nil
	}
	p, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	action, _ := p["action"].(string)
	if action != "opened" {
		return nil
	}
	issue, ok := p["issue"].(map[string]any)
	if !ok {
		return nil
	}
	if _, isPR := issue["pull_request"]; isPR {
		return nil
	}
	number, ok := jsNumber(issue["number"])
	if !ok {
		return nil
	}
	title, ok := issue["title"].(string)
	if !ok {
		return nil
	}
	repo := lookupPath(p, "repository")
	if repo == nil {
		return nil
	}
	fullName, ok := repo["full_name"].(string)
	if !ok {
		return nil
	}
	return &GithubIssueDispatch{
		IssueIdentifier: fmt.Sprintf("%s#%s", fullName, formatJSNumber(number)),
		Title:           title,
	}
}
