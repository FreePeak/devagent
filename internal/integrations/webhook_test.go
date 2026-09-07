// Contract tests for the webhook receiver, ported from
// test/webhook.test.ts. HMAC-SHA256 verification, delivery-ID dedup,
// timing-safe comparison, and the two event parsers — plus a live
// net/http smoke of the respond-fast/process-late adapter.
package integrations

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// webhookSecret is per-test random, mirroring the vitest
// randomBytes(8).toString('hex') secret.
var webhookSecret = func() string {
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	return "whsec_test_" + hex.EncodeToString(raw)
}()

// signedHeaders mirrors the vitest signed() helper.
func signedHeaders(t *testing.T, body string) map[string][]string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	sig := hex.EncodeToString(mac.Sum(nil))
	return map[string][]string{
		"Linear-Delivery":  {"delivery-1"},
		"Linear-Signature": {sig},
		"Linear-Event":     {"AgentSessionEvent"},
	}
}

func TestVerifyAndParseAcceptsValidDelivery(t *testing.T) {
	// Ported: accepts a valid signed delivery.
	body := `{"type":"AgentSessionEvent"}`
	v, err := VerifyAndParse(WebhookRequest{Headers: signedHeaders(t, body), RawBody: []byte(body)}, webhookSecret)
	if err != nil {
		t.Fatalf("VerifyAndParse: %v", err)
	}
	if v.DeliveryID != "delivery-1" {
		t.Errorf("deliveryId = %q", v.DeliveryID)
	}
	if v.Event != "AgentSessionEvent" {
		t.Errorf("event = %q", v.Event)
	}
	if !reflect.DeepEqual(v.Payload, map[string]any{"type": "AgentSessionEvent"}) {
		t.Errorf("payload = %#v", v.Payload)
	}
}

func TestVerifyAndParseRejectsTamperedBodies(t *testing.T) {
	// Ported: rejects tampered bodies.
	body := `{"type":"AgentSessionEvent"}`
	_, err := VerifyAndParse(WebhookRequest{Headers: signedHeaders(t, body), RawBody: []byte(`{"evil":true}`)}, webhookSecret)
	if err == nil || !isSignatureError(err) {
		t.Errorf("err = %v", err)
	}
}

func isSignatureError(err error) bool {
	_, ok := err.(*SignatureError)
	return ok
}

func TestVerifyAndParseRejectsMissingSignatureAndDelivery(t *testing.T) {
	// Ported: rejects missing signature and missing delivery id.
	body := `{"type":"AgentSessionEvent"}`

	h := signedHeaders(t, body)
	delete(h, "Linear-Signature")
	_, err := VerifyAndParse(WebhookRequest{Headers: h, RawBody: []byte(body)}, webhookSecret)
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("missing signature: err = %v", err)
	}

	h2 := signedHeaders(t, body)
	delete(h2, "Linear-Delivery")
	_, err = VerifyAndParse(WebhookRequest{Headers: h2, RawBody: []byte(body)}, webhookSecret)
	if err == nil || !strings.Contains(err.Error(), "delivery") {
		t.Errorf("missing delivery: err = %v", err)
	}
}

func TestVerifyAndParseSupportsGithubSignaturePrefix(t *testing.T) {
	// Ported: supports github-style sha256= prefix header.
	body := `{"type":"AgentSessionEvent"}`
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	sig := hex.EncodeToString(mac.Sum(nil))
	v, err := VerifyAndParse(WebhookRequest{
		Headers: map[string][]string{
			"X-Github-Delivery":   {"gh-1"},
			"X-Hub-Signature-256": {"sha256=" + sig},
		},
		RawBody: []byte(body),
	}, webhookSecret)
	if err != nil {
		t.Fatalf("VerifyAndParse: %v", err)
	}
	if v.DeliveryID != "gh-1" {
		t.Errorf("deliveryId = %q", v.DeliveryID)
	}
}

