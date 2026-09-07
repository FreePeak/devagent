// Port of src/sessionguard/spawn.ts: the child_process implementation of
// the guard's AttemptRunner. Streams claude's stdout/stderr through
// untouched while parsing stream-json events for session id, retry state,
// and terminal errors.

package sessionguard

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const watchdogPollMs = 1_000

// SpawnClaude launches argv, forwarding stdout/stderr lines through handler
// while observing stream-json events, and returns the attempt outcome.
// Stdin is inherited from the parent (TS stdio 'inherit'). An error return
// corresponds to a TS spawn rejection (e.g. binary not found).
func SpawnClaude(argv []string, handler LineHandler, opts SpawnOpts) (AttemptResult, error) {
	if len(argv) == 0 || argv[0] == "" {
		return AttemptResult{}, errors.New("empty argv")
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = opts.Env
	cmd.Stdin = os.Stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return AttemptResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return AttemptResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return AttemptResult{}, err
	}

	var (
		mu           sync.Mutex
		res          AttemptResult
		lastProgress = time.Now()
		stderrLines  []string
		timedOut     bool
	)

	touch := func() {
		mu.Lock()
		lastProgress = time.Now()
		mu.Unlock()
	}

	observe := func(event StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		switch event.Kind {
		case KindInit:
			res.SessionID = event.SessionID
		case KindResult:
			res.SawResult = true
			res.ResultIsError = event.IsError
			if event.SessionID != "" && res.SessionID == "" {
				res.SessionID = event.SessionID
			}
		case KindSyntheticError:
			res.SyntheticErrorText = event.Text
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			handler.OnLine(line, StreamStdout)
			touch()
			observe(ParseStreamLine(line))
		}
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			handler.OnLine(line, StreamStderr)
			mu.Lock()
			stderrLines = append(stderrLines, line)
			mu.Unlock()
			touch()
		}
	}()

	stop := make(chan struct{})
	if opts.NoProgressTimeoutMs > 0 {
		timeout := time.Duration(opts.NoProgressTimeoutMs) * time.Millisecond
		go func() {
			ticker := time.NewTicker(watchdogPollMs * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					mu.Lock()
					since := time.Since(lastProgress)
					mu.Unlock()
					if since >= timeout {
						mu.Lock()
						timedOut = true
						mu.Unlock()
						_ = cmd.Process.Kill() // SIGKILL, like child.kill('SIGKILL')
						return
					}
				}
			}
		}()
	}

	// Drains must finish before Wait: exec.Wait closes the pipes, so read
	// everything first (mirrors readline 'close' firing before 'close' on
	// the child in Node).
	wg.Wait()
	waitErr := cmd.Wait()
	close(stop)

	var exitCode *int
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			if code := exitErr.ExitCode(); code >= 0 {
				exitCode = &code
			}
			// Negative code = killed by a signal; stays nil (TS null).
		}
	}
	res.ExitCode = exitCode
	res.TimedOut = timedOut

	mu.Lock()
	if res.SyntheticErrorText == "" &&
		(exitCode == nil || *exitCode != 0) &&
		!res.TimedOut &&
		len(stderrLines) > 0 &&
		res.SessionID == "" {
		// Launch-level failure before any stream output; surface stderr tail.
		tail := stderrLines
		if len(tail) > 5 {
			tail = tail[len(tail)-5:]
		}
		res.SyntheticErrorText = strings.Join(tail, "\n")
	}
	mu.Unlock()

	return res, nil
}
