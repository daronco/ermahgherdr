package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daronco/ermahgherdr/internal/testclaude"
)

func TestMain(m *testing.M) {
	testclaude.Main()
	os.Exit(m.Run())
}

// A private tmux server (-L, never the user's) with one pane running "claude"
// and one plain shell. The Claude registry knows a live session for the first
// and carries a stale entry — live pid, transcript on disk — that points a
// ghost sid at the shell. Only the first is a session the server discovered.
type tmuxRig struct {
	dir                  string
	sock                 string
	realPane, ghostPane  string
	realOut, ghostMarker string
}

func newTmuxRig(t *testing.T) *tmuxRig {
	t.Helper()
	for _, bin := range []string{"tmux", "bash"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	rig := &tmuxRig{dir: t.TempDir(), sock: fmt.Sprintf("erma-conv-test-%d", os.Getpid())}
	claude := testclaude.Link(t, rig.dir)
	rig.realOut = filepath.Join(rig.dir, "real.out")
	rig.ghostMarker = filepath.Join(rig.dir, "ghost-ran")

	rig.tm(t, "new-session", "-d", "-x", "120", "-y", "30", "exec "+claude+" > "+rig.realOut)
	path := rig.tm(t, "display-message", "-p", "#{socket_path}")
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", rig.sock, "kill-server").Run()
		_ = os.Remove(path)
	})
	rig.tm(t, "new-window", "-d", "exec bash --norc --noprofile")

	pids := map[string]string{}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		for _, ln := range strings.Split(rig.tm(t, "list-panes", "-a", "-F", "#{pane_id} #{pane_pid} #{pane_current_command}"), "\n") {
			f := strings.Fields(ln)
			if len(f) != 3 {
				continue
			}
			switch f[2] {
			case "claude":
				rig.realPane, pids[f[0]] = f[0], f[1]
			case "bash":
				rig.ghostPane = f[0]
			}
		}
		if rig.realPane != "" && rig.ghostPane != "" {
			break
		}
	}
	if rig.realPane == "" || rig.ghostPane == "" {
		t.Fatal("fixture panes never came up")
	}

	cfg := filepath.Join(rig.dir, "cfg")
	t.Setenv("TMUX", path+",0,0")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", rig.dir)
	t.Setenv("XDG_STATE_HOME", rig.dir)
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg := func(pid, sid, pane string) string {
		return fmt.Sprintf(`{"pid":%s,"sessionId":%q,"tmux":"0:@0.%s"}`, pid, sid, pane)
	}
	write(filepath.Join(cfg, "sessions", "real.json"), reg(pids[rig.realPane], "real-sid", rig.realPane))
	write(filepath.Join(cfg, "sessions", "stale.json"), reg(fmt.Sprint(os.Getpid()), "ghost-sid", rig.ghostPane))
	line := `{"type":"user","message":{"role":"user","content":"hi"}}` + "\n"
	write(filepath.Join(cfg, "projects", "p", "real-sid.jsonl"), line)
	write(filepath.Join(cfg, "projects", "p", "ghost-sid.jsonl"), line)
	return rig
}

func (rig *tmuxRig) tm(t *testing.T, a ...string) string {
	t.Helper()
	out, err := exec.Command("tmux", append([]string{"-L", rig.sock, "-f", "/dev/null"}, a...)...).Output()
	if err != nil {
		t.Fatalf("tmux %v: %v", a, err)
	}
	return strings.TrimSpace(string(out))
}

// token: the CSRF token GET /conversation hands out for sid, "" when it 404s.
func token(srv *server, sid string) string {
	r := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid+"/conversation", nil)
	r.Header.Set(headerLogin, "ok@example.com")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	return w.Header().Get("X-Erma-CSRF")
}

func postText(srv *server, sid, tok, text string) int {
	body, _ := json.Marshal(textBody{Text: text})
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid+"/text", bytes.NewReader(body))
	r.Header.Set(headerLogin, "ok@example.com")
	r.Header.Set("Origin", "http://"+r.Host)
	if tok == "" {
		tok = strings.Repeat("0", 64)
	}
	r.Header.Set("X-Erma-CSRF", tok)
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	return w.Code
}

// A stale registry entry must not turn a shell into a write target: only the
// panes the server discovered as Claude sessions are.
func TestStaleRegistryCannotReachAShell(t *testing.T) {
	rig := newTmuxRig(t)
	srv := newServer(serveFlags{tailscaleLogin: "ok@example.com"})
	srv.hub.tick() // the live snapshot, from the real tmux above

	if code := postText(srv, "ghost-sid", token(srv, "ghost-sid"), "touch "+rig.ghostMarker); code != http.StatusNotFound {
		t.Errorf("POST /text to the ghost sid: got %d, want 404", code)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(rig.ghostMarker); err == nil {
		t.Error("the shell ran the text sent to the ghost sid")
	}
	if screen := rig.tm(t, "capture-pane", "-p", "-t", rig.ghostPane); strings.Contains(screen, "ghost-ran") {
		t.Errorf("text reached the shell pane:\n%s", screen)
	}

	// Control: the discovered session still takes text, so the 404 above is the
	// gate and not a fixture that cannot deliver anything.
	if code := postText(srv, "real-sid", token(srv, "real-sid"), "hello-real"); code != http.StatusNoContent {
		t.Fatalf("POST /text to the real sid: got %d, want 204", code)
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if b, _ := os.ReadFile(rig.realOut); strings.Contains(string(b), "hello-real") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("text never reached the Claude pane")
		}
	}
}
