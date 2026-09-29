// erma — slim status sidebar of claude tmux sessions (Bubble Tea).
//
// Each session is a TWO-LINE block — name on
// top, time below — with a fixed 5-col meter repeated on both lines and the
// right edge reserved for a single glyph: the ▸ arrow of the CURRENT session.
//
// Color semantics — inverted from the usual convention: color means urgency,
// nothing else, so the many idle sessions stay quiet and the one that needs you
// is the only loud thing on screen.
//
//	red   = needs you (permission)  · spine + ● + tint + bold + arrival pulse
//	green = finished, unread        · spine + ✓ + tint, until you look at it
//	amber = processing (moving)     · spine + braille spinner
//	gray  = idle (the many)         · no spine + static ○
//
// violet sits beside the status and never replaces it: ◔N% = context about to be
// auto-compacted (published by contrib/erma-ctx only while it is low).
//
// cyan means only "you": selection band ▐, current-session arrow ▸, focused count.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// immune palette (identical focused/unfocused)
var (
	cUrgent   = lipgloss.Color("#e05b57")
	cDone     = lipgloss.Color("#63c07a")
	cAmber    = lipgloss.Color("#e8a33d")
	cCtx      = lipgloss.Color("#b392f0")
	cCtxRest  = lipgloss.Color("#7e8c8c") // the meter at rest: neutral, and cool like the rest of the type
	bgUrgent  = lipgloss.Color("#2b1e1e")
	bgBoth    = lipgloss.Color("#3a2a2a")
	bgDone    = lipgloss.Color("#1d2721")
	bgDoneSel = lipgloss.Color("#293830")
	lblUrgent = lipgloss.Color("#f2dedd")
	lblDone   = lipgloss.Color("#cfe8d5")
	lblAmber  = lipgloss.Color("#e8c58a")
	metaUrg   = lipgloss.Color("#a97b79")
	metaDone  = lipgloss.Color("#7d9c88")
	metaAmber = lipgloss.Color("#8a7550")
	metaSel   = lipgloss.Color("#8a9a9a")
	badgeFg   = lipgloss.Color("#16191a") // dark text on the (status-colored) badge
	badgeIdle = lipgloss.Color("#93a3a3") // idle badge bg: light, like the item font
)

// badgeBgFor: the window-# badge takes the item's status color (red/amber), or a
// light gray for idle — same language as the bullet.
func badgeBgFor(st state) lipgloss.Color {
	switch st {
	case urgent:
		return cUrgent
	case done:
		return cDone
	case proc:
		return cAmber
	default:
		return badgeIdle
	}
}

// theme = the parts that recede when the bar is unfocused (1c/1b).
type theme struct {
	panel, rule, white, calm, meta, spin, focus, bgSel lipgloss.Color
}

// one bright theme regardless of focus — losing focus no longer dims the panel
// (that read as strange). Focus is signaled only by the cyan header bg.
func themeFor(_ bool) theme {
	// panel #1f1f1f = the default terminal/foot/tmux bg, so the bar matches the
	// rest of the screen. bgSel = a light, cyan-tinted step for the selected item.
	return theme{"#1f1f1f", "#3a3a3a", "#dceded", "#93a3a3", "#4a4a4a", "#5a5a5a", "#2dccd3", "#2c3434"}
}

var borderGray = lipgloss.Color("#3a3a3a")

// appID: the sway app_id the launcher tags the bar's window with. Used to tell
// whether the bar itself is the focused sway window (see detectFocus).
const appID = "erma"

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧"}

// 👾 as a pixel bitmap (space-invader), two frames: arms down (unfocused) and
// arms up (focused — the little guy raises its hands when the bar lights up).
var invaderDown = []string{
	"..X.....X..",
	"...X...X...",
	"..XXXXXXX..",
	".XX.XXX.XX.",
	"XXXXXXXXXXX",
	"X.XXXXXXX.X",
	"X.X.....X.X",
	"...XX.XX...",
}
var invaderUp = []string{
	"..X.....X..",
	"X..X...X..X",
	"X.XXXXXXX.X",
	"XXX.XXX.XXX",
	"XXXXXXXXXXX",
	".XXXXXXXXX.",
	"...X...X...",
	"..X.....X..",
}

type state int

const (
	idle   state = iota // gray, static ○ — nothing to see (the many)
	done                // green, ✓ — finished, and you have not looked yet
	proc                // amber, spinner — processing
	urgent              // red, ● — needs you
)

// A "perm" mark is dropped only on repeated evidence. The notification fires as
// the prompt is drawn and can win the race against the draw, and a single blind
// read would flip the item green and back — the one direction worth being slow
// about, since a missed red is a session waiting on you forever.
const (
	permGrace  = 2 // seconds of unconditional belief in a fresh mark
	permMisses = 2 // consecutive reads with no prompt before it is dropped
)

type sess struct {
	pane, sessWin, name string
	mark                string // the pane option, verbatim — "unread" resists auto-read
	st                  state
	since               int64
	ctx                 string // @erma_ctx_used: % of the room to auto-compact already spent
	s, w, p             int
}

type model struct {
	rows     []sess
	sel      int
	w, h     int
	frame    int
	reload   bool
	focused  bool
	termFoc  bool // the terminal running tmux (not the bar) holds the keyboard
	current  string
	pulse    map[string]int
	wasUrg   map[string]bool
	permMiss map[string]int // consecutive polls a "perm" mark went uncorroborated
	top      bool           // lay out top-down (the --top flag) instead of bottom-up
	comp     bool           // compact (geometry-based, with 1-item hysteresis)
	sound    bool           // audible alerts on (mirrors tmux global @erma_sound)
	sway     *swayWatch     // the compositor feed: who has focus, are we on screen
	hidden   bool           // the bar's window is on a workspace nobody is showing
	anim     bool           // the animation tick is running (see moving)
	icache   map[itemKey]string
	pcache   map[string]string
}

// ---------- render cache ----------
//
// lipgloss re-resolves a colour every time it applies one: "#e05b57" goes
// through fmt.Sscanf on every single Render. A frame applies a few hundred
// styles, which put 80% of the bar's CPU in View — to redraw a picture where
// one spinner cell moved. Both caches key on exactly the inputs their renderer
// reads, so a hit is the same bytes the miss would have produced.

// itemKey is every input renderItem reads. glyph, meta and age are pre-resolved
// because they are what fold m.frame, m.pulse and the clock into a row.
type itemKey struct {
	pane, name, glyph, meta, age, ctx string
	glyphCol                          lipgloss.Color
	st                                state
	win, rowW, nameW                  int
	sel, cur, comp                    bool
}

// cacheCap: the age text ticks every second, so keys churn. Dropping the whole
// map beats keeping an eviction order for something this small.
const cacheCap = 512

// itemLines is renderItem, memoised. A nil cache renders straight through, so a
// model built without one (the tests) still behaves.
func (m model) itemLines(it sess, idx int, th theme) string {
	if m.icache == nil {
		return m.renderItem(it, idx, th)
	}
	g, gc := m.glyphRune(it, th)
	k := itemKey{
		pane: it.pane, name: it.name, glyph: g, glyphCol: gc,
		meta: metaLine(it), age: fmtAge(it.since), ctx: it.ctx, st: it.st,
		win: it.w, rowW: m.rowW(), nameW: m.nameW(),
		sel: idx == m.sel, cur: it.pane == m.current, comp: m.comp,
	}
	if s, ok := m.icache[k]; ok {
		return s
	}
	s := m.renderItem(it, idx, th)
	if len(m.icache) >= cacheCap {
		clear(m.icache)
	}
	m.icache[k] = s
	return s
}

