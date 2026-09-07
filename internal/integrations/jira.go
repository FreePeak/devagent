// Jira Cloud adapter (Phase 4): the Go port of src/integrations/jira.ts.
// Fetches a ticket via REST API v3 with scoped API-token Basic auth.
// Mirrors the Linear adapter shape. Auth: `email:apiToken` base64 — the
// token inherits the user's permissions, so use a dedicated service account
// with minimal project scope.
package integrations

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// JiraCredentials is the REST v3 auth triple.
type JiraCredentials struct {
	// Domain, e.g. "acme.atlassian.net".
	Domain string
	// Email of the token owner.
	Email string
	// APIToken inherits the user's permissions; use a dedicated service
	// account with minimal project scope.
	APIToken string
}

// ADFToText mirrors ADF_TO_TEXT: flattens an Atlassian Document Format
// document to plain text. Text nodes render their text, hardBreak a
// newline; block containers join children with newlines, inline containers
// (paragraph) concatenate their children directly.
func ADFToText(node any) string {
	n, ok := node.(map[string]any)
	if !ok {
		return ""
	}
	typ, _ := n["type"].(string)
	if typ == "text" {
		if t, ok := n["text"].(string); ok {
			return t
		}
		return ""
	}
	if typ == "hardBreak" {
		return "\n"
	}
	// block containers (doc, bulletList, ...) separate children by newline;
	// inline containers (paragraph) concatenate their children directly
	sep := "\n"
	if typ == "paragraph" {
		sep = ""
	}
	children, _ := n["content"].([]any)
	parts := make([]string, 0, len(children))
	for _, c := range children {
		parts = append(parts, ADFToText(c))
	}
	return strings.Join(parts, sep)
}

var (
	jiraHeadingRe        = regexp.MustCompile(`^#{1,6}\s`)
	jiraChecklistRe      = regexp.MustCompile(`^\s*[-*]\s*\[( |x|X)\]\s*(.+)$`)
	jiraAcceptanceSecsel = regexp.MustCompile(`(?i)#{1,6}\s*(acceptance criteria|ac)\b`)
)

// ExtractAcceptanceCriteria mirrors jira.extractAcceptanceCriteria (also
// used by the GitHub Issues adapter): extract `- [ ]` checklist items under
// an "Acceptance Criteria" heading; fall back to all checklist items.
func ExtractAcceptanceCriteria(markdown string) []string {
	lines := strings.Split(markdown, "\n")
	inAC := false
	all := []string{}
	fromAC := []string{}
	for _, line := range lines {
		if jiraAcceptanceSecsel.MatchString(line) {
			inAC = true
			continue
		}
		if jiraHeadingRe.MatchString(line) {
			inAC = false
		}
		if m := jiraChecklistRe.FindStringSubmatch(line); m != nil {
			item := strings.TrimSpace(m[2])
			all = append(all, item)
			if inAC {
				fromAC = append(fromAC, item)
			}
		}
	}
	if len(fromAC) > 0 {
		return fromAC
	}
	return all
}

// jiraIssuePayload mirrors the REST v3 issue shape this adapter consumes.
type jiraIssuePayload struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string   `json:"summary"`
		Description any      `json:"description"` // ADF document
		Labels      []string `json:"labels"`
	} `json:"fields"`
}

// ParseJiraIssue mirrors parseJiraIssue.
func ParseJiraIssue(payload jiraIssuePayload) TicketSpec {
	description := ADFToText(payload.Fields.Description)
	labels := payload.Fields.Labels
	if labels == nil {
		labels = []string{}
	}
	return TicketSpec{
		ID:                 payload.Key,
		Title:              payload.Fields.Summary,
		Description:        description,
		Labels:             labels,
		AcceptanceCriteria: ExtractAcceptanceCriteria(description),
		TrackerInternalID:  payload.Key,
	}
}

// FetchJiraTicket mirrors fetchJiraTicket: REST v3 fetch with 429 retries
// honoring Retry-After. Cross-runtime note: a V8 JSON parse failure would
// throw SyntaxError here — Go surfaces the decoder error instead (no
// fixture pins that message).
func FetchJiraTicket(issueKey string, creds JiraCredentials, opts FetchOptions) (TicketSpec, error) {
	auth := base64.StdEncoding.EncodeToString([]byte(creds.Email + ":" + creds.APIToken))
	url := fmt.Sprintf("https://%s/rest/api/3/issue/%s?fields=summary,description,labels",
		creds.Domain, encodeURIComponent(issueKey))
	res, err := fetchWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Basic "+auth)
		req.Header.Set("Accept", "application/json")
		return req, nil
	}, opts, func(status int) bool { return status == 429 })
	if err != nil {
		return TicketSpec{}, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return TicketSpec{}, err
	}

	if !isOK(res.StatusCode) {
		return TicketSpec{}, fmt.Errorf("Jira API request failed: HTTP %d", res.StatusCode)
	}

	var payload jiraIssuePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return TicketSpec{}, fmt.Errorf("invalid Jira response: %w", err)
	}
	if payload.Key == "" {
		return TicketSpec{}, fmt.Errorf("Jira issue not found: %s", issueKey)
	}
	return ParseJiraIssue(payload), nil
}
