package loopdriver

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processAlive is declared per-platform in proc_unix.go / proc_other.go.

func pidFromFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// acquireLock ports the bash lock block: mkdir-based mutual exclusion with a
// pid file, stale-holder clearing with exactly one retry, and a no-op exit
// (ok=false) when another live driver owns the lock.
func acquireLock(cfg LoopConfig, d *driver) (func(), bool) {
	lockDir := cfg.LockDir
	if lockDir == "" {
		lockDir = d.stateDir + "/loop.lock.d"
	}
	pidPath := lockDir + "/pid"
	for attempt := 0; ; attempt++ {
		if err := os.Mkdir(lockDir, 0o755); err == nil {
			_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
			return func() { _ = os.RemoveAll(lockDir) }, true
		}
		holder := pidFromFile(pidPath)
		live := false
		if holder != "" && holder != "?" {
			if pid, cerr := strconv.Atoi(holder); cerr == nil && processAlive(pid) {
				live = true
			}
		}
		if live {
			fmt.Fprintf(cfg.Stdout, "[lock] another selfbuild-loop driver already holds %s (pid %s) — exiting\n", lockDir, holder)
			return nil, false
		}
		if attempt == 0 {
			if holder != "" && holder != "?" {
				fmt.Fprintf(cfg.Stdout, "[lock] stale holder pid %s is gone — clearing lock and retrying\n", holder)
			}
			_ = os.RemoveAll(lockDir)
			continue
		}
		fmt.Fprintf(cfg.Stdout, "[lock] another driver still holds %s — exiting\n", lockDir)
		return nil, false
	}
}
