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
//
// Two marks, set from the right-click menu, move a session instead of coloring
// it: paused (‖) folds it to one dim line by the counters, priority (★) puts it
// in the band just past those. They live per Claude session, so they outlive the
// pane.
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/daronco/ermahgherdr/internal/session"
)

// Re-exports from internal/session so this file can keep its lower-case names
// across every rendering call site — the shapes and semantics live in the
// package, these are just aliases.
type (
	state    = session.State
	screen   = session.Screen
	sess     = session.Session
	tmuxPoll = session.Poll
)

const (
	idle   = session.Idle
	done   = session.Done
	proc   = session.Proc
	urgent = session.Urgent
)

const (
	permGrace  = session.PermGrace
	permMisses = session.PermMisses
)

// Thin wrappers over the session package: the bar's refresh loop still writes
// against the small lowercase surface it was built for.
func statusOf(wait string, sc screen, seen bool) state { return session.StatusOf(wait, sc, seen) }
func permAlive(sc screen, seen bool, age int64, misses int) bool {
	return session.PermAlive(sc, seen, age, misses)
}
func readScreen(body string) screen            { return session.ReadScreen(body) }
func gather(permMiss map[string]int) ([]sess, tmuxPoll) {
	return session.Gather(permMiss)
}
func arrange(rows []sess, top bool) { session.Arrange(rows, top) }
func bandOf(fl string) int          { return session.BandOf(fl) }
func cleanName(title, path string) string {
	return session.CleanName(title, path)
}
func markRead(pane string)              { session.MarkRead(pane) }
func markUnread(pane string)            { session.MarkUnread(pane) }
func saveFlag(sid, fl string)           { session.SaveFlag(sid, fl) }
func setPaneFlag(pane, fl string)       { session.SetPaneFlag(pane, fl) }

func tmuxOut(a ...string) string   { return session.TmuxOut(a...) }
func run(name string, a ...string) { _ = exec.Command(name, a...).Run() }

// immune palette (identical focused/unfocused)
var (
	cUrgent   = lipgloss.Color("#ab7bf2") // bullet, spine, badge and counter of a session waiting on you
	cDone     = lipgloss.Color("#63c07a")
	cAmber    = lipgloss.Color("#e8a33d")
	cCtx      = lipgloss.Color("#b392f0") // context, wherever it speaks: the mark, the meter at its loudest
	cCtxRest  = lipgloss.Color("#7e8c8c") // the meter at rest: neutral, and cool like the rest of the type
	cCtxDim   = lipgloss.Color("#5c6666") // its number, while the figure is not worth acting on
	bgUrgent  = lipgloss.Color("#2a2036")
	bgBoth    = lipgloss.Color("#382c4a")
	bgDone    = lipgloss.Color("#1d2721")
	bgDoneSel = lipgloss.Color("#293830")
	lblUrgent = lipgloss.Color("#e6dcf7")
	lblDone   = lipgloss.Color("#cfe8d5")
	lblAmber  = lipgloss.Color("#e8c58a")
	metaUrg   = lipgloss.Color("#8f7bab")
	metaDone  = lipgloss.Color("#7d9c88")
	metaAmber = lipgloss.Color("#8a7550")
	metaSel   = lipgloss.Color("#8a9a9a")
	badgeFg   = lipgloss.Color("#16191a") // dark text on the (status-colored) badge
	badgeIdle = lipgloss.Color("#93a3a3") // idle badge bg: light, like the item font
	cPaused   = lipgloss.Color("#5c6666") // a parked session: there, asking for nothing
	cPausedMt = lipgloss.Color("#3f4646") // its age, one step quieter
	cBackdrop = lipgloss.Color("#3a4040") // the list, flattened behind an open menu
	cShadow   = lipgloss.Color("#121414") // the menu's drop shadow
	cMenu     = lipgloss.Color("#283131") // inside the menu
	cMenuAt   = lipgloss.Color("#24494b") // the option under the pointer: cyan, dimmed
	cHoverBar = lipgloss.Color("#243e40") // the band of the row under the pointer
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

// The assistants' badges beside the mascot: the same Font Awesome icons as their
// waybar modules (satellite-dish, briefcase), tinted by the same states.
// dai-live listening sits by the antennae, workwatch by the feet. The terminal needs
// Font Awesome 7 among its fonts (foot: a fallback in its font list), or it
// draws a box.
const (
	badgeLive  = ""
	badgeWatch = ""
	// A button: a space, the icon, two spaces. The icon is ~1.7 cells wide and
	// overflows into the cell after it, so the second space is what centres it.
	badgeW = 4
)

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
	live     string         // dai-live's state (off, armed, capturing, nosignal, error, stopped)
	watch    string         // workwatch: "recording", "paused", or "" when it is not installed
	sway     *swayWatch     // the compositor feed: who has focus, are we on screen
	hidden   bool           // the bar's window is on a workspace nobody is showing
	anim     bool           // the animation tick is running (see moving)
	menu     *menu          // the right-click menu, when one is open
	hover    string         // the pane of the row under the mouse, "" when none
	hoverB   string         // the badge under the mouse ("live", "watch"), "" when none
	hoverAt  time.Time      // the last mouse motion, to retire a hover left behind
	icache   map[itemKey]string
	pcache   map[string]string
}

// menu: the right-click menu, open on one session. It holds the session by pane
// because a poll can reorder the rows under it.
type menu struct {
	pane string
	at   int  // highlighted option, under the mouse or the keyboard; -1 none
	held bool // the bar has had the keyboard since it opened
}

// hoverTTL: the terminal reports the mouse moving, never leaving. A hover with
// no motion for this long is taken as the pointer gone elsewhere.
const hoverTTL = 4 * time.Second

