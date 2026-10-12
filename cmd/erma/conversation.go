// erma serve — per-session conversation endpoints: transcript, SSE stream,
// reply box (text), quick keys, and the read ack. Every state-changing POST
// goes through two gates (Origin match + CSRF token) and resolves the sid to
// a tmux pane via the registry; a sid the registry doesn't know is a 404.
//
// Security: tmux is only ever invoked via exec.Command("tmux", argv…); the
// user's text goes to `tmux load-buffer -b <buf> -` on stdin, NEVER on argv.
// `sh -c` is never used — the review tests assert both.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/daronco/ermahgherdr/internal/session"
)

// tmuxRunner: the single abstraction every tmux call in this file goes
// through. The default implementation builds exec.Command and runs it; the
// tests swap in a recording stub so no real tmux is spawned, and so the
// "argv-only" and "stdin-only" invariants can be asserted directly.
//
// Spec contract (asserted by TestTmuxArgvOnly):
//   - run("tmux", argv…) — no sh -c, no string concatenation, no shell.
//   - When stdin is non-nil, it carries the text payload; the payload is
//     NEVER on argv.
type tmuxRunner func(stdin io.Reader, argv ...string) error

// defaultTmux: the one real implementation.
func defaultTmux(stdin io.Reader, argv ...string) error {
	cmd := exec.Command("tmux", argv...) // no sh -c, no string interpolation
	if stdin != nil {
		cmd.Stdin = stdin
	}
	return cmd.Run()
}

// convHub: per-session state the conversation routes need across requests.
//
//   - csrf: a random 32-byte token per sid, cached until the server restarts.
//     The conversation GET returns it; every state-changing POST requires it in
//     the X-Erma-CSRF header. One sid → one token; a replay beats a reused
//     string, which is enough against a dumb attacker behind the Tailscale
//     perimeter.
//   - paneResolver: sid → pane, defaults to session.PaneSessions(). A test
//     replaces this with a static map so no tmux is required.
//   - tmux: the runner above. A test replaces it with a recording stub.
//   - mark: the read-ack call. Default is session.MarkRead; a test counts
//     invocations.
//   - cursors: cursor the SSE loop advanced to for the last subscriber,
//     so a reconnect does not resend the whole transcript. (The handler itself
//     is stateful — this is just a hint for log clarity.)
type convHub struct {
	mu           sync.Mutex
	csrf         map[string]string
	paneResolver func() map[string]string
	tmux         tmuxRunner
	mark         func(pane string)
	// stateFn returns the current snapshot — the conversation stream reuses
	// the main hub's gather path to emit `state` events.
	stateFn func() []apiSession
	// stateSubscribe hooks the main sseHub so the per-session stream wakes
	// on bar updates without its own tmux fork.
	stateSubscribe   func() chan []apiSession
	stateUnsubscribe func(chan []apiSession)
}

func newConvHub() *convHub {
	return &convHub{
		csrf:         map[string]string{},
		paneResolver: session.PaneSessions,
		tmux:         defaultTmux,
		mark:         session.MarkRead,
	}
}

// tokenFor returns the per-sid CSRF token, minting one on first ask.
func (c *convHub) tokenFor(sid string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.csrf[sid]; ok {
		return t
	}
	var b [32]byte
	_, _ = rand.Read(b[:])
	t := hex.EncodeToString(b[:])
	c.csrf[sid] = t
	return t
}

// checkToken: constant-time compare so a timing attacker cannot peel it off.
func (c *convHub) checkToken(sid, got string) bool {
	c.mu.Lock()
	want, ok := c.csrf[sid]
	c.mu.Unlock()
	if !ok || got == "" {
		return false
	}
	if len(got) != len(want) {
		return false
	}
	var diff byte
	for i := 0; i < len(want); i++ {
		diff |= want[i] ^ got[i]
	}
	return diff == 0
}

