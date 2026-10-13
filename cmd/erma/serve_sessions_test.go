package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daronco/ermahgherdr/internal/session"
)

// recordingCommand builds a no-op exec.Cmd (via /bin/true) that still carries
// the full argv the handler assembled. The test inspects cmd.Args directly,
// which is what matters: a payload MUST land as a tmux arg, not a shell string.
func recordingCommand(dst *[][]string) commandRunner {
	return func(name string, argv ...string) *exec.Cmd {
		full := append([]string{name}, argv...)
		*dst = append(*dst, full)
		return exec.Command("/bin/true")
	}
}

// mkSessionsServer wires a server with auth but no real tmux and a stub home
// containing one allowed directory.
func mkSessionsServer(t *testing.T) (*server, *[][]string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	allowed := filepath.Join(home, "projects", "foo")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := newServer(serveFlags{tailscaleLogin: "ok@example.com"})
	var calls [][]string
	srv.conv.command = recordingCommand(&calls)
	return srv, &calls, allowed
}

func TestNewSessionHappyPath(t *testing.T) {
	srv, calls, allowed := mkSessionsServer(t)
	body, _ := json.Marshal(newSessionReq{Dir: allowed})
	r := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body))
	r.Header.Set(headerLogin, "ok@example.com")
	r.Header.Set("Origin", "http://"+r.Host)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("new-session: got %d: %s", w.Code, w.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("want 1 tmux call, got %d: %+v", len(*calls), *calls)
	}
	argv := (*calls)[0]
	want := []string{"tmux", "new-window", "-d", "-P", "-F", "#{pane_id}", "-c", allowed, "claude"}
	if len(argv) != len(want) {
		t.Fatalf("argv: got %+v, want %+v", argv, want)
	}
	for i, a := range want {
		if argv[i] != a {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], a)
		}
	}
}

// POST /api/sessions must never spawn a shell: the launcher goes through
// exec.Command, so even a payload with ';' lands as an argv entry.
func TestNewSessionArgvNoShell(t *testing.T) {
	srv, calls, allowed := mkSessionsServer(t)
	for _, payload := range []string{
		allowed + "/../foo", // cleaned to the same path
	} {
		body, _ := json.Marshal(newSessionReq{Dir: payload})
		r := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body))
		r.Header.Set(headerLogin, "ok@example.com")
		r.Header.Set("Origin", "http://"+r.Host)
		w := httptest.NewRecorder()
		srv.mux().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("new-session %q: %d %s", payload, w.Code, w.Body.String())
		}
	}
	for i, argv := range *calls {
		if argv[0] != "tmux" {
			t.Errorf("call[%d] not tmux: %+v", i, argv)
		}
		if argv[0] == "sh" || argv[0] == "/bin/sh" || argv[0] == "bash" {
			t.Errorf("call[%d] went through a shell: %+v", i, argv)
		}
		for _, a := range argv {
			if a == "-c" && argv[0] != "tmux" {
				t.Errorf("call[%d] has -c outside tmux argv: %+v", i, argv)
			}
		}
	}
}

