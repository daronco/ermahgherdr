// Package session owns the tmux-side truth that both the erma bar and the
// erma serve server read: pane discovery, screen parsing (what Claude Code is
// showing right now), the @erma_* and @dai_live pane options, the flags.json
// marks keyed by Claude session id, and the registry at
// ~/.claude/sessions/<pid>.json that ties a pane to a Claude session.
//
// Everything here is read-only against tmux state EXCEPT the three writes the
// bar already owns: setting the "@erma_wait" marks (MarkRead, MarkUnread, and
// the "done" writeback inside Gather) and the "@erma_flag" pane option
// mirrored from flags.json (SetPaneFlag). The erma serve subcommand MUST NOT
// call any of those mutators — see cmd/erma/serve.go.
package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// State: the color-coded severity the bar paints a session with.
type State int

const (
	Idle   State = iota // gray, static ○ — nothing to see (the many)
	Done                // green, ✓ — finished, and you have not looked yet
	Proc                // amber, spinner — processing
	Urgent              // red, ● — needs you
)

// A "perm" mark is dropped only on repeated evidence. The notification fires as
// the prompt is drawn and can win the race against the draw, and a single blind
// read would flip the item green and back — the one direction worth being slow
// about, since a missed red is a session waiting on you forever.
const (
	PermGrace  = 2 // seconds of unconditional belief in a fresh mark
	PermMisses = 2 // consecutive reads with no prompt before it is dropped
)

// Screen: what a pane is showing right now. The hooks say what happened; only
// the screen says what is happening — the terminal title used to stand in for
// this and cannot: Claude Code writes it once per turn and leaves it there, so
// a working pane and a finished one carry the same glyph.
type Screen struct {
	Busy   bool // the status line is ticking, or it waits on background agents
	Prompt bool // a choice may be up: enough to keep a hook's "perm" alive
	Form   bool // a dialog is certainly up: enough to raise red with no hook
	Viewer bool // the transcript viewer covers the session: the screen says nothing
}

// Session: one pane running Claude Code, as the bar wants to render it.
type Session struct {
	Pane, SessWin, Name string
	Mark                string // the pane option, verbatim — "unread" resists auto-read
	Sid                 string // the Claude session id, "" until its registry file is found
	Flag                string // your mark on it: "" · "paused" · "prio"
	St                  State
	Since               int64
	Ctx                 string // @erma_ctx_used: % of the room to auto-compact already spent
	Sub                 int    // subagents running under it (@erma_sub)
	Dai                 bool   // the Dai session dai-live keeps (pane option @dai_live)
	S, W, P             int
}

// Poll: one tmux round-trip's result — what every pane is showing, which
// clients are attached, and the shared sound flag.
type Poll struct {
	Screens map[string]Screen
	Clients []int  // client pids, for the focus rule
	Current string // the pane the first attached client is showing
	Win     string // and its window, as session:index
	Sound   bool
	Live    string // @dai_live_state, set by dai-live; "" without it
}

// FlagRec: one row of flags.json.
type FlagRec struct {
	Flag  string `json:"flag"`
	Since int64  `json:"since"`
}

// busyLine matches Claude Code's ticking status line — "✢ Reticulating… (15m
// 52s · ↓ 45.1k tokens)". The elapsed counter is the part that only exists
// while it works, and it survives the verb list and the glyph set changing.
var busyLine = regexp.MustCompile(`…\s*\(\d+[smh]`)

// bgWait: the turn ended but the agents it started are still running, and
// Claude resumes on its own when they finish — nothing for you to do yet. The
// line outlives the wait in the transcript, so only the newest status line counts.
var bgWait = regexp.MustCompile(`Waiting for [1-9]\d* background agents? to finish`)

// optLine matches one entry of a choice list — "❯ 1. Yes", "  2. No" — after the
// box border is trimmed off. One alone is not a prompt: the input box is also a
// ❯ and you may well have typed "2. and check the other one" into it.
var optLine = regexp.MustCompile(`^(❯\s+)?[1-9]\.\s`)

