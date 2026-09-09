# TUI conventions research — how professional Go terminal tools are built

*Research pass 2026-09-09; feeds the conventions-research polish wave (PRD §20.8).
Goal: benchmark devagent's hand-rolled Go TUI (internal/tui, no bubbletea — the
no-new-deps contract, FR-GO-11) against the tools operators actually use, and close
every gap that is pure convention rather than library magic.*

## Benchmarks studied

| Tool | Stack | What it does best |
|---|---|---|
| k9s | bubbletea-adjacent | glanceable header strip, zero-hierarchy `:` nav, help on `?`, risk-tiered destructive keys, OSC title via status |
| lazygit | gocui | panel focus model, `?`-generated keybinding menu, `/` search + `n/N` iteration, modal confirm menus |
| htop | C (ncurses) | meters row, F1–F10 permanent footer, semantic color, `-C` monochrome / `--no-unicode` degradation first-class |
| gh-dash | bubbletea + lipgloss | sections-as-tabs, named columns with width/grow/hidden/align, sidebar preview, YAML-configurable keybindings |
| opencode | bubbletea | chat transcript, `ctrl+x` leader combos, plan/build permission profiles, theme JSON with `none` escape, dark/light variants |
| crush | bubbletea + lipgloss | session manager, permission queue, notifications only when unfocused, command palette |
| bubbles canon | bubbletea components | KeyMap = binding+help in one table; help auto-generates; pagination dots; toasts |
| dry / cointop / superfile | termui etc. | `f` log follow, chart+table split, sidebar+preview, narrow-terminal fallback layouts |

## The Elm-architecture lesson (bubbletea)

Model + `Update(msg)` + pure `View()`, async results as messages. Devagent's Go TUI
already matches the *discipline* if not the framework: one guarded state struct, a
serial `handleKey` transition table, goroutines funneling through one mutex, and a
pure `RenderLines → RenderFrame` diff. No rewrite needed — the architecture was right.

## What the polish wave adopted (all dependency-free)

1. **NO_COLOR / TERM=dumb → monochrome palette** (no-color.org; htop `-C`). Structure
   (Bold/Dim/Inverse) kept, color SGR emptied. `NO_COLOR` beats `COLORTERM=truecolor`.
2. **PAUSED aggregate** — permission-needed is never silent (opencode/crush): amber
   `● PAUSED` chip + hero banner with the exact next key (`[g] answer · [k] kill`).
3. **`/` log search** (lazygit/gh-dash): live prompt with cursor cell, case-insensitive
   filter, `n`/`N` walking matches with wraparound. Scroll math lives in *filtered*
   space — the viewport paginates matches, not the raw buffer.
4. **Selection-following viewports** (lazygit/htop): worker cards and the sessions list
   window around the cursor; the cut is announced (`↑N/↓N hidden`) on the title line.
5. **OSC-2 terminal title** mirrors the aggregate (`devagent — RUNNING`) — k9s-style.
6. **Grouped help** — flat list reorganized into views/act/move/live-log/general.
7. **termios IUTF8** in raw mode; **upgrade overlay reads `internal/version`** (the
   hardcoded 0.1.0 went stale the moment releases got `-ldflags` stamping).

## Deliberately NOT adopted

- Mouse support (SGR 1000/1006) — keyboard-first is the Charm norm; defer.
- Theme config files / user palettes — one muted palette is the product's identity.
- gocui/bubbletea migration — violates the no-new-deps contract; hand-rolled diff
  renderer already has superior guarantees (zero-repaint frames, tested).
- Custom-remappable keybindings — no operator demand yet.

## Sources

bubbletea/lipgloss/bubbles/gh-dash/crush/gum/x READMEs; gh-dash.dev keybinding+layout
docs; k9scli.io commands; lazygit docs/keybindings + Searching.md; htop.dev + man page;
opencode.ai/docs tui+themes. Fetched 2026-09-09 (all succeeded; two 404s worked around).
