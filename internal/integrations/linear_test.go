// Contract tests for the Linear thin client, ported from
// test/integrations.test.ts (describe('linear')) with the recorded fixtures
// in testdata/. All HTTP is seamed through Doer — zero network.

package integrations

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func noopSleep(int) {}

func fixedRand(int) int { return 0 }

// graphqlBody decodes the outgoing GraphQL request body.
func graphqlBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

// wantIssueJSON loads the recorded success envelope.
func wantIssueJSON(t *testing.T) []byte {
	t.Helper()
	return readFixture(t, "linear-issue-response.json")
}

func TestLinearIssueQueryShape(t *testing.T) {
	// Ported: exports a query selecting expected fields by identifier.
	for _, want := range []string{
		"issue(id: $id)", "title", "description", "url", "labels", "name",
	} {
		if !strings.Contains(LinearIssueQuery, want) {
			t.Errorf("LinearIssueQuery missing %q", want)
		}
	}
}

func TestParseLinearIssueMapsLabelsDescriptionURL(t *testing.T) {
	// Ported: parseLinearIssue maps labels, description and url.
	var payload any
	if err := json.Unmarshal(wantIssueJSON(t), &payload); err != nil {
		t.Fatal(err)
	}
	// The fixture pins the fetchTicket flow; here it is re-id'd to the
	// pure-parse fixture from the vitest file (title "Add endpoint").
	payload = map[string]any{
		"data": map[string]any{
			"issue": map[string]any{
				"title":       "Add endpoint",
				"description": "Do the thing",
				"url":         "https://linear.app/proj/issue/ENG-1",
				"labels":      map[string]any{"nodes": []any{map[string]any{"name": "backend"}, map[string]any{"name": "P1"}}},
			},
		},
	}
	ticket, err := ParseLinearIssue(payload)
	if err != nil {
		t.Fatalf("ParseLinearIssue: %v", err)
	}
	if ticket.Title != "Add endpoint" {
		t.Errorf("title = %q", ticket.Title)
	}
	if ticket.Description != "Do the thing" {
		t.Errorf("description = %q", ticket.Description)
	}
	if !reflect.DeepEqual(ticket.Labels, []string{"backend", "P1"}) {
		t.Errorf("labels = %#v", ticket.Labels)
	}
	if ticket.URL != "https://linear.app/proj/issue/ENG-1" {
		t.Errorf("url = %q", ticket.URL)
	}
}