// capSep marks a block boundary in the batched capture. Printable on purpose:
// display-message escapes control bytes into their \ooo spelling.
const capSep = "~~erma-block~~"

// liveLines: how far above the input box the status line can sit. The task list
// Claude draws between the two runs to six lines.
const liveLines = 12

// lastStatus: the newest status line in live — the first one, going up from the
// input box, that opens with one of Claude's spinner glyphs.
func lastStatus(live []string) string {
	for _, ln := range live {
		if r := []rune(strings.TrimSpace(ln)); len(r) > 0 && strings.ContainsRune("*·✢✳✶✻✽", r[0]) {
			return ln
		}
	}
	return ""
}

// regions splits a pane at its dividers. live is what sits just above the input
// box (status line, task list); foot is what follows the last divider (a dialog,
// or the footer). Only these two can be live — everything above is transcript,
// and an answer that quotes "Do you want to" must not read as a prompt.
func regions(lines []string) (live, foot []string) {
	var rules []int
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "──") {
			rules = append(rules, i)
		}
	}
	top, foot := len(lines), lines
	switch n := len(rules); {
	case n >= 2:
		top, foot = rules[n-2], lines[rules[n-1]+1:]
	case n == 1:
		top, foot = rules[0], lines[rules[0]+1:]
	}
	for i := top - 1; i >= 0 && len(live) < liveLines; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			live = append(live, lines[i])
		}
	}
	return live, foot
}

// TmuxOut runs tmux and returns stdout. Exported so the server can share the
// same path when it needs an ad-hoc read; errors fold to the empty string, the
// same as every other reader in this package.
func TmuxOut(a ...string) string { out, _ := exec.Command("tmux", a...).Output(); return string(out) }

func run(name string, a ...string) { _ = exec.Command(name, a...).Run() }

// PollPanes reads every pane in ONE tmux round-trip: a `display-message` marks
// where each block starts, the `capture-pane` after it carries the content.
// Per-pane calls cost ~5ms each, which at two polls a second is real CPU for a
// bar that is always open; batched, a poll stays two forks however many
// sessions are up.
//
// The client list and the sound flag ride the same round-trip, and go FIRST:
// tmux stops a command list at its first error, so a pane that dies mid-poll
// truncates the tail. Losing pane blocks is already tolerated; losing the client
// list would move the focus rule out from under the bar.
//
// Blocks come back in the order asked — the marker cannot carry the pane id,
// because display-message runs its output through strftime and would eat the
// leading %. Panes that come back empty, or not at all, are left out of the map
// and keep whatever their mark says. Downgrading a state on missing evidence is
// exactly the bug this file is fixing.
func PollPanes(panes []string) Poll {
	args := []string{
		"display-message", "-p", capSep, ";",
		"list-clients", "-F", "#{client_pid}\t#{pane_id}\t#{session_name}:#{window_index}", ";",
		"display-message", "-p", capSep, ";",
		"show", "-gqv", "@erma_sound", ";",
		"display-message", "-p", capSep, ";",
		// -q: unset without dai-live, and an error would end the list here.
		"show", "-gqv", "@dai_live_state",
	}
	for _, p := range panes {
		args = append(args, ";", "display-message", "-p", "-t", p, capSep,
			";", "capture-pane", "-p", "-t", p)
	}
	blocks := strings.Split(TmuxOut(args...), capSep+"\n")
	var poll Poll
	if len(blocks) < 4 {
		return poll // tmux gave us nothing usable
	}
	for _, ln := range strings.Split(blocks[1], "\n") {
		f := strings.Split(strings.TrimSpace(ln), "\t")
		if len(f) != 3 || f[0] == "" {
			continue
		}
		poll.Clients = append(poll.Clients, atoi(f[0]))
		if poll.Current == "" {
			poll.Current, poll.Win = f[1], f[2]
		}
	}
	poll.Sound = strings.TrimSpace(blocks[2]) == "on"
	poll.Live = strings.TrimSpace(blocks[3])
	if len(blocks) > len(panes)+4 {
		return poll // a pane is showing our marker: bail rather than misalign
	}
	poll.Screens = map[string]Screen{}
	for i, blk := range blocks[4:] {
		if i < len(panes) && strings.TrimSpace(blk) != "" {
			if sc := ReadScreen(blk); !sc.Viewer {
				poll.Screens[panes[i]] = sc
			}
		}
	}
	return poll
}

