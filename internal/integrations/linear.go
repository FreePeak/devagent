// Linear GraphQL thin client: the Go port of src/integrations/linear.ts.
// There is no official Linear Go SDK, so the GraphQL documents are
// hand-rolled against the recorded shapes and contract-tested from fixtures.
// Rate limiting: honor Retry-After on 429 with jittered backoff (max 3
// retries — the shared fetchWithRetry default).
package integrations

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const linearGraphQLEndpoint = "https://api.linear.app/graphql"

// LinearIssueQuery is the GraphQL query fetching a Linear issue by
// identifier (e.g. "ENG-204"). Exported so tests can assert on its shape
// without network access. Byte-identical to the TS LINEAR_ISSUE_QUERY.
const LinearIssueQuery = `query Issue($id: String!) {
  issue(id: $id) {
    id
    title
    description
    url
    labels {
      nodes {
        name
      }
    }
  }
}`

// linearCommentMutation posts a progress/clarification comment (FR-TICKET-03).
const linearCommentMutation = `mutation Comment($input: CommentCreateInput!) {
  commentCreate(input: $input) {
    success
  }
}`

// linearRequest builds a POST against the Linear GraphQL endpoint with the
func linearRequest(body []byte, apiKey string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, linearGraphQLEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", apiKey)
	return req, nil
}

// FetchLinear mirrors fetchLinear: POST body with retries on 429. The
// returned response is the caller's to close; the final status is returned
// even when still 429 (the TS version returns the last Response as-is).
func FetchLinear(body []byte, apiKey string, opts FetchOptions) (*http.Response, error) {
	return fetchWithRetry(func() (*http.Request, error) {
		return linearRequest(body, apiKey)
	}, opts, func(status int) bool { return status == 429 })
}

// linearGraphQLResponse mirrors the TS GraphQLResponse envelope. Field
// values stay untyped (any) because parse semantics follow the TS
// typeof-guard discipline.
type linearGraphQLResponse struct {
	Data struct {
		Issue map[string]any `json:"issue"`
	} `json:"data"`
	Errors []struct {
		Message any `json:"message"`
	} `json:"errors"`
}

// decodeLinearResponse parses a GraphQL envelope; a null/absent data.issue
// leaves Issue nil.
func decodeLinearResponse(body []byte) (*linearGraphQLResponse, error) {
	var out linearGraphQLResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

var (
	linearChecklistRe  = regexp.MustCompile(`^\s*-\s+\[[ xX]\]\s+`)
	linearHeadingRe    = regexp.MustCompile(`^#{1,6}\s+`)
	linearHeadingLevel = regexp.MustCompile(`^#+`)
	linearAcceptanceRe = regexp.MustCompile(`(?i)^#{1,6}\s+.*acceptance`)
)

// ExtractLinearAcceptanceCriteria mirrors linear.extractAcceptanceCriteria.
// Preference order:
//  1. Checklist items under any heading containing "acceptance"
//     (case-insensitive) up to the next heading of the same-or-higher level.
//  2. All checklist items anywhere in the document.
//  3. Empty list.
func ExtractLinearAcceptanceCriteria(description string) []string {
	lines := splitLines(description)

	acceptanceHeadingIdx := -1
	for i, line := range lines {
		if linearAcceptanceRe.MatchString(line) {
			acceptanceHeadingIdx = i
			break
		}
	}

	var scope []string
	if acceptanceHeadingIdx >= 0 {
		headingLevel := headingDepth(lines[acceptanceHeadingIdx])
		end := len(lines)
		for i := acceptanceHeadingIdx + 1; i < len(lines); i++ {
			line := lines[i]
			if line == "" {
				continue
			}
			if linearHeadingRe.MatchString(line) && headingDepth(line) <= headingLevel {
				end = i
				break
			}
		}
		scope = lines[acceptanceHeadingIdx+1 : end]
	} else {
		scope = lines
	}

	out := []string{}
	for _, line := range scope {
		if linearChecklistRe.MatchString(line) {
			out = append(out, strings.TrimSpace(linearChecklistRe.ReplaceAllString(line, "")))
		}
	}
	return out
}

// headingDepth extracts the number of leading '#' characters.
func headingDepth(line string) int {
	m := linearHeadingLevel.FindString(line)
	if m == "" {
		return 1
	}
	return len(m)
}

// splitLines mirrors the TS description.split(/\r?\n/).
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(s, "\n")
}

