package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tmuxFixture starts a private tmux server (-L, never the user's) with one pane
// whose foreground process is named "claude" and whose screen is not blank, so
// Gather sees it. TMUX points this package's bare `tmux` calls at that server.
// The pane carries a stale @erma_flag and a "working" mark on an idle screen —
// the two conditions under which the writing Gather reconciles tmux.
func tmuxFixture(t *testing.T) (opt func(name string) string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not installed")
	}
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	if err := os.Symlink(sleep, claude); err != nil {
		t.Fatal(err)
	}
	sock := fmt.Sprintf("erma-test-%d", os.Getpid())
	tm := func(a ...string) string {
		out, err := exec.Command("tmux", append([]string{"-L", sock, "-f", "/dev/null"}, a...)...).Output()
		if err != nil {
			t.Fatalf("tmux %v: %v", a, err)
		}
		return strings.TrimSpace(string(out))
	}
	tm("new-session", "-d", "-x", "80", "-y", "24", fmt.Sprintf("sh -c 'echo hi; exec %s 300'", claude))
	path := tm("display-message", "-p", "#{socket_path}")
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
		_ = os.Remove(path) // kill-server leaves the socket file behind
	})

	var pane string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if f := strings.Fields(tm("list-panes", "-a", "-F", "#{pane_id} #{pane_current_command}")); len(f) == 2 && f[1] == "claude" {
			pane = f[0]
			break
		}
	}
	if pane == "" {
		t.Fatal("fixture pane never ran claude")
	}
	t.Setenv("TMUX", path+",0,0")
	t.Setenv("HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	// @erma_sound is deliberately left unset, as on a fresh tmux server: PollPanes
	// must still read every pane (tmux >= 3.3 errors on `show -gv` of it).
	tm("set", "-p", "-t", pane, "@erma_flag", "prio")
	tm("set", "-p", "-t", pane, "@erma_wait", "working")
	return func(name string) string { return tm("show", "-p", "-q", "-v", "-t", pane, name) }
}

func TestGatherReadOnlyLeavesTmuxAlone(t *testing.T) {
	opt := tmuxFixture(t)
	permMiss := map[string]int{}
	var rows []Session
	for i := 0; i < 3; i++ {
		rows, _ = GatherReadOnly(permMiss)
	}
	if len(rows) != 1 || rows[0].St != Done || rows[0].Flag != "" {
		t.Fatalf("GatherReadOnly rows = %+v, want one Done row with no flag", rows)
	}
	if got := opt("@erma_wait"); got != "working" {
		t.Errorf("@erma_wait = %q, want it left at %q", got, "working")
	}
	if got := opt("@erma_flag"); got != "prio" {
		t.Errorf("@erma_flag = %q, want it left at %q", got, "prio")
	}
}

// The control: the same fixture does make the writing Gather write, so the
// read-only test above is not passing on a fixture that writes nothing anyway.
func TestGatherWritesBack(t *testing.T) {
	opt := tmuxFixture(t)
	Gather(map[string]int{})
	if got := opt("@erma_wait"); got != "done" {
		t.Errorf("@erma_wait = %q, want %q", got, "done")
	}
	if got := opt("@erma_flag"); got != "" {
		t.Errorf("@erma_flag = %q, want it unset", got)
	}
}
