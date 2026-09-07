// GitHub Issues adapter: the Go port of src/integrations/github-issues.ts.
// Fetches a webhook-dispatched issue via the REST API so it can enter the
// same pipeline as Linear/Jira tickets. Issue refs are shaped `owner/repo#n`;
// auth is a PAT passed as Bearer — use a fine-grained token with read-only
// access to the target repos. Rate limits: 429 too-many-requests and 403
// secondary rate limit both retry with Retry-After-aware backoff.

package integrations

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
)

// GitHubIssueRef matches `owner/repo#n` composite issue refs.
var GitHubIssueRef = regexp.MustCompile(`^[\w.-]+/[\w.-]+#\d+$`)

var githubIssueRefParts = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)#(\d+)$`)

// GitHubIssueRefParts is a parsed `owner/repo#n` ref.
type GitHubIssueRefParts struct {
	Owner  string
	Repo   string
	Number int
}

// ParseGitHubIssueRef mirrors parseGitHubIssueRef; ok=false when malformed.
func ParseGitHubIssueRef(ref string) (GitHubIssueRefParts, bool) {
	m := githubIssueRefParts.FindStringSubmatch(ref)
	if m == nil {
		return GitHubIssueRefParts{}, false
	}
	number, err := strconv.Atoi(m[3])
	if err != nil {
		return GitHubIssueRefParts{}, false
	}
	return GitHubIssueRefParts{Owner: m[1], Repo: m[2], Number: number}, true
}

// GitHubIssuePayload mirrors the REST issue shape this adapter consumes.
// Title is a pointer so an absent title (TS `typeof title !== 'string'`)
// is distinguishable from an empty one.
type GitHubIssuePayload struct {
	Number int     `json:"number"`
	Title  *string `json:"title"`
	Body   *string `json:"body"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	HTMLURL string `json:"html_url"`
}

// ParseGitHubIssue mirrors parseGitHubIssue.
func ParseGitHubIssue(ref string, payload GitHubIssuePayload) TicketSpec {
	description := ""
	if payload.Body != nil {
		description = *payload.Body
	}
	labels := make([]string, 0, len(payload.Labels))
	for _, l := range payload.Labels {
		labels = append(labels, l.Name)
	}
	return TicketSpec{
		ID:                 ref,
		Title:              *payload.Title,
		Description:        description,
		Labels:             nonEmptyStrings(labels),
		AcceptanceCriteria: ExtractAcceptanceCriteria(description),
		TrackerInternalID:  ref,
		URL:                payload.HTMLURL,
	}
}

// FetchGitHubTicket mirrors fetchGitHubTicket: REST fetch of
// `owner/repo#n` with 429 and 403 (secondary rate limit) retries.
func FetchGitHubTicket(issueRef, token string, opts FetchOptions) (TicketSpec, error) {
	parsed, ok := ParseGitHubIssueRef(issueRef)
	if !ok {
		return TicketSpec{}, fmt.Errorf("Invalid GitHub issue ref: %s (expected owner/repo#n)", issueRef)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", parsed.Owner, parsed.Repo, parsed.Number)
	res, err := fetchWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		return req, nil
	}, opts, func(status int) bool { return status == 429 || status == 403 })
	if err != nil {
		return TicketSpec{}, err
	}
	defer func() { _ = res.Body.Close() }() // errcheck: close is best-effort after the body is consumed
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return TicketSpec{}, err
	}

	if !isOK(res.StatusCode) {
		return TicketSpec{}, fmt.Errorf("GitHub Issues API request failed: HTTP %d", res.StatusCode)
	}

	var payload GitHubIssuePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return TicketSpec{}, fmt.Errorf("invalid GitHub Issues response: %w", err)
	}
	if payload.Number == 0 || payload.Title == nil {
		return TicketSpec{}, fmt.Errorf("GitHub issue not found: %s", issueRef)
	}
	return ParseGitHubIssue(issueRef, payload), nil
}