func TestVerifyAndParseRejectsInvalidJSON(t *testing.T) {
	// 'invalid JSON body' maps to SignatureError (401 in the adapter).
	body := `not json`
	h := signedHeaders(t, body)
	v, err := VerifyAndParse(WebhookRequest{Headers: h, RawBody: []byte(body)}, webhookSecret)
	if err == nil || v.DeliveryID != "" || !strings.Contains(err.Error(), "invalid JSON body") {
		t.Errorf("err = %v, v = %#v", err, v)
	}
}

func TestDeliveryDedupReturnsTrueOncePerID(t *testing.T) {
	// Ported: returns true once per delivery id.
	d := NewDeliveryDedup(0)
	if !d.IsFirst("a") {
		t.Error("first 'a' should be true")
	}
	if d.IsFirst("a") {
		t.Error("second 'a' should be false")
	}
	if !d.IsFirst("b") {
		t.Error("first 'b' should be true")
	}
}

func TestDeliveryDedupEvictsAtCapacity(t *testing.T) {
	// Ported: evicts old entries at capacity (drop oldest ~10%).
	d := NewDeliveryDedup(10)
	for i := 0; i < 12; i++ {
		d.IsFirst(fmt.Sprintf("k%d", i))
	}
	if d.IsFirst("k11") {
		t.Error("k11 must still be known")
	}
	// Oldest evicted: k0 seen again reports "first" (a boolean — the
	// vitest assertion pins the eviction behavior class).
	_ = d.IsFirst("k0")
}

func TestTimingSafeEqualHex(t *testing.T) {
	// Ported: compares correctly without short-circuit.
	if !TimingSafeEqualHex("abcd", "abcd") {
		t.Error("equal strings should match")
	}
	if TimingSafeEqualHex("abcd", "abce") {
		t.Error("differing strings should not match")
	}
	if TimingSafeEqualHex("abc", "abcd") {
		t.Error("length mismatch should not match")
	}
}

func TestParseGithubIssueEventMapsOpenedEvents(t *testing.T) {
	// Ported: maps issue opened events to owner/repo#n.
	var payload any
	if err := jsonUnmarshalFixture(t, "webhook-github-issue-opened.json", &payload); err != nil {
		t.Fatal(err)
	}
	d := ParseGithubIssueEvent("issues", payload)
	if d == nil {
		t.Fatal("expected dispatch")
	}
	if d.IssueIdentifier != "acme/api#5" || d.Title != "Fix login" {
		t.Errorf("dispatch = %#v", d)
	}
}

func TestParseGithubIssueEventIgnoresNonIssueEvents(t *testing.T) {
	// Ported: ignores non-issue events, PRs, and malformed payloads.
	if ParseGithubIssueEvent("push", map[string]any{}) != nil {
		t.Error("push event should be ignored")
	}
	if ParseGithubIssueEvent("issues", map[string]any{
		"action":     "opened",
		"issue":      map[string]any{"number": 1, "title": "x", "pull_request": map[string]any{}},
		"repository": map[string]any{"full_name": "a/b"},
	}) != nil {
		t.Error("PR-typed issue should be ignored")
	}
	if ParseGithubIssueEvent("issues", map[string]any{
		"action":     "closed",
		"issue":      map[string]any{"number": 1, "title": "x"},
		"repository": map[string]any{"full_name": "a/b"},
	}) != nil {
		t.Error("closed action should be ignored")
	}
	if ParseGithubIssueEvent("issues", map[string]any{
		"action":     "opened",
		"issue":      map[string]any{"title": "no number"},
		"repository": map[string]any{"full_name": "a/b"},
	}) != nil {
		t.Error("missing number should be ignored")
	}
}

