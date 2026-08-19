# dai-bar 👾

A slim status **sidebar** for the [Claude Code](https://claude.ai/code) agents you
have running across tmux. It lives on the left edge of the screen and, at a
glance, tells you **which agent needs you right now** — navigate with arrows or
the mouse, hit Enter to jump straight to that pane.

Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea) +
[Lip Gloss](https://github.com/charmbracelet/lipgloss).

## What it shows

Each running `claude` session is a two-line block — name on top, elapsed time
below — with a fixed left "meter" and, on the right, a `▸` arrow marking the
session your terminal is currently showing.

The panel reads **bottom-up**: the sessions and the counter row sit at the foot,
where your eyes already rest on a tall sidebar, and the mascot takes the empty
space above. `dai-bar --top` mirrors that — counters and sessions at the top,
mascot at the bottom.

Status by color (**color means urgency, nothing else**):

- 🔴 **red** — needs you: a permission prompt is waiting.
- 🟢 **green** — unread: it finished and you haven't looked at the answer yet.
- 🟡 **amber** — processing: the agent is working (animated spinner).
- ⚪ **gray** — idle: read / doing nothing (most sessions, most of the time).

Green clears the moment you actually see the pane — you jump to it from the bar,
or you focus the terminal with tmux showing it. Reading it in the bar's preview
(arrows) doesn't count; that's browsing, not reading.

Cyan means only *you*: the selection band, the current-session arrow, and the
focused-header underline. When the bar has keyboard focus the little 👾 at the
bottom raises its hands.

## Requirements

- **tmux** — sessions are discovered as panes whose foreground command is `claude`.
- **sway** *(optional)* — powers the focus underline and the "jump keyboard focus
  to the target terminal" behavior. Without it the bar still lists and previews.
- **Go 1.24+** to build.

The bar itself needs only tmux. The helpers in [`contrib/`](contrib) add sway
(and, for `dai-bar-open`, the [foot](https://codeberg.org/dnkl/foot) terminal) —
see [contrib/README.md](contrib/README.md).

## Install

```sh
go install github.com/daronco/dai-bar@latest
```

The binary is `dai-bar`. Run it in a terminal you keep on the side. A helper that
opens-or-focuses it in a tagged sway window is in
[`contrib/dai-bar-open`](contrib/dai-bar-open).

## Status hook (recommended)

dai-bar reads the live state from a tmux pane option `@dai_bar_wait` that a Claude
Code hook keeps updated. Point Claude Code's hooks at
[`contrib/dai-bar-hook`](contrib/dai-bar-hook) — in `~/.claude/settings.json`:

```json
{
  "hooks": {
    "Stop":             [{ "hooks": [{ "type": "command", "command": "/path/to/dai-bar-hook stop" }] }],
    "Notification":     [{ "hooks": [{ "type": "command", "command": "/path/to/dai-bar-hook notify" }] }],
    "UserPromptSubmit": [{ "hooks": [{ "type": "command", "command": "/path/to/dai-bar-hook submit" }] }]
  }
}
```

The hook only ever sets `perm`, `working` and `done`; `waiting` (= read) is
written by the bar itself, when you look at the pane.

None of those marks is trusted on its own. A mark is set by one hook and cleared
by another, so a turn that ends without a Stop hook — you esc out of a prompt,
you decline a question — used to strand the session on red for good. The bar
reads the bottom of each pane every poll and lets the screen decide whether the
agent is actually working and whether a prompt is actually up. What the hook
adds on top is the part the screen can't show: that a stopped session is
*unread* rather than merely idle.

So without the hook the bar still gets 🟡 and ⚪ right — it just can't tell 🔴
and 🟢 apart from a session sitting quietly.

## sway integration (optional)

- **Focus return** — run [`contrib/dai-bar-sway-focus`](contrib/dai-bar-sway-focus)
  from your sway config (`exec_always …`). It records the last real terminal so
  pressing Enter in the bar hands keyboard focus back to it.
- **Unread clearing** — telling "you are looking at the session" from "it merely
  is the active tmux pane while you read your mail" means knowing which window
  has the keyboard, which is a sway question. Without sway, 🟢 clears only when
  you jump from the bar.
- **Bindings** — for example:

  ```
  # open/focus the bar
  bindsym $mod+c exec dai-bar-open
  # a slim left strip
  for_window [app_id="dai-bar"] floating enable, resize set 12 ppt 96 ppt, move position 0 ppt 2 ppt
  ```

## Keys

`↑/↓` (or `j/k`) move · `Enter` / click jump to the session · `r` reload · `q` quit.

`alt+<n>` jumps straight to tmux window `<n>`. `s` toggles the sound, same as
clicking the badge in the counter row.

---

Made for [Dai](https://github.com/daronco) 👾
