// Test seams shared by the integrations contract tests: recorded-response
// transports (zero network) and fixture helpers.
package integrations

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordedResponse builds an *http.Response from a recorded status,
// headers, and body.
func recordedResponse(status int, headers map[string]string, body string) *http.Response {
	res := &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	for k, v := range headers {
		res.Header.Set(k, v)
	}
	return res
}

// scriptedResponse is the recorded-HTTP shape stored in testdata: the
// status, the response headers, and the raw body a provider actually sent.
type scriptedResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

// loadRecorded replays a recorded provider response from testdata.
func loadRecorded(t *testing.T, name string) *http.Response {
	t.Helper()
	var script scriptedResponse
	if err := json.Unmarshal(readFixture(t, name), &script); err != nil {
		t.Fatalf("parse recorded response %s: %v", name, err)
	}
	return recordedResponse(script.Status, script.Headers, string(script.Body))
}

// recordedTransport is a Doer replaying scripted responses in order; an
// exhausted script fails the test (mirrors the TS fakeClient's
// "unexpected extra HTTP call").
type recordedTransport struct {
	t         *testing.T
	responses []*http.Response
	calls     []*http.Request
	bodies    [][]byte
}

func newRecordedTransport(t *testing.T, responses ...*http.Response) *recordedTransport {
	return &recordedTransport{t: t, responses: responses}
}

func (tr *recordedTransport) Do(req *http.Request) (*http.Response, error) {
	tr.calls = append(tr.calls, req)
	var body []byte
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = raw
	}
	tr.bodies = append(tr.bodies, body)
	if len(tr.responses) == 0 {
		tr.t.Fatalf("unexpected extra HTTP call: %s %s", req.Method, req.URL)
	}
	next := tr.responses[0]
	tr.responses = tr.responses[1:]
	return next, nil
}

// readFixture loads a testdata file.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

// fixtureString loads a testdata file as a string, without the trailing
// newline heredoc-written fixture files carry (payload bytes themselves
// are compared byte-for-byte).
func fixtureString(t *testing.T, name string) string {
	t.Helper()
	return strings.TrimRight(string(readFixture(t, name)), "\n")
}

// jsonUnmarshalFixture decodes a testdata JSON file into out.
func jsonUnmarshalFixture(t *testing.T, name string, out any) error {
	t.Helper()
	return json.Unmarshal(readFixture(t, name), out)
}

// errTransport always fails at the transport level, distinct from a
// recorded HTTP status.
type errTransport struct{ err error }

func (e errTransport) Do(*http.Request) (*http.Response, error) { return nil, e.err }

// drain closes a response body, ignoring the read error a replayed
// fixture can produce after the caller consumed it.
func drain(res *http.Response) {
	if res == nil || res.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
}