// ReadScreen: is this pane working, and is a prompt up? Claude Code redraws the
// visible screen every turn — a finished tool call replaces its own prompt box,
// and the ticking status line replaces itself with a "Churned for 15s" summary —
// so a marker still on screen means it is still live.
func ReadScreen(body string) Screen {
	var sc Screen
	live, foot := regions(strings.Split(body, "\n"))
	for _, ln := range live {
		sc.Busy = sc.Busy || busyLine.MatchString(ln)
	}
	sc.Busy = sc.Busy || bgWait.MatchString(lastStatus(live))
	opts, cursored, esc, ask := 0, false, false, false
	for _, ln := range foot {
		l := strings.ToLower(ln)
		sc.Viewer = sc.Viewer || strings.Contains(l, "showing detailed transcript")
		// "Esc to cancel" is the footer every dialog carries; the second half tells
		// a dialog waiting on you from a menu you opened yourself. The question is
		// phrased per tool ("Do you want to create X?"), so only its opening counts.
		esc = esc || strings.Contains(l, "esc to cancel")
		ask = ask || strings.Contains(l, "do you want to ") ||
			strings.Contains(l, "enter to select") || strings.Contains(l, "enter to confirm") ||
			strings.Contains(l, "requests your input") || strings.Contains(l, "run a dynamic workflow?")
		sc.Prompt = sc.Prompt || strings.Contains(l, "choose an option:")
		if m := optLine.FindStringSubmatch(strings.TrimLeft(ln, " │")); m != nil {
			opts++
			cursored = cursored || m[1] != ""
		}
	}
	sc.Form = esc && ask
	// A list you are choosing from: several entries, one of them under the
	// cursor. A numbered list Claude merely wrote out has no cursor.
	sc.Prompt = sc.Prompt || esc || ask || (cursored && opts > 1)
	return sc
}

// StatusOf folds the pane mark (what the hooks last saw), the screen and the
// mark's age. seen is false when the pane could not be read.
//
// A mark is set by one hook and cleared by another, so a turn that ends without
// a Stop hook — you esc out of a prompt, you decline a question — used to strand
// the item on red or amber for good. The screen is what breaks that.
//
// Anything that stopped and has not been looked at is `done`, not idle: "waiting"
// means read, and only the bar writes it, when you look. "unread" is the same
// green, put there by hand — see MarkUnread.
//
// A dialog on screen wins over everything: not every one of them sends the
// permission notification, and a spinner left over from the turn is not work.
func StatusOf(wait string, sc Screen, seen bool) State {
	switch {
	case sc.Form:
		return Urgent
	case sc.Busy:
		return Proc
	case wait == "perm":
		return Urgent // Gather has already dropped the marks it disbelieves
	case wait == "working" && !seen:
		return Proc
	case wait == "working", wait == "done", wait == "unread":
		return Done
	}
	return Idle
}

// PermAlive: is a "perm" mark still worth believing? misses counts the
// consecutive polls that read the pane and found no prompt on it.
func PermAlive(sc Screen, seen bool, age int64, misses int) bool {
	if !seen || sc.Prompt {
		return true
	}
	return age <= PermGrace || misses < PermMisses
}

// MarkRead clears the unread green. It goes to the pane option so it outlives a
// restart of the bar and so the hook sees it on its next transition.
func MarkRead(pane string) {
	run("tmux", "set", "-p", "-t", pane, "@erma_wait", "waiting")
}

