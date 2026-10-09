package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// The state table is where the bug lived: a mark alone could pin a session on
// red forever. These cases pin down who wins between the mark and the screen.
func TestStatusOf(t *testing.T) {
	for _, c := range []struct {
		name string
		wait string
		sc   screen
		seen bool
		want state
	}{
		{"permission up", "perm", screen{prompt: true}, true, urgent},
		{"dialog, no notification", "", screen{prompt: true, form: true}, true, urgent},
		{"dialog under a stale spinner", "working", screen{busy: true, prompt: true, form: true}, true, urgent},
		{"perm, pane unread", "perm", screen{}, false, urgent},
		{"approved, now working", "perm", screen{busy: true}, true, proc},
		{"working", "working", screen{busy: true}, true, proc},
		{"working, stopped without a Stop hook", "working", screen{}, true, done},
		{"working, pane unread", "working", screen{}, false, proc},
		{"finished", "done", screen{}, true, done},
		{"finished, then off again", "done", screen{busy: true}, true, proc},
		{"read", "waiting", screen{}, true, idle},
		{"no hook, quiet", "", screen{}, true, idle},
		{"no hook, working", "", screen{busy: true}, true, proc},
	} {
		if got := statusOf(c.wait, c.sc, c.seen); got != c.want {
			t.Errorf("%s: statusOf(%q, %+v, %v) = %v, want %v",
				c.name, c.wait, c.sc, c.seen, got, c.want)
		}
	}
}

func TestPermAlive(t *testing.T) {
	for _, c := range []struct {
		name   string
		sc     screen
		seen   bool
		age    int64
		misses int
		want   bool
	}{
		{"prompt on screen", screen{prompt: true}, true, 999, 9, true},
		{"pane could not be read", screen{}, false, 999, 9, true},
		{"still within the grace", screen{}, true, 1, 1, true},
		{"first miss", screen{}, true, 999, 1, true},
		{"second miss", screen{}, true, 999, 2, false},
	} {
		if got := permAlive(c.sc, c.seen, c.age, c.misses); got != c.want {
			t.Errorf("%s: permAlive(%+v, %v, %d, %d) = %v, want %v",
				c.name, c.sc, c.seen, c.age, c.misses, got, c.want)
		}
	}
}

