// Contract tests for the GitHub Issues adapter, ported from
// test/github-issues.test.ts with recorded fixtures. All HTTP is seamed
// through Doer — zero network. (The buildDeps routing describe block is the
// deps-builder's contract — its fetchTicket routing lands with the
// deps/executor port; the adapter-level contract is fully covered here.)
package integrations

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseGitHubIssueRef(t *testing.T) {
	// Ported: parses owner/repo#n refs.
	parts, ok := ParseGitHubIssueRef("acme/widget#42")
	if !ok {
		t.Fatal("expected ok")
	}
	if parts.Owner != "acme" || parts.Repo != "widget" || parts.Number != 42 {
		t.Errorf("parts = %#v", parts)
	}
}

func TestParseGitHubIssueRefRejectsMalformed(t *testing.T) {
	// Ported: rejects malformed refs.
	if _, ok := ParseGitHubIssueRef("ENG-204"); ok {
		t.Error("ENG-204 should not parse")
	}
	if _, ok := ParseGitHubIssueRef("acme/widget#abc"); ok {
		t.Error("acme/widget#abc should not parse")
	}
	if GitHubIssueRef.MatchString("ENG-204") {
		t.Error("GITHUB_ISSUE_REF should not match ENG-204")
	}
	if !GitHubIssueRef.MatchString("acme.co/repo.x#7") {
		t.Error("GITHUB_ISSUE_REF should match acme.co/repo.x#7")
	}
}

func TestParseGitHubIssueMapsFields(t *testing.T) {
	// Ported: maps fields into TicketSpec.
	var payload GitHubIssuePayload
	if err := jsonUnmarshalFixture(t, "github-issue-response.json", &payload); err != nil {
		t.Fatal(err)
	}
	spec := ParseGitHubIssue("acme/widget#42", payload)
	if spec.ID != "acme/widget#42" {
		t.Errorf("id = %q", spec.ID)
	}
	if spec.Title != "Ship the thing" {
		t.Errorf("title = %q", spec.Title)
	}
	if !reflect.DeepEqual(spec.Labels, []string{"backend"}) {
		t.Errorf("labels = %#v (empty names filtered)", spec.Labels)
	}
	if !reflect.DeepEqual(spec.AcceptanceCriteria, []string{"works", "tested"}) {
		t.Errorf("acceptanceCriteria = %#v", spec.AcceptanceCriteria)
	}
	if spec.TrackerInternalID != "acme/widget#42" {
		t.Errorf("trackerInternalId = %q", spec.TrackerInternalID)
	}
	if spec.URL != "https://github.com/acme/widget/issues/42" {
		t.Errorf("url = %q", spec.URL)
	}
}

func TestFetchGitHubTicketBearerAuthAndRetryAfter(t *testing.T) {
	// Ported: calls the REST endpoint with Bearer auth and honors 429
	// Retry-After.
	tr := newRecordedTransport(t,
		loadRecorded(t, "github-issues-429-retry-after.json"),
		recordedResponse(200, nil, fixtureString(t, "github-issue-response.json")),
	)
	var sleeps []int
	ticket, err := FetchGitHubTicket("acme/widget#42", "tok", FetchOptions{Retries: 2, Sleep: func(ms int) { sleeps = append(sleeps, ms) }, Doer: tr})
	if err != nil {
		t.Fatalf("FetchGitHubTicket: %v", err)
	}
	if ticket.ID != "acme/widget#42" {
		t.Errorf("id = %q", ticket.ID)
	}
	if !reflect.DeepEqual(sleeps, []int{5000}) {
		t.Errorf("sleeps = %#v", sleeps)
	}
	if len(tr.calls) != 2 {
		t.Errorf("calls = %d", len(tr.calls))
	}
	req := tr.calls[0]
	if req.URL.String() != "https://api.github.com/repos/acme/widget/issues/42" {
		t.Errorf("url = %q", req.URL)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("Accept = %q", got)
	}
}

func TestFetchGitHubTicketRetriesOn403SecondaryLimit(t *testing.T) {
	// Ported: retries on 403 secondary rate limits then succeeds.
	tr := newRecordedTransport(t,
		loadRecorded(t, "github-issues-403-secondary.json"),
		recordedResponse(200, nil, fixtureString(t, "github-issue-response.json")),
	)
	ticket, err := FetchGitHubTicket("acme/widget#42", "tok", FetchOptions{Retries: 1, Sleep: noopSleep, Doer: tr, Rand: fixedRand})
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Title != "Ship the thing" {
		t.Errorf("title = %q", ticket.Title)
	}
	if len(tr.calls) != 2 {
		t.Errorf("calls = %d", len(tr.calls))
	}
}

func TestFetchGitHubTicketThrowsOnNonOKAfterRetries(t *testing.T) {
	// Ported: throws on non-ok after retries exhausted.
	tr := newRecordedTransport(t,
		loadRecorded(t, "github-issues-not-found.json"),
		loadRecorded(t, "github-issues-not-found.json"),
	)
	_, err := FetchGitHubTicket("acme/widget#404", "tok", FetchOptions{Retries: 1, Sleep: noopSleep, Doer: tr})
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchGitHubTicketRejectsMalformedRefBeforeAPI(t *testing.T) {
	// Ported: rejects malformed refs before calling the API.
	tr := newRecordedTransport(t) // no recorded responses: any HTTP call fails the test
	_, err := FetchGitHubTicket("ENG-204", "tok", FetchOptions{Doer: tr})
	if err == nil || !strings.Contains(err.Error(), "owner/repo#n") {
		t.Errorf("err = %v", err)
	}
	if len(tr.calls) != 0 {
		t.Errorf("calls = %d, want 0", len(tr.calls))
	}
}