type menuOpt struct {
	icon  string
	col   lipgloss.Color
	label string
	act   func(*model)
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
	pane, name, glyph, meta, age, ctx, flag string
	glyphCol                                lipgloss.Color
	st                                      state
	win, rowW, nameW                        int
	sel, cur, comp, hov, dai                bool
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
		pane: it.Pane, name: it.Name, glyph: g, glyphCol: gc,
		meta: metaLine(it), age: fmtAge(it.Since), ctx: it.Ctx, flag: it.Flag, st: it.St,
		win: it.W, rowW: m.rowW(), nameW: m.nameW(),
		sel: idx == m.sel, cur: it.Pane == m.current, comp: m.comp,
		hov: it.Pane == m.hover && idx != m.sel, dai: it.Dai,
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

// ---------- sway glue ----------

func switchView(s sess) {
	own := ""
	if os.Getenv("TMUX") != "" {
		own = strings.TrimSpace(tmuxOut("display-message", "-p", "#{client_name}"))
	}
	for _, c := range strings.Split(tmuxOut("list-clients", "-F", "#{client_name}"), "\n") {
		c = strings.TrimSpace(c)
		if c != "" && c != own {
			run("tmux", "switch-client", "-c", c, "-t", s.SessWin)
		}
	}
	run("tmux", "select-window", "-t", s.SessWin)
	run("tmux", "select-pane", "-t", s.Pane)
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
		return atoiLocal(f[1])
	}
	return 0
}