// MarkUnread puts the green back by hand — you read it, and you want it to keep
// asking. It is a mark of its own rather than a plain "done" because the pane
// you are staring at while you press u would be auto-read again on the next
// poll; only a deliberate jump (or the session doing something new) clears it.
func MarkUnread(pane string) {
	run("tmux", "set", "-p", "-t", pane, "@erma_wait", "unread")
}

// FlagsPath: the marks you set by hand, keyed by Claude session id. A file and
// not a pane option, so a mark survives tmux restarting and follows the session
// into another pane. erma-hook drops "paused" from it when you prompt the session.
func FlagsPath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(dir, "erma", "flags.json")
}

// LoadFlags reads flags.json; a missing or malformed file yields an empty map.
func LoadFlags() map[string]FlagRec {
	m := map[string]FlagRec{}
	if b, err := os.ReadFile(FlagsPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// SaveFlag sets one session's mark, "" clearing it. erma-hook takes the same
// lock to unpause, and the rename means no reader ever sees half a file.
func SaveFlag(sid, fl string) {
	p := FlagsPath()
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	lk, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	defer lk.Close()
	_ = syscall.Flock(int(lk.Fd()), syscall.LOCK_EX)
	m := LoadFlags()
	if fl == "" {
		delete(m, sid)
	} else {
		m[sid] = FlagRec{fl, time.Now().Unix()}
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if tmp := p + ".tmp"; os.WriteFile(tmp, append(b, '\n'), 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}

// SetPaneFlag mirrors a mark onto the pane as @erma_flag: the tmux tab and
// erma-hook's sound rules read it there, without parsing the file.
func SetPaneFlag(pane, fl string) {
	if fl == "" {
		run("tmux", "set", "-p", "-u", "-t", pane, "@erma_flag")
	} else {
		run("tmux", "set", "-p", "-t", pane, "@erma_flag", fl)
	}
	run("tmux", "refresh-client", "-S")
}

// PaneSessions maps tmux pane -> Claude session id, from the registry Claude
// Code keeps as one file per running process. A claude that crashed leaves its
// file behind, so a file whose process is gone does not count.
func PaneSessions() map[string]string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "sessions", "*.json"))
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var r struct {
			Pid       int    `json:"pid"`
			SessionID string `json:"sessionId"`
			Tmux      string `json:"tmux"` // "0:@8.%16" — session:window.pane
		}
		if json.Unmarshal(b, &r) != nil || r.SessionID == "" {
			continue
		}
		i := strings.LastIndexByte(r.Tmux, '.')
		if i < 0 || syscall.Kill(r.Pid, 0) == syscall.ESRCH {
			continue
		}
		out[r.Tmux[i+1:]] = r.SessionID
	}
	return out
}

// BandOf: which stretch of the list a mark puts a row in, counted from the far
// end. The unmarked rows sit there, with no heading and first to scroll off;
// then priority; then the paused, folded to a line each, against the counters.
func BandOf(fl string) int {
	switch fl {
	case "prio":
		return 1
	case "paused":
		return 2
	}
	return 0
}

// Arrange orders rows by band and keeps tmux order inside each. The far end is
// the top of the screen bottom-up, and the bottom with --top. The Dai session
// comes before every band: the first row is the top of the list in both layouts.
func Arrange(rows []Session, top bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Dai != rows[j].Dai {
			return rows[i].Dai
		}
		a, b := BandOf(rows[i].Flag), BandOf(rows[j].Flag)
		if top {
			return a > b
		}
		return a < b
	})
}

// CleanName extracts a human-readable name from a pane title, falling back to
// the basename of its current path.
func CleanName(title, path string) string {
	t := strings.TrimSpace(title)
	for len(t) > 0 {
		r, sz := utf8.DecodeRuneInString(t)
		if (r >= 'A' && r <= 'z') || r == '(' || r == '~' || (r >= '0' && r <= '9') {
			break
		}
		t = t[sz:]
	}
	t = strings.TrimSpace(strings.TrimPrefix(t, "=> "))
	if t == "" || strings.HasPrefix(t, "/") || strings.Contains(t, "/mnt/") {
		return filepath.Base(strings.TrimRight(path, "/"))
	}
	return t
}

