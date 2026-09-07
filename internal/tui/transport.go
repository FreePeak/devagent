package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Transport layer for the TUI (FR-TUI over FR-CTRL): bearer-token HTTP and
// Unix-domain-socket calls to the daemon, plus the SSE /events subscription.
// Port of src/tui/transport.ts.

// HTTPResponse is one daemon response; Status 0 means unreachable.
type HTTPResponse struct {
	Status int
	Body   string
}

// tokenFilePath is the path of the daemon's 0600 daemon-token file. The home
// directory comes from the environment, so traversal-shaped values (any `..`
// component) are refused outright — the file must live inside the devagent
// home, nowhere else.
func tokenFilePath() string {
	home := os.Getenv("DEVAGENT_HOME")
	if home == "" {
		h := os.Getenv("HOME")
		if h == "" {
			if hd, err := os.UserHomeDir(); err == nil {
				h = hd
			}
		}
		home = filepath.Join(h, ".devagent")
	}
	for _, part := range strings.FieldsFunc(home, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return ""
		}
	}
	return filepath.Join(home, "daemon-token")
}

// ResolveToken resolves the bearer token for daemon calls:
// opts > env > the 0600 daemon-token file.
func ResolveToken(opts TuiOptions) string {
	if opts.Token != "" {
		return opts.Token
	}
	if t := os.Getenv("DEVAGENT_DAEMON_TOKEN"); t != "" {
		return t
	}
	file := tokenFilePath()
	if file == "" {
		return ""
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// daemonBaseURL resolves the daemon base URL: opts.URL > env > default.
func daemonBaseURL(opts TuiOptions) string {
	if opts.URL != "" {
		return strings.TrimRight(opts.URL, "/")
	}
	if env := os.Getenv("DEVAGENT_DAEMON_URL"); env != "" {
		return strings.TrimRight(env, "/")
	}
	return DefaultDaemonURL
}

// DaemonRequest issues one request against the daemon. Never fails: Status 0
// with an empty body means unreachable.
func DaemonRequest(opts TuiOptions, method, path, body string, timeout time.Duration) HTTPResponse {
	var req *http.Request
	var client *http.Client
	if opts.UDSPath != "" {
		var err error
		req, err = http.NewRequest(method, "http://127.0.0.1"+path, bodyReader(body))
		if err != nil {
			return HTTPResponse{}
		}
		req.Host = "127.0.0.1"
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", opts.UDSPath)
			},
		}
		client = &http.Client{Transport: tr, Timeout: timeout}
	} else {
		u, err := url.Parse(daemonBaseURL(opts) + path)
		if err != nil {
			return HTTPResponse{}
		}
		req, err = http.NewRequest(method, u.String(), bodyReader(body))
		if err != nil {
			return HTTPResponse{}
		}
		client = &http.Client{Timeout: timeout}
	}
	req.Header.Set("Accept", "application/json")
	if token := ResolveToken(opts); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return HTTPResponse{}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil && len(data) == 0 {
		return HTTPResponse{Status: resp.StatusCode}
	}
	return HTTPResponse{Status: resp.StatusCode, Body: string(data)}
}

func bodyReader(body string) io.Reader {
	if body == "" {
		return nil
	}
	return strings.NewReader(body)
}

// GetJSON issues one GET and decodes the 200 body as JSON (nil otherwise).
func GetJSON(opts TuiOptions, path string) (status int, value any) {
	r := DaemonRequest(opts, http.MethodGet, path, "", 4*time.Second)
	if r.Status == 200 && r.Body != "" {
		var v any
		if err := json.Unmarshal([]byte(r.Body), &v); err == nil {
			return r.Status, v
		}
	}
	return r.Status, nil
}

// PostResult carries the PostJSON outcome.
type PostResult struct {
	Status int
	OK     bool
	Note   string
}

// PostJSON issues one POST; never rejects. The note is the daemon's note or
// error field, falling back to the HTTP status text.
func PostJSON(opts TuiOptions, path string, body any) PostResult {
	payload := "{}"
	if body != nil {
		if b, err := json.Marshal(body); err == nil {
			payload = string(b)
		}
	}
	r := DaemonRequest(opts, http.MethodPost, path, payload, 4*time.Second)
	if r.Status == 0 {
		return PostResult{Status: 0, OK: false, Note: "daemon unreachable"}
	}
	var parsed struct {
		Note  *string `json:"note"`
		OK    *bool   `json:"ok"`
		Error *string `json:"error"`
	}
	_ = json.Unmarshal([]byte(r.Body), &parsed) // non-JSON error body tolerated
	note := ""
	switch {
	case parsed.Note != nil:
		note = *parsed.Note
	case parsed.Error != nil:
		note = *parsed.Error
	case r.Status == 401:
		note = "unauthorized (bad token?)"
	default:
		note = "HTTP " + strconv.Itoa(r.Status)
	}
	ok := r.Status >= 200 && r.Status < 300 && (parsed.OK == nil || *parsed.OK)
	return PostResult{Status: r.Status, OK: ok, Note: note}
}

// EventsState mirrors the SSE connection lifecycle.
type EventsState string

const (
	StateConnecting EventsState = "connecting"
	StateLive       EventsState = "live"
	StateDown       EventsState = "down"
)

