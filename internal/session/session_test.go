package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Smoke test: the parser is correct wherever it is called from. cmd/erma holds
// the exhaustive table (see cmd/erma/main_test.go); this proves the package is
// usable without the bar.
func TestReadScreenSmoke(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want Screen
	}{
		{"working", "✢ Reticulating… (15m 52s)\n───\n❯ \n───\n", Screen{Busy: true}},
		{"stopped", "✻ Churned for 15s\n───\n❯ \n───\n", Screen{}},
		{"permission", " Do you want to create X?\n ❯ 1. Yes\n   2. No\n\n Esc to cancel\n",
			Screen{Prompt: true, Form: true}},
	} {
		if got := ReadScreen(c.body); got != c.want {
			t.Errorf("%s: ReadScreen = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestStatusOfSmoke(t *testing.T) {
	for _, c := range []struct {
		wait string
		sc   Screen
		seen bool
		want State
	}{
		{"perm", Screen{Prompt: true}, true, Urgent},
		{"", Screen{Form: true}, true, Urgent},
		{"working", Screen{Busy: true}, true, Proc},
		{"done", Screen{}, true, Done},
		{"", Screen{}, true, Idle},
	} {
		if got := StatusOf(c.wait, c.sc, c.seen); got != c.want {
			t.Errorf("StatusOf(%q, %+v, %v) = %v, want %v",
				c.wait, c.sc, c.seen, got, c.want)
		}
	}
}

func TestArrangeSmoke(t *testing.T) {
	rows := []Session{
		{Name: "a", Flag: "prio"}, {Name: "b"}, {Name: "c", Flag: "paused"},
		{Name: "dai", Dai: true},
	}
	Arrange(rows, false)
	if rows[0].Name != "dai" {
		t.Errorf("Dai not first: %v", rows)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if got := strings.Join(names, " "); got != "dai b a c" {
		t.Errorf("order bottom-up: %q", got)
	}
}

func TestBandOf(t *testing.T) {
	for _, c := range []struct {
		fl   string
		want int
	}{{"", 0}, {"prio", 1}, {"paused", 2}, {"other", 0}} {
		if got := BandOf(c.fl); got != c.want {
			t.Errorf("BandOf(%q) = %d, want %d", c.fl, got, c.want)
		}
	}
}

// A `claude -p` started from inside a pane (a server the session runs, a
// script) registers under that pane too, and its file can sort after the
// interactive one's: the pane must still map to the interactive session.
func TestPaneSessionsSkipsHeadless(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	reg := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(reg, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid() // alive, so no file is dropped as a crashed claude
	for name, rec := range map[string][2]string{
		"100.json": {"main", "cli"},
		"200.json": {"child", "sdk-cli"},
	} {
		body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"entrypoint":%q,"tmux":"1:@3.%%7"}`, pid, rec[0], rec[1])
		if err := os.WriteFile(filepath.Join(reg, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := PaneSessions()["%7"]; got != "main" {
		t.Errorf("pane %%7 maps to %q, want the interactive session %q", got, "main")
	}
}

// A crashed claude leaves its registry file behind; PaneSessions keeps only the live.
func TestPaneSessionsSkipsEndedSessions(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	ended := exec.Command("true")
	if err := ended.Run(); err != nil {
		t.Skip("cannot run true")
	}
	write := func(name string, pid int, sid, pane string) {
		p := filepath.Join(cfg, "sessions", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"tmux":"0:@0.%s"}`, pid, sid, pane)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("live.json", os.Getpid(), "live", "%1")
	write("ended.json", ended.Process.Pid, "ended", "%2")

	if ps := PaneSessions(); len(ps) != 1 || ps["%1"] != "live" {
		t.Errorf("PaneSessions = %v, want only the live session", ps)
	}
}
