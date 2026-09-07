package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// newestRunLog mirrors newestRunLog: the newest run log under <home>/runs
// ("" when none exist yet).
func newestRunLog(home string) string {
	dir := filepath.Join(home, "runs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "" // no runs dir
	}
	newest := ""
	var newestM int64 = -1
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "" // TS statSync throws abort the whole lookup
		}
		if m := info.ModTime().UnixMilli(); m > newestM {
			newestM = m
			newest = e.Name()
		}
	}
	if newest == "" {
		return ""
	}
	return filepath.Join(dir, newest)
}

// eventSink is the follower-facing sink; send writes one `id:`/`data:` SSE
// event (errors swallowed — a vanished client is cleaned up on close).
type eventSink interface {
	send(id int, data string)
}

// followerSource is one (path, linesRead) pair: the run log plus the repo
// orchestration stream.
type followerSource struct {
	path   string
	loaded int
}

// runLogFollower mirrors RunLogFollower: replays the tail of the newest
// DEVAGENT_HOME/runs/*.jsonl as SSE events (ids = line index within the
// file, Last-Event-ID resumable) and fans live lines out to every sink.
type runLogFollower struct {
	mu        sync.Mutex
	sources   []followerSource
	buffer    []string
	loaded    int
	sinks     map[eventSink]struct{}
	started   bool
	stopCh    chan struct{}
	stopOnce  sync.Once
	stoppedCh chan struct{}
}

// newRunLogFollower mirrors the constructor: seed from the newest run log
// then the extra sources, skipping duplicates (a repo may live under
// DEVAGENT_HOME).
func newRunLogFollower(home string, extraSources []string) *runLogFollower {
	f := &runLogFollower{
		sinks:     map[eventSink]struct{}{},
		stopCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
	runLog := newestRunLog(home)
	if runLog != "" {
		f.sources = append(f.sources, followerSource{path: runLog})
	}
	for _, p := range extraSources {
		if p != runLog {
			f.sources = append(f.sources, followerSource{path: p})
		}
	}
	return f
}

// loadLocked mirrors load(): read new lines from every source and fan them
// out (ids shift-safe). The whole pass sits in one error-tolerant block — a
// file may rotate under us; retry on the next tick.
func (f *runLogFollower) loadLocked() {
	for i := range f.sources {
		s := &f.sources[i]
		raw, err := os.ReadFile(s.path)
		if err != nil {
			continue // TS existsSync guard; missing/unreadable file skipped
		}
		all := nonBlankLines(string(raw))
		for s.loaded < len(all) {
			data := all[s.loaded]
			id := f.loaded
			f.loaded++
			s.loaded++
			f.buffer = append(f.buffer, data)
			if len(f.buffer) > eventsReplayLines {
				f.buffer = f.buffer[1:]
			}
			for sink := range f.sinks {
				sink.send(id, data)
			}
		}
	}
}

// nonBlankLines mirrors `.split("\n").filter((l) => l.trim())`.
func nonBlankLines(s string) []string {
	parts := strings.Split(s, "\n")
	out := make([]string, 0, len(parts))
	for _, l := range parts {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// replayTo mirrors replayTo: replay the tail to one client, skipping
// everything <= lastEventId.
func (f *runLogFollower) replayTo(sink eventSink, lastEventID *int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loaded == 0 {
		f.loadLocked()
	}
	base := f.loaded - len(f.buffer)
	for i, data := range f.buffer {
		id := base + i
		if lastEventID != nil && id <= *lastEventID {
			continue
		}
		sink.send(id, data)
	}
}

// add mirrors add().
func (f *runLogFollower) add(sink eventSink) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sinks[sink] = struct{}{}
}

// remove mirrors remove().
func (f *runLogFollower) remove(sink eventSink) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sinks, sink)
}

// Start mirrors start(): begin the poll loop (idempotent, no-op with no
// sources — setInterval on an empty source list never fires meaningfully).
// The Go poller is a goroutine rather than an unref'd timer.
func (f *runLogFollower) Start() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started || len(f.sources) == 0 {
		return
	}
	f.started = true
	ticker := time.NewTicker(eventsPollInterval)
	go func() {
		defer close(f.stoppedCh)
		defer ticker.Stop()
		for {
			select {
			case <-f.stopCh:
				return
			case <-ticker.C:
				f.mu.Lock()
				f.loadLocked()
				f.mu.Unlock()
			}
		}
	}()
}

// Stop mirrors stop(): halt the poll loop.
func (f *runLogFollower) Stop() {
	f.stopOnce.Do(func() {
		close(f.stopCh)
		<-f.stoppedCh
	})
}