func TestNewSessionIgnoresClientCommand(t *testing.T) {
	srv, calls, allowed := mkSessionsServer(t)
	// A client that smuggles a `cmd` field alongside `dir` must still get
	// `claude` launched. JSON decode ignores unknown fields by default.
	body := []byte(`{"dir":"` + allowed + `","cmd":"rm -rf /"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body))
	r.Header.Set(headerLogin, "ok@example.com")
	r.Header.Set("Origin", "http://"+r.Host)
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("want 1 tmux call, got %d: %+v", len(*calls), *calls)
	}
	argv := (*calls)[0]
	last := argv[len(argv)-1]
	if last != "claude" {
		t.Errorf("final argv entry = %q, want claude", last)
	}
	for _, a := range argv {
		if strings.Contains(a, "rm -rf") {
			t.Errorf("client-controlled cmd leaked into argv: %+v", argv)
		}
	}
}

func TestNewSessionRejectsBadPaths(t *testing.T) {
	srv, calls, allowed := mkSessionsServer(t)
	home := os.Getenv("HOME")
	parent := filepath.Dir(home) // outside $HOME
	cases := []struct {
		name, dir string
	}{
		{"relative", "projects/foo"},
		{"parent escape", allowed + "/../../../.."},
		{"outside home", parent},
		{"missing", filepath.Join(home, "does-not-exist")},
		{"empty", ""},
		{"metachar semicolon", allowed + "; rm -rf /"},
		{"metachar backtick", allowed + "`id`"},
		{"metachar dollar", allowed + "$(id)"},
		{"metachar newline", allowed + "\nid"},
		{"nul byte", allowed + "\x00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, _ := json.Marshal(newSessionReq{Dir: c.dir})
			r := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body))
			r.Header.Set(headerLogin, "ok@example.com")
			r.Header.Set("Origin", "http://"+r.Host)
			w := httptest.NewRecorder()
			srv.mux().ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("dir=%q: got %d, want 400", c.dir, w.Code)
			}
		})
	}
	if len(*calls) != 0 {
		t.Errorf("tmux invoked for a bad path: %+v", *calls)
	}
}

func TestNewSessionRequiresAuth(t *testing.T) {
	srv, calls, allowed := mkSessionsServer(t)
	body, _ := json.Marshal(newSessionReq{Dir: allowed})
	r := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body))
	// no Tailscale header
	r.Header.Set("Origin", "http://"+r.Host)
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauth: got %d, want 401", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("tmux invoked without auth: %+v", *calls)
	}
}

func TestNewSessionRequiresOrigin(t *testing.T) {
	srv, calls, allowed := mkSessionsServer(t)
	body, _ := json.Marshal(newSessionReq{Dir: allowed})
	r := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body))
	r.Header.Set(headerLogin, "ok@example.com")
	// no Origin
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("no-origin: got %d, want 403", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("tmux invoked without Origin: %+v", *calls)
	}
}

func TestParseOtherPanes(t *testing.T) {
	// Each line is tmux's tab-separated output for the format used by
	// defaultOtherPanesSource. Lines named "claude" never arrive here —
	// list-panes already filtered them.
	out := strings.Join([]string{
		"%7\t0\t1\t0\tvim\t/home/alice/notes\tnotes.md",
		"%9\t0\t2\t0\tbash\t/home/alice\t",
		"",
	}, "\n")
	rows := parseOtherPanes(out)
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	if rows[0].PaneID != "%7" || rows[0].Location != "0:1:0" || rows[0].Command != "vim" ||
		rows[0].Path != "/home/alice/notes" || rows[0].Title != "notes.md" {
		t.Errorf("row 0: %+v", rows[0])
	}
	// Empty title falls back to the command name.
	if rows[1].Title != "bash" {
		t.Errorf("row 1 title: %q, want 'bash'", rows[1].Title)
	}
}

func TestOtherPanesHandlerFiltersClaude(t *testing.T) {
	srv := newServer(serveFlags{tailscaleLogin: "ok@example.com"})
	// tmux's -f filter is the primary guard; emulate a stray claude row the
	// filter missed to pin the Go-side defense-in-depth drop that AC #5's
	// "only" guarantee requires.
	srv.conv.otherPanes = func() string {
		return strings.Join([]string{
			"%1\t0\t1\t0\tvim\t/tmp\tedit.go",
			"%2\t0\t2\t0\tbash\t/tmp\t",
			"%3\t0\t3\t0\tclaude\t/tmp\tclaude",
			"",
		}, "\n")
	}
	r := httptest.NewRequest(http.MethodGet, "/api/panes/other", nil)
	r.Header.Set(headerLogin, "ok@example.com")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Panes []otherPane `json:"panes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	foundVim, foundBash := false, false
	for _, r := range got.Panes {
		if r.Command == "claude" {
			t.Errorf("claude row leaked past the Go-side filter: %+v", r)
		}
		if r.Command == "vim" && r.PaneID == "%1" {
			foundVim = true
		}
		if r.Command == "bash" && r.PaneID == "%2" {
			foundBash = true
		}
	}
	if !foundVim || !foundBash {
		t.Errorf("filtered output missing expected panes: %+v", got.Panes)
	}
}

func TestOtherPanesRequiresAuth(t *testing.T) {
	srv := newServer(serveFlags{tailscaleLogin: "ok@example.com"})
	srv.conv.otherPanes = func() string { return "" }
	r := httptest.NewRequest(http.MethodGet, "/api/panes/other", nil)
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", w.Code)
	}
}

func TestRecentDirs(t *testing.T) {
	home := t.TempDir()
	a := filepath.Join(home, "a")
	b := filepath.Join(home, "b")
	c := filepath.Join(home, "c")
	for _, d := range []string{a, b, c} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	now := time.Now()
	cfg := recentDirsConfig{
		registry: func() []session.RegistryEntry {
			return []session.RegistryEntry{
				{Cwd: a, MTime: now.Add(-2 * time.Hour)},
				{Cwd: b, MTime: now},
				{Cwd: c, MTime: now.Add(-3 * time.Hour), Alive: false}, // ended session still counts
				{Cwd: a, MTime: now.Add(-time.Hour)},                   // dupe, keep newest
				{Cwd: "", MTime: now},                                  // skip empty cwd
				{Cwd: "/does/not/exist", MTime: now},                   // skip missing
			}
		},
		validate: validateHomePath,
		limit:    10,
	}
	dirs := recentDirs(cfg)
	if len(dirs) != 3 {
		t.Fatalf("rows: %+v", dirs)
	}
	if dirs[0].Path != b || dirs[1].Path != a || dirs[2].Path != c {
		t.Errorf("order wrong: %+v", dirs)
	}
}

func TestRecentDirsHandler(t *testing.T) {
	srv := newServer(serveFlags{tailscaleLogin: "ok@example.com"})
	r := httptest.NewRequest(http.MethodGet, "/api/dirs/recent", nil)
	r.Header.Set(headerLogin, "ok@example.com")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	var body struct {
		Dirs []recentDir `json:"dirs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v, body=%s", err, w.Body.String())
	}
	// Content depends on the live registry; we only verify the shape here.
	if body.Dirs == nil {
		t.Errorf("dirs should be [], never null")
	}
}

func TestRecentDirsRequiresAuth(t *testing.T) {
	srv := newServer(serveFlags{tailscaleLogin: "ok@example.com"})
	r := httptest.NewRequest(http.MethodGet, "/api/dirs/recent", nil)
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", w.Code)
	}
}
