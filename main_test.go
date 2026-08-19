package main

import (
	"os"
	"strings"
	"testing"

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
			screen{prompt: true}},
		{"question", "Choose an option:\n❯ 1. Keep it\n  2. Drop it\n", screen{prompt: true}},
		{"typed at the input box", "─── a session ──\n❯ 2. also check the other one\n───\n", screen{}},
		{"a numbered list Claude wrote", "● Steps:\n  1. build\n  2. test\n  3. ship\n─── a session ──\n❯ \n",
			screen{}},
	} {
		if got := readScreen(c.body); got != c.want {
			t.Errorf("%s: readScreen = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// testdata/screen-*.txt are whole panes captured off a live Claude Code, before
// and after answering a real permission prompt.
func TestReadScreenCaptures(t *testing.T) {
	for _, c := range []struct {
		file string
		want screen
	}{
		{"testdata/screen-permission.txt", screen{prompt: true}},
		{"testdata/screen-answered.txt", screen{}},
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

// Every rendered row must fit rowW: one cell over and lipgloss wraps it onto a
// second line, which shifts everything below it and breaks hitTest.
func TestRowsFitWidth(t *testing.T) {
	rows := []sess{
		{pane: "%1", name: "dados financeiros no bigquery", st: idle, w: 6, since: 1},
		{pane: "%2", name: "abel", st: urgent, w: 12, since: 1},
	}
	th := themeFor(false)
	for _, comp := range []bool{false, true} {
		for _, w := range []int{27, 40, 14} {
			m := model{w: w, h: 40, comp: comp, rows: rows, current: "%1"}
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
