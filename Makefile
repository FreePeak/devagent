# DevAgent local automation control (macOS LaunchAgents).
#
#   make agents-status     # what is loaded / disabled / running
#   make agents-off        # stop + disable all auto-trigger agents (survives reboot)
#   make agents-on         # re-enable + load them again
#   make loop-status       # is the loop running? (+ supervision mode) tail of its latest iteration log
#   make agents-uninstall  # bootout + disable + delete installed plists (repo copies kept)
#   make loop-start        # start the Go selfbuild loop (./devagent-go loop) in the background
#   make loop-stop         # stop the background selfbuild loop
#   make loop-status       # is the loop running? tail of its latest iteration log
#   make loop-log          # tail -f the running loop's driver output
#   make daemon-start      # start the FR-CTRL daemon (./devagent-go daemon) in the background
#   make daemon-stop       # stop the background daemon
#   make kill              # kill running devagent loops/workers (no launchctl changes)
# NOTE: launchagents/com.devagent.selfbuild-loop.plist.template is OPT-IN
# (issue #321): agents-install does NOT install it — the loop deliberately
# runs unsupervised (nohup); copy+render it manually if you want launchd
# supervision, keeping the KeepAlive {SuccessfulExit=false} policy.
#   make orca-quit         # quit Orca app + background daemon
#
# Plist sources live in ./launchagents/ with ${HOME} placeholders; they are
# rendered at install time. Uninstalling removes them from macOS but the repo
# copies stay, so agents-install restores everything later.

GUI := gui/$(shell id -u)

DEVAGENT_LABELS := \
	com.devagent.scout \
	com.devagent.builder \
	com.devagent.orchestrator \
	com.devagent.tracker \
	com.devagent.git-cleanup-merged \
	com.devagent.orca-selfbuild-cleanup

# Resurrectors: these respawn killed devagent/opencode runs if left loaded.
WATCHDOG_LABELS := \
	com.opencode.goal-watchdog \
	com.opencode.auto-retry

ALL_LABELS := $(DEVAGENT_LABELS) $(WATCHDOG_LABELS)

.PHONY: loop-start loop-stop loop-status loop-log daemon-start daemon-stop

# --- Selfbuild loop (background) --------------------------------------------
# The loop runs via hub-managed nohup; workers default to visible herdr panes
# (attach with `devagent attach <task>`), headless via DEVAGENT_VISIBILITY=headless.
LOOP_LOG_DIR := .selfbuild/logs

loop-start:
	@if pgrep -f "devagent-go loop" >/dev/null 2>&1; then \
		echo "selfbuild loop already running (pid $$(pgrep -f 'devagent-go loop' | head -1))"; exit 0; \
	fi
	@if [ ! -x ./devagent-go ]; then echo "no ./devagent-go — run: make build" >&2; exit 1; fi
	@mkdir -p "$(LOOP_LOG_DIR)"
	@nohup ./devagent-go loop >> "$(LOOP_LOG_DIR)/driver.log" 2>&1 & \
	echo "selfbuild loop started (pid $$!) — log: $(LOOP_LOG_DIR)/driver.log; TUI: devagent tui"

loop-stop:
	@if pgrep -f "devagent-go loop" >/dev/null 2>&1; then \
		pkill -f "devagent-go loop" && echo "selfbuild loop stopped"; \
	else echo "selfbuild loop not running"; fi

loop-status:
	@if pgrep -f "devagent-go loop" >/dev/null 2>&1; then \
		echo "RUNNING (pid $$(pgrep -f 'devagent-go loop' | head -1))"; \
	else echo "STOPPED — start with: make loop-start"; fi
	@./devagent-go supervision 2>/dev/null || echo "supervision: unknown (./devagent-go missing — run: make build)"
	@test -f "$(LOOP_LOG_DIR)/driver.log" && tail -3 "$(LOOP_LOG_DIR)/driver.log" || true

loop-log:
	@tail -f "$(LOOP_LOG_DIR)/driver.log"

# --- FR-CTRL daemon (background; the TUI's data source) ---------------------
DAEMON_LOG_DIR := .selfbuild/logs

daemon-start:
	@if pgrep -f "devagent-go daemon" >/dev/null 2>&1; then \
		echo "daemon already running (pid $$(pgrep -f 'devagent-go daemon' | head -1))"; exit 0; \
	fi
	@if [ ! -x ./devagent-go ]; then echo "no ./devagent-go — run: make build" >&2; exit 1; fi
	@mkdir -p "$(DAEMON_LOG_DIR)"
	@nohup ./devagent-go daemon --repo "$(CURDIR)" >> "$(DAEMON_LOG_DIR)/daemon.log" 2>&1 & \
	echo "daemon started (pid $$!) — log: $(DAEMON_LOG_DIR)/daemon.log; TUI: devagent tui"

daemon-stop:
	@if pgrep -f "devagent-go daemon" >/dev/null 2>&1; then \
		pkill -f "devagent-go daemon" && echo "daemon stopped"; \
	else echo "daemon not running"; fi

PLIST_DIR := launchagents

