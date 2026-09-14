# Making devagent easy to set up, easy to use, and easy to run unattended (local selfbuild loop)

*Research pass 2026-09-14. Method: three read-only code/doc scouts over
`internal/**`, `docs/**`, `Makefile`, `scripts/`, `launchagents/`; a quantified
read of the production ledger (321 rows, 2026-08-23 → 2026-09-13) and 250
iteration logs; and live cold-start experiments run against throwaway clones in
`/tmp` with a **local** git mirror as `origin` (no dispatch, no writes to the
real repo or GitHub). Every claim below carries either a `path:line` in the
current tree or the command that produced it.*

## 1. What "easy" has to mean here

devagent is a local-first factory: one Go binary that drives worker CLIs
(`omp`/`claude`/`opencode`/`grok`/`codex`) through a deterministic loop —
state sync → provider gate → doc gates → pick work → research → PO → implement
→ PR → auto-merge — and writes an append-only ledger of every iteration. "Easy"
is therefore not a nicer README; it is four properties:

1. **Cold start is one command** and ends in a *live* driver, not a process
   receipt.
2. **Zero env exports.** Every knob the operator needs is in `devagent.json`.
3. **Every stop is explained.** If the factory is not shipping, one command
   says why and what to do next.
4. **The factory is quarantined.** Starting it in one checkout cannot change
   another checkout's state (issue #354).

Today 1–4 all fail, and the failure is cheap to demonstrate.

## 2. Measured cold start (the experiment that should own the roadmap)