// panelLines is panel(), one line at a time so the unchanged ones come from the
// map. Byte-identical to styling the whole block — TestPanelPerLineMatchesBlock
// pins that. The cache is keyed by line content alone, so a width change has to
// drop it (see tea.WindowSizeMsg).
func (m model) panelLines(th theme, w, h int, lines []string) string {
	st := lipgloss.NewStyle().Background(th.panel).
		Border(lipgloss.NormalBorder(), false, true, false, false).
		BorderForeground(borderGray).BorderBackground(th.panel)
	if w > 1 {
		st = st.Width(w - 1)
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		s, ok := m.pcache[l]
		if !ok {
			s = st.Render(l)
			if len(m.pcache) >= cacheCap {
				clear(m.pcache)
			}
			m.pcache[l] = s
		}
		out[i] = s
	}
	return strings.Join(out, "\n")
}

type animMsg time.Time
type dataMsg time.Time

func animCmd() tea.Cmd {
	return tea.Tick(140*time.Millisecond, func(t time.Time) tea.Msg { return animMsg(t) })
}

// dataCmd polls tmux. Hidden — the bar's window is parked on a workspace no
// output is showing — it polls at a third of the rate: the list still has to be
// right when the workspace comes back, just not right to the half-second.
func dataCmd(hidden bool) tea.Cmd {
	d := 600 * time.Millisecond
	if hidden {
		d = 2 * time.Second
	}
	return tea.Tick(d, func(t time.Time) tea.Msg { return dataMsg(t) })
}

// ---------- tmux / sway glue ----------

func tmuxOut(a ...string) string   { out, _ := exec.Command("tmux", a...).Output(); return string(out) }
func run(name string, a ...string) { _ = exec.Command(name, a...).Run() }

// screen: what a pane is showing right now. The hooks say what happened; only
// the screen says what is happening — the terminal title used to stand in for
// this and cannot: Claude Code writes it once per turn and leaves it there, so
// a working pane and a finished one carry the same glyph.
type screen struct {
	busy   bool // the status line is ticking: the agent is working
	prompt bool // a choice is up: a permission prompt, or a question
}

// busyLine matches Claude Code's ticking status line — "✢ Reticulating… (15m
// 52s · ↓ 45.1k tokens)". The elapsed counter is the part that only exists
// while it works, and it survives the verb list and the glyph set changing.
var busyLine = regexp.MustCompile(`…\s*\(\d+[smh]`)

// optLine matches one entry of a choice list — "❯ 1. Yes", "  2. No" — after the
// box border is trimmed off. One alone is not a prompt: the input box is also a
// ❯ and you may well have typed "2. and check the other one" into it.
var optLine = regexp.MustCompile(`^(❯\s+)?[1-9]\.\s`)

// capSep marks a block boundary in the batched capture. Printable on purpose:
// display-message escapes control bytes into their \ooo spelling.
const capSep = "~~erma-block~~"

// tmuxPoll is one round-trip's worth of tmux: what every pane is showing, which
// clients are attached, and the shared sound flag.
type tmuxPoll struct {
	screens map[string]screen
	clients []int  // client pids, for the focus rule
	current string // the pane the first attached client is showing
	sound   bool
}

// pollPanes reads every pane in ONE tmux round-trip: a `display-message` marks
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
func pollPanes(panes []string) tmuxPoll {
	args := []string{
		"display-message", "-p", capSep, ";",
		"list-clients", "-F", "#{client_pid}\t#{pane_id}", ";",
		"display-message", "-p", capSep, ";",
		"show", "-gv", "@erma_sound",
	}
	for _, p := range panes {
		args = append(args, ";", "display-message", "-p", "-t", p, capSep,
			";", "capture-pane", "-p", "-t", p)
	}
	blocks := strings.Split(tmuxOut(args...), capSep+"\n")
	var poll tmuxPoll
	if len(blocks) < 3 {
		return poll // tmux gave us nothing usable
	}
	for _, ln := range strings.Split(blocks[1], "\n") {
		f := strings.Split(strings.TrimSpace(ln), "\t")
		if len(f) != 2 || f[0] == "" {
			continue
		}
		poll.clients = append(poll.clients, atoi(f[0]))
		if poll.current == "" {
			poll.current = f[1]
		}
	}
	poll.sound = strings.TrimSpace(blocks[2]) == "on"
	if len(blocks) > len(panes)+3 {
		return poll // a pane is showing our marker: bail rather than misalign
	}
	poll.screens = map[string]screen{}
	for i, blk := range blocks[3:] {
		if i < len(panes) && strings.TrimSpace(blk) != "" {
			poll.screens[panes[i]] = readScreen(blk)
		}
	}
	return poll
}

// readScreen: is this pane working, and is a prompt up? Claude Code redraws the
// visible screen every turn — a finished tool call replaces its own prompt box,
// and the ticking status line replaces itself with a "Churned for 15s" summary —
// so a marker still on screen means it is still live.
func readScreen(body string) screen {
	var sc screen
	opts, cursored := 0, false
	for _, ln := range strings.Split(body, "\n") {
		switch {
		case busyLine.MatchString(ln):
			sc.busy = true
		// The question is phrased per tool ("Do you want to create X?", "…to
		// proceed?"), so only the opening is worth matching. "Esc to cancel" is
		// the footer every one of these boxes carries.
		case strings.Contains(ln, "Do you want to "),
			strings.Contains(ln, "Choose an option:"),
			strings.Contains(ln, "Esc to cancel"):
			sc.prompt = true
		}
		if m := optLine.FindStringSubmatch(strings.TrimLeft(ln, " │")); m != nil {
			opts++
			cursored = cursored || m[1] != ""
		}
	}
	// A list you are choosing from: several entries, one of them under the
	// cursor. A numbered list Claude merely wrote out has no cursor.
	sc.prompt = sc.prompt || (cursored && opts > 1)
	return sc
}

// statusOf folds the pane mark (what the hooks last saw), the screen and the
// mark's age. seen is false when the pane could not be read.
//
// A mark is set by one hook and cleared by another, so a turn that ends without
// a Stop hook — you esc out of a prompt, you decline a question — used to strand
// the item on red or amber for good. The screen is what breaks that.
//
// Anything that stopped and has not been looked at is `done`, not idle: "waiting"
// means read, and only the bar writes it, when you look. "unread" is the same
// green, put there by hand — see markUnread.
func statusOf(wait string, sc screen, seen bool) state {
	switch {
	case sc.busy:
		return proc
	case wait == "perm":
		return urgent // gather has already dropped the marks it disbelieves
	case wait == "working" && !seen:
		return proc
	case wait == "working", wait == "done", wait == "unread":
		return done
	}
	return idle
}

