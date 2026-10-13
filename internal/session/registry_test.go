package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Registry keeps ended sessions (their cwd is still a good place to start the
// next one) and says which are alive; PaneSessions keeps only the live.
func TestRegistryKeepsEndedSessions(t *testing.T) {
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
		body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"tmux":"0:@0.%s","cwd":"/work/%s"}`, pid, sid, pane, sid)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("live.json", os.Getpid(), "live", "%1")
	write("ended.json", ended.Process.Pid, "ended", "%2")

	got := map[string]RegistryEntry{}
	for _, e := range Registry() {
		got[e.Sid] = e
	}
	if len(got) != 2 || !got["live"].Alive || got["ended"].Alive || got["ended"].Cwd != "/work/ended" {
		t.Errorf("Registry = %+v", got)
	}
	if ps := PaneSessions(); len(ps) != 1 || ps["%1"] != "live" {
		t.Errorf("PaneSessions = %v, want only the live session", ps)
	}
}
