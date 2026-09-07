// GitLab publisher (Phase 4): the Go port of src/integrations/gitlab.ts.
// Opens a merge request via REST with a project/group access token. Auth:
// PRIVATE-TOKEN header; git push uses any username + token as password.
// Bot users cannot approve their own MRs, which preserves the human-review
// gate. Closed-loop support: PostMrNote attaches validation evidence to an
// MR (trust per PR), WaitForMrPipeline polls CI until a terminal state so
// callers can refuse to hand off unverified changes.
package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitlabCredentials identifies the project and access token.
type GitlabCredentials struct {
	// BaseURL, e.g. "https://gitlab.com" or a self-hosted root.
	BaseURL string
	// ProjectID is the numeric id or URL-encoded path, in string form
	// (the TS number|string is String()-ed before encoding either way).
	ProjectID string
	// Token sent as the PRIVATE-TOKEN header.
	Token string
}

// CreateMrOptions mirrors CreateMrOptions.
type CreateMrOptions struct {
	SourceBranch string
	TargetBranch string
	Title        string
	Description  string
}

// MrPipelineSummary mirrors the pipeline poll row shape.
type MrPipelineSummary struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
}

// projectAPIRoot mirrors the TS projectApiRoot with encodeURIComponent
// parity for path-shaped project ids.
func projectAPIRoot(creds GitlabCredentials) string {
	return fmt.Sprintf("%s/api/v4/projects/%s", creds.BaseURL, encodeURIComponent(creds.ProjectID))
}

// doGitlabRequest issues one REST call with the PRIVATE-TOKEN + JSON
// headers. The PRIVATE-TOKEN key is set verbatim (not via Header.Set)
// because Go's canonicalization would rewrite it to "Private-Token" and
// break byte-parity of recorded requests.
func doGitlabRequest(ctx context.Context, doer Doer, method, url, token string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header["PRIVATE-TOKEN"] = []string{token}
	req.Header.Set("Content-Type", "application/json")
	return doer.Do(req)
}

// apiErrorDetail mirrors apiErrorDetail: extract GitLab's
// `{ message | error }` body for actionable failure output, falling back to
// the first 300 characters of raw text. Cross-runtime note: JSON.stringify
// of a non-string detail preserves TS key order only for scalar details;
// nested-object details serialize with Go's sorted map keys (no fixture
// pins a nested detail).
func apiErrorDetail(res *http.Response) string {
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err == nil {
		detail := payload["message"]
		if detail == nil {
			detail = payload["error"]
		}
		if detail != nil {
			if s, ok := unknownString(detail); ok && s != "" {
				return s
			}
			return string(marshalJSON(detail))
		}
	}
	if text := strings.TrimSpace(string(raw)); text != "" {
		// TS slices UTF-16 code units; Go truncates on runes to avoid
		// splitting a multi-byte character mid-sequence.
		runes := []rune(text)
		if len(runes) > 300 {
			runes = runes[:300]
		}
		return string(runes)
	}
	return ""
}

// gitlabCreateMrBody preserves the TS JSON.stringify field order for
// byte-equal publisher payloads.
type gitlabCreateMrBody struct {
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
}

type gitlabMrResponse struct {
	WebURL string `json:"web_url"`
	IID    int    `json:"iid"`
}

// CreateMergeRequest mirrors createMergeRequest: POST the MR and resolve
// its web_url. Errors carry the HTTP status and GitLab's error body.
func CreateMergeRequest(ctx context.Context, creds GitlabCredentials, opts CreateMrOptions, doer Doer) (string, error) {
	if doer == nil {
		doer = DefaultDoer
	}
	body := marshalJSON(gitlabCreateMrBody{
		SourceBranch: opts.SourceBranch,
		TargetBranch: opts.TargetBranch,
		Title:        opts.Title,
		Description:  opts.Description,
	})
	res, err := doGitlabRequest(ctx, doer, http.MethodPost, projectAPIRoot(creds)+"/merge_requests", creds.Token, body)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if !isOK(res.StatusCode) {
		detail := apiErrorDetail(res)
		if detail != "" {
			detail = ": " + detail
		}
		return "", fmt.Errorf("GitLab MR creation failed: HTTP %d%s", res.StatusCode, detail)
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	var payload gitlabMrResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("invalid GitLab MR response: %w", err)
	}
	if payload.WebURL == "" {
		return "", fmt.Errorf("GitLab MR response missing web_url")
	}
	return payload.WebURL, nil
}

// gitlabNoteBody preserves the TS JSON.stringify field order.
type gitlabNoteBody struct {
	Body string `json:"body"`
}

// PostMrNote mirrors postMrNote: post a note (comment) on a merge request,
// used to attach validation evidence so reviewers see gate results without
// leaving GitLab.
func PostMrNote(ctx context.Context, creds GitlabCredentials, mrIid int, body string, doer Doer) error {
	if doer == nil {
		doer = DefaultDoer
	}
	payload := marshalJSON(gitlabNoteBody{Body: body})
	url := fmt.Sprintf("%s/merge_requests/%d/notes", projectAPIRoot(creds), mrIid)
	res, err := doGitlabRequest(ctx, doer, http.MethodPost, url, creds.Token, payload)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if !isOK(res.StatusCode) {
		detail := apiErrorDetail(res)
		if detail != "" {
			detail = ": " + detail
		}
		return fmt.Errorf("GitLab MR note failed: HTTP %d%s", res.StatusCode, detail)
	}
	return nil
}