// resolvePane: sid → pane via the registry. "" + false when the registry does
// not currently know the sid. (The spec: the client NEVER passes a path or a
// pane; the server does the lookup every time.)
func (c *convHub) resolvePane(sid string) (string, bool) {
	for pane, s := range c.paneResolver() {
		if s == sid {
			return pane, true
		}
	}
	return "", false
}

// extractSid: the one place the "/api/sessions/{id}/<rest>" shape is parsed.
// Returns sid, trailing path (after the id, with the leading slash) and ok.
func extractSid(path string) (sid, tail string, ok bool) {
	const prefix = "/api/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := path[len(prefix):]
	i := strings.Index(rest, "/")
	if i < 0 {
		return rest, "", rest != ""
	}
	return rest[:i], rest[i:], rest[:i] != ""
}

// originMatches: a POST is accepted only when the Origin header's host matches
// the request's own Host. A missing Origin is rejected: the PWA sets it on
// every fetch, so a request without one is either a browser we did not plan
// for or a direct curl, and both deserve a 403.
func originMatches(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// --- route handlers ------------------------------------------------------

type conversationBody struct {
	Csrf   string         `json:"csrf"`
	Cursor session.Cursor `json:"cursor"`
	Turns  []session.Turn `json:"turns"`
}

func (s *server) handleConversation(w http.ResponseWriter, r *http.Request, sid string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Require the sid to live in the registry; a GET for an unknown id is a
	// 404 the same as a POST, so a probe can't tell them apart.
	if _, ok := s.conv.resolvePane(sid); !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	var cur session.Cursor
	if v := r.URL.Query().Get("after"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			cur = session.Cursor(n)
		}
	}
	turns, next, err := session.ReadConversation(sid, cur)
	if err != nil {
		if errors.Is(err, session.ErrUnknownSession) {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		http.Error(w, "read transcript: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	token := s.conv.tokenFor(sid)
	w.Header().Set("X-Erma-CSRF", token)
	_ = json.NewEncoder(w).Encode(conversationBody{
		Csrf:   token,
		Cursor: next,
		Turns:  turns,
	})
}

// streamEvent: one SSE frame.
type streamEvent struct {
	Event string
	Data  any
}

// handleConversationStream: SSE for one sid. On connect, emits:
//   - one `conversation` event with the full transcript and the current cursor
//   - one `state` event with the session's current apiSession row
//
// Then it tails the file (same 600 ms cadence as the bar) and emits a `delta`
// event every time new turns show up, plus a `state` event whenever the main
// hub broadcasts a change to this sid.
func (s *server) handleConversationStream(w http.ResponseWriter, r *http.Request, sid string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.conv.resolvePane(sid); !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	fmt.Fprintf(w, "retry: 3000\n\n")

	// Initial transcript dump.
	turns, cursor, err := session.ReadConversation(sid, 0)
	if err != nil && !errors.Is(err, session.ErrUnknownSession) {
		http.Error(w, "read transcript: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeSSE(w, "conversation", conversationBody{Cursor: cursor, Turns: turns})
	flusher.Flush()

	// Initial state snapshot: find the row for this sid in the main hub.
	if row, ok := findRow(s.hub, sid); ok {
		writeSSE(w, "state", row)
		flusher.Flush()
	}

	// Subscribe to the main hub for state echoes.
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	// Tail the transcript at the bar's cadence.
	tick := time.NewTicker(servePollInterval)
	defer tick.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			newTurns, next, err := session.ReadConversation(sid, cursor)
			if err != nil {
				// The session file may vanish mid-stream (claude crashed, file
				// rotated). Keep the stream open — the next tick may recover.
				continue
			}
			if next != cursor && len(newTurns) > 0 {
				cursor = next
				writeSSE(w, "delta", conversationBody{Cursor: cursor, Turns: newTurns})
				flusher.Flush()
			} else {
				cursor = next
			}
		case snap := <-ch:
			if row, ok := findRowIn(snap, sid); ok {
				writeSSE(w, "state", row)
				flusher.Flush()
			}
		}
	}
}

// findRow: look up the apiSession for sid in the hub's latest snapshot. ok is
// false when the hub has not emitted yet (the stream handler then skips the
// initial state echo rather than send a zero row).
func findRow(h *sseHub, sid string) (apiSession, bool) {
	snap, ok := h.snapshot()
	if !ok {
		return apiSession{}, false
	}
	return findRowIn(snap, sid)
}

func findRowIn(rows []apiSession, sid string) (apiSession, bool) {
	for _, r := range rows {
		if r.ID == sid {
			return r, true
		}
	}
	return apiSession{}, false
}

type textBody struct {
	Text string `json:"text"`
}

func (s *server) handleText(w http.ResponseWriter, r *http.Request, sid string) {
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
	var body textBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Text == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.sendTextToPane(pane, body.Text); err != nil {
		http.Error(w, "tmux: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sendTextToPane: the three tmux calls that make typing on the phone show up
// in a Claude pane. Text goes to `load-buffer -b <buf> -` via stdin — argv is
// static (buffer name + "-"), so a payload with "\n" or shell metacharacters
// cannot interact with argument parsing anywhere along the way.
func (s *server) sendTextToPane(pane, text string) error {
	buf, err := randomBuffer()
	if err != nil {
		return err
	}
	if err := s.conv.tmux(strings.NewReader(text),
		"load-buffer", "-b", buf, "-",
	); err != nil {
		return fmt.Errorf("load-buffer: %w", err)
	}
	// -p enables bracketed paste so the pane doesn't execute lines mid-stream.
	// -t <pane> is the target. Even though we use send-keys to press Enter
	// afterwards, -p alone will not submit the paste.
	if err := s.conv.tmux(nil,
		"paste-buffer", "-p", "-b", buf, "-t", pane, "-d",
	); err != nil {
		return fmt.Errorf("paste-buffer: %w", err)
	}
	if err := s.conv.tmux(nil, "send-keys", "-t", pane, "Enter"); err != nil {
		return fmt.Errorf("send-keys Enter: %w", err)
	}
	return nil
}

// randomBuffer: short random hex suffix. load-buffer overwrites a buffer with
// the same name, so even a collision would not corrupt a concurrent request —
// the suffix just keeps the tmux buffer list tidy.
func randomBuffer() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "erma-" + hex.EncodeToString(b[:]), nil
}

type keyBody struct {
	Key string `json:"key"`
}

// keyAllowlist: the full set of key names the server will send to tmux. The
// map value is the exact tmux key name — the mapping is server-side so a
// client cannot invent a tmux key by typing it as the request body.
var keyAllowlist = map[string]string{
	"esc":    "Escape",
	"enter":  "Enter",
	"1":      "1",
	"2":      "2",
	"3":      "3",
	"ctrl-c": "C-c",
}

func (s *server) handleKey(w http.ResponseWriter, r *http.Request, sid string) {
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
	tmuxKey, ok := keyAllowlist[strings.ToLower(strings.TrimSpace(body.Key))]
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

func (s *server) handleRead(w http.ResponseWriter, r *http.Request, sid string) {
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
	s.conv.mark(pane)
	w.WriteHeader(http.StatusNoContent)
}

// serveSessionRoute dispatches /api/sessions/{id}/<tail> to the right handler.
// Unknown tails are 404.
func (s *server) serveSessionRoute(w http.ResponseWriter, r *http.Request) {
	sid, tail, ok := extractSid(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch tail {
	case "/conversation":
		s.handleConversation(w, r, sid)
	case "/stream":
		s.handleConversationStream(w, r, sid)
	case "/text":
		s.handleText(w, r, sid)
	case "/key":
		s.handleKey(w, r, sid)
	case "/read":
		s.handleRead(w, r, sid)
	default:
		http.NotFound(w, r)
	}
}

// contextDone: helper so the stream handler stops reading transcripts the
// moment the client leaves. Exported for the test that drives it.
func contextDone(ctx context.Context) <-chan struct{} { return ctx.Done() }
