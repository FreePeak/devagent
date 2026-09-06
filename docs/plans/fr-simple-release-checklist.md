# FR-SIMPLE release checklist (§21.1 / FR-SIMPLE-06)

**Maps to:** `docs/PRD.md` §21.1 four principles · issue #144 R5  
**Audience:** DocOps / release operators  
**Scope:** Markdown walkthrough only — no required CI job in this slice.

Walk these surfaces on a cold or freshly-init'd repo before each release.
A change that adds a **required step** or a **required concept** on the happy
path must be fixed or flagged before shipping.

---

## §21.1 Four principles

1. **Few steps to value** — common paths (state a goal, approve a gate, read
   the result) are reachable in ≤3 commands; longer paths are power-user only.
2. **Show only what is needed** — each surface shows the current step and the
   single next action; advanced knobs stay out of the happy path.
3. **Visualized for human reading** — status, gate outcomes, queue, and ledger
   use the §20.8 card/chip language; raw JSON/NDJSON is opt-in via `--json`.
4. **Easy to set up and install** — clean machine → running factory via one
   guided command that checks prereqs and can prove itself with smoke.

---

## Walkthrough

### 1. `devagent init`

- [ ] Prints a plain-language checklist with chips (git, worker, optional
      provider / credentials / **docker** advisory).
- [ ] Missing Docker does **not** fail required `ok` / exit non-zero alone.
- [ ] Config write is idempotent; goal next-action is one sentence.
- [ ] Optional `devagent init --smoke` (default **off**) prints a hermetic
      fixture checklist ending in `done` with **no raw logs** and no live
      provider call.
- [ ] Post-init orientation ≤3 lines: Now / Next / Look (from `buildStatusView`).

**Principle check:** (4) easy setup · (1) ≤3 steps after init · (2) one next action.

### 2. Goal path (`devagent orchestrate --goal "…"`)

- [ ] From init'd repo, one-sentence goal dispatches without extra required
      concepts (FR-SIMPLE-02).
- [ ] `--smoke` is optional proof, not a fourth required step.

**Principle check:** (1) few steps to value.

### 3. `devagent status`

- [ ] Default output is a §20.8 phase card + one next action (PR #139).
- [ ] `--json` emits the same view as machine data (no ANSI).
- [ ] Idle / not-started with config points at the one-sentence goal command
      (matches post-init orientation).

**Principle check:** (2)(3) show only what is needed · visualized.

### 4. `devagent queue list`

- [ ] Default (no flags): card with counts + task rows + **exactly one**
      next-action line.
- [ ] `--json` still parses as a **task array** (byte-stable for existing fields).

**Principle check:** (3) human default · `--json` opt-in.

### 5. `devagent validate`

- [ ] Default: card/chip per gate (G1 / G3 / SKIP) + footer next action.
- [ ] `--json` emits `{ gates: [...], ok: boolean }`.

**Principle check:** (3) gate outcomes visualized.

### 6. `devagent ledger` / `--summary`

- [ ] Default list uses chips (not `+/x/?` alone).
- [ ] `--summary` renders a summary card with audits/resolved/unresolved chips
      + one next action.
- [ ] `--json` available for scripts (summary or list payload).

**Principle check:** (3) ledger visualized · (2) one next action.

---

## Sign-off

| Principle | Pass? | Notes |
|---|---|---|
| 1. Few steps to value | | |
| 2. Show only what is needed | | |
| 3. Visualized for human reading | | |
| 4. Easy to set up and install | | |

Release: ________  Reviewer: ________  Date: ________
