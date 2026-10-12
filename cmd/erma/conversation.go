// erma serve — per-session conversation endpoints: transcript, SSE stream,
// reply box (text), quick keys, and the read ack. Every state-changing POST
// needs a matching Origin, a sid that is one of the Claude panes the hub
// discovered (else 404), and that sid's CSRF token.
//
// Security: tmux is only ever invoked via exec.Command("tmux", argv…); the
// user's text goes to `tmux load-buffer -b <buf> -` on stdin, NEVER on argv.
// `sh -c` is never used — the review tests assert both.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"

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
//   - paneResolver: pane → sid for the Claude panes in the hub's live snapshot
//     (livePanes). A test replaces this with a static map so no tmux is required.
//   - tmux: the runner above. A test replaces it with a recording stub.
//   - mark: the read-ack call. Default is session.MarkRead; a test counts
//     invocations.
type convHub struct {
	mu           sync.Mutex
	csrf         map[string]string
	paneResolver func() map[string]string
	tmux         tmuxRunner
	mark         func(pane string)
}

// livePanes: pane → sid for the Claude panes the hub last discovered. The raw
// registry is not enough: an entry whose pid was reused, or that names a pane
// on another tmux server, points a sid at whatever pane now has that id.
func livePanes(h *sseHub) func() map[string]string {
	return func() map[string]string {
		rows, _ := h.snapshot()
		out := map[string]string{}
		for _, r := range rows {
			if r.ID != r.Pane { // a registry-backed sid, not the pane-id fallback
				out[r.Pane] = r.ID
			}
		}
		return out
	}
}

func newConvHub(hub *sseHub) *convHub {
	return &convHub{
		csrf:         map[string]string{},
		paneResolver: livePanes(hub),
		tmux:         defaultTmux,
		mark:         session.MarkRead,
	}
}

// tokenFor returns the per-sid CSRF token, minting one on first ask. Also
// prunes entries for sids no longer in the registry — the map only ever grows
// here, so pruning on the same path keeps it bounded for a long-running serve.
func (c *convHub) tokenFor(sid string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneCSRFLocked()
	if t, ok := c.csrf[sid]; ok {
		return t
	}
	var b [32]byte
	_, _ = rand.Read(b[:])
	t := hex.EncodeToString(b[:])
	c.csrf[sid] = t
	return t
}

// pruneCSRFLocked drops csrf entries whose sid is no longer in the registry.
// Caller holds c.mu. Skips the registry read when the map is empty.
func (c *convHub) pruneCSRFLocked() {
	if len(c.csrf) == 0 {
		return
	}
	alive := map[string]struct{}{}
	for _, s := range c.paneResolver() {
		alive[s] = struct{}{}
	}
	for sid := range c.csrf {
		if _, ok := alive[sid]; !ok {
			delete(c.csrf, sid)
		}
	}
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

// resolvePane: sid → pane among the live Claude panes. "" + false when none
// carries the sid. The client never passes a path or a pane.
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
	Cursor session.Cursor `json:"cursor"` // where the stream picks up (?after=)
	Start  session.Cursor `json:"start"`  // where these turns begin (?before= for the page above; 0 = none)
	Turns  []session.Turn `json:"turns"`
}

// cursorParam: a non-negative byte offset from a query value or header.
func cursorParam(v string) (session.Cursor, bool) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return session.Cursor(n), true
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
	// Default: the tail of the transcript (TailBytes), never the whole file —
	// a long session's jsonl runs to tens of MB. ?before= pages back from a
	// previous `start`; ?after= reads forward from a cursor.
	var (
		turns       []session.Turn
		start, next session.Cursor
		err         error
	)
	q := r.URL.Query()
	if after, ok := cursorParam(q.Get("after")); ok {
		start = after
		turns, next, err = session.ReadConversation(sid, after)
	} else {
		before, _ := cursorParam(q.Get("before"))
		turns, start, next, err = session.ReadConversationBefore(sid, before, session.TailBytes)
	}
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
		Start:  start,
		Turns:  turns,
	})
}

// streamEvent: one SSE frame.
type streamEvent struct {
	Event string
	Data  any
}

// handleConversationStream: SSE for one sid. On connect it sends a `state`
// event with the session's row and, unless the client says where it already is
// (?after=, or Last-Event-ID on a reconnect), a `conversation` event with the
// tail of the transcript.
//
// Then it tails the file via session.WatchTranscript (same 600 ms cadence as
// the bar) and emits a `delta` event every time new turns show up, plus a
// `state` event whenever the main hub broadcasts a change to this sid. Every
// transcript frame carries its cursor as the SSE id, so the browser's
// automatic reconnect resumes instead of replaying.
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
	flusher.Flush() // the client is connected even when nothing else is due yet

	// Initial state snapshot: find the row for this sid in the main hub.
	if row, ok := findRow(s.hub, sid); ok {
		writeSSE(w, "state", row)
		flusher.Flush()
	}

	// Subscribe to the main hub for state echoes.
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	// A Reset frame — the initial tail, or a fresh one after the file rotated
	// or was truncated — is a `conversation` event: the client replaces its list.
	from := session.Cursor(-1)
	if c, ok := cursorParam(r.Header.Get("Last-Event-ID")); ok {
		from = c
	} else if c, ok := cursorParam(r.URL.Query().Get("after")); ok {
		from = c
	}
	ctx := r.Context()
	transcripts := session.WatchTranscript(ctx, sid, servePollInterval, from)

	for {
		select {
		case <-ctx.Done():
			return
		case tr, ok := <-transcripts:
			if !ok {
				return
			}
			event := "delta"
			if tr.Reset {
				event = "conversation"
			}
			fmt.Fprintf(w, "id: %d\n", tr.Cursor)
			writeSSE(w, event, conversationBody{Cursor: tr.Cursor, Start: tr.Start, Turns: tr.Turns})
			flusher.Flush()
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
	pane, ok := s.conv.resolvePane(sid)
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if !s.conv.checkToken(sid, r.Header.Get("X-Erma-CSRF")) {
		http.Error(w, "forbidden: CSRF", http.StatusForbidden)
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
	pane, ok := s.conv.resolvePane(sid)
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if !s.conv.checkToken(sid, r.Header.Get("X-Erma-CSRF")) {
		http.Error(w, "forbidden: CSRF", http.StatusForbidden)
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
	pane, ok := s.conv.resolvePane(sid)
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if !s.conv.checkToken(sid, r.Header.Get("X-Erma-CSRF")) {
		http.Error(w, "forbidden: CSRF", http.StatusForbidden)
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