// terminalWaitState mirrors the TS TerminalWaitState union.
type terminalWaitState struct {
	done      bool
	succeeded bool
	pipelines []MrPipelineSummary
}

// evaluatePipelines mirrors evaluatePipelines: the latest pipeline (GitLab
// returns newest first) decides; failed/canceled/skipped are terminal
// failures.
func evaluatePipelines(pipelines []MrPipelineSummary) terminalWaitState {
	if len(pipelines) == 0 {
		return terminalWaitState{}
	}
	latest := pipelines[0]
	if latest.Status == "success" {
		return terminalWaitState{done: true, succeeded: true}
	}
	switch latest.Status {
	case "failed", "canceled", "skipped":
		return terminalWaitState{done: true, pipelines: pipelines}
	}
	return terminalWaitState{}
}

// decodePipelines mirrors `Array.isArray(pipelines) ? pipelines : []`:
// valid-but-non-array JSON degrades to an empty list; invalid JSON is an
// error (the TS res.json() would throw).
func decodePipelines(raw []byte) ([]MrPipelineSummary, error) {
	var pipelines []MrPipelineSummary
	if err := json.Unmarshal(raw, &pipelines); err == nil {
		if pipelines == nil {
			pipelines = []MrPipelineSummary{}
		}
		return pipelines, nil
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("invalid GitLab pipeline response: %w", err)
	}
	return []MrPipelineSummary{}, nil
}

// PipelineWaitOptions carries the waitForMrPipeline knobs and seams.
// Convention mirrors FetchOptions: 0 = the TS default; negative = the TS
// explicit 0.
type PipelineWaitOptions struct {
	// TimeoutMs wall-clock budget; 0 = 10 minutes, negative = the deadline
	// has already passed (immediate timeout, used by the ported test).
	TimeoutMs int
	// PollIntervalMs wait between polls; 0 = 15s, negative = no wait.
	PollIntervalMs int
	// Sleep replaces the wall-clock wait between polls (tests record).
	Sleep func(ms int)
	// Doer replaces the default transport.
	Doer Doer
}

// WaitForMrPipeline mirrors waitForMrPipeline: poll the MR's pipelines
// until the latest reaches a terminal state or the deadline passes.
// Resolves true on success; errors with pipeline statuses on failure or
// timeout so callers can fail loudly instead of shipping red.
func WaitForMrPipeline(ctx context.Context, creds GitlabCredentials, mrIid int, opts PipelineWaitOptions) (bool, error) {
	timeoutMs := opts.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = 600000
	}
	pollIntervalMs := opts.PollIntervalMs
	if pollIntervalMs == 0 {
		pollIntervalMs = 15000
	}
	if pollIntervalMs < 0 {
		pollIntervalMs = 0
	}
	doer := opts.Doer
	if doer == nil {
		doer = DefaultDoer
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)

	for {
		url := fmt.Sprintf("%s/merge_requests/%d/pipelines", projectAPIRoot(creds), mrIid)
		res, err := doGitlabRequest(ctx, doer, http.MethodGet, url, creds.Token, nil)
		if err != nil {
			return false, err
		}
		raw, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			return false, err
		}
		if !isOK(res.StatusCode) {
			detail := apiErrorDetailFromRaw(raw)
			if detail != "" {
				detail = ": " + detail
			}
			return false, fmt.Errorf("GitLab pipeline poll failed: HTTP %d%s", res.StatusCode, detail)
		}
		pipelines, err := decodePipelines(raw)
		if err != nil {
			return false, err
		}
		state := evaluatePipelines(pipelines)
		if state.done {
			if state.succeeded {
				return true, nil
			}
			summary := make([]string, 0, 3)
			for i, p := range state.pipelines {
				if i == 3 {
					break
				}
				summary = append(summary, fmt.Sprintf("#%d:%s", p.ID, p.Status))
			}
			return false, fmt.Errorf("GitLab pipeline did not pass (%s)", strings.Join(summary, ", "))
		}
		if !time.Now().Before(deadline) {
			return false, fmt.Errorf("GitLab pipeline wait timed out after %dms for MR !%d", timeoutMs, mrIid)
		}
		if opts.Sleep != nil {
			opts.Sleep(pollIntervalMs)
		} else {
			time.Sleep(time.Duration(pollIntervalMs) * time.Millisecond)
		}
	}
}

// apiErrorDetailFromRaw is apiErrorDetail over an already-read body (the
// pipeline loop needs the raw bytes for decoding too).
func apiErrorDetailFromRaw(raw []byte) string {
	return apiErrorDetail(&http.Response{Body: io.NopCloser(strings.NewReader(string(raw)))})
}
