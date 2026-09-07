// Contract tests for the Jira adapter, ported from test/jira.test.ts with
// recorded fixtures. All HTTP is seamed through Doer — zero network.
package integrations

import (
	"reflect"
	"strings"
	"testing"
)

func TestADFToTextFlattensParagraphsAndText(t *testing.T) {
	// Ported: flattens paragraphs and text nodes.
	doc := map[string]any{
		"type": "doc",
		"content": []any{
			map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": "Line one."}}},
			map[string]any{"type": "paragraph", "content": []any{
				map[string]any{"type": "text", "text": "Line two "},
				map[string]any{"type": "text", "text": "continued."},
			}},
		},
	}
	if got := ADFToText(doc); got != "Line one.\nLine two continued." {
		t.Errorf("got %q", got)
	}
}

func TestADFToTextHandlesHardBreakAndNull(t *testing.T) {
	// Ported: handles hardBreak and null safely.
	got := ADFToText(map[string]any{
		"type":    "paragraph",
		"content": []any{map[string]any{"type": "hardBreak"}, map[string]any{"type": "text", "text": "x"}},
	})
	if got != "\nx" {
		t.Errorf("got %q", got)
	}
	if ADFToText(nil) != "" {
		t.Error("nil should flatten to empty string")
	}
}

func TestJiraExtractAcceptanceCriteriaPrefersHeading(t *testing.T) {
	// Ported: prefers checklist under an acceptance heading.
	md := "## Context\n- [ ] not AC\n\n## Acceptance Criteria\n- [ ] first\n- [X] second"
	got := ExtractAcceptanceCriteria(md)
	want := []string{"first", "second"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestJiraExtractAcceptanceCriteriaFallsBackToAll(t *testing.T) {
	// Ported: falls back to all checklist items without a heading.
	got := ExtractAcceptanceCriteria("- [ ] alpha\n- [x] beta")
	want := []string{"alpha", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestParseJiraIssueMapsFields(t *testing.T) {
	// Ported: maps fields into TicketSpec (payload mirrored from the
	// vitest fixture; the recorded REST fixture is exercised end-to-end in
	// TestFetchJiraTicketBasicAuthAndRetryAfter).
	var payload jiraIssuePayload
	if err := jsonUnmarshalFixture(t, "jira-issue-response.json", &payload); err != nil {
		t.Fatal(err)
	}
	spec := ParseJiraIssue(payload)
	if spec.ID != "PROJ-7" {
		t.Errorf("id = %q", spec.ID)
	}
	if spec.Title != "Ship the thing" {
		t.Errorf("title = %q", spec.Title)
	}
	if !reflect.DeepEqual(spec.Labels, []string{"backend"}) {
		t.Errorf("labels = %#v", spec.Labels)
	}
	if !reflect.DeepEqual(spec.AcceptanceCriteria, []string{"works"}) {
		t.Errorf("acceptanceCriteria = %#v", spec.AcceptanceCriteria)
	}
	if spec.TrackerInternalID != "PROJ-7" {
		t.Errorf("trackerInternalId = %q", spec.TrackerInternalID)
	}
}

func TestFetchJiraTicketBasicAuthAndRetryAfter(t *testing.T) {
	// Ported: uses Basic auth and honors 429 Retry-After.
	tr := newRecordedTransport(t,
		loadRecorded(t, "jira-429-retry-after.json"),
		recordedResponse(200, nil, fixtureString(t, "jira-issue-response.json")),
	)
	var sleeps []int
	ticket, err := FetchJiraTicket("PROJ-7", JiraCredentials{Domain: "acme.atlassian.net", Email: "bot@acme.io", APIToken: "tok"},
		FetchOptions{Retries: 2, Sleep: func(ms int) { sleeps = append(sleeps, ms) }, Doer: tr})
	if err != nil {
		t.Fatalf("FetchJiraTicket: %v", err)
	}
	if ticket.ID != "PROJ-7" {
		t.Errorf("id = %q", ticket.ID)
	}
	if !reflect.DeepEqual(sleeps, []int{3000}) {
		t.Errorf("sleeps = %#v", sleeps)
	}
	if len(tr.calls) != 2 {
		t.Errorf("calls = %d", len(tr.calls))
	}
	req := tr.calls[0]
	if req.URL.String() != "https://acme.atlassian.net/rest/api/3/issue/PROJ-7?fields=summary,description,labels" {
		t.Errorf("url = %q", req.URL)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Basic ") {
		t.Errorf("Authorization = %q, want Basic auth", auth)
	}
	// bot@acme.io:tok base64-encoded.
	if auth != "Basic Ym90QGFjbWUuaW86dG9r" {
		t.Errorf("Authorization = %q (expected email:apiToken base64)", auth)
	}
}

func TestFetchJiraTicketThrowsOnNon200AfterRetries(t *testing.T) {
	// Ported: throws on non-200 after retries exhausted. Only 429 is
	// retryable for Jira — a 403 surfaces immediately (single call).
	tr := newRecordedTransport(t, loadRecorded(t, "jira-forbidden.json"))
	_, err := FetchJiraTicket("PROJ-9", JiraCredentials{Domain: "d", Email: "e", APIToken: "t"},
		FetchOptions{Retries: 1, Sleep: noopSleep, Doer: tr})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("err = %v", err)
	}
	if len(tr.calls) != 1 {
		t.Errorf("calls = %d, want 1 (403 is not retryable for Jira)", len(tr.calls))
	}
}

func TestFetchJiraTicketMissingIssue(t *testing.T) {
	tr := newRecordedTransport(t, recordedResponse(200, nil, `{}`))
	_, err := FetchJiraTicket("NOPE-1", JiraCredentials{Domain: "d", Email: "e", APIToken: "t"},
		FetchOptions{Doer: tr, Sleep: noopSleep})
	if err == nil || !strings.Contains(err.Error(), "Jira issue not found: NOPE-1") {
		t.Errorf("err = %v", err)
	}
}
