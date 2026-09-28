# contrib

Optional helpers. erdr runs without any of them — they're **reference
implementations** of the small contracts below, so you can drop them in as-is or
reimplement them in whatever your setup already uses.

| Script | Needs | What it does |
|---|---|---|
| `erdr-hook` | tmux | Claude Code hook that publishes each session's live state. Without it the 🔴 permission state never shows. |
| `erdr-ctx` | tmux, jq | Claude Code `statusLine` segment with the context left before auto-compact; flags the session in the bar when it runs low. |
| `erdr-open` | sway, [foot](https://codeberg.org/dnkl/foot) | Opens the bar, or focuses it if already open, in a window tagged with a stable `app_id`. |
| `erdr-sway-focus` | sway, python3 | Records the last focused non-erdr window so `Enter` can hand keyboard focus back to it. |

Using a different terminal or compositor? `erdr-open` is a ~15-line wrapper —
swap `foot` for your terminal, keeping the `app_id`/class tag. `erdr-sway-focus`
is sway-specific; on another compositor, write the focused window's id to the
state file below in whatever form your compositor's `focus` command accepts.

## The contracts

Everything the helpers do is written to tmux options and one state file:

| Key | Written by | Read by | Values |
|---|---|---|---|
| `@erdr_wait` (pane) | `erdr-hook` | erdr | `perm` · `waiting` · `working` |
| `@erdr_wait_since` (pane) | `erdr-hook` | erdr | unix timestamp of the last change |
| `@erdr_ctx_left` (pane) | `erdr-ctx` | erdr | % of context left before auto-compact · unset while above the threshold |
| `@erdr_sound` (global) | erdr (`s` key / ♫ chip) | `erdr-hook`, `erdr-ctx` | `on` · anything else = off |
| `$XDG_RUNTIME_DIR/erdr-last-term` | `erdr-sway-focus` | erdr | sway `con_id` of the last real terminal |

Sessions themselves are discovered by erdr directly — every tmux pane whose
foreground command is `claude` — so a session shows up with no hook installed at
all; only its state stays coarse (processing vs idle, inferred from the terminal
title).

## Sound

`erdr-hook` plays a sound on the transitions worth your attention, gated by
`@erdr_sound`. Override the files with `ERDR_SOUND_PERM` and
`ERDR_SOUND_DONE`; it needs `paplay` and falls silent if either the file or
`paplay` is missing. `erdr-ctx` plays `ERDR_SOUND_CTX` (default
`phone-outgoing-busy.oga`) once when a session crosses into low context, under the
same gate.