// permAlive: is a "perm" mark still worth believing? misses counts the
// consecutive polls that read the pane and found no prompt on it.
func permAlive(sc screen, seen bool, age int64, misses int) bool {
	if !seen || sc.prompt {
		return true
	}
	return age <= permGrace || misses < permMisses
}

// markRead clears the unread green. It goes to the pane option so it outlives a
// restart of the bar and so the hook sees it on its next transition.
func markRead(pane string) {
	run("tmux", "set", "-p", "-t", pane, "@erma_wait", "waiting")
}

// markUnread puts the green back by hand — you read it, and you want it to keep
// asking. It is a mark of its own rather than a plain "done" because the pane
// you are staring at while you press u would be auto-read again on the next
// poll; only a deliberate jump (or the session doing something new) clears it.
func markUnread(pane string) {
	run("tmux", "set", "-p", "-t", pane, "@erma_wait", "unread")
}

func cleanName(title, path string) string {
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

func gather(permMiss map[string]int) ([]sess, tmuxPoll) {
	f := strings.Join([]string{
		"#{pane_id}", "#{session_name}", "#{window_index}", "#{pane_index}",
		"#{@erma_wait}", "#{@erma_wait_since}", "#{pane_current_path}", "#{pane_title}",
		"#{@ctx_label}", "#{@erma_ctx_used}",
	}, "\t")
	out := tmuxOut("list-panes", "-a", "-f", "#{==:#{pane_current_command},claude}", "-F", f)
	var recs [][]string
	var panes []string
	for _, line := range strings.Split(out, "\n") {
		c := strings.Split(line, "\t")
		if len(c) < 10 || c[0] == "" {
			continue
		}
		recs, panes = append(recs, c), append(panes, c[0])
	}
	poll := pollPanes(panes)
	screens := poll.screens
	now := time.Now().Unix()
	var rows []sess
	for _, c := range recs {
		wait, since := c[4], atoi64(c[5])
		sc, seen := screens[c[0]]
		if wait == "perm" && seen && !sc.prompt {
			permMiss[c[0]]++ // only a read that found no prompt counts against it
		} else {
			delete(permMiss, c[0])
		}
		if wait == "perm" && !permAlive(sc, seen, now-since, permMiss[c[0]]) {
			wait = "done"
		}
		st := statusOf(wait, sc, seen)
		// Write the conclusion back, so the hook's next transition compares
		// against the state we are actually showing — and does not ping for a
		// "finished" it thinks is new.
		if seen && st == done && c[4] != "done" && c[4] != "unread" {
			run("tmux", "set", "-p", "-t", c[0], "@erma_wait", "done")
		}
		// @ctx_label, when something outside sets it, is the same short label the
		// tmux tab shows — the bar and the tab should not disagree about a session.
		name := strings.TrimSpace(c[8])
		if name == "" {
			name = cleanName(c[7], c[6])
		}
		rows = append(rows, sess{
			pane: c[0], sessWin: c[1] + ":" + c[2], name: name,
			mark: wait, st: st, since: since, ctx: strings.TrimSpace(c[9]),
			s: atoi(c[1]), w: atoi(c[2]), p: atoi(c[3]),
		})
	}
	sort.Slice(rows, func(i, j int) bool { // stable by loc — never reorders under the cursor
		a, b := rows[i], rows[j]
		if a.s != b.s {
			return a.s < b.s
		}
		if a.w != b.w {
			return a.w < b.w
		}
		return a.p < b.p
	})
	return rows, poll
}

func switchView(s sess) {
	own := ""
	if os.Getenv("TMUX") != "" {
		own = strings.TrimSpace(tmuxOut("display-message", "-p", "#{client_name}"))
	}
	for _, c := range strings.Split(tmuxOut("list-clients", "-F", "#{client_name}"), "\n") {
		c = strings.TrimSpace(c)
		if c != "" && c != own {
			run("tmux", "switch-client", "-c", c, "-t", s.sessWin)
		}
	}
	run("tmux", "select-window", "-t", s.sessWin)
	run("tmux", "select-pane", "-t", s.pane)
}

func swayFocusMain() {
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		rt = "/tmp"
	}
	if b, err := os.ReadFile(filepath.Join(rt, "erma-last-term")); err == nil {
		if cid := strings.TrimSpace(string(b)); cid != "" {
			run("swaymsg", fmt.Sprintf("[con_id=%s] focus", cid))
		}
	}
}

// ---------- sway watch ----------

// swayWatch holds the two compositor facts the bar needs: who has the keyboard,
// and whether the bar's own window is on a workspace anyone is showing.
//
// Both used to come from a `swaymsg -t get_tree` on every poll — 58 KB of JSON
// twice a second, serialized by sway and parsed here, to learn something that
// changes a few times an hour. A subscription pays one read per actual change.
// The tree poll survives only as the fallback for when the feed is down.
type swayWatch struct {
	mu      sync.Mutex
	live    bool              // the subscription is up
	known   bool              // sway has named a focused window at least once
	app     string            // app_id of the focused window
	pid     int               // and its pid
	barWS   string            // workspace holding the bar's window ("" = unknown)
	visible map[string]string // output -> the workspace it is showing
}

func newSwayWatch() *swayWatch {
	w := &swayWatch{visible: map[string]string{}}
	w.seed()
	go w.run()
	return w
}

// focus reports the cached state. live is false when the feed has not told us
// anything yet, and the caller falls back to the tree.
func (w *swayWatch) focus() (app string, pid int, live bool) {
	if w == nil {
		return "", 0, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.app, w.pid, w.live && w.known
}

// isHidden: the bar's window sits on a workspace no output is showing. Not
// knowing answers false — the bar slows down on evidence, never on a guess.
func (w *swayWatch) isHidden() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.barWS == "" || len(w.visible) == 0 {
		return false
	}
	for _, ws := range w.visible {
		if ws == w.barWS {
			return false
		}
	}
	return true
}

// seed establishes what the event stream can only update: who is focused right
// now, where the bar's window lives, and which workspace each output shows.
func (w *swayWatch) seed() {
	if app, pid, ok := focusFromTree(); ok {
		w.mu.Lock()
		w.app, w.pid, w.known = app, pid, true
		w.mu.Unlock()
	}
	if ws, ok := barWorkspace(); ok {
		w.mu.Lock()
		w.barWS = ws
		w.mu.Unlock()
	}
	// get_workspaces is the only place sway reports `visible`: the workspace
	// nodes inside get_tree leave the field null.
	out, err := exec.Command("swaymsg", "-t", "get_workspaces").Output()
	if err != nil {
		return
	}
	var wss []struct {
		Name, Output string
		Visible      bool
	}
	if json.Unmarshal(out, &wss) != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ws := range wss {
		if ws.Visible {
			w.visible[ws.Output] = ws.Name
		}
	}
}

// run streams sway's event feed; `swaymsg -m` writes one JSON object per line.
// sway restarting or the socket going away is not fatal — the tree fallback
// covers the gap and the next attempt picks the feed back up.
func (w *swayWatch) run() {
	for {
		cmd := exec.Command("swaymsg", "-t", "subscribe", "-m", `["window","workspace"]`)
		if out, err := cmd.StdoutPipe(); err == nil && cmd.Start() == nil {
			w.setLive(true)
			sc := bufio.NewScanner(out)
			sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
			for sc.Scan() {
				w.apply(sc.Bytes())
			}
			_ = cmd.Wait()
		}
		w.setLive(false)
		time.Sleep(2 * time.Second)
		w.seed()
	}
}