// ParseLinearIssue mirrors parseLinearIssue: pure mapping from the raw
// GraphQL issue payload to a TicketSpec. Errors when the payload does not
// contain an issue with a usable title. The data argument is the decoded
// JSON of the GraphQL response.
func ParseLinearIssue(data any) (TicketSpec, error) {
	issue := lookupPath(data, "data", "issue")

	title := ""
	if issue != nil {
		title, _ = issue["title"].(string)
	}
	if issue == nil || title == "" {
		return TicketSpec{}, fmt.Errorf("Linear issue not found or malformed response")
	}

	description, _ := issue["description"].(string)

	labels := []string{}
	if labelsObj, ok := issue["labels"].(map[string]any); ok {
		if nodes, ok := labelsObj["nodes"].([]any); ok {
			for _, node := range nodes {
				if m, ok := node.(map[string]any); ok {
					if name, ok := m["name"].(string); ok {
						labels = append(labels, name)
					}
				}
			}
		}
	}

	spec := TicketSpec{
		ID:                 "",
		Title:              title,
		Description:        description,
		Labels:             labels,
		AcceptanceCriteria: ExtractLinearAcceptanceCriteria(description),
	}
	if url, ok := issue["url"].(string); ok {
		spec.URL = url
	}
	if id, ok := issue["id"].(string); ok {
		spec.TrackerInternalID = id
	}
	return spec, nil
}

// linearEnvelope is the wire shape for GraphQL requests: query + variables.
type linearEnvelope struct {
	Query     string `json:"query"`
	Variables any    `json:"variables"`
}

// linearIssueVariables shapes { id: string }.
type linearIssueVariables struct {
	ID string `json:"id"`
}

// linearCommentInput shapes { input: { issueId, body } }.
type linearCommentInput struct {
	IssueID string `json:"issueId"`
	Body    string `json:"body"`
}

// linearCommentVariables shapes { input: CommentCreateInput }.
type linearCommentVariables struct {
	Input linearCommentInput `json:"input"`
}

// LinearFetchTicket mirrors linear.fetchTicket: fetch by identifier and map
// to a TicketSpec, throwing on HTTP, GraphQL, and parse errors.
func LinearFetchTicket(id, apiKey string, opts FetchOptions) (TicketSpec, error) {
	body := marshalJSON(linearEnvelope{Query: LinearIssueQuery, Variables: linearIssueVariables{ID: id}})
	res, err := FetchLinear(body, apiKey, opts)
	if err != nil {
		return TicketSpec{}, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return TicketSpec{}, err
	}

	if !isOK(res.StatusCode) {
		return TicketSpec{}, fmt.Errorf("Linear API request failed: HTTP %d", res.StatusCode)
	}

	var envelope linearGraphQLResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return TicketSpec{}, fmt.Errorf("invalid GraphQL response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		first := "unknown error"
		if s, ok := jsScalarString(envelope.Errors[0].Message); ok {
			first = s
		}
		return TicketSpec{}, fmt.Errorf("Linear GraphQL error: %s", first)
	}

	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return TicketSpec{}, fmt.Errorf("invalid GraphQL response: %w", err)
	}
	ticket, err := ParseLinearIssue(payload)
	if err != nil {
		return TicketSpec{}, err
	}
	ticket.ID = id
	return ticket, nil
}

// LinearPostTicketComment mirrors linear.postTicketComment: post a
// progress/clarification comment to a Linear issue (FR-TICKET-03).
func LinearPostTicketComment(issueID, body, apiKey string, opts FetchOptions) error {
	payload := marshalJSON(linearEnvelope{
		Query:     linearCommentMutation,
		Variables: linearCommentVariables{Input: linearCommentInput{IssueID: issueID, Body: body}},
	})
	res, err := FetchLinear(payload, apiKey, opts)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}

	if !isOK(res.StatusCode) {
		return fmt.Errorf("Linear comment failed: HTTP %d", res.StatusCode)
	}

	var gql struct {
		Errors []struct {
			Message any `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &gql); err != nil {
		return fmt.Errorf("invalid GraphQL response: %w", err)
	}
	if len(gql.Errors) > 0 {
		first := "unknown"
		if s, ok := jsScalarString(gql.Errors[0].Message); ok {
			first = s
		}
		return fmt.Errorf("Linear comment GraphQL error: %s", first)
	}
	return nil
}
