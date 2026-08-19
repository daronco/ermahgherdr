// dai-bar — slim status sidebar of claude tmux sessions (Bubble Tea).
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
// cyan means only "you": selection band ▐, current-session arrow ▸, focused count.
package main

import (
	"encoding/json"
	"fmt"
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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// immune palette (identical focused/unfocused)
var (
	cUrgent   = lipgloss.Color("#e05b57")
	cDone     = lipgloss.Color("#63c07a")
	cAmber    = lipgloss.Color("#e8a33d")
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
const appID = "dai-bar"

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
	st                  state
	since               int64
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
	comp     bool           // compact (geometry-based, with 1-item hysteresis)
	sound    bool           // audible alerts on (mirrors tmux global @dai_bar_sound)
}

type animMsg time.Time
type dataMsg time.Time

func animCmd() tea.Cmd {
	return tea.Tick(140*time.Millisecond, func(t time.Time) tea.Msg { return animMsg(t) })
}
func dataCmd() tea.Cmd {
	return tea.Tick(600*time.Millisecond, func(t time.Time) tea.Msg { return dataMsg(t) })
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
const capSep = "~~dai-bar-block~~"

// captureAll reads every pane in ONE tmux round-trip: a `display-message` marks
// where each block starts, the `capture-pane` after it carries the content.
// Per-pane calls cost ~5ms each, which at two polls a second is real CPU for a
// bar that is always open; batched, a poll stays two forks however many
// sessions are up.
//
// Blocks come back in the order asked — the marker cannot carry the pane id,
// because display-message runs its output through strftime and would eat the
// leading %. tmux also stops a command list at its first error, so a pane that
// dies mid-poll truncates the tail: those panes, and any that come back empty,
// are left out of the map and keep whatever their mark says. Downgrading a
// state on missing evidence is exactly the bug this file is fixing.
func captureAll(panes []string) map[string]screen {
	if len(panes) == 0 {
		return nil
	}
	var args []string
	for i, p := range panes {
		if i > 0 {
			args = append(args, ";")
		}
		args = append(args, "display-message", "-p", "-t", p, capSep, ";",
			"capture-pane", "-p", "-t", p)
	}
	blocks := strings.Split(tmuxOut(args...), capSep+"\n")
	if len(blocks) > len(panes)+1 {
		return nil // a pane is showing our marker: bail rather than misalign
	}
	res := map[string]screen{}
	for i, blk := range blocks[1:] {
		if i < len(panes) && strings.TrimSpace(blk) != "" {
			res[panes[i]] = readScreen(blk)
		}
	}
	return res
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
// means read, and only the bar writes it, when you look.
func statusOf(wait string, sc screen, seen bool) state {
	switch {
	case sc.busy:
		return proc
	case wait == "perm":
		return urgent // gather has already dropped the marks it disbelieves
	case wait == "working" && !seen:
		return proc
	case wait == "working", wait == "done":
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
	run("tmux", "set", "-p", "-t", pane, "@dai_bar_wait", "waiting")
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

func gather(permMiss map[string]int) []sess {
	f := strings.Join([]string{
		"#{pane_id}", "#{session_name}", "#{window_index}", "#{pane_index}",
		"#{@dai_bar_wait}", "#{@dai_bar_wait_since}", "#{pane_current_path}", "#{pane_title}",
	}, "\t")
	out := tmuxOut("list-panes", "-a", "-f", "#{==:#{pane_current_command},claude}", "-F", f)
	var recs [][]string
	var panes []string
	for _, line := range strings.Split(out, "\n") {
		c := strings.Split(line, "\t")
		if len(c) < 8 || c[0] == "" {
			continue
		}
		recs, panes = append(recs, c), append(panes, c[0])
	}
	screens := captureAll(panes)
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
		if seen && st == done && c[4] != "done" {
			run("tmux", "set", "-p", "-t", c[0], "@dai_bar_wait", "done")
		}
		rows = append(rows, sess{
			pane: c[0], sessWin: c[1] + ":" + c[2], name: cleanName(c[7], c[6]),
			st: st, since: since,
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
	return rows
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
	if b, err := os.ReadFile(filepath.Join(rt, "dai-bar-last-term")); err == nil {
		if cid := strings.TrimSpace(string(b)); cid != "" {
			run("swaymsg", fmt.Sprintf("[con_id=%s] focus", cid))
		}
	}
}

// detectFocus: who holds the keyboard — the bar, or the terminal running a tmux
// client? The second half is what separates "you are looking at this session"
// from "it merely is the active tmux pane while you read your mail".
func detectFocus() (bar, term bool) {
	out, err := exec.Command("swaymsg", "-t", "get_tree").Output()
	if err != nil {
		return true, false
	}
	var tree map[string]any
	if json.Unmarshal(out, &tree) != nil {
		return true, false
	}
	var find func(n map[string]any) (map[string]any, bool)
	find = func(n map[string]any) (map[string]any, bool) {
		if f, _ := n["focused"].(bool); f {
			return n, true
		}
		for _, key := range []string{"nodes", "floating_nodes"} {
			if kids, ok := n[key].([]any); ok {
				for _, k := range kids {
					if km, ok := k.(map[string]any); ok {
						if r, found := find(km); found {
							return r, true
						}
					}
				}
			}
		}
		return nil, false
	}
	if f, ok := find(tree); ok {
		if app, _ := f["app_id"].(string); app == appID {
			return true, false
		}
		pid, _ := f["pid"].(float64)
		return false, hostsTmuxClient(int(pid))
	}
	return false, false
}

// hostsTmuxClient: does the window with this pid hold a tmux client? The client
// is a grandchild of the terminal (terminal → shell → tmux), so walk up from
// every client and see if one lands on the window.
func hostsTmuxClient(win int) bool {
	if win <= 1 {
		return false
	}
	for _, c := range strings.Split(tmuxOut("list-clients", "-F", "#{client_pid}"), "\n") {
		pid := atoi(strings.TrimSpace(c))
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

func detectCurrent() string {
	for _, c := range strings.Split(tmuxOut("list-clients", "-F", "#{client_name}"), "\n") {
		if c = strings.TrimSpace(c); c != "" {
			if p := strings.TrimSpace(tmuxOut("display-message", "-p", "-t", c, "#{pane_id}")); p != "" {
				return p
			}
		}
	}
	return ""
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

const contentTop = 4 // top margin + header (2 lines) + margin

func (m model) Init() tea.Cmd { return tea.Batch(animCmd(), dataCmd()) }

// soundOn: is the audible alert enabled? (shared with the hook via a tmux global)
func soundOn() bool { return strings.TrimSpace(tmuxOut("show", "-gv", "@dai_bar_sound")) == "on" }

// toggleSound flips the shared flag; the hook reads it on the next perm/waiting.
func (m *model) toggleSound() {
	m.sound = !m.sound
	v := "off"
	if m.sound {
		v = "on"
	}
	run("tmux", "set", "-g", "@dai_bar_sound", v)
}

func (m *model) refresh() {
	if m.permMiss == nil {
		m.permMiss = map[string]int{}
	}
	m.rows = gather(m.permMiss)
	m.focused, m.termFoc = detectFocus()
	m.current = detectCurrent()
	m.sound = soundOn()
	// Looking at the pane is what clears the unread green: the terminal holds
	// the keyboard and tmux is showing that pane, so the answer is on screen.
	if m.termFoc && m.current != "" {
		for i := range m.rows {
			if m.rows[i].pane == m.current && m.rows[i].st == done {
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
	avail := m.h - contentTop - 1
	if avail < 1 || len(m.rows) == 0 {
		return
	}
	tall := 0
	for _, it := range m.rows {
		tall += m.itemHeight(it, false) + 1
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
		return m, animCmd()
	case dataMsg:
		m.refresh()
		return m, dataCmd()
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.recomputeDensity()
		return m, nil
	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			if msg.Y == 1 && msg.X >= m.rowW()-5 {
				m.toggleSound() // sound badge lives at the header's right edge
			} else if idx := m.hitTest(msg.Y); idx >= 0 {
				m.sel = idx
				m.jump()
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
	return len(m.nameLines(it)) + 1 + 2
}

func (m model) hitTest(y int) int {
	row := contentTop
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
		age := padL(fmtAge(it.since), 4)
		arrow := c(rbg, " ", false)
		if it.pane == m.current {
			arrow = c(th.focus, "➤", false)
		}
		line := band + spine + c(rbg, " ", false) + c(gc, gg, false) + c(rbg, " ", false) +
			c(nc, padR(nm, nwC), bold) + c(rbg, " ", false) +
			c(metaColorOf(it, sel, th), age, false) + arrow
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
	// second line: the time (gutter cols 3-5 empty)
	lines = append(lines, band+spine+c(rbg, "   ", false)+
		c(metaColorOf(it, sel, th), padR(metaLine(it), nw), false))

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

func (m model) overflowNow() bool {
	avail := m.h - contentTop - 2 // footer + its margin
	tall := 0
	for _, it := range m.rows {
		tall += m.itemHeight(it, m.comp) // padding is inside each item; no gap
	}
	return tall > avail
}

// header: 2 lines in both states — a content line and a base line. Focus is
// carried by the base (heavy cyan ━ vs thin gray ─) + brightness, never a block.
func (m model) header() string {
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
	// (glyph on a filled chip) so it's easy to see — cyan when on; when off, an
	// opaque red chip with the note struck through (clearly "muted").
	sBg, sFg, sGlyph := lipgloss.Color("#6e3634"), lipgloss.Color("#eccbca"), " ✕ "
	if m.sound {
		sBg, sFg, sGlyph = lipgloss.Color("#2dccd3"), badgeFg, " ♫ "
	}
	soundBadge := lipgloss.NewStyle().Background(sBg).Foreground(sFg).Render(sGlyph)
	gap := rw - lipgloss.Width(left) - lipgloss.Width(soundBadge) - 1
	if gap < 1 {
		gap = 1
	}
	top := left + strings.Repeat(" ", gap) + soundBadge + " "

	var base string
	if live {
		n := max(4, (rw-2)*4/10)
		if n > rw-2 {
			n = rw - 2
		}
		base = " " + fg(lipgloss.Color("#2dccd3"), strings.Repeat("━", n)) +
			fg(lipgloss.Color("#24494b"), strings.Repeat("━", rw-2-n))
	} else {
		base = " " + fg(lipgloss.Color("#2a3030"), strings.Repeat("─", rw-2))
	}
	return top + "\n" + base
}

func (m model) View() string {
	th := themeFor(m.focused)

	var b strings.Builder
	b.WriteString("\n")              // top margin (a full line — reads better than a thin one)
	b.WriteString(m.header() + "\n") // header: 2 lines
	b.WriteString("\n")              // margin below the header

	if len(m.rows) == 0 {
		b.WriteString(" " + fg(th.meta, "no claude") + "\n")
		b.WriteString(" " + fg(th.meta, "sessions") + "\n")
		return m.finish(th, b.String())
	}

	for idx, it := range m.rows {
		b.WriteString(m.renderItem(it, idx, th) + "\n")
	}
	b.WriteString("\n") // margin above the footer
	b.WriteString(" " + fg(th.calm, "↑↓ move  ⏎ go"))
	return m.finish(th, b.String())
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

// finish: pin the faded mascot to the bottom of the panel, then render.
func (m model) finish(th theme, body string) string {
	art := m.mascotArt(m.rowW(), m.focused)
	used := strings.Count(body, "\n") + 1
	pad := m.h - used - len(art)
	if pad < 1 {
		pad = 1
	}
	body += strings.Repeat("\n", pad) + strings.Join(art, "\n")
	return panel(th, m.w, m.h, body)
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
	m := model{}
	m.refresh()
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