func TestReadScreen(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want screen
	}{
		{"working", "● Read(main.go)\n✢ Reticulating… (15m 52s · ↓ 45.1k tokens)\n───\n❯ \n───\n  ⏵⏵ auto mode on\n",
			screen{busy: true}},
		{"stopped", "● all set\n✻ Churned for 15s\n─── a session ──\n❯ \n───\n  ⏵⏵ auto mode on\n",
			screen{}},
		// verbatim from Claude Code 2.1.236: the question is phrased per tool,
		// so "Do you want to proceed?" is not the string to look for.
		{"permission", " Create file\n permprobe-xyz.txt\n╌╌╌\n  1 hello\n╌╌╌\n" +
			" Do you want to create permprobe-xyz.txt?\n ❯ 1. Yes\n" +
			"   2. Yes, and switch to accept edits for this session (shift+tab)\n   3. No\n\n" +
			" Esc to cancel · Tab to amend\n",
			screen{prompt: true, form: true}},
		{"question", "Choose an option:\n❯ 1. Keep it\n  2. Drop it\n", screen{prompt: true}},
		{"typed at the input box", "─── a session ──\n❯ 2. also check the other one\n───\n", screen{}},
		{"a numbered list Claude wrote", "● Steps:\n  1. build\n  2. test\n  3. ship\n─── a session ──\n❯ \n",
			screen{}},
		{"an answer quoting a dialog", "● It asks: Do you want to proceed? Esc to cancel\n✻ Churned for 3s\n" +
			"───\n❯ \n───\n  ⏵⏵ auto mode on\n", screen{}},
		{"MCP elicitation", "───\n MCP server \"notion\" requests your input\n ❯ Accept\n   Decline\n Esc to cancel\n",
			screen{prompt: true, form: true}},
		{"a menu you opened", "───\n Select model\n ❯ 1. Default\n   2. Opus\n Enter to set as default · Esc to cancel\n",
			screen{prompt: true}},
	} {
		if got := readScreen(c.body); got != c.want {
			t.Errorf("%s: readScreen = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// testdata/screen-*.txt are whole panes captured off a live Claude Code. When its
// UI changes, capture the state that broke (tmux capture-pane -p) and add it here.
func TestReadScreenCaptures(t *testing.T) {
	for _, c := range []struct {
		file string
		want screen
	}{
		{"testdata/screen-permission.txt", screen{prompt: true, form: true}},
		{"testdata/screen-answered.txt", screen{}},
		// 2.1.287 from here down
		{"testdata/screen-permission-bash.txt", screen{prompt: true, form: true}},
		{"testdata/screen-question.txt", screen{prompt: true, form: true}},
		{"testdata/screen-working-tasks.txt", screen{busy: true}},
		{"testdata/screen-background-wait.txt", screen{busy: true}},
		{"testdata/screen-background-done.txt", screen{}},
		{"testdata/screen-transcript.txt", screen{viewer: true}},
	} {
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		if got := readScreen(string(b)); got != c.want {
			t.Errorf("%s: readScreen = %+v, want %+v", c.file, got, c.want)
		}
	}
}

// listTop feeds hitTest, and View draws the list — derived separately, they can
// drift apart, and a click then lands on the wrong session. This pins them
// together in both layouts, including when the panel is too short to fit.
func TestListTopMatchesRender(t *testing.T) {
	rows := []sess{
		{pane: "%1", name: "alphamark", st: idle, w: 1},
		{pane: "%2", name: "betamark", st: done, w: 2},
		{pane: "%3", name: "gammamark", st: proc, w: 3},
		{pane: "%4", name: "deltamark", st: urgent, w: 4},
	}
	for _, top := range []bool{false, true} {
		for _, h := range []int{44, 24, 14} {
			m := model{w: 28, h: h, top: top, rows: rows}
			m.recomputeDensity()
			lines := strings.Split(m.View(), "\n")
			if len(lines) != h {
				t.Errorf("top=%v h=%d: rendered %d lines", top, h, len(lines))
			}
			drawn := -1
			for i, ln := range lines {
				if strings.Contains(ln, "alphamark") {
					drawn = i
					break
				}
			}
			want := m.listTop()
			if !m.comp {
				want++ // a full-size item block opens with a padding row
			}
			if want < 0 { // scrolled off the top: nothing to compare against
				continue
			}
			if drawn != want {
				t.Errorf("top=%v h=%d comp=%v: first item name drawn at %d, want %d",
					top, h, m.comp, drawn, want)
			}
		}
	}
}

func TestMetaLine(t *testing.T) {
	now := time.Now().Unix()
	for _, c := range []struct {
		name string
		it   sess
		want string
	}{
		{"idle, no age", sess{st: idle}, ""},
		{"done", sess{st: done, since: now - 120}, "DONE · 2m"},
		{"working with subagents", sess{st: proc, since: now - 120, sub: 3}, "⤷3 · 2m"},
		{"waiting on you, one subagent", sess{st: urgent, sub: 1}, "PERM · ⤷1"},
	} {
		if got := metaLine(c.it); got != c.want {
			t.Errorf("%s: metaLine = %q, want %q", c.name, got, c.want)
		}
	}
}

// Every rendered row must fit rowW: one cell over and lipgloss wraps it onto a
// second line, which shifts everything below it and breaks hitTest.
func TestRowsFitWidth(t *testing.T) {
	rows := []sess{
		{pane: "%1", name: "dados financeiros no bigquery", st: idle, w: 6, since: 1},
		{pane: "%2", name: "abel", st: urgent, w: 12, since: 1},
		{pane: "%3", name: "dai/compact-warn", st: done, w: 3, since: 1, ctx: "88", sub: 12},
		{pane: "%4", name: "portal/2747-ident", st: proc, w: 4, since: 1, flag: "prio"},
		{pane: "%6", name: "hovered", st: done, w: 7, since: 1},
		{pane: "%5", name: "abel/lgpd-export", st: urgent, w: 5, since: 1, flag: "paused"},
		{pane: "%7", name: "dai/live", st: done, w: 1, since: 1, ctx: "40", dai: true},
	}
	th := themeFor(false)
	for _, comp := range []bool{false, true} {
		for _, w := range []int{27, 40, 14} {
			m := model{w: w, h: 40, comp: comp, rows: rows, current: "%1", hover: "%6"}
			for idx, it := range m.rows {
				for _, ln := range strings.Split(m.renderItem(it, idx, th), "\n") {
					if got := lipgloss.Width(ln); got > m.rowW() {
						t.Errorf("comp=%v w=%d item %d: row is %d wide, rowW is %d",
							comp, w, idx, got, m.rowW())
					}
				}
			}
		}
	}
}

// The focus rule spans the full width — it used to light 40% and stop, which
// read as a progress bar. Width is what the layout depends on.
func TestFocusRuleWidth(t *testing.T) {
	for _, w := range []int{4, 12, 25, 60} {
		if got := lipgloss.Width(focusRule(w)); got != w {
			t.Errorf("focusRule(%d) is %d wide", w, got)
		}
	}
	if lerp(0x2dccd3, 0x24494b, 0, 10) != "#2dccd3" {
		t.Errorf("lerp should start on the bright end, got %v", lerp(0x2dccd3, 0x24494b, 0, 10))
	}
	if lerp(0x2dccd3, 0x24494b, 10, 10) != "#24494b" {
		t.Errorf("lerp should land on the dim end, got %v", lerp(0x2dccd3, 0x24494b, 10, 10))
	}
}

// The pinned unread mark exists so the session you are looking at can still be
// green; if it read like a plain "done" the auto-read would wipe it next poll.
func TestUnreadMark(t *testing.T) {
	if got := statusOf("unread", screen{}, true); got != done {
		t.Errorf("unread should render green, got %v", got)
	}
	if got := statusOf("unread", screen{busy: true}, true); got != proc {
		t.Errorf("a session back at work outranks the pin, got %v", got)
	}
}

// The legend must never be wider than the row, at any width.
func TestLegendFits(t *testing.T) {
	for w := 8; w <= 60; w++ {
		m := model{w: w}
		if got := lipgloss.Width(m.legend()); got > m.rowW() {
			t.Errorf("w=%d: legend is %d wide, rowW is %d", w, got, m.rowW())
		}
	}
}

// soundRow has to name the counter row in both layouts: a click there toggles
// the sound instead of jumping to a session.
func TestSoundRowIsTheCounterRow(t *testing.T) {
	rows := []sess{{pane: "%1", name: "one", st: idle, w: 1}}
	for _, top := range []bool{false, true} {
		m := model{w: 28, h: 30, top: top, rows: rows}
		m.recomputeDensity()
		lines := strings.Split(m.View(), "\n")
		if !strings.Contains(lines[m.soundRow()], "👾") {
			t.Errorf("top=%v: row %d is not the counter row: %q", top, m.soundRow(), lines[m.soundRow()])
		}
	}
}

// The render caches are only safe while their keys cover every input their
// renderer reads. These two pin that: the first that styling a line on its own
// is what styling the block does, the second that a cache hit is byte-for-byte
// the render it stands in for.

func TestPanelPerLineMatchesBlock(t *testing.T) {
	rows := []sess{
		{pane: "%1", name: "alphamark", st: idle, w: 1, since: 1},
		{pane: "%2", name: "betamark", st: done, w: 2, since: 1},
		{pane: "%3", name: "gammamark", st: proc, w: 3, since: 1},
		{pane: "%4", name: "deltamark", st: urgent, w: 4, since: 1},
	}
	for _, h := range []int{81, 44, 24, 14} {
		for _, w := range []int{38, 28, 22} {
			m := model{w: w, h: h, rows: rows, pulse: map[string]int{},
				pcache: map[string]string{}}
			m.recomputeDensity()
			body := m.View() // fills pcache via panelLines
			plain := model{w: w, h: h, rows: rows, pulse: map[string]int{}}
			plain.recomputeDensity()
			if want := plain.View(); body != want {
				t.Errorf("w=%d h=%d: panelLines diverge de panel", w, h)
			}
		}
	}
}

func TestItemCacheMatchesRender(t *testing.T) {
	th := themeFor(false)
	names := []string{"abel/segurança", "pp/semana-35", "x", "um nome bem comprido que trunca"}
	for _, comp := range []bool{false, true} {
		for _, st := range []state{idle, done, proc, urgent} {
			for _, nm := range names {
				for _, since := range []int64{0, 1, time.Now().Unix() - 90} {
					for _, pulse := range []int{0, 3} {
						for frame := 0; frame < 9; frame++ {
							for _, sel := range []bool{false, true} {
								for _, cur := range []bool{false, true} {
									it := sess{pane: "%7", name: nm, st: st, w: 3, since: since}
									m := model{w: 38, h: 44, rows: []sess{it}, comp: comp,
										frame: frame, pulse: map[string]int{"%7": pulse},
										icache: map[itemKey]string{}}
									if sel {
										m.sel = 0
									} else {
										m.sel = -1
									}
									if cur {
										m.current = "%7"
									}
									want := m.renderItem(it, 0, th)
									// twice: the first fills the cache, the second must hit it
									if got := m.itemLines(it, 0, th); got != want {
										t.Fatalf("miss diverge: st=%v comp=%v frame=%d sel=%v cur=%v nm=%q",
											st, comp, frame, sel, cur, nm)
									}
									if got := m.itemLines(it, 0, th); got != want {
										t.Fatalf("hit diverge: st=%v comp=%v frame=%d sel=%v cur=%v nm=%q",
											st, comp, frame, sel, cur, nm)
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

// A cache that never invalidates would show the old frame forever: walking the
// spinner through its cycle has to produce a different row each step.
func TestItemCacheFollowsSpinner(t *testing.T) {
	th := themeFor(false)
	it := sess{pane: "%7", name: "trabalhando", st: proc, w: 3, since: 1}
	m := model{w: 38, h: 44, rows: []sess{it}, sel: -1,
		pulse: map[string]int{}, icache: map[itemKey]string{}}
	seen := map[string]bool{}
	for f := 0; f < len(spinnerFrames); f++ {
		m.frame = f
		seen[m.itemLines(it, 0, th)] = true
	}
	if len(seen) != len(spinnerFrames) {
		t.Errorf("spinner rendeu %d quadros distintos, esperava %d", len(seen), len(spinnerFrames))
	}
}

// Context rides the same cached row, so it has to be part of the key: a session
// burning through its window would otherwise keep its old picture. A full row
// shows the meter, a compact one has no room for it and falls back to the mark.
func TestCtxShownAndCached(t *testing.T) {
	th := themeFor(false)
	for _, comp := range []bool{false, true} {
		want := ctxCellFull // the meter
		if comp {
			want = "◔88%"
		}
		m := model{w: 38, h: 44, sel: -1, comp: comp,
			pulse: map[string]int{}, icache: map[itemKey]string{}}
		it := sess{pane: "%7", name: "dai/compact-warn", st: idle, w: 3, since: 1}
		if got := m.itemLines(it, 0, th); strings.Contains(got, want) {
			t.Errorf("comp=%v: nothing published yet, %q not expected", comp, want)
		}
		it.ctx = "88"
		if got := m.itemLines(it, 0, th); !strings.Contains(got, want) {
			t.Errorf("comp=%v: ctx=88 should show %q", comp, want)
		}
	}
}

// The meter shows on a session at risk or on the one in focus, and nowhere
// else. itemHeight has to agree with the render or every row below it is drawn
// in the wrong place and hitTest picks the wrong session.
func TestCtxShownMatchesHeight(t *testing.T) {
	th := themeFor(false)
	for _, tc := range []struct {
		ctx  string
		cur  bool
		want bool
	}{
		{"20", false, false}, // calm and not in focus: no meter
		{"20", true, true},   // in focus: show it whatever the figure
		{"70", false, true},  // at the warning: show it wherever it is
		{"99", false, true},
		{"", true, false}, // nothing published yet
	} {
		it := sess{pane: "%1", name: "s", st: idle, w: 1, since: 1, ctx: tc.ctx}
		m := model{w: 28, h: 40, sel: -1, rows: []sess{it}}
		if tc.cur {
			m.current = it.pane
		}
		if got := m.ctxShown(it); got != tc.want {
			t.Errorf("ctx=%q cur=%v: shown=%v, want %v", tc.ctx, tc.cur, got, tc.want)
		}
		got := len(strings.Split(m.renderItem(it, 0, th), "\n"))
		if want := m.itemHeight(it, false); got != want {
			t.Errorf("ctx=%q cur=%v: rendered %d lines, itemHeight says %d", tc.ctx, tc.cur, got, want)
		}
	}
}

// The mark is the compact row's only channel, so it must stay quiet until the
// session is actually low — otherwise it just repeats what the age line says.
func TestCtxTagOnlyWhenLow(t *testing.T) {
	for _, tc := range []struct {
		ctx  string
		want string
	}{{"", ""}, {"40", ""}, {"84", ""}, {"85", "◔85%"}, {"97", "◔97%"}} {
		if got := ctxTag(sess{ctx: tc.ctx}); got != tc.want {
			t.Errorf("ctx=%q: tag %q, want %q", tc.ctx, got, tc.want)
		}
	}
}

// The meter fills from the used end and never overflows its cells.
func TestCtxMeterWidth(t *testing.T) {
	for _, used := range []int{0, 1, 42, 75, 99, 100} {
		fill, track := ctxMeter(used, 12)
		if got := lipgloss.Width(fill + track); got != 12 {
			t.Errorf("used=%d: meter is %d cells, want 12", used, got)
		}
	}
	if fill, _ := ctxMeter(0, 12); fill != "" {
		t.Errorf("used=0 should draw no fill, got %q", fill)
	}
	if _, track := ctxMeter(100, 12); track != "" {
		t.Errorf("used=100 should leave no track, got %q", track)
	}
}

// From the far end: unmarked, priority, paused against the counters; inside a
// band, tmux order holds.
func TestArrange(t *testing.T) {
	names := func(rows []sess) string {
		var out []string
		for _, r := range rows {
			out = append(out, r.name)
		}
		return strings.Join(out, " ")
	}
	mk := func() []sess {
		return []sess{
			{name: "a", flag: "prio"}, {name: "b"}, {name: "c", flag: "paused"},
			{name: "d"}, {name: "e", flag: "prio"}, {name: "f", flag: "paused"},
		}
	}
	rows := mk()
	arrange(rows, false)
	if got := names(rows); got != "b d a e c f" {
		t.Errorf("bottom-up: %q", got)
	}
	rows = mk()
	arrange(rows, true)
	if got := names(rows); got != "c f a e b d" {
		t.Errorf("top-down: %q", got)
	}
	// The Dai session leads in both layouts, whatever its mark.
	for _, top := range []bool{false, true} {
		rows = append(mk(), sess{name: "dai", dai: true, flag: "prio"})
		arrange(rows, top)
		if rows[0].name != "dai" {
			t.Errorf("top=%v: Dai session not first: %q", top, names(rows))
		}
	}
}

// The band headings and gaps are rows that belong to no session; a click on a
// session's name has to land on that session in both layouts and densities.
func TestHitTestMatchesBands(t *testing.T) {
	base := []sess{
		{pane: "%1", name: "alphamark", st: idle, w: 1, flag: "paused"},
		{pane: "%2", name: "betamark", st: done, w: 2},
		{pane: "%3", name: "gammamark", st: proc, w: 3, flag: "prio"},
		{pane: "%4", name: "deltamark", st: urgent, w: 4, flag: "paused"},
		{pane: "%5", name: "epsilonmark", st: idle, w: 5},
		{pane: "%6", name: "daimark", st: done, w: 6, dai: true},
	}
	for _, top := range []bool{false, true} {
		for _, h := range []int{60, 24} {
			rows := append([]sess(nil), base...)
			arrange(rows, top)
			m := model{w: 28, h: h, top: top, rows: rows}
			m.recomputeDensity()
			lines := strings.Split(m.View(), "\n")
			for idx, it := range m.rows {
				y := -1
				for i, ln := range lines {
					if strings.Contains(ln, it.name) {
						y = i
						break
					}
				}
				if y < 0 {
					continue // scrolled off
				}
				if got := m.hitTest(y); got != idx {
					t.Errorf("top=%v h=%d comp=%v: %s drawn at %d, hitTest says %d, want %d",
						top, h, m.comp, it.name, y, got, idx)
				}
			}
		}
	}
}

func TestBandHeadFits(t *testing.T) {
	rows := []sess{{flag: "paused"}, {flag: "prio"}}
	for _, w := range []int{14, 27, 40} {
		m := model{w: w, rows: rows}
		for _, fl := range []string{"paused", "prio"} {
			if got := lipgloss.Width(m.bandHead(fl, themeFor(false))); got > m.rowW() {
				t.Errorf("w=%d %s: heading is %d wide, rowW is %d", w, fl, got, m.rowW())
			}
		}
	}
}

// The menu has to stay inside the list area, off the counters, and leave its
// session's name in view — in both layouts, for a session at either end.
func TestMenuPlacement(t *testing.T) {
	base := []sess{
		{pane: "%1", name: "alphamark", st: idle, w: 1},
		{pane: "%2", name: "betamark", st: done, w: 2, flag: "prio"},
		{pane: "%3", name: "gammamark", st: idle, w: 3, flag: "paused"},
		{pane: "%4", name: "deltamark", st: proc, w: 4},
	}
	for _, top := range []bool{false, true} {
		rows := append([]sess(nil), base...)
		arrange(rows, top)
		m := model{w: 28, h: 50, top: top, rows: rows}
		m.recomputeDensity()
		lo, hi := 0, m.h-chromeRows
		if top {
			lo, hi = chromeRows, m.h
		}
		for _, it := range m.rows {
			m.menu = &menu{pane: it.pane}
			y, n := m.menuTop(), len(m.menuOpts())+3
			if y < lo || y+n > hi {
				t.Errorf("top=%v %s: menu at %d..%d, list area is %d..%d", top, it.name, y, y+n, lo, hi)
			}
			lines := strings.Split(m.View(), "\n")
			for r, ln := range lines {
				if got := lipgloss.Width(ln); got != m.w {
					t.Errorf("top=%v %s: row %d is %d wide with the menu open, want %d", top, it.name, r, got, m.w)
				}
			}
			if !strings.Contains(lines[y], "╭") {
				t.Errorf("top=%v %s: no menu box at row %d", top, it.name, y)
			}
			for k := 0; k < n-3; k++ {
				if got := m.menuHit(m.menuX()+2, y+1+k); got != k {
					t.Errorf("top=%v %s: row %d hits option %d, want %d", top, it.name, y+1+k, got, k)
				}
			}
			seen := false
			for _, ln := range lines {
				seen = seen || strings.Contains(ln, it.name)
			}
			if !seen {
				t.Errorf("top=%v: the menu covers %s", top, it.name)
			}
			if m.menuHit(m.menuX()+2, y) != -1 {
				t.Errorf("top=%v %s: the box's top edge counts as an option", top, it.name)
			}
		}
	}
}

// A shell split beside claude can hold the active pane; the session is still
// the one on screen, so it keeps its arrow, its meter and the cursor.
func TestCurrentPaneFollowsWindow(t *testing.T) {
	rows := []sess{{pane: "%16", sessWin: "0:9"}, {pane: "%12", sessWin: "0:7"}}
	for _, c := range []struct{ pane, win, want string }{
		{"%16", "0:9", "%16"}, // claude itself is active
		{"%41", "0:9", "%16"}, // a shell beside it is
		{"%5", "0:3", "%5"},   // a window with no claude in it
	} {
		if got := currentPane(rows, c.pane, c.win); got != c.want {
			t.Errorf("currentPane(%s, %s) = %s, want %s", c.pane, c.win, got, c.want)
		}
	}
}

// The first frame is drawn before the window size arrives (h 0), and a short
// bar can be smaller than the pinned Dai block: neither may panic.
func TestPinnedViewAnyHeight(t *testing.T) {
	rows := []sess{
		{pane: "%1", name: "a", st: idle, w: 1},
		{pane: "%2", name: "b", st: done, w: 2, flag: "prio"},
		{pane: "%9", name: "dai/live", st: urgent, w: 9, dai: true},
	}
	arrange(rows, false)
	for _, h := range []int{0, 1, 2, 3, 5, 8, 40} {
		m := model{w: 28, h: h, rows: rows}
		m.recomputeDensity()
		if got := len(strings.Split(m.View(), "\n")); h > 0 && got > h+2 {
			t.Errorf("h=%d: view has %d lines", h, got)
		}
	}
}

// With a Dai session up, the mascot is lit while its pane is in front of you;
// without one, the bar's own focus lights it, as before.
func TestMascotLit(t *testing.T) {
	dai := []sess{{pane: "%1", name: "a"}, {pane: "%9", name: "dai/live", dai: true}}
	for _, c := range []struct {
		rows             []sess
		termFoc, focused bool
		current          string
		want             bool
	}{
		{dai, true, false, "%9", true},
		{dai, true, true, "%1", false},
		{dai, false, true, "%9", false},
		{nil, false, true, "", true},
		{nil, true, false, "%1", false},
	} {
		m := model{rows: c.rows, termFoc: c.termFoc, focused: c.focused, current: c.current}
		if got := m.mascotLit(); got != c.want {
			t.Errorf("%+v: got %v, want %v", c, got, c.want)
		}
	}
}

// A click on a badge lands on the badge drawn there, in both sizes of mascot.
func TestBadgeAtMatchesRender(t *testing.T) {
	rows := []sess{{pane: "%1", name: "a", st: idle, w: 1}, {pane: "%9", name: "dai/live", st: done, w: 9, dai: true}}
	arrange(rows, false)
	for _, w := range []int{28, 36, 45} {
		m := model{w: w, h: 60, rows: rows, live: "armed", watch: "recording"}
		m.recomputeDensity()
		g := m.topGeo()
		if m.mascotH() == 0 || g.bx < 0 {
			t.Fatalf("w=%d: no badges drawn", w)
		}
		lines := strings.Split(m.View(), "\n")
		for _, b := range []struct {
			name string
			y    int
		}{{"live", mascotPad + g.liveY}, {"watch", mascotPad + g.watchY}} {
			if got := m.badgeAt(g.bx+1, b.y); got != b.name {
				t.Errorf("w=%d: click on %s at row %d says %q", w, b.name, b.y, got)
			}
			if !strings.Contains(lines[b.y], map[string]string{"live": badgeLive, "watch": badgeWatch}[b.name]) {
				t.Errorf("w=%d: no %s badge on row %d: %q", w, b.name, b.y, lines[b.y])
			}
		}
		if got := m.badgeAt(0, mascotPad+g.liveY); got != "" {
			t.Errorf("w=%d: a click left of the badges hit %q", w, got)
		}
	}
}

// At full size the badges share the arm's column but never cover a pixel of the
// mascot, arms down or up.
func TestBadgesClearMascot(t *testing.T) {
	m := model{w: 45, h: 60, live: "armed", watch: "recording"}
	m.recomputeDensity()
	g := m.topGeo()
	if g.pw != 2 {
		t.Fatalf("pw=%d, want the full-size mascot", g.pw)
	}
	for _, bmp := range [][]string{invaderDown, invaderUp} {
		art := pixels(bmp, g.pw)
		for _, y := range []int{g.liveY, g.watchY} {
			r := []rune(art[y])
			for c := g.bx - g.x; c < min(len(r), g.bx-g.x+badgeW); c++ {
				if r[c] != ' ' {
					t.Errorf("badge on line %d covers the mascot at column %d: %q", y, c, art[y])
				}
			}
		}
	}
}
