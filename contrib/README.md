# contrib

Optional helpers. dai-bar runs without any of them — they're **reference
implementations** of the small contracts below, so you can drop them in as-is or
reimplement them in whatever your setup already uses.

| Script | Needs | What it does |
|---|---|---|
| `dai-bar-hook` | tmux | Claude Code hook that publishes each session's live state. Without it the 🔴 permission state never shows. |
| `dai-bar-ctx` | tmux, jq | Claude Code `statusLine` segment with the context left before auto-compact; flags the session in the bar when it runs low. |
| `dai-bar-open` | sway, [foot](https://codeberg.org/dnkl/foot) | Opens the bar, or focuses it if already open, in a window tagged with a stable `app_id`. |
| `dai-bar-sway-focus` | sway, python3 | Records the last focused non-dai-bar window so `Enter` can hand keyboard focus back to it. |

Using a different terminal or compositor? `dai-bar-open` is a ~15-line wrapper —
swap `foot` for your terminal, keeping the `app_id`/class tag. `dai-bar-sway-focus`
is sway-specific; on another compositor, write the focused window's id to the
state file below in whatever form your compositor's `focus` command accepts.

## The contracts

Everything the helpers do is written to tmux options and one state file:

| Key | Written by | Read by | Values |
|---|---|---|---|
| `@dai_bar_wait` (pane) | `dai-bar-hook` | dai-bar | `perm` · `waiting` · `working` |
| `@dai_bar_wait_since` (pane) | `dai-bar-hook` | dai-bar | unix timestamp of the last change |
| `@dai_ctx_left` (pane) | `dai-bar-ctx` | dai-bar | % of context left before auto-compact · unset while above the threshold |
| `@dai_bar_sound` (global) | dai-bar (`s` key / ♫ chip) | `dai-bar-hook`, `dai-bar-ctx` | `on` · anything else = off |
| `$XDG_RUNTIME_DIR/dai-bar-last-term` | `dai-bar-sway-focus` | dai-bar | sway `con_id` of the last real terminal |

Sessions themselves are discovered by dai-bar directly — every tmux pane whose
foreground command is `claude` — so a session shows up with no hook installed at
all; only its state stays coarse (processing vs idle, inferred from the terminal
title).

## Sound

`dai-bar-hook` plays a sound on the transitions worth your attention, gated by
`@dai_bar_sound`. Override the files with `DAI_BAR_SOUND_PERM` and
`DAI_BAR_SOUND_DONE`; it needs `paplay` and falls silent if either the file or
`paplay` is missing. `dai-bar-ctx` plays `DAI_BAR_SOUND_CTX` (default
`phone-outgoing-busy.oga`) once when a session crosses into low context, under the
same gate.
