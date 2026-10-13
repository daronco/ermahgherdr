// erma serve — new-session launcher and non-Claude pane listing.
//
// POST /api/sessions opens a `claude` window in a validated directory via
// tmux. The command is fixed and goes on tmux's own argv — never through a
// shell — so a payload that tries to carry `; rm -rf ~` as the directory still
// lands as a literal -c argument. GET /api/panes/other returns every pane the
// user has that is NOT running claude so the sidebar can route to the terminal
// view (#8) for them.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"

	"github.com/daronco/ermahgherdr/internal/session"
)

// commandRunner: the seam the real launcher reaches for. The default returns a
// built *exec.Cmd (via exec.Command, no shell). Tests swap in a recorder so the
// assertion against cmd.Args can run without a tmux binary.
type commandRunner func(name string, argv ...string) *exec.Cmd

var defaultCommand commandRunner = exec.Command

// newSessionReq is the JSON body of POST /api/sessions. Only `dir` is honored
// (empty: the configured default directory) — a client-supplied command is
// deliberately ignored and left for the acceptance tests to pin.
type newSessionReq struct {
	Dir string `json:"dir"`
}

// newSessionResp is the response body on success. PaneID is best-effort — tmux
// -P -F '#{pane_id}' usually fills it, but a stubbed runner (tests) or an older
// tmux may leave stdout empty, so the field is omitted when blank.
type newSessionResp struct {
	Dir    string `json:"dir"`
	PaneID string `json:"paneId,omitempty"`
}

func (s *server) handleRecentDirs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dirs := recentDirs(defaultRecentDirsConfig())
	if dirs == nil {
		dirs = []recentDir{}
	}
	token := s.conv.newSessionToken()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Erma-CSRF", token)
	defDir, _ := resolveNewSessionDir(s.flags.newSessionDir)
	_ = json.NewEncoder(w).Encode(struct {
		Csrf    string      `json:"csrf"`    // required as X-Erma-CSRF by POST /api/sessions
		Default string      `json:"default"` // where a session opens when no dir is sent
		Dirs    []recentDir `json:"dirs"`
	}{token, defDir, dirs})
}

func (s *server) handleNewSession(w http.ResponseWriter, r *http.Request) {
	// Method is already gated by handleSessionsRoot.
	if !originMatches(r) {
		http.Error(w, "forbidden: Origin mismatch", http.StatusForbidden)
		return
	}
	if !sameToken(s.conv.newSessionToken(), r.Header.Get("X-Erma-CSRF")) {
		http.Error(w, "forbidden: CSRF", http.StatusForbidden)
		return
	}
	var body newSessionReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	var dir string
	var err error
	if strings.TrimSpace(body.Dir) == "" {
		dir, err = resolveNewSessionDir(s.flags.newSessionDir)
	} else {
		dir, err = validateHomePath(body.Dir)
	}
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Fixed argv: tmux and its arguments live here, never from the client.
	// -d keeps the current client's view unchanged; the sidebar picks the new
	// pane up on its next poll tick.
	runner := s.conv.command
	if runner == nil {
		runner = defaultCommand
	}
	// -P -F '#{pane_id}' asks tmux to print the new pane's id so the client can
	// route to it without a second poll. Still fixed argv — no shell.
	cmd := runner("tmux", "new-window", "-d", "-P", "-F", "#{pane_id}", "-c", dir, "claude")
	out, err := cmd.Output()
	if err != nil {
		http.Error(w, "tmux: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(newSessionResp{Dir: dir, PaneID: strings.TrimSpace(string(out))})
}

// otherPane is the on-the-wire shape of GET /api/panes/other.
type otherPane struct {
	PaneID   string `json:"paneId"`   // tmux pane id ("%42")
	Location string `json:"location"` // session:window:index
	Title    string `json:"title"`    // pane_title, or the command when empty
	Command  string `json:"command"`  // pane_current_command
	Path     string `json:"path"`     // pane_current_path
}

// otherPanesSource lets tests inject a canned tmux reply.
type otherPanesSource func() string

// defaultOtherPanesSource reads the same list-panes call that gather() uses,
// but with the inverse filter (not claude) and a lean format slice.
var defaultOtherPanesSource otherPanesSource = func() string {
	f := strings.Join([]string{
		"#{pane_id}",
		"#{session_name}",
		"#{window_index}",
		"#{pane_index}",
		"#{pane_current_command}",
		"#{pane_current_path}",
		"#{pane_title}",
	}, "\t")
	return session.TmuxOut("list-panes", "-a",
		"-f", "#{!=:#{pane_current_command},claude}",
		"-F", f,
	)
}

// parseOtherPanes decodes the tab-separated list-panes output into the API
// shape the front-end consumes. One malformed line is skipped, not fatal.
// Claude rows are dropped here as defense in depth: tmux's -f filter is the
// primary guard, but AC #5 is an "only vim and bash" guarantee — a filter
// miss or an edge-case tmux build must not leak a claude row to the client.
func parseOtherPanes(out string) []otherPane {
	var rows []otherPane
	for _, line := range strings.Split(out, "\n") {
		c := strings.Split(line, "\t")
		if len(c) < 7 || c[0] == "" {
			continue
		}
		cmd := strings.TrimSpace(c[4])
		if cmd == "claude" {
			continue
		}
		title := strings.TrimSpace(c[6])
		if title == "" {
			title = cmd
		}
		rows = append(rows, otherPane{
			PaneID:   c[0],
			Location: fmt.Sprintf("%s:%s:%s", c[1], c[2], c[3]),
			Title:    title,
			Command:  cmd,
			Path:     strings.TrimSpace(c[5]),
		})
	}
	return rows
}

func (s *server) handleOtherPanes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	src := s.conv.otherPanes
	if src == nil {
		src = defaultOtherPanesSource
	}
	rows := parseOtherPanes(src())
	if rows == nil {
		rows = []otherPane{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Panes []otherPane `json:"panes"`
	}{rows})
}