# Render repo plist templates into ~/Library/LaunchAgents, then load them.
agents-install:
	@mkdir -p "$(HOME)/Library/LaunchAgents"
	@if [ -z "$(wildcard $(PLIST_DIR)/*.plist)" ]; then echo "no plists in $(PLIST_DIR)/"; exit 1; fi
	@for f in $(PLIST_DIR)/*.plist; do \
		sed "s|\$${HOME}|$(HOME)|g" "$$f" > "$(HOME)/Library/LaunchAgents/$$(basename $$f)" || exit 1; \
		echo "installed:  $$(basename $$f)"; \
	done
	@plutil -lint "$(HOME)/Library/LaunchAgents/"com.devagent.*.plist >/dev/null || { echo "rendered plist failed lint"; exit 1; }
	@$(MAKE) --no-print-directory agents-on

# Stop everything and delete the installed plists from macOS.
# Repo copies in $(PLIST_DIR)/ are untouched, so agents-install restores later.
agents-uninstall:
	@for label in $(ALL_LABELS); do \
		launchctl bootout "$(GUI)/$$label" >/dev/null 2>&1 && echo "booted out: $$label" || echo "not loaded:  $$label"; \
		launchctl disable "$(GUI)/$$label" >/dev/null 2>&1 || true; \
	done
	@for label in $(ALL_LABELS); do \
		if [ -f "$(HOME)/Library/LaunchAgents/$$label.plist" ]; then \
			rm "$(HOME)/Library/LaunchAgents/$$label.plist" && echo "removed:     $$label.plist"; \
		else echo "no plist:    $$label.plist"; fi; \
	done
	@$(MAKE) --no-print-directory kill

agents-off:
	@for label in $(ALL_LABELS); do \
		launchctl bootout "$(GUI)/$$label" >/dev/null 2>&1 && echo "booted out: $$label" || echo "not loaded:  $$label"; \
		launchctl disable "$(GUI)/$$label" >/dev/null 2>&1 && echo "disabled:    $$label"; \
	done
	@$(MAKE) --no-print-directory kill

agents-on:
	@for label in $(ALL_LABELS); do \
		launchctl enable "$(GUI)/$$label" 2>/dev/null && echo "enabled:     $$label"; \
		plist="$${HOME}/Library/LaunchAgents/$$label.plist"; \
		if [ -f "$$plist" ]; then \
			launchctl bootstrap "$(GUI)" "$$plist" >/dev/null 2>&1 && echo "loaded:      $$label" || echo "load failed/already loaded: $$label"; \
		else \
			echo "no plist (skip load): $$label"; \
		fi; \
	done

LABEL_RE := (com\.devagent\.|goal-watchdog|auto-retry)

agents-status:
	@echo "== launchctl loaded =="
	@launchctl list | grep -E "$(LABEL_RE)" || echo "(none loaded)"
	@echo; echo "== disabled registry =="
	@launchctl print-disabled $(GUI) | grep -E "$(LABEL_RE)" || echo "(none disabled)"
	@echo; echo "== running devagent/orca processes =="
	@(ps aux | grep -Ei "devagent|build-loop|orchestrate-loop|cli\.js (scout|track|orchestrate)|Orca\.app|orca/daemon|goal-watchdog|auto-retry-daemon" | grep -v grep | grep -v "agents-status") || echo "(none)"

# Kill running loops/workers + detached watchdog daemons; never touches launchctl state.
kill:
	@pkill -f "build-loop.sh" 2>/dev/null && echo "killed build-loop.sh" || true
	@pkill -f "orchestrate-loop.sh" 2>/dev/null && echo "killed orchestrate-loop.sh" || true
	@pkill -f "devagent-go loop" 2>/dev/null && echo "killed devagent-go loop" || true
	@pkill -f "devagent-go task" 2>/dev/null && echo "killed devagent-go task workers" || true
	@pkill -9 -f "opencode/scripts/goal-watchdog.sh" 2>/dev/null && echo "killed goal-watchdog daemon" || true
	@pkill -9 -f "opencode/scripts/auto-retry-daemon.sh" 2>/dev/null && echo "killed auto-retry daemon" || true
	@pkill -9 -f "ORPHANED long-running goal" 2>/dev/null && echo "killed orphaned-goal resume run" || true
	@echo "done (interactive 'opencode run' sessions are left alone)"

orca-quit:
	@osascript -e 'tell application "Orca" to quit' 2>/dev/null || true
	@sleep 3
	@pkill -f "Orca.app" 2>/dev/null && echo "sent TERM to Orca helpers" || true
	@sleep 2
	@pkill -9 -f "Orca.app" 2>/dev/null || true
	@pkill -9 -f "orca/daemon" 2>/dev/null && echo "killed Orca daemon" || true
	@echo "Orca stopped"

# --- Go binary ---------------------------------------------------------------
#
#   make build    # ./devagent-go — the production CLI
#
# The unstamped default in internal/version is 0.1.0; release tooling stamps
# the version at publish time, so local builds stay unstamped.

.PHONY: build
build:
	go build -trimpath -o devagent-go ./cmd/devagent
