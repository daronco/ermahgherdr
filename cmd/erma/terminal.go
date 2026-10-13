// erma serve — Terminal tab endpoints: a GET that snapshots the target pane
// with SGR escapes intact, and a POST that forwards a single key from a
// server-side allowlist. Both reuse the sid → pane resolver and the auth
// middleware, so an id that is not a live Claude session (or a pane with a
// stale registry entry) returns 404 before any tmux call runs.
//
// Scope: snapshot + send-keys only. No PTY, no WebSocket, no batched key
// endpoint — one HTTP request per keystroke, every one through the same
// allowlist.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// terminalKeyAllowlist: the full set of key tokens POST /keys will forward
// to tmux. The map value is the exact tmux `send-keys` key name; the key
// side is the only label the client may send, so a request body cannot
// invent a new tmux key name by typing it. Covers the keys an Android soft
// keyboard cannot type: Esc, Tab, arrows, Enter, Backspace, and the Ctrl
// combos that unstick a shell. C-z and C-d are deliberately absent: C-z
// suspends claude (the pane drops to a shell and leaves the list, ending
// remote access) and C-d can end it.
var terminalKeyAllowlist = map[string]string{
	"Escape": "Escape",
	"Tab":    "Tab",
	"Up":     "Up",
	"Down":   "Down",
	"Left":   "Left",
	"Right":  "Right",
	"Enter":  "Enter",
	"BSpace": "BSpace",
	"C-c":    "C-c",
	"C-r":    "C-r",
	"C-l":    "C-l",
	"C-u":    "C-u",
	"C-w":    "C-w",
	"C-a":    "C-a",
	"C-e":    "C-e",
}

// handleTerminal returns the target pane as it looks on screen right now,
// SGR escapes intact. The front-end polls this at ~500 ms while the Terminal
// tab is active and parses the ANSI client-side.
func (s *server) handleTerminal(w http.ResponseWriter, r *http.Request, sid string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pane, ok := s.conv.resolvePane(sid)
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	out, err := s.conv.tmuxOut("capture-pane", "-e", "-p", "-t", pane)
	if err != nil {
		http.Error(w, "tmux: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}

// handleSendKey forwards one keystroke, from the allowlist, to tmux
// send-keys against the target pane. Free-text send-keys -l is NEVER
// accepted here — the only path to tmux is the server-side lookup.
func (s *server) handleSendKey(w http.ResponseWriter, r *http.Request, sid string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !originMatches(r) {
		http.Error(w, "forbidden: Origin mismatch", http.StatusForbidden)
		return
	}
	if !s.conv.checkToken(sid, r.Header.Get("X-Erma-CSRF")) {
		http.Error(w, "forbidden: CSRF", http.StatusForbidden)
		return
	}
	pane, ok := s.conv.resolvePane(sid)
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	var body keyBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	tmuxKey, ok := terminalKeyAllowlist[strings.TrimSpace(body.Key)]
	if !ok {
		http.Error(w, "bad key: not in allowlist", http.StatusBadRequest)
		return
	}
	if err := s.conv.tmux(nil, "send-keys", "-t", pane, tmuxKey); err != nil {
		http.Error(w, "tmux: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