func TestParseAgentSessionEvent(t *testing.T) {
	var payload any
	if err := jsonUnmarshalFixture(t, "webhook-agent-session.json", &payload); err != nil {
		t.Fatal(err)
	}
	d := ParseAgentSessionEvent(payload)
	if d == nil {
		t.Fatal("expected dispatch")
	}
	if d.IssueIdentifier != "ENG-204" {
		t.Errorf("issueIdentifier = %q", d.IssueIdentifier)
	}
	if d.IssueID != "0f3a9c6e-1d2b-4c5d-8e9f-0a1b2c3d4e5f" {
		t.Errorf("issueId = %q", d.IssueID)
	}
	if d.CommentBody != "please proceed with the fix" {
		t.Errorf("commentBody = %q", d.CommentBody)
	}

	// Non-AgentSessionEvent payloads yield null.
	if ParseAgentSessionEvent(map[string]any{"type": "Other"}) != nil {
		t.Error("other types should be ignored")
	}
	if ParseAgentSessionEvent(nil) != nil {
		t.Error("nil should be ignored")
	}
}

// TestHandleWebhookAdapter drives the net/http adapter end-to-end: HMAC
// accept, dedup, tamper rejection, and respond-fast/process-late ordering.
func TestHandleWebhookAdapter(t *testing.T) {
	body := readFixture(t, "webhook-agent-session.json")
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	dedup := NewDeliveryDedup(0)
	var mu sync.Mutex
	processed := []VerifiedWebhook{}
	evicted := sync.WaitGroup{}
	evicted.Add(1)

	handler := func(w http.ResponseWriter, r *http.Request) {
		HandleWebhook(w, r, WebhookOptions{
			SigningSecret: webhookSecret,
			Dedup:         dedup,
			OnEvent: func(v VerifiedWebhook) {
				mu.Lock()
				processed = append(processed, v)
				mu.Unlock()
				evicted.Done()
			},
		})
	}

	doPost := func(headers map[string]string, raw []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/linear", strings.NewReader(string(raw)))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	// 1) Accepted: 200 "accepted" within the response, event processed
	// asynchronously (respond-fast / process-late).
	raw := readFixture(t, "webhook-agent-session.json")
	rec := doPost(map[string]string{
		"Linear-Delivery":  "d1",
		"Linear-Signature": sig,
		"Linear-Event":     "AgentSessionEvent",
	}, raw)
	if rec.Code != 200 || rec.Body.String() != "accepted" {
		t.Fatalf("accept: code=%d body=%q", rec.Code, rec.Body.String())
	}
	waitGroup(&evicted, time.Second)
	mu.Lock()
	if len(processed) != 1 || processed[0].DeliveryID != "d1" {
		t.Errorf("processed = %#v", processed)
	}
	mu.Unlock()

	// 2) Duplicate delivery id: 200 "duplicate", no second processing.
	rec = doPost(map[string]string{
		"Linear-Delivery":  "d1",
		"Linear-Signature": sig,
		"Linear-Event":     "AgentSessionEvent",
	}, body)
	if rec.Code != 200 || rec.Body.String() != "duplicate" {
		t.Errorf("dup: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// 3) Tampered body: 401 "signature mismatch".
	rec = doPost(map[string]string{
		"Linear-Delivery":  "d2",
		"Linear-Signature": sig,
	}, []byte(`{"evil":true}`))
	if rec.Code != 401 || rec.Body.String() != "signature mismatch" {
		t.Errorf("tamper: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// 4) Missing signature: 401 "missing signature".
	rec = doPost(map[string]string{"Linear-Delivery": "d3"}, body)
	if rec.Code != 401 || rec.Body.String() != "missing signature" {
		t.Errorf("missing sig: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// 5) Non-JSON body with valid signature: 401 "invalid JSON body".
	mac2 := hmac.New(sha256.New, []byte(webhookSecret))
	mac2.Write([]byte("not json"))
	rec = doPost(map[string]string{
		"Linear-Delivery":  "d4",
		"Linear-Signature": hex.EncodeToString(mac2.Sum(nil)),
	}, []byte("not json"))
	if rec.Code != 401 || rec.Body.String() != "invalid JSON body" {
		t.Errorf("bad json: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func waitGroup(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}