// atoiLocal: a tiny helper that mirrors the one in internal/session; keeping a
// local copy avoids re-exporting it just for ppidOf.
func atoiLocal(s string) int { n, _ := strconv.Atoi(s); return n }

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
		if r.St == proc {
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
	arrange(m.rows, m.top)
	m.focused, m.termFoc = detectFocus(m.sway, poll.Clients)
	m.current = currentPane(m.rows, poll.Current, poll.Win)
	m.sound = poll.Sound
	m.live = poll.Live
	m.watch = watchState()
	m.hidden = m.sway.isHidden()
	// Looking at the pane is what clears the unread green: the terminal holds
	// the keyboard and tmux is showing that pane, so the answer is on screen.
	if m.termFoc && m.current != "" {
		for i := range m.rows {
			if m.rows[i].Pane == m.current && m.rows[i].St == done &&
				m.rows[i].Mark != "unread" {
				markRead(m.rows[i].Pane)
				m.rows[i].St = idle
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
		live[r.Pane] = true
		if r.St == urgent && !m.wasUrg[r.Pane] {
			m.pulse[r.Pane] = 14
		}
		m.wasUrg[r.Pane] = r.St == urgent
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
	if (m.hover != "" || m.hoverB != "") && time.Since(m.hoverAt) > hoverTTL {
		m.hover, m.hoverB = "", ""
	}
	// An open menu holds the cursor on its session. It closes if that session is
	// gone, or once the bar loses the keyboard — you clicked somewhere else.
	if m.menu != nil {
		if m.focused {
			m.menu.held = true
		}
		if i := m.menuIdx(); i >= 0 && (m.focused || !m.menu.held) {
			m.sel = i
		} else {
			m.menu = nil
		}
	}
	// selection follows the actually-current pane, so switching panes in tmux
	// moves the cursor too — coming back to the bar it never "jumps" to a stale item.
	if m.current != "" && m.menu == nil {
		for i, r := range m.rows {
			if r.Pane == m.current {
				m.sel = i
				break
			}
		}
	}
	m.recomputeDensity()
}

// currentPane: the session the terminal is showing. A shell split beside claude
// can hold the active pane while the session is still right there on screen,
// so a window with claude in it counts as showing that claude.
func currentPane(rows []sess, pane, win string) string {
	for _, r := range rows {
		if r.Pane == pane {
			return pane
		}
	}
	for _, r := range rows {
		if r.SessWin == win {
			return r.Pane
		}
	}
	return pane
}

// density switches by available geometry, with 1-item hysteresis.
func (m *model) recomputeDensity() {
	avail := m.h - chromeRows - 1
	if avail < 1 || len(m.rows) == 0 {
		return
	}
	tall := 0
	for i, it := range m.rows {
		tall += m.leadN(i) + m.itemHeight(it, false) // an item's padding is inside it
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
		if msg.Action == tea.MouseActionMotion {
			m.hoverAt = time.Now()
			if m.menu != nil {
				m.menu.at = m.menuHit(msg.X, msg.Y)
				return m, nil
			}
			m.hoverB = m.badgeAt(msg.X, msg.Y)
			// Passing the right edge is the pointer on its way out of the bar.
			m.hover = ""
			if idx := m.hitTest(msg.Y); idx >= 0 && msg.X < m.rowW()-1 {
				m.hover = m.rows[idx].Pane
			}
			return m, nil
		}
		if msg.Action != tea.MouseActionPress {
			return m, nil
		}
		if m.menu != nil {
			if k := m.menuHit(msg.X, msg.Y); k >= 0 {
				m.pick(k)
				return m, nil
			}
			m.menu = nil // a click anywhere else closes it; a right-click opens another
			if msg.Button != tea.MouseButtonRight {
				return m, nil
			}
		}
		switch msg.Button {
		case tea.MouseButtonLeft:
			if b := m.badgeAt(msg.X, msg.Y); b != "" {
				toggleBadge(b)
				m.refresh()
			} else if msg.Y == m.soundRow() && msg.X >= m.rowW()-5 {
				m.toggleSound() // sound badge lives at the counter row's right edge
			} else if idx := m.hitTest(msg.Y); idx >= 0 {
				m.sel = idx
				m.jump()
			}
		case tea.MouseButtonRight:
			if idx := m.hitTest(msg.Y); idx >= 0 {
				m.sel = idx
				m.hover = ""
				m.menu = &menu{pane: m.rows[idx].Pane, at: -1, held: m.focused}
			}
		case tea.MouseButtonMiddle:
			if idx := m.hitTest(msg.Y); idx >= 0 {
				m.sel = idx
				m.toggleRead()
			}
		}
		return m, nil
	case tea.KeyMsg:
		if m.menu != nil {
			switch msg.String() {
			case "up", "k":
				m.menu.at = max(0, m.menu.at-1)
				return m, nil
			case "down", "j":
				m.menu.at = min(len(m.menuOpts())-1, m.menu.at+1)
				return m, nil
			case "enter":
				if m.menu.at >= 0 {
					m.pick(m.menu.at)
				}
				return m, nil
			case "esc":
				m.menu = nil
				return m, nil
			}
			m.menu = nil // any other key closes it and does what it always does
		}
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
		case "p", "P":
			m.toggleFlag("paused")
		case "o", "O":
			m.toggleFlag("prio")
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
					if it.W == n {
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

// unread: put the green back on the selected session.
func (m *model) unread() {
	if s, ok := m.cur(); ok {
		markUnread(s.Pane)
		m.rows[m.sel].Mark, m.rows[m.sel].St = "unread", done
	}
}

// read clears the green on the selected session without jumping to it.
func (m *model) read() {
	if s, ok := m.cur(); ok {
		markRead(s.Pane)
		m.rows[m.sel].Mark = "waiting"
		if s.St == done {
			m.rows[m.sel].St = idle
		}
	}
}

// toggleRead flips the selected session between read and unread: the middle
// click, and the menu's first entry.
func (m *model) toggleRead() {
	if s, ok := m.cur(); ok && s.St == done {
		m.read()
	} else {
		m.unread()
	}
}

// menuOpts: what the menu offers for its session. Each entry names what it will
// do, so a mark already set reads as the way to take it off.
func (m model) menuOpts() []menuOpt {
	i := m.menuIdx()
	if i < 0 {
		return nil
	}
	s := m.rows[i]
	opts := []menuOpt{{"✓", cDone, "unread", (*model).toggleRead}}
	if s.St == done {
		opts[0].icon, opts[0].col, opts[0].label = "○", badgeIdle, "read"
	}
	pause := menuOpt{"‖", metaSel, "pause", func(m *model) { m.toggleFlag("paused") }}
	if s.Flag == "paused" {
		pause.label = "resume"
	}
	prio := menuOpt{"★", themeFor(false).white, "prio", func(m *model) { m.toggleFlag("prio") }}
	if s.Flag == "prio" {
		prio.label = "drop prio"
	}
	return append(opts, pause, prio)
}

// menuIdx: the row the menu is open on, -1 when there is none.
func (m model) menuIdx() int {
	if m.menu == nil {
		return -1
	}
	for i, r := range m.rows {
		if r.Pane == m.menu.pane {
			return i
		}
	}
	return -1
}

// pick runs option k on the menu's session and closes the menu.
func (m *model) pick(k int) {
	opts, i := m.menuOpts(), m.menuIdx()
	m.menu = nil
	if i < 0 || k < 0 || k >= len(opts) {
		return
	}
	m.sel = i
	opts[k].act(m)
}

// toggleFlag marks the selected session, or clears the mark it already has. One
// mark at a time: pausing a priority session drops its priority.
func (m *model) toggleFlag(fl string) {
	s, ok := m.cur()
	if !ok || s.Sid == "" {
		return // no registry file for it yet: nothing to key the mark by
	}
	if s.Flag == fl {
		fl = ""
	}
	saveFlag(s.Sid, fl)
	setPaneFlag(s.Pane, fl)
	m.rows[m.sel].Flag = fl
	arrange(m.rows, m.top)
	for i, r := range m.rows { // the row moved band; the cursor goes with it
		if r.Pane == s.Pane {
			m.sel = i
			break
		}
	}
	m.recomputeDensity()
}

func (m *model) jump() {
	if s, ok := m.cur(); ok {
		switchView(s)
		swayFocusMain()
		m.current = s.Pane
		markRead(s.Pane) // jumping is reading — don't wait for the next poll
		if m.rows[m.sel].St == done {
			m.rows[m.sel].St = idle
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

// markW: the name cells a priority row's ★ takes, with its space. The Dai's
// icon needs none: it is drawn where the status bullet goes.
func markW(it sess) int {
	if it.Flag == "prio" && !it.Dai {
		return 2
	}
	return 0
}

// nameLines: the name column, less what the row's mark takes.
func (m model) nameLines(it sess) []string {
	nw := m.nameW() - markW(it)
	disp := midTruncate(strings.ToLower(it.Name), nw*2)
	return wrapN(disp, nw, 2)
}

// hasMeta: does this item have a bottom (time/PERM) line? Urgent always does;
// others only when there's a recorded age — no empty line eating space.
func (m model) itemHeight(it sess, comp bool) int {
	if comp || it.Flag == "paused" {
		return 1
	}
	// names + the second line (tmux window number + time) + top & bottom pad
	h := len(m.nameLines(it)) + 1 + 2
	if m.ctxShown(it) {
		h++ // the context meter
	}
	return h
}

// pinned: bottom-up, the Dai session (arrange puts it first) is not part of the
// list at all: it is drawn on the top rows of the panel, between two rules,
// where the list growing from the bottom never reaches it.
func (m model) pinned() bool {
	return !m.top && len(m.rows) > 0 && m.rows[0].Dai
}

// mascotLit: with a Dai session up, the mascot is the Dai -- lit while its pane
// is the one in front of you. Without one, the bar's own focus lights it.
func (m model) mascotLit() bool {
	for _, r := range m.rows {
		if r.Dai {
			return m.termFoc && m.current == r.Pane
		}
	}
	return m.focused
}

// watchState reads workwatch's pause switch: one stat, no fork.
func watchState() string {
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "workwatch")); err != nil {
		return ""
	}
	run := os.Getenv("XDG_RUNTIME_DIR")
	if run == "" {
		run = "/tmp"
	}
	if _, err := os.Stat(filepath.Join(run, "workwatch-paused")); err == nil {
		return "paused"
	}
	return "recording"
}

// topGeo: where the mascot and its badges sit, in cells of the art block. With
// badges the mascot moves left to make room; it drops to half size when the
// full one and the badges do not fit.
type topGeo struct {
	pw, x, rows   int // mascot pixel width, its column, its lines
	bx            int // the badges' column, -1 without badges
	liveY, watchY int // each badge's first line, -1 when absent
}

// badgeOff: where the badges start, from the mascot's column. At full size the
// icon sits on the arm's column, on the top and bottom lines -- the only ones
// the arm never reaches, up or down. At half size a line packs two bitmap rows
// and the badge would clip the arm, so it goes beside the mascot.
func badgeOff(pw int) int {
	if pw == 2 {
		return 10*pw - 1
	}
	return 11*pw + 1
}

func (m model) topGeo() topGeo {
	rw := m.rowW()
	g := topGeo{bx: -1, liveY: -1, watchY: -1}
	g.pw = min(2, max(1, (rw*6+55)/110)) // ~60% width / 11 px, rounded
	badges := m.live != "" || m.watch != ""
	if badges && g.pw == 2 && rw < badgeOff(2)+badgeW {
		g.pw = 1
	}
	g.rows = 4
	if g.pw == 2 {
		g.rows = 8
	}
	if !badges {
		g.x = max(0, (rw-11*g.pw)/2)
		return g
	}
	g.x = max(0, (rw-(badgeOff(g.pw)+badgeW))/2) // mascot and badges centred as one block
	g.bx = g.x + badgeOff(g.pw)
	first, second := 0, g.rows-1 // by the antennae, and level with the feet
	if m.live != "" {
		g.liveY = first
	}
	if m.watch != "" {
		g.watchY = second
	}
	return g
}

// badgeAt: which badge a click at (x, y) landed on, or "".
func (m model) badgeAt(x, y int) string {
	g := m.topGeo()
	if g.bx < 0 || m.mascotH() == 0 || x < g.bx || x >= g.bx+badgeW {
		return ""
	}
	y0 := mascotPad // bottom-up: the padding, then the art
	if m.top {
		y0 = m.h - mascotPad - g.rows
	}
	switch {
	case g.liveY >= 0 && y == y0+g.liveY:
		return "live"
	case g.watchY >= 0 && y == y0+g.watchY:
		return "watch"
	}
	return ""
}

// toggleBadge flips what the badge stands for, through the tool's own switch.
func toggleBadge(which string) {
	tool := map[string]string{"live": "dai-live", "watch": "workwatch"}[which]
	path, err := exec.LookPath(tool)
	if err != nil {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".dotfiles", "bin", tool)
	}
	run(path, "toggle")
}

// mascotH: the rows the mascot takes on top -- its padding, the art and, over a
// pinned Dai block, a row of gap -- or 0 when the panel has no room for it. The
// mascot yields whole: a cropped one reads as a glitch, not as a mascot.
func (m model) mascotH() int {
	art := m.topGeo().rows
	list := 2 // "no claude sessions"
	if len(m.rows) > 0 {
		list = 0
		for i := m.listStart(); i < len(m.rows); i++ {
			list += m.leadN(i) + m.itemHeight(m.rows[i], m.comp)
		}
	}
	foot := 4
	if len(m.rows) > 0 {
		foot = 6
	}
	gap := 0
	if m.pinned() {
		gap = 1
	}
	if m.pinH()+list+foot+art+mascotPad+gap >= m.h {
		return 0
	}
	return mascotPad + art + gap
}

// pinTop: the screen row the pinned block starts on, under the mascot.
func (m model) pinTop() int {
	if m.top {
		return 0
	}
	return m.mascotH()
}

// pinH: the rows the pinned block takes, its two rules included.
func (m model) pinH() int {
	if !m.pinned() {
		return 0
	}
	return m.itemHeight(m.rows[0], m.comp) + 2
}

// listStart: the first row index drawn in the list proper.
func (m model) listStart() int {
	if m.pinned() {
		return 1
	}
	return 0
}

// listTop: the screen row the first item is drawn on. Bottom-up, the list is
// pinned above the footer chrome, so where it starts depends on how tall it is.
func (m model) listTop() int {
	if m.top {
		return chromeRows
	}
	tall := 0
	for i := m.listStart(); i < len(m.rows); i++ {
		tall += m.leadN(i) + m.itemHeight(m.rows[i], m.comp)
	}
	return m.h - chromeRows - tall // negative when it overflows: rows scroll off the top
}

// lead: what is drawn above item i that belongs to no session — a blank where
// one band ends and the next begins, and a heading over each marked band.
func (m model) lead(i int) (gap, head bool) {
	it := m.rows[i]
	if it.Dai { // its own band of one, under its own rules
		return false, false
	}
	first := i == m.listStart() // the pinned block has its own rule above the list
	prev := sess{}
	if i > 0 {
		prev = m.rows[i-1]
	}
	gap = !first && (prev.Dai || bandOf(prev.Flag) != bandOf(it.Flag))
	head = it.Flag != "" && (first || prev.Dai || prev.Flag != it.Flag)
	return gap, head
}

func (m model) leadN(i int) int {
	gap, head := m.lead(i)
	n := 0
	if gap {
		n++
	}
	if head {
		n++
	}
	return n
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
	if top := m.pinTop() + 1; m.pinned() && y >= top && y < top+m.itemHeight(m.rows[0], m.comp) {
		return 0 // under the pin's top rule
	}
	row := m.listTop()
	for i := m.listStart(); i < len(m.rows); i++ {
		row += m.leadN(i)
		hh := m.itemHeight(m.rows[i], m.comp)
		if y >= row && y < row+hh && y >= m.pinTop()+m.pinH() { // under the pin it is not drawn
			return i
		}
		row += hh // padding lives inside the item now; no separate gap
	}
	return -1
}

// itemTop: the screen row item i's block starts on.
func (m model) itemTop(i int) int {
	if i == 0 && m.pinned() {
		return m.pinTop() + 1 // under the pin's top rule
	}
	row := m.listTop()
	for j := m.listStart(); j < i; j++ {
		row += m.leadN(j) + m.itemHeight(m.rows[j], m.comp)
	}
	return row + m.leadN(i)
}

// menuTop: the row the menu's box starts on. It opens under its session, or
// over it when there is no room below, and never over the counters — shadow
// included, which must not land on the session either.
func (m model) menuTop() int {
	i := m.menuIdx()
	if i < 0 {
		return -1
	}
	lo, hi := 0, m.h-chromeRows
	if m.top {
		lo, hi = chromeRows, m.h
	}
	n := len(m.menuOpts()) + 3 // the box, and its shadow under it
	y := m.itemTop(i) + m.itemHeight(m.rows[i], m.comp)
	if y+n > hi {
		y = m.itemTop(i) - n
	}
	return max(lo, min(y, hi-n))
}

// menuW: the card's width — as wide as its longest label needs, no wider, so it
// reads as a thing on top of the list and not one more row of it.
func (m model) menuW() int {
	lw := 0
	for _, o := range m.menuOpts() {
		lw = max(lw, utf8.RuneCountInString(o.label))
	}
	return min(lw+6, m.rowW()-1)
}

// menuX: the card's left column, centered with room for its shadow.
func (m model) menuX() int { return max(0, (m.rowW()-m.menuW()-1)/2) }

// menuHit: the option under the pointer, -1 when it is not on one.
func (m model) menuHit(x, y int) int {
	top, x0 := m.menuTop(), m.menuX()
	if top < 0 || x <= x0 || x >= x0+m.menuW()-1 {
		return -1
	}
	if k := y - top - 1; k >= 0 && k < len(m.menuOpts()) {
		return k
	}
	return -1
}

// menuLines: the card, menuW cells per line — a cyan outline on the dimmed
// list, filled inside, the option under the pointer lit.
func (m model) menuLines(th theme) []string {
	bw := m.menuW()
	edge := lipgloss.NewStyle().Foreground(th.focus).Background(th.panel)
	lines := []string{edge.Render("╭" + strings.Repeat("─", bw-2) + "╮")}
	for k, o := range m.menuOpts() {
		bg, lc, bold := cMenu, th.calm, false
		if k == m.menu.at {
			bg, lc, bold = cMenuAt, th.white, true
		}
		c := func(f lipgloss.Color, s string, b bool) string {
			return lipgloss.NewStyle().Foreground(f).Background(bg).Bold(b).Render(s)
		}
		lines = append(lines, edge.Render("│")+c(lc, " ", false)+c(o.col, o.icon, true)+
			c(lc, " "+padR(midTruncate(o.label, bw-6), bw-6)+" ", bold)+edge.Render("│"))
	}
	return append(lines, edge.Render("╰"+strings.Repeat("─", bw-2)+"╯"))
}

// sgr matches the colour codes lipgloss writes, to flatten a line behind the menu.
var sgr = regexp.MustCompile("\x1b\\[[0-9;:]*m")

// cellsOf: a plain line as one string per terminal column, padded to w. A wide
// rune keeps its column and leaves an empty string in the next one.
func cellsOf(s string, w int) []string {
	var out []string
	for _, r := range s {
		out = append(out, string(r))
		if lipgloss.Width(string(r)) == 2 {
			out = append(out, "")
		}
	}
	for len(out) < w {
		out = append(out, " ")
	}
	return out[:w]
}

// overlay floats the menu over the bar. Everything behind it goes flat and dim
// except the session it is open on, and the card casts a shadow: it has to read
// as above the list, not as one more row in it.
func (m model) overlay(rows []string, th theme) []string {
	i := m.menuIdx()
	if i < 0 {
		return rows
	}
	rw, keep := m.rowW(), m.itemTop(i)
	keepEnd := keep + m.itemHeight(m.rows[i], m.comp)
	dim := lipgloss.NewStyle().Foreground(cBackdrop).Background(th.panel)
	shade := lipgloss.NewStyle().Background(cShadow)
	flat := func(cs []string) string { return dim.Render(strings.Join(cs, "")) }
	box, x0, y0, bw := m.menuLines(th), m.menuX(), m.menuTop(), m.menuW()
	for y := range rows {
		on := y >= y0 && y <= y0+len(box) // the card's rows, and its shadow's
		if !on && y >= keep && y < keepEnd {
			continue
		}
		cs := cellsOf(sgr.ReplaceAllString(rows[y], ""), rw)
		switch k := y - y0; {
		case !on:
			rows[y] = flat(cs)
		case k == 0:
			rows[y] = flat(cs[:x0]) + box[0] + flat(cs[x0+bw:])
		case k < len(box):
			rows[y] = flat(cs[:x0]) + box[k] + shade.Render(" ") + flat(cs[x0+bw+1:])
		default:
			rows[y] = flat(cs[:x0+1]) + shade.Render(strings.Repeat(" ", bw)) + flat(cs[x0+bw+1:])
		}
	}
	return rows
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
	g, c := m.stateGlyph(it, th)
	if it.Dai { // the Dai's own icon stands in for the bullet, in the state's colour
		return badgeLive, c
	}
	return g, c
}

func (m model) stateGlyph(it sess, th theme) (string, lipgloss.Color) {
	switch it.St {
	case urgent:
		g := "●"
		if m.pulse[it.Pane] > 0 && (m.frame/2)%2 == 0 {
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

// nameColor: priority speaks in weight, and lifts a quiet name to white — never
// in a hue of its own, since hue is what the states use.
func nameColor(it sess, sel bool, th theme) (lipgloss.Color, bool) {
	prio := it.Flag == "prio"
	if sel {
		return th.white, it.St == urgent || prio
	}
	switch it.St {
	case urgent:
		return lblUrgent, true
	case done:
		return lblDone, prio
	case proc:
		if prio {
			return th.white, true
		}
		return lblAmber, false
	default:
		if prio {
			return th.white, true
		}
		return th.calm, false
	}
}
func metaColorOf(it sess, sel bool, th theme) lipgloss.Color {
	if sel {
		return metaSel
	}
	switch it.St {
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

// metaLine: the state tag, the subagents it has running (⤷N), how long it has
// sat. Each part only when there is one.
func metaLine(it sess) string {
	var parts []string
	switch it.St {
	case urgent:
		parts = append(parts, "PERM")
	case done:
		parts = append(parts, "DONE")
	}
	if it.Sub > 0 {
		parts = append(parts, "⤷"+strconv.Itoa(it.Sub))
	}
	if age := fmtAge(it.Since); age != "" {
		parts = append(parts, age)
	}
	return strings.Join(parts, " · ")
}

// Everything downstream of erma-ctx counts the same direction: how much of the
// room to auto-compact is spent. ctxUsedHigh mirrors ERMA_CTX_WARN there — past
// it the figure is worth reading, and not just the meter.
const (
	ctxUsedAmber = 70
	ctxUsedHigh  = 85
)

// ctxTag: the low-context mark, or "" while there is room. <= 4 cells, so it fits
// the compact row's age slot — which is the only place it still appears, since a
// full row draws the meter instead.
func ctxTag(it sess) string {
	if used := ctxUsed(it); used < ctxUsedHigh {
		return ""
	}
	return "◔" + it.Ctx + "%"
}

// ctxUsed: -1 when the pane has published nothing yet.
func ctxUsed(it sess) int {
	n, err := strconv.Atoi(it.Ctx)
	if it.Ctx == "" || err != nil {
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
	case used >= ctxUsedHigh:
		// Purple is what the bar raises its voice in — the same purple a waiting
		// session takes, and the one Claude Code's own status line uses for this.
		// The two are told apart by form, not hue: waiting fills the row's
		// background, running out draws a thin line under it.
		return cCtx
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
	ctxCellFull  = "▄"
	ctxCellHalf  = "▖"
	ctxCellTrack = "▁"
)

// ctxMeter splits the meter into what is spent and the rail behind it, so the
// two can carry different colors. The rail is deliberately featureless: marks
// for the thresholds were tried and read as an alarm on a row that should not
// raise its voice before it has something to say.
func ctxMeter(used, cells int) (fill, track string) {
	if cells < 1 {
		return "", ""
	}
	halves := used * cells * 2 / 100
	full := min(halves/2, cells)
	fill = strings.Repeat(ctxCellFull, full)
	if halves%2 == 1 && full < cells {
		fill += ctxCellHalf
		full++
	}
	return fill, strings.Repeat(ctxCellTrack, cells-full)
}

// ctxShown: the meter earns its row on a session that is at risk, or on the one
// you are looking at. Everywhere else it is a figure nobody asked for, repeated
// down the length of the bar.
func (m model) ctxShown(it sess) bool {
	used := ctxUsed(it)
	return used >= 0 && (used >= ctxUsedAmber || it.Pane == m.current)
}

func rowBg(it sess, sel bool, th theme) lipgloss.Color {
	switch {
	case it.St == urgent && sel:
		return bgBoth
	case it.St == urgent:
		return bgUrgent
	case it.St == done && sel:
		return bgDoneSel
	case it.St == done:
		return bgDone
	case sel:
		return th.bgSel
	}
	return th.panel
}

func (m model) renderItem(it sess, idx int, th theme) string {
	parked := it.Flag == "paused"
	if parked && it.St == done {
		it.St = idle // a finished answer in a paused session waits quietly for you
	}
	sel := idx == m.sel
	nw := m.nameW()
	rbg := rowBg(it, sel, th)
	hov := it.Pane == m.hover && !sel
	if hov { // the pointer is on it: a click jumps here
		rbg = ctxShade(rbg, 0x06)
	}
	nc, bold := nameColor(it, sel, th)
	// every cell bakes the row bg, so there are no reset "holes" in the fill.
	c := func(f lipgloss.Color, s string, b bool) string {
		return lipgloss.NewStyle().Foreground(f).Background(rbg).Bold(b).Render(s)
	}
	sp, spc := m.spineRune(it.St)
	spine := c(spc, sp, false)
	band := c(rbg, " ", false)
	if sel {
		band = c(th.focus, "▐", false)
	} else if hov {
		band = c(cHoverBar, "▐", false)
	}
	gg, gc := m.glyphRune(it, th)
	star, sw := "", markW(it) // a priority row's ★, and the name cells it takes
	if sw > 0 {
		star = c(th.white, "★", true) + c(rbg, " ", false)
	}

	// 1 line: gutter + name + time(right) + arrow. A paused row is always this
	// shape, dimmed; only a dialog still speaks up in it, since an agent stuck
	// waiting on you is real in any session.
	if m.comp || parked {
		// gutter 5 + a space + the 4-cell age + the arrow: 11, not 10. Being one
		// over made lipgloss wrap the row onto a second line.
		nwC := max(4, m.rowW()-5-1-4-1) - sw
		nm := midTruncate(strings.ToLower(it.Name), nwC)
		age, agec := padL(fmtAge(it.Since), 4), metaColorOf(it, sel, th)
		if parked && it.St != urgent {
			nc, bold, agec = cPaused, false, cPausedMt
			if sel {
				nc = th.calm
			}
			if it.St == idle {
				gg, gc = "‖", cPaused
			}
		}
		if tag := ctxTag(it); tag != "" { // running out of room beats how long it has sat
			age, agec = padL(tag, 4), cCtx
		}
		arrow := c(rbg, " ", false)
		if it.Pane == m.current {
			arrow = c(th.focus, "➤", false)
		}
		line := band + spine + c(rbg, " ", false) + c(gc, gg, false) + c(rbg, " ", false) + star +
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
		if sw > 0 {
			if li == 0 {
				glyph += star
			} else {
				glyph += c(rbg, "  ", false)
			}
		}
		lines = append(lines, band+spine+c(rbg, " ", false)+glyph+c(nc, padR(nm, nw-sw), bold))
	}
	// second line: the time (gutter cols 3-5 empty). The low-context mark stays a
	// compact-mode thing — here the meter below already carries the same number,
	// and in the other direction.
	lines = append(lines, band+spine+c(rbg, "   ", false)+
		c(metaColorOf(it, sel, th), padR(metaLine(it), nw), false))

	// third line: the context meter, spanning the name column, its number on the
	// badge's grid in the right column — see the loop below. Only on the rows
	// where it is worth the height (ctxShown).
	meterRow, meterTail := -1, ""
	if m.ctxShown(it) {
		used := ctxUsed(it)
		cc := ctxColor(used, th)
		fill, track := ctxMeter(used, nw)
		meterRow = len(lines)
		lines = append(lines, band+spine+c(rbg, "   ", false)+
			c(cc, fill, false)+c(ctxShade(rbg, 0x16), track, false))

		// 100 is the one value that fills the slot, and "100%" would sit flush
		// against a full meter. A full bar labelled 100 is not ambiguous.
		pct := strconv.Itoa(used) + "%"
		if used == 100 {
			pct = "100"
		}
		pc := cCtxDim
		if used >= ctxUsedAmber {
			pc = cc
		}
		meterTail = c(pc, padL(pct, 4), used >= ctxUsedHigh) + c(rbg, " ", false)
	}

	// right column (2 cells): the tmux window # as a dark badge on line 0, and
	// the current-session arrow ▸ just below it (line 1) when applicable.
	bstr := " " + strconv.Itoa(it.W) + " " // symmetric 1-space padding inside the badge
	bw := utf8.RuneCountInString(bstr)
	bbg := badgeBgFor(it.St)
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
		case i == 1 && it.Pane == m.current:
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

// counts: a paused session counts as paused, unless it is waiting on you.
func (m model) counts() (u, d, p, i, z int) {
	for _, it := range m.rows {
		switch {
		case it.St == urgent:
			u++
		case it.Flag == "paused":
			z++
		case it.St == done:
			d++
		case it.St == proc:
			p++
		default:
			i++
		}
	}
	return
}

// legendKeys: what the legend names — a glyph, the word, and where in the word
// its key is.
var legendKeys = []struct {
	glyph, word string
	at          int
}{{"✓", "unread", 0}, {"‖", "pause", 0}, {"★", "prio", 3}}

// legend: the widest key hint that fits — each glyph and its word with the key
// letter in bold, or, when even that is too wide, the glyphs and letters alone.
// A row one cell over its width gets wrapped by lipgloss.
func (m model) legend() string {
	th := themeFor(false)
	calm := lipgloss.NewStyle().Foreground(th.calm)
	key := lipgloss.NewStyle().Foreground(th.white).Bold(true)
	for _, v := range []struct {
		sep   string
		short bool
	}{{"  ", false}, {" ", false}, {" ", true}} {
		var plain, out []string
		for _, k := range legendKeys {
			word, at := k.word, k.at
			if v.short {
				word, at = word[at:at+1], 0
			}
			plain = append(plain, k.glyph+" "+word)
			out = append(out, calm.Render(k.glyph+" "+word[:at])+key.Render(word[at:at+1])+calm.Render(word[at+1:]))
		}
		if utf8.RuneCountInString(strings.Join(plain, v.sep))+1 <= m.rowW() {
			return calm.Render(" ") + strings.Join(out, calm.Render(v.sep))
		}
	}
	return ""
}

// bandHead: the heading over a marked band — its glyph, its name and how many,
// then the thin rule out to the edge.
func (m model) bandHead(fl string, th theme) string {
	n := 0
	for _, r := range m.rows {
		if r.Flag == fl {
			n++
		}
	}
	icon, ic, label, lc := "‖", cPaused, "paused", cPaused
	if fl == "prio" {
		icon, ic, label, lc = "★", th.white, "prio", th.calm
	}
	txt := fmt.Sprintf("%s · %d", label, n)
	if utf8.RuneCountInString(txt)+4 > m.rowW() {
		txt = strconv.Itoa(n) // a narrow bar keeps the count, the glyph names the band
	}
	s := " " + lipgloss.NewStyle().Foreground(ic).Bold(true).Render(icon) + " " + fg(lc, txt) + " "
	if w := m.rowW() - lipgloss.Width(s) - 1; w > 0 {
		s += fg(lipgloss.Color("#2a3030"), strings.Repeat("─", w))
	}
	return s
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
	u, d, p, i, z := m.counts()
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
	if z > 0 {
		parts = append(parts, fg(cPaused, fmt.Sprintf("‖%d", z)))
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

	var pin, list []string
	if m.pinned() {
		pin = append(append([]string{rule}, strings.Split(m.itemLines(m.rows[0], 0, th), "\n")...), rule)
	}
	if len(m.rows) == 0 {
		list = []string{" " + fg(th.meta, "no claude"), " " + fg(th.meta, "sessions")}
	} else {
		for idx := m.listStart(); idx < len(m.rows); idx++ {
			it := m.rows[idx]
			gap, head := m.lead(idx)
			if gap {
				list = append(list, "")
			}
			if head {
				list = append(list, m.bandHead(it.Flag, th))
			}
			list = append(list, strings.Split(m.itemLines(it, idx, th), "\n")...)
		}
	}
	// The foot: everything that is not a session, in one block against the edge
	// the eye rests on — a margin, the key legend behind its own divider, then
	// the header's rule and counters. With no sessions the legend has nothing
	// to explain and drops out.
	foot := []string{"", rule, head, ""}
	if len(m.rows) > 0 {
		foot = []string{"", m.rule(), m.legend(), rule, head, ""}
	}
	// The mascot is decoration and yields whole (see mascotH). Bottom-up it is
	// the topmost thing, the pinned Dai block right under it.
	var mascot []string
	if m.mascotH() > 0 {
		art := m.topArt()
		pad := make([]string, mascotPad)
		if m.top {
			mascot = append(art, pad...)
		} else {
			mascot = append(pad, art...)
			if m.pinned() {
				mascot = append(mascot, "")
			}
		}
	}

	var lead, trail []string
	if m.top {
		lead = append(reversed(foot), list...)
		trail = mascot
	} else {
		lead = append(mascot, pin...)
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
		} else if keep := m.h - len(pin); keep > 0 { // the list slides under the pin, never over it
			rows = append(append([]string(nil), pin...), rows[len(rows)-keep:]...)
		} else { // shorter than the pin itself (the first frame has h 0)
			rows = pin[:max(m.h, 0)]
		}
	}
	if m.menu != nil {
		rows = m.overlay(rows, th)
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

// pixels renders a bitmap: one █ per pixel, pw cells wide, at pw 2; at pw 1,
// half-blocks pack two bitmap rows into a line, keeping the proportion.
func pixels(bmp []string, pw int) []string {
	var out []string
	if pw >= 2 {
		for _, row := range bmp {
			var sb strings.Builder
			for _, ch := range row {
				if ch == 'X' {
					sb.WriteString(strings.Repeat("█", pw))
				} else {
					sb.WriteString(strings.Repeat(" ", pw))
				}
			}
			out = append(out, sb.String())
		}
		return out
	}
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
		out = append(out, sb.String())
	}
	return out
}

// badgeColor: the same colours as the waybar modules of dai-live and workwatch.
func badgeColor(state string) lipgloss.Color {
	switch state {
	case "capturing", "recording":
		return lipgloss.Color("#4fd08a")
	case "armed":
		return lipgloss.Color("#e8a33d")
	case "nosignal", "error", "stopped":
		return lipgloss.Color("#e05b57")
	}
	return lipgloss.Color("#7a7a7a")
}

// badgeCell: a badge as a three-cell button, shaded like a row under the pointer.
func (m model) badgeCell(which, icon string, col lipgloss.Color) string {
	st := lipgloss.NewStyle().Foreground(col)
	if m.hoverB == which {
		st = st.Background(cHoverBar)
	}
	return st.Render(" " + icon + "  ")
}

// topArt: the 👾 in a very faded purple -- barely there, a touch more present
// (and arms up) while lit -- with the assistants' badges at its arm.
func (m model) topArt() []string {
	g := m.topGeo()
	lit := m.mascotLit()
	col := lipgloss.Color("#262430")
	bmp := invaderDown
	if lit {
		col, bmp = lipgloss.Color("#38334f"), invaderUp
	}
	art := pixels(bmp, g.pw)
	out := make([]string, len(art))
	for i, row := range art {
		var badge string
		switch {
		case g.bx < 0:
		case i == g.liveY:
			badge = m.badgeCell("live", badgeLive, badgeColor(m.live))
		case i == g.watchY:
			badge = m.badgeCell("watch", badgeWatch, badgeColor(m.watch))
		}
		if badge != "" { // the badge covers the mascot from its column on
			r, n := []rune(row), g.bx-g.x
			row = string(r[:min(len(r), n)]) + strings.Repeat(" ", max(0, n-len(r)))
		}
		out[i] = strings.Repeat(" ", g.x) + fg(col, row) + badge
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
	// Subcommand dispatch: `erma serve ...` goes to its own flagset so --addr,
	// --tailscale-login etc. do not collide with the bar's --top.
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	top := flag.Bool("top", false,
		"lay the bar out top-down: counters and sessions at the top, mascot at the bottom")
	flag.Parse()

	m := model{top: *top, sway: newSwayWatch(),
		icache: map[itemKey]string{}, pcache: map[string]string{}}
	m.refresh()
	m.anim = true // Init starts the tick; the first animMsg decides if it lives
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseAllMotion())
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
