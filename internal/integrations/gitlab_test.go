// Contract tests for the GitLab publisher, ported from test/gitlab.test.ts
// with recorded fixtures. All HTTP is seamed through Doer — zero network.
package integrations

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

var glCreds = GitlabCredentials{BaseURL: "https://gitlab.com", ProjectID: "42", Token: "glpat-x"}

func TestGitlabCreateMergeRequestPostsPrivateToken(t *testing.T) {
	// Ported: posts PRIVATE-TOKEN MR and returns web_url.
	tr := newRecordedTransport(t, recordedResponse(201, nil, fixtureString(t, "gitlab-mr-create-response.json")))
	url, err := CreateMergeRequest(context.Background(), glCreds,
		CreateMrOptions{SourceBranch: "devagent/E-1", TargetBranch: "main", Title: "t", Description: "d"}, tr)
	if err != nil {
		t.Fatalf("CreateMergeRequest: %v", err)
	}
	if url != "https://gitlab.com/g/p/-/merge_requests/3" {
		t.Errorf("url = %q", url)
	}
	req := tr.calls[0]
	if !strings.Contains(req.URL.String(), "/api/v4/projects/42/merge_requests") {
		t.Errorf("url = %q", req.URL)
	}
	// The PRIVATE-TOKEN key is stored verbatim so the wire carries it
	// un-canonicalized, byte-equal to the TS request.
	if got := req.Header["PRIVATE-TOKEN"]; len(got) != 1 || got[0] != "glpat-x" {
		t.Errorf("PRIVATE-TOKEN = %#v", req.Header)
	}
	// Publisher payload byte-parity against the recorded request fixture.
	if string(tr.bodies[0]) != fixtureString(t, "gitlab-mr-create-request.json") {
		t.Errorf("MR payload mismatch:\n got  %s\n want %s", tr.bodies[0], fixtureString(t, "gitlab-mr-create-request.json"))
	}
	parsed := graphqlBody(t, tr.bodies[0])
	if parsed["source_branch"] != "devagent/E-1" {
		t.Errorf("source_branch = %v", parsed["source_branch"])
	}
	if parsed["target_branch"] != "main" {
		t.Errorf("target_branch = %v", parsed["target_branch"])
	}
}

func TestGitlabCreateMergeRequestURLEncodesProjectPath(t *testing.T) {
	// Ported: URL-encodes project path ids.
	tr := newRecordedTransport(t, recordedResponse(201, nil, `{"web_url":"u"}`))
	_, err := CreateMergeRequest(context.Background(),
		GitlabCredentials{BaseURL: "https://gitlab.com", ProjectID: "group/proj", Token: "glpat-x"},
		CreateMrOptions{SourceBranch: "a", TargetBranch: "b", Title: "t", Description: "d"}, tr)
	if err != nil {
		t.Fatal(err)
	}
	// encodeURIComponent("group/proj") = "group%2Fproj".
	if !strings.Contains(tr.calls[0].URL.String(), "group%2Fproj") {
		t.Errorf("url = %q", tr.calls[0].URL)
	}
}

func TestGitlabCreateMergeRequestThrowsWithAPIErrorBody(t *testing.T) {
	// Ported: throws with HTTP status and API error body on failure.
	tr := newRecordedTransport(t, recordedResponse(401, nil, fixtureString(t, "gitlab-mr-error.json")))
	_, err := CreateMergeRequest(context.Background(), glCreds,
		CreateMrOptions{SourceBranch: "a", TargetBranch: "b", Title: "t", Description: "d"}, tr)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401: invalid token") {
		t.Errorf("err = %v", err)
	}
}

func TestGitlabCreateMergeRequestThrowsWhenWebURLMissing(t *testing.T) {
	// Ported: throws when web_url is missing.
	tr := newRecordedTransport(t, recordedResponse(201, nil, fixtureString(t, "gitlab-mr-missing-web-url.json")))
	_, err := CreateMergeRequest(context.Background(), glCreds,
		CreateMrOptions{SourceBranch: "a", TargetBranch: "b", Title: "t", Description: "d"}, tr)
	if err == nil || !strings.Contains(err.Error(), "web_url") {
		t.Errorf("err = %v", err)
	}
}

func TestGitlabPostMrNote(t *testing.T) {
	// Ported: posts note body to the MR notes endpoint.
	tr := newRecordedTransport(t, recordedResponse(201, nil, `{"id":9}`))
	if err := PostMrNote(context.Background(), glCreds, 3, "validation evidence: all gates green", tr); err != nil {
		t.Fatalf("PostMrNote: %v", err)
	}
	if got := tr.calls[0].URL.String(); got != "https://gitlab.com/api/v4/projects/42/merge_requests/3/notes" {
		t.Errorf("url = %q", got)
	}
	// Note payload byte-parity against the recorded request fixture.
	if string(tr.bodies[0]) != fixtureString(t, "gitlab-mr-note-request.json") {
		t.Errorf("note payload mismatch:\n got  %s\n want %s", tr.bodies[0], fixtureString(t, "gitlab-mr-note-request.json"))
	}
}