const (
	sseReconnectMs = 3000
	sseAuthRetryMs = 15000
)

// EventsSubscription handles a live /events subscription; Stop tears it down
// and stops reconnecting.
type EventsSubscription struct {
	mu      sync.Mutex
	stopped bool
	lastID  int
	cancel  context.CancelFunc
}

// LastEventID reports the highest event id seen (or the initial value; -1
// by default) — feed it back to resume without gaps.
func (s *EventsSubscription) LastEventID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastID
}

// Stop tears the subscription down without throwing.
func (s *EventsSubscription) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *EventsSubscription) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *EventsSubscription) setLastID(n int) {
	s.mu.Lock()
	s.lastID = n
	s.mu.Unlock()
}

// SubscribeEvents subscribes to the daemon's SSE /events stream (FR-CTRL
// run-log tail). Events are dispatched as parsed `data` payloads;
// `Last-Event-ID` is sent on (re)connect so the daemon's replay skips what we
// already have. The caller owns the lifetime (Stop); the subscription
// reconnects with backoff until stopped, and state callbacks let the UI show
// LIVE/RECONNECTING.
func SubscribeEvents(opts TuiOptions, onEvent func(id int, data string), onState func(EventsState), lastEventID ...int) *EventsSubscription {
	last := -1
	if len(lastEventID) > 0 {
		last = lastEventID[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	sub := &EventsSubscription{lastID: last, cancel: cancel}
	go func() {
		for !sub.isStopped() {
			if onState != nil {
				onState(StateConnecting)
			}
			waitMs := sseReconnectMs
			req, err := sseRequest(ctx, opts, sub.LastEventID())
			if err != nil {
				sub.backoff(ctx, waitMs, onState)
				continue
			}
			client := sseClient(opts)
			resp, err := client.Do(req)
			if err != nil {
				sub.backoff(ctx, waitMs, onState)
				continue
			}
			if resp.StatusCode != http.StatusOK {
				// 401 will not heal by hammering: retry slowly.
				if resp.StatusCode == 401 {
					waitMs = sseAuthRetryMs
				}
				_ = resp.Body.Close()
				sub.backoff(ctx, waitMs, onState)
				continue
			}
			if onState != nil {
				onState(StateLive)
			}
			sub.readStream(resp.Body, func(id int, data string) {
				if onEvent != nil {
					onEvent(id, data)
				}
			})
			_ = resp.Body.Close()
			sub.backoff(ctx, waitMs, onState)
		}
	}()
	return sub
}

func sseRequest(ctx context.Context, opts TuiOptions, lastID int) (*http.Request, error) {
	var req *http.Request
	var err error
	if opts.UDSPath != "" {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1/events", nil)
		if err == nil {
			req.Host = "127.0.0.1"
		}
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, daemonBaseURL(opts)+"/events", nil)
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if token := ResolveToken(opts); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if lastID >= 0 {
		req.Header.Set("Last-Event-ID", strconv.Itoa(lastID))
	}
	return req, nil
}

// sseClient builds the HTTP client for the long-lived stream: a unix
// dialer over opts.UDSPath, plain transport otherwise. No timeout — SSE
// connections are long-lived by design.
func sseClient(opts TuiOptions) *http.Client {
	if opts.UDSPath == "" {
		return &http.Client{}
	}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", opts.UDSPath)
		},
	}}
}

func (s *EventsSubscription) backoff(ctx context.Context, ms int, onState func(EventsState)) {
	if s.isStopped() {
		return
	}
	if onState != nil {
		onState(StateDown)
	}
	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// readStream parses SSE frames from a 200 stream: frames end at a blank
// line; `:` comments are heartbeats; `data:` lines accumulate (multi-line
// data joined with \n); `id:` updates the resume cursor. One goroutine per
// subscription owns the cursor.
func (s *EventsSubscription) readStream(body io.Reader, onEvent func(id int, data string)) {
	reader := bufio.NewReader(body)
	var buf []byte
	chunk := make([]byte, 8192)
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				idx := indexDoubleNewline(buf)
				if idx < 0 {
					break
				}
				frame := string(buf[:idx])
				buf = buf[idx+2:]
				data := ""
				for _, lineRaw := range strings.Split(frame, "\n") {
					line := strings.TrimSuffix(lineRaw, "\r")
					if strings.HasPrefix(line, ":") {
						continue // comment / heartbeat
					}
					if strings.HasPrefix(line, "data:") {
						if data != "" {
							data += "\n"
						}
						data += strings.TrimLeftFunc(line[5:], unicode.IsSpace)
					} else if strings.HasPrefix(line, "id:") {
						if v, ok := jsNumber(strings.TrimSpace(line[3:])); ok {
							s.setLastID(int(v))
						}
					}
				}
				if data != "" {
					onEvent(s.LastEventID(), data)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// indexDoubleNewline finds "\n\n" like the TS buf.indexOf.
func indexDoubleNewline(buf []byte) int {
	for i := 0; i+1 < len(buf); i++ {
		if buf[i] == '\n' && buf[i+1] == '\n' {
			return i
		}
	}
	return -1
}

// jsNumber mirrors JS Number(): "" → 0 (finite), else the float, else !ok.
func jsNumber(s string) (float64, bool) {
	if s == "" {
		return 0, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