func (w *swayWatch) setLive(v bool) {
	w.mu.Lock()
	w.live = v
	w.mu.Unlock()
}

// apply folds one event in. Window and workspace events share the change name
// "focus" and carry no type of their own; the payload tells them apart — a
// workspace event has `current`, a window event has `container`.
//
// Focusing an EMPTY workspace produces no window event, so the cached focus goes
// stale until something is focused again. That is the one case the tree poll
// used to get right, and it costs a wrong highlight until the next focus.
func (w *swayWatch) apply(line []byte) {
	var ev struct {
		Change    string `json:"change"`
		Container struct {
			AppID string  `json:"app_id"`
			PID   float64 `json:"pid"`
		} `json:"container"`
		Current struct {
			Name   string `json:"name"`
			Output string `json:"output"`
		} `json:"current"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case ev.Current.Name != "":
		if ev.Change == "focus" && ev.Current.Output != "" {
			w.visible[ev.Current.Output] = ev.Current.Name
		}
	case ev.Change == "focus":
		w.app, w.pid, w.known = ev.Container.AppID, int(ev.Container.PID), true
	case ev.Change == "move" && ev.Container.AppID == appID:
		// the bar moved workspace and the event does not say which; stop
		// claiming to know rather than pay a tree read to find out.
		w.barWS = ""
	}
}

// walkTree runs fn over every node of the sway tree, depth first.
func walkTree(n map[string]any, fn func(map[string]any) bool) bool {
	if fn(n) {
		return true
	}
	for _, key := range []string{"nodes", "floating_nodes"} {
		if kids, ok := n[key].([]any); ok {
			for _, k := range kids {
				if km, ok := k.(map[string]any); ok && walkTree(km, fn) {
					return true
				}
			}
		}
	}
	return false
}

func swayTree() (map[string]any, bool) {
	out, err := exec.Command("swaymsg", "-t", "get_tree").Output()
	if err != nil {
		return nil, false
	}
	var tree map[string]any
	if json.Unmarshal(out, &tree) != nil {
		return nil, false
	}
	return tree, true
}

// focusFromTree: the fallback for a feed that is not up.
func focusFromTree() (app string, pid int, ok bool) {
	tree, got := swayTree()
	if !got {
		return "", 0, false
	}
	walkTree(tree, func(n map[string]any) bool {
		if f, _ := n["focused"].(bool); !f {
			return false
		}
		app, _ = n["app_id"].(string)
		p, _ := n["pid"].(float64)
		pid, ok = int(p), true
		return true
	})
	return app, pid, ok
}

// barWorkspace: which workspace holds the bar's own window.
func barWorkspace() (string, bool) {
	tree, got := swayTree()
	if !got {
		return "", false
	}
	var ws, found string
	var ok bool
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		if ok {
			return
		}
		if t, _ := n["type"].(string); t == "workspace" {
			ws, _ = n["name"].(string)
		}
		if a, _ := n["app_id"].(string); a == appID {
			found, ok = ws, true
			return
		}
		for _, key := range []string{"nodes", "floating_nodes"} {
			if kids, has := n[key].([]any); has {
				for _, k := range kids {
					if km, is := k.(map[string]any); is {
						walk(km)
					}
				}
			}
		}
	}
	walk(tree)
	return found, ok
}

// detectFocus: who holds the keyboard — the bar, or the terminal running a tmux
// client? The second half is what separates "you are looking at this session"
// from "it merely is the active tmux pane while you read your mail".
//
// clients are the tmux client pids the poll already brought back; walking up
// from them used to cost a fork of its own.
func detectFocus(w *swayWatch, clients []int) (bar, term bool) {
	app, pid, live := w.focus()
	if !live {
		var ok bool
		if app, pid, ok = focusFromTree(); !ok {
			return true, false
		}
	}
	if app == appID {
		return true, false
	}
	return false, hostsTmuxClient(pid, clients)
}

// hostsTmuxClient: does the window with this pid hold a tmux client? The client
// is a grandchild of the terminal (terminal → shell → tmux), so walk up from
// every client and see if one lands on the window.
func hostsTmuxClient(win int, clients []int) bool {
	if win <= 1 {
		return false
	}
	for _, pid := range clients {
		for i := 0; pid > 1 && i < 12; i++ {
			if pid == win {
				return true
			}
			pid = ppidOf(pid)
		}
	}
	return false
}

// ppidOf reads /proc/<pid>/stat. The command name sits in parens and can hold
// anything, spaces included, so fields are counted from the last ')'.
func ppidOf(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0
	}
	if f := strings.Fields(string(b)[i+1:]); len(f) >= 2 {
		return atoi(f[1])
	}
	return 0
}

// ---------- text helpers ----------

func fmtAge(since int64) string { // <= 4 cells, one unit
	if since == 0 {
		return ""
	}
	d := time.Now().Unix() - since
	if d < 0 {
		d = 0
	}
	switch {
	case d < 60:
		return fmt.Sprintf("%ds", d)
	case d < 3600:
		return fmt.Sprintf("%dm", d/60)
	case d < 36000:
		return fmt.Sprintf("%dh%02d", d/3600, (d%3600)/60)
	default:
		return fmt.Sprintf("%dh", d/3600)
	}
}

func midTruncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	head := (max) / 2
	tail := max - 1 - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

func wrapN(s string, width, n int) []string {
	var lines []string
	cur := ""
	for _, wd := range strings.Fields(s) {
		cand := wd
		if cur != "" {
			cand = cur + " " + wd
		}
		if utf8.RuneCountInString(cand) <= width {
			cur = cand
			continue
		}
		lines = append(lines, cur)
		cur = wd
		if len(lines) == n {
			break
		}
	}
	if len(lines) < n && cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	if len(lines) > n {
		lines = lines[:n]
	}
	return lines
}

func fg(c lipgloss.Color, s string) string { return lipgloss.NewStyle().Foreground(c).Render(s) }

func padR(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s + strings.Repeat(" ", n-len(r))
}
func padL(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[len(r)-n:])
	}
	return strings.Repeat(" ", n-len(r)) + s
}

// ---------- bubbletea ----------

// chromeRows: the foot block that brackets the list — margin, divider, key
// legend, header rule, counters, margin. The same six rows either way up; they
// just sit at the other end.
const chromeRows = 6

// mascotPad: blank rows between the mascot and the panel edge it hangs from.
const mascotPad = 2

func (m model) Init() tea.Cmd { return tea.Batch(animCmd(), dataCmd(m.hidden)) }

// moving: is anything on screen actually animating — a spinner turning, or an
// arrival pulse counting down? When nothing is, the bar is a still image and the
// 140ms tick only rewrote it: ~19 KB a frame, seven times a second, forever. A
// hidden bar animates for nobody, so it does not animate at all.
func (m model) moving() bool {
	if m.hidden {
		return false
	}
	for _, n := range m.pulse {
		if n > 0 {
			return true
		}
	}
	for _, r := range m.rows {
		if r.st == proc {
			return true
		}
	}
	return false
}

// toggleSound flips the shared flag; the hook reads it on the next perm/waiting.
func (m *model) toggleSound() {
	m.sound = !m.sound
	v := "off"
	if m.sound {
		v = "on"
	}
	run("tmux", "set", "-g", "@erma_sound", v)
}

func (m *model) refresh() {
	if m.permMiss == nil {
		m.permMiss = map[string]int{}
	}
	var poll tmuxPoll
	m.rows, poll = gather(m.permMiss)
	m.focused, m.termFoc = detectFocus(m.sway, poll.clients)
	m.current = poll.current
	m.sound = poll.sound
	m.hidden = m.sway.isHidden()
	// Looking at the pane is what clears the unread green: the terminal holds
	// the keyboard and tmux is showing that pane, so the answer is on screen.
	if m.termFoc && m.current != "" {
		for i := range m.rows {
			if m.rows[i].pane == m.current && m.rows[i].st == done &&
				m.rows[i].mark != "unread" {
				markRead(m.rows[i].pane)
				m.rows[i].st = idle
			}
		}
	}
	if m.pulse == nil {
		m.pulse = map[string]int{}
	}
	if m.wasUrg == nil {
		m.wasUrg = map[string]bool{}
	}
	live := map[string]bool{}
	for _, r := range m.rows {
		live[r.pane] = true
		if r.st == urgent && !m.wasUrg[r.pane] {
			m.pulse[r.pane] = 14
		}
		m.wasUrg[r.pane] = r.st == urgent
	}
	for p := range m.wasUrg {
		if !live[p] {
			delete(m.wasUrg, p)
			delete(m.pulse, p)
			delete(m.permMiss, p)
		}
	}
	if m.sel >= len(m.rows) {
		m.sel = max(0, len(m.rows)-1)
	}
	// selection follows the actually-current pane, so switching panes in tmux
	// moves the cursor too — coming back to the bar it never "jumps" to a stale item.
	if m.current != "" {
		for i, r := range m.rows {
			if r.pane == m.current {
				m.sel = i
				break
			}
		}
	}
	m.recomputeDensity()
}

// density switches by available geometry, with 1-item hysteresis.
func (m *model) recomputeDensity() {
	avail := m.h - chromeRows - 1
	if avail < 1 || len(m.rows) == 0 {
		return
	}
	tall := 0
	for _, it := range m.rows {
		tall += m.itemHeight(it, false) // an item's padding is inside it; no gap
	}
	if !m.comp && tall > avail {
		m.comp = true
	} else if m.comp && tall+2 <= avail { // hysteresis: need 1 item of slack to relax
		m.comp = false
	}
}

func (m model) cur() (sess, bool) {
	if m.sel >= 0 && m.sel < len(m.rows) {
		return m.rows[m.sel], true
	}
	return sess{}, false
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case animMsg:
		m.frame++
		for p, n := range m.pulse {
			if n > 0 {
				m.pulse[p] = n - 1
			}
		}
		if !m.moving() {
			m.anim = false // gone still; the next dataMsg starts the tick again
			return m, nil
		}
		return m, animCmd()
	case dataMsg:
		m.refresh()
		if !m.anim && m.moving() {
			m.anim = true
			return m, tea.Batch(dataCmd(m.hidden), animCmd())
		}
		return m, dataCmd(m.hidden)
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.recomputeDensity()
		clear(m.pcache) // keyed by line content, which says nothing about width
		return m, nil
	case tea.MouseMsg:
		if msg.Action != tea.MouseActionPress {
			return m, nil
		}
		switch msg.Button {
		case tea.MouseButtonLeft:
			if msg.Y == m.soundRow() && msg.X >= m.rowW()-5 {
				m.toggleSound() // sound badge lives at the counter row's right edge
			} else if idx := m.hitTest(msg.Y); idx >= 0 {
				m.sel = idx
				m.jump()
			}
		case tea.MouseButtonRight:
			if idx := m.hitTest(msg.Y); idx >= 0 {
				m.sel = idx
				m.unread()
			}
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r", "R":
			m.reload = true
			return m, tea.Quit
		case "s", "S":
			m.toggleSound()
		case "u", "U":
			m.unread()
		case "up", "k":
			if m.sel > 0 {
				m.sel--
			}
			if s, ok := m.cur(); ok {
				switchView(s)
			}
		case "down", "j":
			if m.sel < len(m.rows)-1 {
				m.sel++
			}
			if s, ok := m.cur(); ok {
				switchView(s)
			}
		case "enter":
			m.jump()
		default:
			// alt+<num> jumps straight to the tmux window with that index and
			// focuses it — same as clicking/Enter on it.
			if s := msg.String(); len(s) == 5 && strings.HasPrefix(s, "alt+") &&
				s[4] >= '0' && s[4] <= '9' {
				n := int(s[4] - '0')
				for idx, it := range m.rows {
					if it.w == n {
						m.sel = idx
						m.jump()
						break
					}
				}
			}
		}
	}
	return m, nil
}

// unread: put the green back on the selected session. Right-click does the same
// to whatever is under the pointer.
func (m *model) unread() {
	if s, ok := m.cur(); ok {
		markUnread(s.pane)
		m.rows[m.sel].mark, m.rows[m.sel].st = "unread", done
	}
}

func (m *model) jump() {
	if s, ok := m.cur(); ok {
		switchView(s)
		swayFocusMain()
		m.current = s.pane
		markRead(s.pane) // jumping is reading — don't wait for the next poll
		if m.rows[m.sel].st == done {
			m.rows[m.sel].st = idle
		}
	}
}

// ---------- layout math ----------

func (m model) rowW() int {
	if m.w > 12 {
		return m.w - 1 // reserve 1 col for the right border
	}
	return 27
}
func (m model) nameW() int {
	if w := m.rowW() - 5 - 5; w > 8 { // 5-col gutter + 5-col right (badge/arrow+margin)
		return w
	}
	return 8
}

func (m model) nameLines(it sess) []string {
	nw := m.nameW()
	disp := midTruncate(strings.ToLower(it.name), nw*2)
	return wrapN(disp, nw, 2)
}

// hasMeta: does this item have a bottom (time/PERM) line? Urgent always does;
// others only when there's a recorded age — no empty line eating space.
func (m model) itemHeight(it sess, comp bool) int {
	if comp {
		return 1
	}
	// names + the second line (tmux window number + time) + top & bottom pad
	h := len(m.nameLines(it)) + 1 + 2
	if ctxUsed(it) >= 0 {
		h++ // the context meter
	}
	return h
}

// listTop: the screen row the first item is drawn on. Bottom-up, the list is
// pinned above the footer chrome, so where it starts depends on how tall it is.
func (m model) listTop() int {
	if m.top {
		return chromeRows
	}
	tall := 0
	for _, it := range m.rows {
		tall += m.itemHeight(it, m.comp)
	}
	return m.h - chromeRows - tall // negative when it overflows: rows scroll off the top
}

// soundRow: the row the counter line is drawn on — where the sound badge sits,
// and the one row a click means something other than picking a session. It
// moved to the foot with the rest of the chrome.
func (m model) soundRow() int {
	if m.top {
		return 1
	}
	return m.h - 2
}

func (m model) hitTest(y int) int {
	row := m.listTop()
	for i, it := range m.rows {
		hh := m.itemHeight(it, m.comp)
		if y >= row && y < row+hh {
			return i
		}
		row += hh // padding lives inside the item now; no separate gap
	}
	return -1
}

// ---------- render ----------

func (m model) spineRune(st state) (string, lipgloss.Color) {
	switch st {
	case urgent:
		return "▌", cUrgent
	case done:
		return "▌", cDone
	case proc:
		return "▌", cAmber
	default:
		return " ", cAmber
	}
}
func (m model) glyphRune(it sess, th theme) (string, lipgloss.Color) {
	switch it.st {
	case urgent:
		g := "●"
		if m.pulse[it.pane] > 0 && (m.frame/2)%2 == 0 {
			g = "◉"
		}
		return g, cUrgent
	case done:
		return "✓", cDone
	case proc:
		return spinnerFrames[m.frame%len(spinnerFrames)], cAmber
	default:
		return "○", th.spin
	}
}
func nameColor(it sess, sel bool, th theme) (lipgloss.Color, bool) {
	if sel {
		return th.white, it.st == urgent
	}
	switch it.st {
	case urgent:
		return lblUrgent, true
	case done:
		return lblDone, false
	case proc:
		return lblAmber, false
	default:
		return th.calm, false
	}
}
func metaColorOf(it sess, sel bool, th theme) lipgloss.Color {
	if sel {
		return metaSel
	}
	switch it.st {
	case urgent:
		return metaUrg
	case done:
		return metaDone
	case proc:
		return metaAmber
	default:
		return th.meta
	}
}
func metaLine(it sess) string {
	age, tag := fmtAge(it.since), ""
	switch it.st {
	case urgent:
		tag = "PERM"
	case done:
		tag = "DONE"
	}
	switch {
	case tag == "":
		return age
	case age == "":
		return tag
	}
	return tag + " · " + age
}

// Everything downstream of erma-ctx counts the same direction: how much of the
// room to auto-compact is spent. ctxUsedRed mirrors ERMA_CTX_WARN there — past
// it the figure is worth reading, and not just the meter.
const (
	ctxUsedAmber = 70
	ctxUsedRed   = 85
)

// ctxTag: the low-context mark, or "" while there is room. <= 4 cells, so it fits
// the compact row's age slot — which is the only place it still appears, since a
// full row draws the meter instead.
func ctxTag(it sess) string {
	if used := ctxUsed(it); used < ctxUsedRed {
		return ""
	}
	return "◔" + it.ctx + "%"
}

// ctxUsed: -1 when the pane has published nothing yet.
func ctxUsed(it sess) int {
	n, err := strconv.Atoi(it.ctx)
	if it.ctx == "" || err != nil {
		return -1
	}
	return min(100, max(0, n))
}

// ctxColor: at rest the meter takes the same gray as an idle age line, so it
// sits in the row without asking for anything. It only speaks once the session
// is actually burning through its window. The resting color is the same
// whatever the session's state — this meter reports context, not status, and
// tinting it green on a finished session would say otherwise.
// ctxShade: a row background stepped toward the foreground, keeping its tint.
// The rail and its marks sit on six different row colors, and a fixed gray read
// muddy on the tinted ones. Only these follow the row; the spent part carries a
// meaning of its own and must not change with the background.
func ctxShade(rbg lipgloss.Color, step int) lipgloss.Color {
	var r, g, b int
	if _, err := fmt.Sscanf(string(rbg), "#%02x%02x%02x", &r, &g, &b); err != nil {
		return rbg
	}
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x",
		min(r+step, 0xff), min(g+step, 0xff), min(b+step, 0xff)))
}

func ctxColor(used int, th theme) lipgloss.Color {
	switch {
	case used >= ctxUsedRed:
		return cUrgent
	case used >= ctxUsedAmber:
		return cAmber
	}
	return cCtxRest
}

// The meter sits on the baseline at half the cell height, so the row reads as a
// footnote to the name above it and not as a third line of equal weight. Spent
// is a half-block, the track a thin rule; the quadrant carries the odd half, so
// the meter still moves at ~3% on a row this wide.
const (
	ctxCellFull  = '\u2584'
	ctxCellHalf  = '\u2596'
	ctxCellTrack = '\u2581'
)

// ctxMeter: one rune per cell — what is spent, then the rail behind it. The
// caller paints and slices it, because until the warning the meter runs across
// both columns of the row. The rail is deliberately featureless: marks for the
// thresholds were tried and read as an alarm on a row that should not raise
// its voice before it has something to say.
func ctxMeter(used, cells int) []rune {
	if cells < 1 {
		return nil
	}
	halves := used * cells * 2 / 100
	full := min(halves/2, cells)
	out := make([]rune, 0, cells)
	for range full {
		out = append(out, ctxCellFull)
	}
	if halves%2 == 1 && full < cells {
		out = append(out, ctxCellHalf)
		full++
	}
	for range cells - full {
		out = append(out, ctxCellTrack)
	}
	return out
}

// ctxPaint colors a slice of the meter in two runs, so a row costs two color
// resolutions instead of one per cell.
func ctxPaint(cells []rune, spent, rail lipgloss.Color,
	c func(lipgloss.Color, string, bool) string,
) string {
	i := 0
	for i < len(cells) && cells[i] != ctxCellTrack {
		i++
	}
	return c(spent, string(cells[:i]), false) + c(rail, string(cells[i:]), false)
}

func rowBg(it sess, sel bool, th theme) lipgloss.Color {
	switch {
	case it.st == urgent && sel:
		return bgBoth
	case it.st == urgent:
		return bgUrgent
	case it.st == done && sel:
		return bgDoneSel
	case it.st == done:
		return bgDone
	case sel:
		return th.bgSel
	}
	return th.panel
}

func (m model) renderItem(it sess, idx int, th theme) string {
	sel := idx == m.sel
	nw := m.nameW()
	rbg := rowBg(it, sel, th)
	nc, bold := nameColor(it, sel, th)
	// every cell bakes the row bg, so there are no reset "holes" in the fill.
	c := func(f lipgloss.Color, s string, b bool) string {
		return lipgloss.NewStyle().Foreground(f).Background(rbg).Bold(b).Render(s)
	}
	sp, spc := m.spineRune(it.st)
	spine := c(spc, sp, false)
	band := c(rbg, " ", false)
	if sel {
		band = c(th.focus, "▐", false)
	}
	gg, gc := m.glyphRune(it, th)

	if m.comp { // 1 line: gutter + name + time(right) + arrow
		// gutter 5 + a space + the 4-cell age + the arrow: 11, not 10. Being one
		// over made lipgloss wrap the row onto a second line.
		nwC := max(4, m.rowW()-5-1-4-1)
		nm := midTruncate(strings.ToLower(it.name), nwC)
		age, agec := padL(fmtAge(it.since), 4), metaColorOf(it, sel, th)
		if tag := ctxTag(it); tag != "" { // running out of room beats how long it has sat
			age, agec = padL(tag, 4), cCtx
		}
		arrow := c(rbg, " ", false)
		if it.pane == m.current {
			arrow = c(th.focus, "➤", false)
		}
		line := band + spine + c(rbg, " ", false) + c(gc, gg, false) + c(rbg, " ", false) +
			c(nc, padR(nm, nwC), bold) + c(rbg, " ", false) +
			c(agec, age, false) + arrow
		return lipgloss.NewStyle().Width(m.rowW()).Background(rbg).Render(line)
	}

	names := m.nameLines(it)
	var lines []string
	for li, nm := range names {
		glyph := c(rbg, "  ", false)
		if li == 0 {
			glyph = c(gc, gg, false) + c(rbg, " ", false)
		}
		lines = append(lines, band+spine+c(rbg, " ", false)+glyph+c(nc, padR(nm, nw), bold))
	}
	// second line: the time (gutter cols 3-5 empty). The low-context mark stays a
	// compact-mode thing — here the meter below already carries the same number,
	// and in the other direction.
	lines = append(lines, band+spine+c(rbg, "   ", false)+
		c(metaColorOf(it, sel, th), padR(metaLine(it), nw), false))

	// third line: the context meter. Until the warning it runs the whole row,
	// because a slot held open for a number nobody is reading yet reads as a
	// hole. From there the number takes the tail, on the badge's own grid — see
	// the right-column loop below. Absent until the pane's statusline has
	// published a figure.
	meterRow, meterTail := -1, ""
	if used := ctxUsed(it); used >= 0 {
		cc, rail := ctxColor(used, th), ctxShade(rbg, 0x16)
		cells := nw + 4 // the badge column too, less its 1-col margin
		if used >= ctxUsedAmber {
			cells = nw
		}
		bar := ctxMeter(used, cells)
		meterRow = len(lines)
		lines = append(lines, band+spine+c(rbg, "   ", false)+
			ctxPaint(bar[:min(nw, len(bar))], cc, rail, c))

		if used < ctxUsedAmber {
			meterTail = ctxPaint(bar[nw:], cc, rail, c) + c(rbg, " ", false)
		} else {
			// 100 is the one value that fills the slot, and "100%" would sit flush
			// against a full meter. A full bar labelled 100 is not ambiguous.
			pct := strconv.Itoa(used) + "%"
			if used == 100 {
				pct = "100"
			}
			meterTail = c(cc, padL(pct, 4), false) + c(rbg, " ", false)
		}
	}

	// right column (2 cells): the tmux window # as a dark badge on line 0, and
	// the current-session arrow ▸ just below it (line 1) when applicable.
	bstr := " " + strconv.Itoa(it.w) + " " // symmetric 1-space padding inside the badge
	bw := utf8.RuneCountInString(bstr)
	bbg := badgeBgFor(it.st)
	if sel { // selected = cyan (the "you" color)
		bbg = th.focus
	}
	badge := lipgloss.NewStyle().Background(bbg).Foreground(badgeFg).Render(bstr)
	leadN := 4 - bw // right-align badge; +1 margin -> 5-col slot
	if leadN < 0 {
		leadN = 0
	}
	lead := c(rbg, strings.Repeat(" ", leadN), false)
	badgeCell := lead + badge + c(rbg, " ", false)
	// arrows span the badge width so they read as one aligned block under it
	arrowCell := lead + c(th.focus, strings.Repeat("➤", bw), false) + c(rbg, " ", false)
	for i := range lines {
		var ac string
		switch {
		case i == 0:
			ac = badgeCell
		case i == meterRow:
			ac = meterTail // the rail carries on, or the number takes over
		case i == 1 && it.pane == m.current:
			ac = arrowCell
		default:
			ac = c(rbg, "     ", false)
		}
		lines[i] += ac
	}
	// symmetric top & bottom padding; carry BOTH the band (col1) and the spine
	// (col2) so the two left bars are the same height as the block.
	lines = append([]string{band + spine}, lines...)
	lines = append(lines, band+spine)

	return lipgloss.NewStyle().Width(m.rowW()).Background(rbg).Render(strings.Join(lines, "\n"))
}

func (m model) counts() (u, d, p, i int) {
	for _, it := range m.rows {
		switch it.st {
		case urgent:
			u++
		case done:
			d++
		case proc:
			p++
		default:
			i++
		}
	}
	return
}

// legend: the widest key hint that fits. A row one cell over its width gets
// wrapped by lipgloss, and everything below it shifts.
func (m model) legend() string {
	for _, s := range []string{"↑↓ move  ⏎ go  u unread", "↑↓ ⏎ go  u unread", "⏎ go  u unread", "⏎ · u"} {
		if utf8.RuneCountInString(s)+1 <= m.rowW() {
			return " " + s
		}
	}
	return ""
}

// rule: the thin separator. It divides the key legend from the list, and it is
// what the header's own rule falls back to when the bar is unfocused — focus is
// signalled by that one turning heavy and cyan, and by nothing else.
func (m model) rule() string {
	return " " + fg(lipgloss.Color("#2a3030"), strings.Repeat("─", m.rowW()-2))
}

// focusRule: the header's rule while the bar holds the keyboard — heavy, cyan,
// brightest at the left and fading out across the full width. Lighting a fixed
// 40% and stopping dead read as a progress bar stuck at 40%: an edge that sharp
// promises a meaning, and there was none behind it.
func focusRule(w int) string {
	const from, to = 0x2dccd3, 0x24494b
	var b strings.Builder
	for i := 0; i < w; i++ {
		b.WriteString(fg(lerp(from, to, i, w-1), "━"))
	}
	return b.String()
}

// lerp: step i of n between two packed RGB values.
func lerp(a, b uint32, i, n int) lipgloss.Color {
	if n < 1 {
		n = 1
	}
	ch := func(sh uint) int {
		x, y := int(a>>sh&0xff), int(b>>sh&0xff)
		return x + (y-x)*i/n
	}
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", ch(16), ch(8), ch(0)))
}

// header: 2 lines in both states — a content line and a rule. Focus is carried
// by the rule (heavy cyan ━ vs thin gray ─) + brightness, never a block. The
// caller decides their order: the rule always faces the list.
func (m model) header() (content, rule string) {
	rw := m.rowW()
	live := m.focused && len(m.rows) > 0

	mstyle := lipgloss.NewStyle().Width(3) // absorbs the 👾 1-vs-2-cell ambiguity
	if !live {
		mstyle = mstyle.Faint(true)
	}
	mascot := mstyle.Render(" 👾")

	countCol := lipgloss.Color("#6f7c7c")
	if live {
		countCol = lipgloss.Color("#dceded")
	}
	count := lipgloss.NewStyle().Foreground(countCol).Render(strconv.Itoa(len(m.rows)))

	// left cluster: the breakdown by state (number before glyph) sits right next
	// to the logo + total, split by a faint │ — so the top row tells you *what*
	// the N sessions are, at a glance. Zero categories are omitted.
	u, d, p, i := m.counts()
	var parts []string
	if u > 0 {
		parts = append(parts, fg(cUrgent, fmt.Sprintf("%d●", u)))
	}
	if d > 0 {
		parts = append(parts, fg(cDone, fmt.Sprintf("%d✓", d)))
	}
	if p > 0 {
		parts = append(parts, fg(cAmber, fmt.Sprintf("%d⠹", p)))
	}
	if i > 0 {
		parts = append(parts, fg(lipgloss.Color("#5a5a5a"), fmt.Sprintf("%d○", i)))
	}
	left := mascot + count
	if len(parts) > 0 {
		left += fg(lipgloss.Color("#3f4646"), " │ ") + strings.Join(parts, " ")
	}
	// rightmost element: the sound toggle, rendered like the window-# badges
	// (glyph on a filled chip) so it's easy to see — cyan when on, an opaque red
	// chip with the note struck through when off. It is the same note either
	// way: a ✕ on a red chip read as "close", which is not what it does.
	sBg, sFg := lipgloss.Color("#6e3634"), lipgloss.Color("#eccbca")
	if m.sound {
		sBg, sFg = lipgloss.Color("#2dccd3"), badgeFg
	}
	chip := lipgloss.NewStyle().Background(sBg).Foreground(sFg)
	// only the note carries the strike — a struck chip would risk lining out
	// its padding too, which reads as a dash, not a mute.
	soundBadge := chip.Render(" ") + chip.Strikethrough(!m.sound).Render("♫") + chip.Render(" ")
	gap := rw - lipgloss.Width(left) - lipgloss.Width(soundBadge) - 1
	if gap < 1 {
		gap = 1
	}
	top := left + strings.Repeat(" ", gap) + soundBadge + " "

	base := m.rule()
	if live {
		base = " " + focusRule(rw-2)
	}
	return top, base
}

// View stacks three blocks against an elastic gap the mascot floats on. Which
// end each block sits at is the whole of the layout: bottom-up (the default)
// puts the sessions and the counters down where the eye already rests and
// leaves the mascot the empty top; --top is the mirror of that.
func (m model) View() string {
	th := themeFor(m.focused)
	head, rule := m.header()

	var list []string
	if len(m.rows) == 0 {
		list = []string{" " + fg(th.meta, "no claude"), " " + fg(th.meta, "sessions")}
	} else {
		for idx, it := range m.rows {
			list = append(list, strings.Split(m.itemLines(it, idx, th), "\n")...)
		}
	}
	// The foot: everything that is not a session, in one block against the edge
	// the eye rests on — a margin, the key legend behind its own divider, then
	// the header's rule and counters. With no sessions the legend has nothing
	// to explain and drops out.
	foot := []string{"", rule, head, ""}
	if len(m.rows) > 0 {
		foot = []string{"", m.rule(), fg(th.calm, m.legend()), rule, head, ""}
	}
	// The mascot is decoration and yields whole — a cropped one reads as a
	// glitch, not as a mascot. It needs its padding and a row of gap to earn
	// its place.
	var mascot []string
	if art := m.mascotArt(m.rowW(), m.focused); len(list)+len(foot)+len(art)+mascotPad < m.h {
		pad := make([]string, mascotPad)
		if m.top {
			mascot = append(art, pad...)
		} else {
			mascot = append(pad, art...)
		}
	}

	var lead, trail []string
	if m.top {
		lead = append(reversed(foot), list...)
		trail = mascot
	} else {
		lead = mascot
		trail = append(list, foot...)
	}
	gap := m.h - len(lead) - len(trail)
	if gap < 0 {
		gap = 0
	}
	rows := append(append(lead, make([]string, gap)...), trail...)
	// Too many sessions to fit: the far end scrolls off — the mascot first,
	// then the oldest items. Cutting the whole block from one end (rather than
	// trimming a part) is what keeps listTop's arithmetic true.
	if over := len(rows) - m.h; over > 0 {
		if m.top {
			rows = rows[:m.h]
		} else {
			rows = rows[over:]
		}
	}
	if m.pcache == nil {
		return panel(th, m.w, m.h, strings.Join(rows, "\n"))
	}
	return m.panelLines(th, m.w, m.h, rows)
}

// reversed: the foot block reads the other way round when the layout flips, so
// its margin always faces the panel edge and its rule always faces the list.
func reversed(ss []string) []string {
	out := make([]string, len(ss))
	for i, v := range ss {
		out[len(ss)-1-i] = v
	}
	return out
}

// mascotArt: the 👾 bitmap scaled to ~60% width, centered, in a very faded
// purple — barely there unfocused, a touch more present focused.
func (m model) mascotArt(rw int, focused bool) []string {
	const maxPW = 2         // cap: current size is the max; only shrinks below it
	pw := (rw*6 + 55) / 110 // ~60% width / 11 px, rounded
	if pw > maxPW {
		pw = maxPW
	}
	if pw < 1 {
		pw = 1
	}
	col := lipgloss.Color("#262430") // unfocused: barely above the panel
	if focused {
		col = lipgloss.Color("#38334f") // focused: a bit more, still dim
	}
	artW := 11 * pw
	pad := (rw - artW) / 2
	if pad < 0 {
		pad = 0
	}
	bmp := invaderDown
	if focused {
		bmp = invaderUp // hands up when the bar is focused
	}
	lead := strings.Repeat(" ", pad)
	var out []string
	if pw >= 2 {
		// full size: one █ block per pixel, one row per bitmap row (8 rows)
		for _, row := range bmp {
			var sb strings.Builder
			for _, ch := range row {
				if ch == 'X' {
					sb.WriteString(strings.Repeat("█", pw))
				} else {
					sb.WriteString(strings.Repeat(" ", pw))
				}
			}
			out = append(out, lead+fg(col, sb.String()))
		}
		return out
	}
	// shrunk: half-blocks pack 2 bitmap rows per line -> height halves too,
	// so it scales down keeping the same proportion (not stretched).
	for i := 0; i+1 < len(bmp); i += 2 {
		top, bot := bmp[i], bmp[i+1]
		var sb strings.Builder
		for c := 0; c < len(top); c++ {
			t, b := top[c] == 'X', bot[c] == 'X'
			switch {
			case t && b:
				sb.WriteString("█")
			case t:
				sb.WriteString("▀")
			case b:
				sb.WriteString("▄")
			default:
				sb.WriteString(" ")
			}
		}
		out = append(out, lead+fg(col, sb.String()))
	}
	return out
}

func panel(th theme, w, h int, body string) string {
	st := lipgloss.NewStyle().Background(th.panel).
		Border(lipgloss.NormalBorder(), false, true, false, false). // right edge only
		BorderForeground(borderGray).BorderBackground(th.panel)
	if w > 1 {
		st = st.Width(w - 1) // content + 1-col border = w
	}
	if h > 0 {
		st = st.Height(h)
	}
	return st.Render(body)
}

func main() {
	top := flag.Bool("top", false,
		"lay the bar out top-down: counters and sessions at the top, mascot at the bottom")
	flag.Parse()

	m := model{top: *top, sway: newSwayWatch(),
		icache: map[itemKey]string{}, pcache: map[string]string{}}
	m.refresh()
	m.anim = true // Init starts the tick; the first animMsg decides if it lives
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	fm, err := p.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if fm2, ok := fm.(model); ok && fm2.reload {
		// re-exec the same binary in place (picks up a freshly-installed build)
		bin, _ := filepath.Abs(os.Args[0])
		_ = syscall.Exec(bin, os.Args, os.Environ())
	}
}
