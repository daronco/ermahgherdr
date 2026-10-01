# contrib

Optional helpers. erma runs without any of them — they're **reference
implementations** of the small contracts below, so you can drop them in as-is or
reimplement them in whatever your setup already uses.

| Script | Needs | What it does |
|---|---|---|
| `erma-hook` | tmux | Claude Code hook that publishes each session's live state. Without it the 🔴 permission state never shows. |
| `erma-ctx` | tmux, jq | Claude Code `statusLine` segment with the context left before auto-compact; flags the session in the bar when it runs low. |
| `erma-open` | sway, [foot](https://codeberg.org/dnkl/foot) | Opens the bar, or focuses it if already open, in a window tagged with a stable `app_id`. |
| `erma-sway-focus` | sway, python3 | Records the last focused non-erma window so `Enter` can hand keyboard focus back to it. |

Using a different terminal or compositor? `erma-open` is a ~15-line wrapper —
swap `foot` for your terminal, keeping the `app_id`/class tag. `erma-sway-focus`
is sway-specific; on another compositor, write the focused window's id to the
state file below in whatever form your compositor's `focus` command accepts.

## The contracts

Everything the helpers do is written to tmux options and one state file:

| Key | Written by | Read by | Values |
|---|---|---|---|
| `@erma_wait` (pane) | `erma-hook` | erma | `perm` · `waiting` · `working` |
| `@erma_wait_since` (pane) | `erma-hook` | erma | unix timestamp of the last change |
| `@erma_ctx_used` (pane) | `erma-ctx` | erma | % of the room to auto-compact already spent, republished on every change · erma draws it as a meter |
| `@erma_sound` (global) | erma (`s` key / ♫ chip) | `erma-hook` | `on` · anything else = off |
| `$XDG_RUNTIME_DIR/erma-last-term` | `erma-sway-focus` | erma | sway `con_id` of the last real terminal |

Sessions themselves are discovered by erma directly — every tmux pane whose
foreground command is `claude` — so a session shows up with no hook installed at
all; only its state stays coarse (processing vs idle, inferred from the terminal
title).

## Sound

`erma-hook` plays a sound on the transitions worth your attention, gated by
`@erma_sound`. Override the files with `ERMA_SOUND_PERM` and
`ERMA_SOUND_DONE`; it needs `paplay` and falls silent if either the file or
`paplay` is missing.

Context is silent on purpose — the meter changing color says it, and a session
running low is not something you need to hear the second it happens.