func TestExtractLinearAcceptanceCriteriaPrefersHeading(t *testing.T) {
	// Ported: extractAcceptanceCriteria prefers checklist under Acceptance heading.
	md := strings.Join([]string{
		"## Context",
		"- [ ] not an AC",
		"",
		"## Acceptance Criteria",
		"- [ ] first item",
		"- [x] second item",
		"- [X] third ITEM",
		"",
		"## Notes",
		"- [ ] also not an AC",
	}, "\n")
	got := ExtractLinearAcceptanceCriteria(md)
	want := []string{"first item", "second item", "third ITEM"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestExtractLinearAcceptanceCriteriaFallsBackToAll(t *testing.T) {
	// Ported: falls back to all checklist items without heading.
	md := "Intro text\n\n- [ ] alpha\n- [X] beta"
	got := ExtractLinearAcceptanceCriteria(md)
	want := []string{"alpha", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestParseLinearIssueEmptyAcceptance(t *testing.T) {
	// Ported: returns empty acceptanceCriteria when no checklists.
	ticket, err := ParseLinearIssue(map[string]any{
		"data": map[string]any{"issue": map[string]any{"title": "T", "description": "plain text only"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ticket.AcceptanceCriteria) != 0 {
		t.Errorf("acceptanceCriteria = %#v", ticket.AcceptanceCriteria)
	}
}

func TestLinearFetchTicketPostsRawAuthorization(t *testing.T) {
	// Ported: fetchTicket posts with raw Authorization header (no Bearer).
	tr := newRecordedTransport(t, recordedResponse(200, nil, fixtureString(t, "linear-issue-response.json")))
	ticket, err := LinearFetchTicket("ENG-9", "lin_api_key_123", FetchOptions{Doer: tr, Sleep: noopSleep})
	if err != nil {
		t.Fatalf("LinearFetchTicket: %v", err)
	}
	if ticket.ID != "ENG-9" {
		t.Errorf("id = %q", ticket.ID)
	}
	if !reflect.DeepEqual(ticket.AcceptanceCriteria, []string{"works"}) {
		t.Errorf("acceptanceCriteria = %#v", ticket.AcceptanceCriteria)
	}
	if len(tr.calls) != 1 {
		t.Fatalf("calls = %d", len(tr.calls))
	}
	req := tr.calls[0]
	if req.URL.String() != "https://api.linear.app/graphql" {
		t.Errorf("url = %q", req.URL)
	}
	if got := req.Header.Get("Authorization"); got != "lin_api_key_123" {
		t.Errorf("Authorization = %q (raw, no Bearer, expected)", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	// Byte parity: the request body must equal the recorded request fixture.
	if string(tr.bodies[0]) != fixtureString(t, "linear-issue-request.json") {
		t.Errorf("request body mismatch:\n got  %s\n want %s", tr.bodies[0], fixtureString(t, "linear-issue-request.json"))
	}
	if got := graphqlBody(t, tr.bodies[0])["variables"].(map[string]any)["id"]; got != "ENG-9" {
		t.Errorf("variables.id = %v", got)
	}
}

func TestLinearFetchTicketRetries429HonoringRetryAfter(t *testing.T) {
	// Ported: fetchLinear retries 429 honoring Retry-After, then succeeds.
	okBody := `{"data":{"issue":{"title":"ok","description":""}}}`
	tr := newRecordedTransport(t,
		loadRecorded(t, "linear-429-retry-after.json"),
		recordedResponse(200, nil, okBody),
	)
	var sleeps []int
	res, err := FetchLinear([]byte("{}"), "key", FetchOptions{Retries: 2, Sleep: func(ms int) { sleeps = append(sleeps, ms) }, Doer: tr})
	if err != nil {
		t.Fatal(err)
	}
	drain(res)
	if res.StatusCode != 200 {
		t.Errorf("status = %d", res.StatusCode)
	}
	if len(tr.calls) != 2 {
		t.Errorf("calls = %d", len(tr.calls))
	}
	// Retry-After: 2 seconds honored exactly.
	if !reflect.DeepEqual(sleeps, []int{2000}) {
		t.Errorf("sleeps = %#v", sleeps)
	}
}

func TestLinearFetchLinearGivesUpAfterMaxRetries(t *testing.T) {
	// Ported: fetchLinear gives up after max retries on persistent 429 —
	// this is the persistent-failure stop condition (retry budget
	// exhausted, the last 429 Response comes back to the caller).
	tr := newRecordedTransport(t,
		loadRecorded(t, "linear-429-no-retry-after.json"),
		loadRecorded(t, "linear-429-no-retry-after.json"),
		loadRecorded(t, "linear-429-no-retry-after.json"),
	)
	var sleeps []int
	res, err := FetchLinear([]byte("{}"), "key", FetchOptions{Retries: 2, Sleep: func(ms int) { sleeps = append(sleeps, ms) }, Doer: tr, Rand: fixedRand})
	if err != nil {
		t.Fatal(err)
	}
	drain(res)
	if res.StatusCode != 429 {
		t.Errorf("status = %d", res.StatusCode)
	}
	if len(tr.calls) != 3 { // initial + 2 retries
		t.Errorf("calls = %d", len(tr.calls))
	}
}

func TestLinearFetchTicketThrowsOnNon200(t *testing.T) {
	// Ported: fetchTicket throws on non-200 responses.
	tr := newRecordedTransport(t, loadRecorded(t, "linear-unauthorized.json"))
	_, err := LinearFetchTicket("ENG-1", "key", FetchOptions{Doer: tr, Sleep: noopSleep})
	if err == nil || !strings.Contains(err.Error(), "Linear API request failed: HTTP 401") {
		t.Errorf("err = %v", err)
	}
}

func TestLinearFetchTicketThrowsWhenIssueMissing(t *testing.T) {
	// Ported: fetchTicket throws when issue missing from response.
	tr := newRecordedTransport(t, recordedResponse(200, nil, fixtureString(t, "linear-issue-missing.json")))
	_, err := LinearFetchTicket("NOPE-404", "key", FetchOptions{Doer: tr, Sleep: noopSleep})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "issue not found") {
		t.Errorf("err = %v", err)
	}
}

func TestLinearFetchTicketGraphQLErrorPrefersMessage(t *testing.T) {
	tr := newRecordedTransport(t, recordedResponse(200, nil, `{"errors":[{"message":"Field 'nope' not found"}]}`))
	_, err := LinearFetchTicket("ENG-1", "key", FetchOptions{Doer: tr, Sleep: noopSleep})
	if err == nil || !strings.Contains(err.Error(), "Linear GraphQL error: Field 'nope' not found") {
		t.Errorf("err = %v", err)
	}
}

func TestLinearPostTicketCommentByteEqualPayload(t *testing.T) {
	// Publisher payload parity: comment mutation body byte-equal to the
	// recorded fixture.
	tr := newRecordedTransport(t, recordedResponse(200, nil, `{"data":{"commentCreate":{"success":true}}}`))
	if err := LinearPostTicketComment("0f3a9c6e-1d2b-4c5d-8e9f-0a1b2c3d4e5f", "Gate results: all green", "lin_key", FetchOptions{Doer: tr, Sleep: noopSleep}); err != nil {
		t.Fatalf("LinearPostTicketComment: %v", err)
	}
	if string(tr.bodies[0]) != fixtureString(t, "linear-comment-request.json") {
		t.Errorf("comment payload mismatch:\n got  %s\n want %s", tr.bodies[0], fixtureString(t, "linear-comment-request.json"))
	}
}

func TestLinearPostTicketCommentHTTPErrors(t *testing.T) {
	tr := newRecordedTransport(t, loadRecorded(t, "linear-unauthorized.json"))
	err := LinearPostTicketComment("i", "b", "k", FetchOptions{Doer: tr, Sleep: noopSleep})
	if err == nil || !strings.Contains(err.Error(), "Linear comment failed: HTTP 401") {
		t.Errorf("err = %v", err)
	}

	tr2 := newRecordedTransport(t, recordedResponse(200, nil, `{"errors":[{"message":"nope"}]}`))
	err = LinearPostTicketComment("i", "b", "k", FetchOptions{Doer: tr2, Sleep: noopSleep})
	if err == nil || !strings.Contains(err.Error(), "Linear comment GraphQL error: nope") {
		t.Errorf("err = %v", err)
	}
}

func TestLinearTransportErrorPropagates(t *testing.T) {
	// Transport-level failure surfaces (distinct from recorded statuses).
	sentinel := errors.New("connect refused")
	_, err := LinearFetchTicket("ENG-1", "key", FetchOptions{Doer: errTransport{sentinel}})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v", err)
	}
}