Throwaway clone of `origin/main` (`68c36cf`) → local mirror as `origin` →
`make build` (1 s) → the documented sequence (`docs/SELF-BUILD-LOOP.md` §"Run
the loop"):

| Step | Result |
|---|---|
| `make build` | ✓ 1 s, warm Go cache; 16 MB binary |
| `./devagent-go doctor` | **✗ exit 1** — `herdr` (no `devagent` session) and `daemon` (`:7788` refused) fail; both are "not set up yet", not "broken" |
| `./devagent-go loop --dry-run --max-iterations 2` | `max iterations reached`, **exit 0**, 0.5 s, zero work |
| `./devagent-go loop` (cap raised) | `[state] pulled 321 ledger entries` → `[starvation] 5 consecutive non-productive iterations — halting loop`, **exit 0**, <1 s |
| delete `.selfbuild/ledger.jsonl`, run again | `[state] pulled 321 ledger entries` → same starvation halt. The tail is **re-fetched from the shared `selfbuild/state` branch**, so the local delete cannot help |
| `SELFBUILD_HOME=/tmp/isolated ./devagent-go loop` | state still pulled into `/tmp/dvx/.selfbuild` — the loopdriver derives its state dir from `cfg.Repo` (`run.go:90`, `state.go:51`), so `SELFBUILD_HOME` **does not isolate a second driver** (it isolates the daemon and the runner only) |
| `./devagent-go up --no-daemon` (the in-flight #371 command, run twice: 07:36:30Z and 07:58:14Z) | **`ok=true`** — `checks` 6 passed, `dirs` ready, `intake: 0 open PRD item(s) … queue depth 0`, `loop: driver started (pid 11415 / 74548)`. Both drivers were dead at the next probe (t+5 s, t+10 s); the iteration log says `[starvation] 5 consecutive non-productive iterations — halting loop`. Because the driver dies and releases the lock, the *next* `up` starts a *new* one and reports green again — the idempotence check is correct and useless at the same time |
| second `up` | starts another pid, reports success again (the lock is free, because the last driver is dead) |

Reproduce any of these; nothing above costs more than a minute.

### 2.1 Why the halt is unavoidable

The starvation verdict is a tail-walk over the ledger, with degraded rows
exempt and only `ok|pr-open|merged|pushed` breaking the streak
(`internal/orchestrator/selfbuild-gate.go:14,32,34,100-126`), evaluated
*before* any work (`internal/loopdriver/run.go:156-160`), and the ledger is a
cross-machine union pulled at driver start (`run.go:113` →
`state.go:213-222`, `mergeLedgerLines` `state.go:131-165`).

The shared tail right now is **15 consecutive non-productive rows** (all
`invalid`, from 2026-09-12) measured by replaying that exact walk over
`.selfbuild/ledger.jsonl`. Limit is 5 (`config.go:130`). So the state branch
itself is the thing that refuses to start — not this machine, not the config.
There is no acknowledge/resume seam anywhere: no `resume` event in the ledger
schema (`internal/ledger/ledger.go`), no flag, no reset command. Exit is 0, so
`hub restart=on-failure` and the opt-in launchd template
(`KeepAlive {SuccessfulExit=false}`) deliberately do **not** bring it back.

### 2.2 The `--max-iterations` trap in the same table

`MaxIterations` is compared against the **absolute loop number**, not a count:
`n := nextLoopNumber(...)`, `if cfg.MaxIterations > 0 && n >= cfg.MaxIterations`
(`run.go:134-142`). On a repo whose next number is 362, `--max-iterations 2`
(and `20`, and `100`) means "run zero iterations", silently. The flag help says
"cap the iteration count (0 = unbounded)" (`internal/cli/actions_loop.go:37`).

## 3. Why the operator can't see any of this

**The stop has no face.** The halt line goes to
`.selfbuild/logs/loop-N.log` (`run.go:157`), which is *gitignored* and read by
nobody. `driver.log` — the thing `make loop-log` tails — never sees it (it
receives only the state-sync banners and the `tail -5` of fall-through
iterations, `run.go:175`). The TUI/dashboard read
`.selfbuild/heartbeat.json` (`internal/daemon/endpoints.go:51`), which the
driver last wrote *while working* and never updates on a terminal halt: the
production heartbeat right now still says
`{"iteration":361,"phase":"task","pid":51174}` with a dead pid, 18 h stale.
So the observed behavior of an unattended factory is "stopped, looks busy,
says nothing".

**The hygiene report is a false all-clear.** `devagent herdr-sweep --dry-run`
printed `[devagent] no stale panes` and exited **0** while the `devagent` herdr
session had no server at all (`server_not_running`). On a CLI error the pane
list simply comes back empty, so the sweep reports the same "nothing stale"
line and exits 0 (`internal/herdr/commands.go:39-42`,
`internal/herdr/sweep.go:155-183`), and `loopdriver/herdrSweep` echoes exactly
the last three lines regardless of rc (`internal/loopdriver/dispatch.go:214-219`).
That string appears **359 times across the 242 retained production iteration
logs**, and because the failure path is indistinguishable from the clean path,
none of those 359 lines proves anything: the sweep ran, or the sweep could not
run.

**Status mixes projects.** `devagent status` on a clean clone printed the
correct "● not started" card and then ~25 event rows belonging to *other*
repos, because `runStatusHuman` tails `$HOME/.devagent/runs/*.jsonl`
(`internal/cli/actions_status.go:125-133`), a process-global directory with
2 712 files and no repo column. Same global-scope class for
`~/.devagent/locks/*` (`doctor.go:535-601` reported other tasks' stale locks
on a pristine checkout).

## 4. Two config systems, one binary

`devagent.json` is the product's config surface (`internal/config`, written by
`init`/`create`); the selfbuild driver is **env-only**
(`loopdriver.ConfigFromEnv`, `internal/loopdriver/config.go:274-331` — no
config-file read anywhere in the package). The consequences are concrete:

| Setting | File path | Env path | Effect today |
|---|---|---|---|
| worker | `devagent.json:worker` | `SELFBUILD_WORKER` (default `omp`) | the TUI and the loop can run different workers |
| model | `devagent.json:model` | `SELFBUILD_MODEL` (default `""`) | **the gates disagree**: `devagent preflight` resolves the model from `config.Load(repo)` (`internal/cli/actions_gates.go:180-190`), while dispatch pins `--model` only when `SELFBUILD_MODEL` is set (`internal/loopdriver/dispatch.go:475-476`). The probe green-lights one model, the workers run another |
| herdr session | `devagent.json:herdr.session` | `DEVAGENT_HERDR_SESSION` (`internal/herdr/herdr.go:99-109`), and `ResolveSession("")` also exists (`internal/config/config.go:594-602`) | two resolvers with the same precedence, no shared entry point |
| daemon | no config key at all — the TUI default is the hardcoded `http://127.0.0.1:7788` (`internal/tui/tui.go:45-46`) | `--port` flag (`internal/daemon/daemon.go:61-63`, nil → 7788), `DEVAGENT_DAEMON_TOKEN` (`:457`), `DEVAGENT_HOME` (`:481`) | port is a flag, token is an env, URL is a compiled-in default; the driver reads none of them |

The 27 distinct `SELFBUILD_*` knobs the driver reads (`internal/loopdriver/config.go`,
measured by enumeration) are fully documented (`docs/SELF-BUILD-LOOP.md`
§"Configuration", :144-163) and still require a shell. And the surface leaks:
`os.Environ()` is forwarded whole to every dispatch
(`internal/loopdriver/dispatch.go:34-38`, which only adds
`DEVAGENT_VISIBILITY`), so a stale `GITHUB_TOKEN` in the driver's shell lands
inside every worker pane — the `env -u GITHUB_TOKEN` workaround every operator
has memorised for `gh` is a symptom of this.

The single highest-leverage cleanup is the identity one: the driver's default
`DevagentBin` is the bare name `devagent` (`config.go:234-235`, pinned by
`config_test.go:9,25`), so `preflight`, `pane-run`, `task`, `sync-docs`,
`herdr-sweep`, `ledger --clusters`, `scan-text` and `page-degrade-breach` are
all shells out to whatever `devagent` is on `PATH`. In my `/tmp` experiment the
fresh clone's driver called **the installed binary of a different checkout**
(`command -v devagent` → `~/.local/bin/devagent` → the main worktree). The
in-flight `up` fixes this for the path it starts
(`SELFBUILD_DEVAGENT_BIN=<os.Executable()>`, `internal/commands/updown.go:~281`)
but `make loop-start`, a bare `devagent loop`, and a hand-made `hub` job all
keep the trap.

Two residue items in the same category. (a) `devagent create` *is* fixing up
right now: the retired Node argv (`devagentBin :=
filepath.Join(opts.RepoPath,"dist","src","cli.js")`, `internal/pipeline/create.go:164`
at HEAD, used at :171/:178, plus `scripts/build-loop.sh` at :183/:195) is
replaced by the self-executable in today's working tree — but the builder still
forces `KeepAlive=true` (`create.go:148`) for every agent it writes, which is
the restart policy the repo retired in #321 for a halting driver. `create
--dry-run` itself is honest (it needs `--repo`; measured plan-only output).
(b) `devagent loop` does not accept `--repo` at all (`Error: unknown flag:
--repo`, measured) while the handler uses `os.Getwd()`
(`internal/cli/actions_loop.go:29`), so pointing the factory at another repo
means `cd`-ing into it and losing the `make`-target muscle memory; and
`daemon --repo` *is* honored (`actions_serve.go:237-239`), so the two
long-running commands disagree about their own flags.

## 5. What is already good (build on this, don't rebuild it)

- **`up` is the right shape and it already exists.** `devagent up` / `down` /
  `prd-intake` are registered (`internal/cli/root.go:474-476`,
  `internal/cli/actions_updown.go`, `internal/commands/updown.go`) with a
  469-line hermetic suite (`updown_test.go`): checks → dirs → intake → daemon →
  driver, own-session detach (`updown_unix.go`), pid-recorded stop, the
  loop-lock holder outranking its own pid file, `--dry-run`/`--json`/
  `--foreground`/`--no-daemon`, and plain-language hints. It also pins
  `SELFBUILD_DEVAGENT_BIN` to `os.Executable()` (`updown.go:292`), which is the
  identity fix §4 asks for.
- **The gates already know how to explain themselves.** `preflight` writes a
  structured `operator-degraded` row, exempts the omp startup wedge from opening
  the shared circuit, and pages only after 3 consecutive breaches
  (`internal/cli/actions_gates.go:149-232`,
  `internal/resilience/preflight-gate.go:154-175`); `status --providers` counts
  the degraded window (`actions_status.go:417-460`); `sync-docs` refuses with
  the blocking file named and exit 2 (`internal/git/doc_sync.go:186-241`). The
  plumbing for "why did it stop" exists — it just never reaches the loop's
  terminal paths (§3).
- **Sweep hygiene is session-scoped already** (`herdr-sweep --session`,
  `internal/herdr/sweep.go:301-310`, with the 2026-09-13 exemption ordering at
  `:105-120`) — but it has no *repo* scope, and `--orphans` from the loop is
  invoked without either (`dispatch.go:214-219`).
- `Makefile` `loop-start/stop/status/log` and `daemon-start/stop` exist with
  binary-presence and already-running guards (Makefile:49-73, :76-…): the right
  shape, minus the env that makes the driver trustworthy, and with a
  `pkill -f` stop (:60, :149) that can reach another checkout's driver.

## 6. The failure taxonomy (what unattended running actually dies of)

Production data, 21 days, 321 ledger rows, 250 iteration logs. Statuses:
`provider-degraded 65 · ok 62 · operator-degraded 55 · failed 49 · pr-open 21 ·
invalid 21 · failed-tests 14 · no-pr 11 · operator-diverged 10 · skipped 9 ·
merged 3 · push-failed 1`. Throughput was real — 97 merged `devagent/TASK-*`
branches in the window — at a median iteration of **28.3 min** (p90 141 min).

| # | Class | Evidence | Fix shape |
|---|---|---|---|
| F1 | Inherited starvation blocks every start | §2; 15-row non-productive shared tail | scope the streak to the driver's own rows + a `resume` marker |
| F2 | Operator pause reads as a fault | 55 × `operator-degraded: doc-sync deferred: PRD locally modified`, ~1 spin/73 s (49 rows on 09-12 alone); gate at `run.go:232-238`; still live today | the refusal already names the blocking file and exits 2 (`internal/git/doc_sync.go:186-241`, mapped to `operator-degraded` at `run.go:675-679`) — surface that text in `up`/`status` instead of only in a gitignored log; there is no `sync-docs --dry-run` (flags: `--branch/--json/--repo`) |
| F3 | Provider/model mismatch | 65 × `provider-degraded`, 50 with `preflight: provider probe failed`; 71 `operator-degraded` events whose detail is a raw session line (the probe's answer-shape parsing, not an outage); §4 model split | one model source of truth + the probe result echoed per iteration |
| F4 | Empty deterministic lane | 35 × `no open selfbuild issue found — falling back to LLM selection`; queue = 54 done / 1 failed / **0 pending** today | (#355) + make the empty lane a distinct ledger detail, which #355 already asks for |
| F5 | Intake has no input | `up --dry-run` on this repo reports `0 open PRD item(s)`; `git show HEAD:docs/PRD.md \| grep -c '^- \[ \]'` → **0** | convert the PRD's actionable backlog lines to `- [ ]` (content work, no code) |
| F6 | Dispatch timeouts, then blind fallback | 25 × `pane-run dispatch failed (rc=124) — falling back to direct dispatch` (`internal/loopdriver/run.go:697-705` — a non-zero `pane-run` rc logs one line and re-runs the same prompt *directly*, doubling the wall budget); 46 × circuit breaker; 49 × `task failed` | make the fallback a reported, budgeted event (ledger detail), not a log line in a gitignored file |
| F7 | Pane/orphan leak | 31 × `closed … reason=agent-idle`. The liveness probe **is** session-scoped now (`sweep.go:301-310` passes `--session`; the 2026-09-13 comment at `:105-120` records the fix and the new exemption ordering), so the #317 blind spot is closed — but nothing scopes a *repo*: `herdr-sweep --help` offers only `--dry-run/--orphans/--session` (measured) and the loop runs bare `herdr-sweep --orphans` (`dispatch.go:214-219`), so one checkout's sweep still acts on another checkout's panes (#354) | add a repo/ownership scope to `herdr-sweep` and pass it from the loop |
| F8 | Cross-checkout contamination | #354; and `SELFBUILD_HOME` doesn't isolate the driver (§2) | quarantine: refuse a non-primary checkout, `--repo`-scope the sweep, per-row provenance |
| F9 | Global `~/.devagent` mixing | §3 | key `runs/` + `locks/` by repo hash; keep a global index for `status` with an explicit `--all-repos` |
| F10 | Node-era plist/argv | §4(a): argv fixed in today's tree, `KeepAlive=true` still forced (`create.go:148`) | self-executable argv (done), `KeepAlive {SuccessfulExit=false}` for the loop, no restart-resurrect of an intentional halt |

## 7. The design (in the order that buys the most trust per line)

**D1 — Make the stop explain itself, and make resuming safe. *(~60 lines)***
Add a terminal-halt row to the ledger: `{"event":"loop-halt","loop":N,"reason":
"starved|circuit-breaker|max-iterations|lock-held","detail":…}`, written by
`RunLoop` at each `haltExit` site, plus a matching `haltReason` field in
`heartbeat.json` so `devagent status` / the TUI header / `GET /status` show
*"loop 361 halted: starved after 15 non-productive iterations (newest:
invalid)"* instead of a stale "task" phase. Pair it with a stamp in the
`loop-result` rows — `origin:<remote>` + `repo:` — so `EvaluateStarvation` can
count only this checkout's streak (F1) and #354's provenance complaint dies with
it. `devagent up --resume` appends a `loop-resume` row that breaks the streak
explicitly; nothing about the gate becomes silent again.

**D2 — One model/worker truth: read the file, keep env as override. *(~120 lines)***
`loopdriver.ConfigFromRepo(repo)` = `config.Load(repo)` merged *under*
`SELFBUILD_*`, with `DevagentBin` defaulting to `os.Executable()` (env still
wins) and `preflight`/dispatch taking the same resolved pair. Echo the resolved
pair in the iteration log so a mismatch is visible in the artifact that
survives (`[gate] worker=omp model=onegw/free bin=<abs path>`). This is what
turns "export 14 variables from a table in a doc" into "edit `devagent.json`".

**D3 — Finish `up` as a health receipt, not a spawn receipt. *(~80 lines)***
After starting the driver, `up` waits a bounded window and asserts the three
things that actually prove life: lock holder pid alive, `heartbeat.json`
advanced (mtime or iteration), and no `loop-halt` row. Report `ok=false` with
the halt reason when they don't. Then close the two gaps in the gate:
`up`'s prerequisite step (`RunInit`) never checks herdr or the daemon — so it
green-lit a factory whose visible-worker path was dead — and nothing in the
product creates the herdr session it assumes (the only mention is a doctor
*hint*, `doctor.go:463`). Either `up` provisions it (`herdr session create
--session <resolved>`, or falls back to `visibility=headless` and says so), or
`up` must fail with that hint. Note the control plane offers no help here: the
daemon has **no** loop start/stop route at all (its surface is
`/status,/events,/agents,/dispatch,/approve,/history,/healthz`,
`internal/daemon/daemon.go:301-352`), so `up`'s own spawn path is the only
lifecycle code and must be the one that is right.

**D4 — Quarantine by default. *(~40 lines, mostly the #354 ask)***
Refuse to drive from a linked worktree (`git rev-parse --git-common-dir` vs
`--git-dir`), give `herdr-sweep` a repo scope — it has none today, only
`--dry-run/--orphans/--session` (measured `--help`) — and pass it from the loop,
make `--dry-run` skip `record()`
and the state push (it currently appends an `ok`-status `(dry-run)` row,
`run.go:395-397`, which then satisfies the Q27 already-shipped guard and can
close an issue with nothing landed — #354), and stop forwarding a stale
`GITHUB_TOKEN` into panes.

**D5 — Make the lane fillable, and say when it's empty. *(content + ~20 lines)***
F4/F5 are the "long runtime, little value" half: intake is a no-op because this
repo's PRD contains zero checkboxes, and the queue holds zero pending items.
Convert the PRD's actionable items to `- [ ]` in the same change that ships
intake, keep #355's distinct `queue-empty` ledger detail, and have `up` print
`lane: 0 issues labeled selfbuild, 0 pending queue rows, 0 open PRD checkboxes`
— the one number that predicts whether the next 10 hours produce code.

**D6 — Stop the false all-clears. *(~30 lines)***
`herdr-sweep` must return the CLI error (`server_not_running` ≠ clean), the
loop's sweep must pass `--session`/`--repo`, and `status`'s recent-runs must be
repo-scoped. Each is a one-line behavior change with a test that a *real* herdr
error path can satisfy — the current fakes strip `--session` from argv, which
is why no suite can see F7/D4 today.

**D7 — Delete the second runbook. *(docs)***
`make loop-start` and `devagent up` must not coexist as the entry point: keep
the make targets as thin wrappers (`devagent up` / `devagent down`) so the
muscle memory survives, retire the `pkill -f` stop (Makefile:60, :149) in favor
of the pid-recorded stop, fix the plist argv (F10), and replace the
`SELFBUILD_*` table in §"Configuration" with "`up` reads `devagent.json`;
`SELFBUILD_*` overrides it" plus a short "knob reference" sub-list.

## 8. Definition of done for "easy to run locally"

A single scriptable check, runnable on any machine, that fails today and must
pass after D1–D3:

```bash
git clone https://github.com/FreePeak/devagent /tmp/dvx && cd /tmp/dvx
make build && ./devagent-go up --dry-run --json   # 1: no env, exit 0, reports lane depth
./devagent-go up --json                            # 2: within 90 s the driver holds the lock,
                                                   #    heartbeat advanced, no loop-halt row
./devagent-go status                               # 3: names this repo's state, not ~/.devagent's
                                                   #    aggregate; a halted loop says WHY
./devagent-go down                                 # 4: no loop/daemon pid survives, no pane leaks
```

Criterion 2 is the one to hold every design to: **green `up` must mean a
running factory.** Everything in §6 is either a way to break that promise or a
way to hide that it broke.

## 9. What this analysis does *not* propose

No new supervisor (launchd/hub/nohup already cover it; the loop intentionally
exits 0 on purpose and `devagent supervision` already reports the mode truth
correctly, `Makefile:66-73`), no loop control inside the daemon (it has no
start/stop route today and adding one duplicates `up`), no new config format,
no queue service, no TUI work beyond reading the halt reason, and no attempt to
make `scripts/*.sh` ergonomic — those five scripts are a separate convergence
decision already tracked as #365.

## 10. State of play at the end of this pass (2026-09-14 ~14:40 local)

The work analysed here is landing *while* this note is written. In the working
tree at the time of writing (all uncommitted, one `omp` session driving it):
`internal/commands/updown.go` + `updown_unix.go` + `updown_other.go` +
`updown_test.go` (469 lines), `internal/cli/actions_updown.go`,
`actions_prdintake.go`, `internal/prdintake/` + tests,
`internal/loopdriver/intake.go` + tests, `create.go`'s plist argv fix, and the
`README.md` / `docs/SELF-BUILD-LOOP.md` one-command runbook. So **D3 (spawn
half), F5's mechanism, F10's argv, and the second-runbook doc problem are in
flight**; what this pass adds to that plan is the measured list the WIP does
not yet cover: **F1 (inherited starvation halt), D1 (halt reason + resume seam),
D3's verify-after-start and herdr provisioning, F3's model two-truths, F9
(global `~/.devagent`), and the false `[devagent] no stale panes` in §3.**

Reproduce the headline result in one minute: clone, `make build`, run
`devagent up --no-daemon` in the clone, then `cat .selfbuild/logs/loop-*.log`.
Nothing from this pass was left running: no loop, no daemon, no worker, no
sandbox process; port 7788 is closed; the real `selfbuild/state` branch on
GitHub is untouched (tip `85add7a`, verified after the experiments, which wrote
only to a `/tmp` mirror).
## Related

- Issues: #371 (`up`/`down` — in flight in the working tree as of this pass),
  #370 (PRD checkbox intake), #355 (empty lane), #354 (quarantine — D4 is its
  fix), #350/#352 (pane liveness, revision stamping), #365 (shell residue),
  #362 (stale readiness doc), #286 (dry-run side effects).
- Docs: `docs/SELF-BUILD-LOOP.md` (protocol + knob table), `README.md` §"Run
  the self-build loop manually", `docs/HERDR.md`, `docs/TUI.md`,
  `docs/GROK.md` §2.3/§3/§5 (the earlier F1–F7 install-friction list this pass
  supersedes for the loop path), PRD §21 FR-SIMPLE and §20.1.
- Data: `.selfbuild/ledger.jsonl` (321 rows), `.selfbuild/logs/loop-*.log`
  (250 files), `.devagent/runs/orchestration/events.jsonl` (6 203 rows, 120
  `operator-degraded` probes classified in §6).