<p align="center">
  <img src="assets/logo.svg" alt="ermahgherdr" width="240">
</p>

# ermahgherdr 👾

A slim status **sidebar** for the [Claude Code](https://claude.ai/code) agents you
have running across tmux. It lives on the left edge of the screen and, at a
glance, tells you **which agent needs you right now** — navigate with arrows or
the mouse, hit Enter to jump straight to that pane.

Two names, one project: **ermahgherdr** is the project (module path, this repo);
**erma** is the command you type — the binary, the sway `app_id`, the tmux
option prefix (`@erma_*`), the contrib scripts (`erma-hook`, `erma-ctx`,
`erma-open`, `erma-sway-focus`) and their environment variables (`ERMA_*`).

Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea) +
[Lip Gloss](https://github.com/charmbracelet/lipgloss).

## What it shows

Each running `claude` session is a two-line block — name on top, elapsed time
below — with a fixed left "meter" and, on the right, a `▸` arrow marking the
session your terminal is currently showing.

The name comes from the pane title claude publishes. Set the pane option
`@ctx_label` and that wins instead — for when something else already labels your
tmux tabs and the two should agree.

The panel reads **bottom-up**: the sessions and the counter row sit at the foot,
where your eyes already rest on a tall sidebar, and the mascot takes the empty
space above. `erma --top` mirrors that — counters and sessions at the top,
mascot at the bottom.

Status by color (**color means urgency, nothing else**):

- 🟣 **purple** — needs you: a permission prompt or a question is waiting.
- 🟢 **green** — unread: it finished and you haven't looked at the answer yet.
- 🟡 **amber** — processing: the agent is working (animated spinner), or waiting on background agents it started.
- ⚪ **gray** — idle: read / doing nothing (most sessions, most of the time).

Green clears the moment you actually see the pane — you jump to it from the bar,
or you focus the terminal with tmux showing it. Reading it in the bar's preview
(arrows) doesn't count; that's browsing, not reading.

`u`, a middle-click or **unread** in the right-click menu puts the green back (a middle-click on a green one reads it): you read it and want it
to keep asking. That mark is pinned — unlike the automatic green it survives the
pane being right there in front of you, and only a deliberate jump, or the
session doing something new, clears it.

A right-click opens a small menu on the session — floating over the list, which dims behind it — with **unread** (or **read**), **pause** and **prio**, each turning into the way back once set. The row under the mouse lights up, in the list and in the menu. The legend at the foot names the same three, key letter in bold. The two marks move a session rather than color it. A **paused** session folds to one dim line in a `‖ paused` block right by the counters, and a finished answer there stays quiet (no green, no sound) — a permission prompt still turns it purple; prompting it resumes it. A **prio** session gets a ★, a bold name and a `★ prio` block just above the paused one; its sounds play even with ♫ off. The unmarked sessions stay at the far end with no heading, first to scroll off when the bar overflows. One mark at a time. Marks are kept per Claude session in `$XDG_STATE_HOME/erma/flags.json`, so they outlive the pane and a tmux restart.

A pane carrying the tmux option `@dai_live` — the always-on assistant session of dai-live marks itself so — is pinned: bottom-up it leaves the list and sits on the top rows of the panel between two rules, its status bullet replaced by a satellite dish in the state's colour, and the list slides under it when the bar overflows. With `--top` it is just the first row.

Beside the mascot sit two badges, the same Font Awesome icons as their waybar modules and tinted by the same states: a satellite dish by the ear for dai-live (from the tmux option `@dai_live_state`, which dai-live keeps) and a briefcase by the feet for workwatch (its pause file). The terminal needs Font Awesome 7 among its fonts, or it draws a box. A click on one switches it on or off; the pointer over one shades it like a row. With a Dai session up, the mascot lights up, arms raised, while that session's pane is the one in front of you.

Any pane can carry its own sounds in the tmux options `@erma_sound_perm` and `@erma_sound_done`: a file to play instead of the default, or `off`. A sound the pane chose plays even with ♫ off — dai-live uses it to stay silent when a round ends and call out only when it asks something.

A meter under the name shows how much of the room to auto-compact a session has spent. It draws on the session you are looking at and on any past 70%, turns amber there and purple from 85% — the same purple as waiting on you, told apart by form: waiting fills the row, the meter is a thin line under it. Compact rows show it as **◔N%** (N% spent), from 85%.

**⤷N** on the time line means that session has N subagents running.

Cyan means only *you*: the selection band, the current-session arrow, and the
focused-header underline. When the bar has keyboard focus the little 👾 at the
bottom raises its hands.

## Requirements

- **tmux** — sessions are discovered as panes whose foreground command is `claude`.
- **sway** *(optional)* — powers the focus underline and the "jump keyboard focus
  to the target terminal" behavior. Without it the bar still lists and previews.
- **Go 1.24+** to build.

The bar itself needs only tmux. The helpers in [`contrib/`](contrib) add sway
(and, for `erma-open`, the [foot](https://codeberg.org/dnkl/foot) terminal) —
see [contrib/README.md](contrib/README.md).

## Install

From a checkout:

```sh
make install
```

Builds the `erma` binary and copies it, along with the four `contrib/erma-*`
scripts, into `$PREFIX/bin` (default `~/.local/bin`). Override with
`make install PREFIX=/somewhere/else`; on hosts where `go` is not on `PATH`,
pass it explicitly: `make GO=/usr/local/go/bin/go install`.

`make uninstall` removes exactly those five files and nothing else.

Alternative for the binary only:

```sh
go install github.com/daronco/ermahgherdr/cmd/erma@latest
```

## Setup

Make sure `~/.local/bin` is on your `PATH`, then wire the pieces you want.

### Claude Code hooks and status line

erma reads the live state from a tmux pane option `@erma_wait` that a Claude Code hook keeps updated. The `statusLine` segment prints how much of the room to auto-compact is spent, colored past `ERMA_CTX_WARN` (default 85), and feeds the meter in the bar; ahead of it goes the session's mark (★ prio, ‖ paused). In `~/.claude/settings.json`:

```json
{
  "hooks": {
    "Stop":             [{ "hooks": [{ "type": "command", "command": "~/.local/bin/erma-hook stop" }] }],
    "Notification":     [{ "hooks": [{ "type": "command", "command": "~/.local/bin/erma-hook notify" }] }],
    "UserPromptSubmit": [{ "hooks": [{ "type": "command", "command": "~/.local/bin/erma-hook submit" }] }],
    "SubagentStart":    [{ "hooks": [{ "type": "command", "command": "~/.local/bin/erma-hook substart" }] }],
    "SubagentStop":     [{ "hooks": [{ "type": "command", "command": "~/.local/bin/erma-hook substop" }] }]
  },
  "statusLine": { "type": "command", "command": "~/.local/bin/erma-ctx" }
}
```

The hook only ever sets `perm`, `working` and `done`; `waiting` (= read) is
written by the bar itself, when you look at the pane.

None of those marks is trusted on its own. A mark is set by one hook and cleared
by another, so a turn that ends without a Stop hook — you esc out of a prompt,
you decline a question — used to strand the session on waiting for good. The bar
reads the bottom of each pane every poll and lets the screen decide whether the
agent is actually working and whether a prompt is actually up. What the hook
adds on top is the part the screen can't show: that a stopped session is
*unread* rather than merely idle.

So without the hook the bar still gets 🟡, ⚪ and a dialog's 🟣 right — it just can't tell 🟢 apart from a session sitting quietly, and it can't count subagents.

### sway

```
# open/focus the bar
bindsym $mod+c exec erma-open
# a slim left strip
for_window [app_id="erma"] floating enable, resize set 12 ppt 96 ppt, move position 0 ppt 2 ppt
# record the last real terminal so Enter in the bar returns focus to it
exec_always erma-sway-focus
```

Without `erma-sway-focus`, 🟢 clears only when you jump from the bar — telling
"you are looking at the session" from "it is merely the active tmux pane while
you read your mail" needs to know which window has the keyboard, which is a sway
question.

### tmux options and state-file contract

erma, `erma-hook`, `erma-ctx` and `erma-sway-focus` talk to each other through
a handful of tmux options and one state file. See
[contrib/README.md](contrib/README.md) for the full table.

## Keys

`↑/↓` (or `j/k`) move · `Enter` / click jump to the session · right-click menu · middle-click read/unread · `u` unread · `p` pause · `o` prio · `s` sound · `r` reload · `q` quit.

`u` marks a session unread again. `alt+<n>` jumps straight to tmux
window `<n>`. `s` toggles the sound, same as clicking the badge in the counter
row.

## Security

Every pull request to `main` (and every push to `main`) runs the following
GitHub Actions checks:

- **CI** — `gofmt -l`, `go build`, `go vet`, `go test`, and
  [`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck)
  across the whole module. A reachable vulnerability fails the job.
- **gitleaks** — scans the PR diff for secrets. One match fails the job.
- **CodeQL** — GitHub's static analysis for Go with the default query suite,
  also on a weekly schedule.
- **Dependabot** — opens weekly PRs for outdated Go modules and GitHub
  Actions.

Two settings-only safeguards are also enabled on github.com for this repo:
**secret scanning** and **push protection** — pushes carrying a known secret
pattern are blocked at the server before the commit lands. See
[GitHub's docs](https://docs.github.com/en/code-security/secret-scanning/introduction/about-secret-scanning).

---

Made for [Dai](https://github.com/daronco) 👾