func TestGitlabPostMrNoteThrowsWithAPIErrorDetail(t *testing.T) {
	// Ported: throws with API error detail on failure.
	tr := newRecordedTransport(t, recordedResponse(404, nil, fixtureString(t, "gitlab-mr-note-error.json")))
	err := PostMrNote(context.Background(), glCreds, 3, "x", tr)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404: 404 Not Found") {
		t.Errorf("err = %v", err)
	}
}

func TestGitlabWaitForMrPipelineResolvesOnSuccess(t *testing.T) {
	// Ported: resolves true when the latest pipeline succeeds.
	tr := newRecordedTransport(t, recordedResponse(200, nil, fixtureString(t, "gitlab-pipelines-success.json")))
	ok, err := WaitForMrPipeline(context.Background(), glCreds, 3, PipelineWaitOptions{PollIntervalMs: -1, Sleep: noopSleep, Doer: tr})
	if err != nil || !ok {
		t.Errorf("ok = %v, err = %v", ok, err)
	}
}

func TestGitlabWaitForMrPipelinePollsUntilTerminal(t *testing.T) {
	// Ported: polls until terminal state, sleeping between attempts.
	tr := newRecordedTransport(t,
		recordedResponse(200, nil, fixtureString(t, "gitlab-pipelines-running.json")),
		recordedResponse(200, nil, fixtureString(t, "gitlab-pipelines-pending.json")),
		recordedResponse(200, nil, fixtureString(t, "gitlab-pipelines-success.json")),
	)
	var sleeps []int
	ok, err := WaitForMrPipeline(context.Background(), glCreds, 3,
		PipelineWaitOptions{PollIntervalMs: 2000, Sleep: func(ms int) { sleeps = append(sleeps, ms) }, Doer: tr})
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v", ok, err)
	}
	if !reflect.DeepEqual(sleeps, []int{2000, 2000}) {
		t.Errorf("sleeps = %#v", sleeps)
	}
}

func TestGitlabWaitForMrPipelineThrowsListingStatuses(t *testing.T) {
	// Ported: throws listing pipeline statuses on terminal failure.
	tr := newRecordedTransport(t, recordedResponse(200, nil, fixtureString(t, "gitlab-pipelines-failed.json")))
	_, err := WaitForMrPipeline(context.Background(), glCreds, 3, PipelineWaitOptions{PollIntervalMs: -1, Sleep: noopSleep, Doer: tr})
	if err == nil || !strings.Contains(err.Error(), "#6:failed") {
		t.Errorf("err = %v", err)
	}
}

func TestGitlabWaitForMrPipelineTimesOutLoudly(t *testing.T) {
	// Ported: times out loudly while pipelines stay non-terminal.
	// TimeoutMs: -1 puts the deadline in the past so the first
	// non-terminal poll times out (the vitest case used timeoutMs 0).
	tr := newRecordedTransport(t, recordedResponse(200, nil, fixtureString(t, "gitlab-pipelines-running.json")))
	_, err := WaitForMrPipeline(context.Background(), glCreds, 3, PipelineWaitOptions{TimeoutMs: -1, PollIntervalMs: -1, Sleep: noopSleep, Doer: tr})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v", err)
	}
}

func TestGitlabWaitForMrPipelineWaitsWhenNoPipelinesYet(t *testing.T) {
	// Ported: waits (not fails) when no pipelines exist yet.
	tr := newRecordedTransport(t,
		recordedResponse(200, nil, fixtureString(t, "gitlab-pipelines-empty.json")),
		recordedResponse(200, nil, fixtureString(t, "gitlab-pipelines-success.json")),
	)
	ok, err := WaitForMrPipeline(context.Background(), glCreds, 3, PipelineWaitOptions{PollIntervalMs: -1, Sleep: noopSleep, Doer: tr})
	if err != nil || !ok {
		t.Errorf("ok = %v, err = %v", ok, err)
	}
}

func TestGitlabWaitForMrPipelineNonArrayBodyDegradesToEmpty(t *testing.T) {
	// Mirrors `Array.isArray(pipelines) ? pipelines : []`: a valid-JSON
	// non-array body degrades to an empty list (then the deadline check
	// times out), while invalid JSON is a decode error.
	tr := newRecordedTransport(t, recordedResponse(200, nil, `{"unexpected":"shape"}`))
	_, err := WaitForMrPipeline(context.Background(), glCreds, 3, PipelineWaitOptions{TimeoutMs: -1, PollIntervalMs: -1, Sleep: noopSleep, Doer: tr})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v", err)
	}
	tr2 := newRecordedTransport(t, recordedResponse(200, nil, `not-json`))
	_, err = WaitForMrPipeline(context.Background(), glCreds, 3, PipelineWaitOptions{PollIntervalMs: -1, Sleep: noopSleep, Doer: tr2})
	if err == nil || !strings.Contains(err.Error(), "invalid GitLab pipeline response") {
		t.Errorf("err = %v", err)
	}
}