func atoi(s string) int     { n, _ := strconv.Atoi(s); return n }
func atoi64(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

// Gather is one full read: every Claude pane, its screen, its flags, its sid.
// It also writes the "done" conclusion back to the pane mark when the screen
// says the turn ended without a Stop hook — the one write Gather owns, so the
// hook does not ping for a "finished" it thinks is new.
//
// permMiss is the across-polls counter PermAlive consults; the caller owns the
// map so the state survives across calls.
func Gather(permMiss map[string]int) ([]Session, Poll) { return gather(permMiss, true) }

// GatherReadOnly is Gather without the write-backs (the "done" conclusion and
// the @erma_flag sync): the same rows, for a reader that must not touch tmux.
func GatherReadOnly(permMiss map[string]int) ([]Session, Poll) { return gather(permMiss, false) }

func gather(permMiss map[string]int, write bool) ([]Session, Poll) {
	f := strings.Join([]string{
		"#{pane_id}", "#{session_name}", "#{window_index}", "#{pane_index}",
		"#{@erma_wait}", "#{@erma_wait_since}", "#{pane_current_path}", "#{pane_title}",
		"#{@ctx_label}", "#{@erma_ctx_used}", "#{@erma_sub}", "#{@erma_flag}", "#{@dai_live}",
	}, "\t")
	out := TmuxOut("list-panes", "-a", "-f", "#{==:#{pane_current_command},claude}", "-F", f)
	var recs [][]string
	var panes []string
	for _, line := range strings.Split(out, "\n") {
		c := strings.Split(line, "\t")
		if len(c) < 13 || c[0] == "" {
			continue
		}
		recs, panes = append(recs, c), append(panes, c[0])
	}
	poll := PollPanes(panes)
	screens := poll.Screens
	flags, sids := LoadFlags(), PaneSessions()
	now := time.Now().Unix()
	var rows []Session
	for _, c := range recs {
		wait, since := c[4], atoi64(c[5])
		sc, seen := screens[c[0]]
		if wait == "perm" && seen && !sc.Prompt {
			permMiss[c[0]]++ // only a read that found no prompt counts against it
		} else {
			delete(permMiss, c[0])
		}
		if wait == "perm" && !PermAlive(sc, seen, now-since, permMiss[c[0]]) {
			wait = "done"
		}
		st := StatusOf(wait, sc, seen)
		// Write the conclusion back, so the hook's next transition compares
		// against the state we are actually showing — and does not ping for a
		// "finished" it thinks is new.
		if write && seen && st == Done && c[4] != "done" && c[4] != "unread" {
			run("tmux", "set", "-p", "-t", c[0], "@erma_wait", "done")
		}
		// @ctx_label, when something outside sets it, is the same short label the
		// tmux tab shows — the bar and the tab should not disagree about a session.
		name := strings.TrimSpace(c[8])
		if name == "" {
			name = CleanName(c[7], c[6])
		}
		sid := sids[c[0]]
		fl := ""
		if sid != "" {
			fl = flags[sid].Flag
		}
		if write && fl != c[11] { // erma-hook unpaused it, or the pane is new to the mark
			SetPaneFlag(c[0], fl)
		}
		rows = append(rows, Session{
			Pane: c[0], SessWin: c[1] + ":" + c[2], Name: name, Sid: sid, Flag: fl,
			Mark: wait, St: st, Since: since, Ctx: strings.TrimSpace(c[9]), Sub: len(strings.Fields(c[10])),
			Dai: c[12] != "", // dai-live marks it with its data directory
			S:   atoi(c[1]), W: atoi(c[2]), P: atoi(c[3]),
		})
	}
	sort.Slice(rows, func(i, j int) bool { // stable by loc — never reorders under the cursor
		a, b := rows[i], rows[j]
		if a.S != b.S {
			return a.S < b.S
		}
		if a.W != b.W {
			return a.W < b.W
		}
		return a.P < b.P
	})
	return rows, poll
}
