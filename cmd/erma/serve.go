// erma serve — a loopback-only HTTP endpoint that lets the user drive the
// erma sidebar from Chrome on Android over `tailscale serve`. This file owns
// the subcommand's flag parsing, the HTTP mux, the SSE hub, and the Tailscale
// identity middleware. It is deliberately READ-ONLY against tmux state in this
// PR: no handler writes an @erma_* option, flags.json or anything that would
// move a session under the bar.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/daronco/ermahgherdr/internal/session"
)

//go:embed all:web
var webFS embed.FS

// servePollInterval mirrors the bar's own 600 ms cadence. The two reach into
// tmux independently and must not drift apart: a slower server would show stale
// rows for seconds at a time, a faster one would waste forks.
const servePollInterval = 600 * time.Millisecond

// apiSession is the on-the-wire shape of GET /api/sessions. It only exports
// what the PWA actually needs today — enough to render the list the bar paints.
// Keep the field names in lockstep with the client (cmd/erma/web/app.js).
type apiSession struct {
	ID    string `json:"id"`    // sessionId, or the pane id when no registry file exists yet
	Pane  string `json:"pane"`  // tmux pane id (%42)
	Name  string `json:"name"`  // human-readable label
	State string `json:"state"` // idle | done | proc | urgent
	Since int64  `json:"since"` // unix seconds the state has held
	Marks string `json:"marks"` // "" | paused | prio
	Sub   int    `json:"sub"`   // subagents under this session
	Ctx   string `json:"ctx"`   // @erma_ctx_used (percent as string), may be empty
	Dai   bool   `json:"dai"`   // the dai-live pinned session
}

// stateString: the state enum, in the exact spelling the front-end switches on.
func stateString(s session.State) string {
	switch s {
	case session.Urgent:
		return "urgent"
	case session.Done:
		return "done"
	case session.Proc:
		return "proc"
	default:
		return "idle"
	}
}

// toAPI projects an internal Session into the API shape. The id falls back to
// the pane when the Claude registry file is not there yet — the UI needs a
// stable key for its list no matter what.
func toAPI(s session.Session) apiSession {
	id := s.Sid
	if id == "" {
		id = s.Pane
	}
	return apiSession{
		ID: id, Pane: s.Pane, Name: s.Name,
		State: stateString(s.St), Since: s.Since,
		Marks: s.Flag, Sub: s.Sub, Ctx: s.Ctx, Dai: s.Dai,
	}
}

// serveFlags: the subcommand's full flag set, held together so main_test can
// build and inspect it without invoking a real listener.
type serveFlags struct {
	addr               string
	tailscaleLogin     string
	insecureNonLoopbck bool
	devSkipAuth        bool
}

// defaultServeFlags: a stable base other callers (tests included) can tweak.
// The default addr is loopback with an ephemeral port (0) so two processes
// never collide, and so a non-loopback bind is impossible without the override.
func defaultServeFlags() serveFlags {
	return serveFlags{addr: "127.0.0.1:0"}
}

// parseServeFlags: the shared parser between main() and the tests. It writes
// usage/errors to w, which lets tests swallow help output.
func parseServeFlags(args []string, w *os.File) (serveFlags, error) {
	sf := defaultServeFlags()
	fs := flag.NewFlagSet("erma serve", flag.ContinueOnError)
	if w != nil {
		fs.SetOutput(w)
	}
	fs.StringVar(&sf.addr, "addr", sf.addr,
		"TCP address to listen on. Must be loopback unless --insecure-non-loopback is set.")
	fs.StringVar(&sf.tailscaleLogin, "tailscale-login", os.Getenv("ERMA_TAILSCALE_LOGIN"),
		"the Tailscale login this server trusts (also from $ERMA_TAILSCALE_LOGIN).")
	fs.BoolVar(&sf.insecureNonLoopbck, "insecure-non-loopback", false,
		"allow binding to a non-loopback address (DANGEROUS: the port is open to anyone who can reach it).")
	fs.BoolVar(&sf.devSkipAuth, "dev-skip-auth", false,
		"bypass Tailscale identity auth. Only for local development.")
	if err := fs.Parse(args); err != nil {
		return sf, err
	}
	return sf, nil
}

// runServe is the subcommand entry point. It validates the flags, starts the
// hub + listener, and blocks until a terminal signal (SIGINT/SIGTERM) is
// delivered via context cancellation — the current main() exits on return.
func runServe(args []string) error {
	sf, err := parseServeFlags(args, os.Stderr)
	if err != nil {
		return err
	}
	return runServeWith(sf)
}

// runServeWith: the step after flag parsing, split so tests can hand a built-up
// serveFlags straight in.
func runServeWith(sf serveFlags) error {
	if err := validateAddr(sf.addr, sf.insecureNonLoopbck); err != nil {
		return err
	}
	if !sf.devSkipAuth && sf.tailscaleLogin == "" {
		return errors.New("erma serve: --tailscale-login (or $ERMA_TAILSCALE_LOGIN) is required unless --dev-skip-auth is set")
	}
	if sf.devSkipAuth {
		log.Printf("WARNING: --dev-skip-auth is ON — every request is served without identity verification. DO NOT use this flag with any public listener.")
	}

	ln, err := net.Listen("tcp", sf.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", sf.addr, err)
	}
	defer ln.Close()
	log.Printf("erma serve: listening on %s", ln.Addr())

	srv := newServer(sf)
	// signal.NotifyContext wires SIGINT/SIGTERM to the hub + shutdown path so
	// Ctrl+C triggers a graceful stop instead of killing in-flight responses.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.hub.run(ctx)

	s := &http.Server{Handler: srv.mux(), ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve(ln) }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

// validateAddr: a non-loopback bind requires the explicit override. Hostnames
// that resolve to multiple addresses would also need checking in principle, but
// this subcommand is only ever bound to a literal IP in production — the laptop
// does not know what else it is on.
func validateAddr(addr string, allowNonLoopback bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--addr %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("--addr %q: host is empty — a bare port means every interface, which is not loopback", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("--addr %q: host %q is not an IP literal; use 127.0.0.1 or ::1", addr, host)
	}
	if ip.IsLoopback() {
		return nil
	}
	if !allowNonLoopback {
		return fmt.Errorf("--addr %q is not loopback; pass --insecure-non-loopback to override", addr)
	}
	return nil
}

// server bundles the mux-building state the handlers close over. The hub owns
// the only gather path — handlers never call session.Gather directly, so the
// permMiss map is touched by exactly one goroutine (the hub tick) and can't
// race between the poller and HTTP handlers.
type server struct {
	flags serveFlags
	hub   *sseHub
}

func newServer(sf serveFlags) *server {
	return &server{
		flags: sf,
		hub:   newSSEHub(),
	}
}

func (s *server) mux() http.Handler {
	root, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // the embed directive guarantees web/ exists at compile time
	}
	m := http.NewServeMux()
	m.HandleFunc("/api/sessions", s.handleSessions)
	m.HandleFunc("/api/stream", s.handleStream)
	m.Handle("/", http.FileServer(http.FS(root)))
	return s.authMiddleware(m)
}

// authMiddleware enforces the Tailscale identity rule on EVERY request —
// static assets, API, SSE. Rejecting a Funnel-marked request matters because
// Funnel strips identity: a request that arrived via the public relay would
// otherwise impersonate "no identity" and fall through the 401 branch below.
const (
	// Tailscale serve sets `Tailscale-User-Login` (and `Tailscale-User-Name`,
	// `Tailscale-User-Profile-Pic`) on the incoming request. The login is a
	// stable identifier (email-shaped for most tailnets).
	headerLogin  = "Tailscale-User-Login"
	headerFunnel = "Tailscale-Funnel-Request"
)

func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerFunnel) != "" {
			http.Error(w, "forbidden: Tailscale Funnel requests are not accepted", http.StatusForbidden)
			return
		}
		if s.flags.devSkipAuth {
			next.ServeHTTP(w, r)
			return
		}
		login := r.Header.Get(headerLogin)
		if login == "" {
			http.Error(w, "unauthorized: missing Tailscale identity header", http.StatusUnauthorized)
			return
		}
		if login != s.flags.tailscaleLogin {
			http.Error(w, "forbidden: Tailscale identity does not match", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// Always read from the hub snapshot: the hub owns permMiss and the only
	// goroutine that calls session.Gather. Before the first tick there is no
	// snapshot — return an empty array so the client can render without racing.
	rows, _ := s.hub.snapshot()
	if rows == nil {
		rows = []apiSession{}
	}
	_ = json.NewEncoder(w).Encode(rows)
}

// handleStream pushes one Server-Sent Events `sessions` event on connect, then
// one every time the hub broadcasts a diff. Clients reconnect via the browser's
// built-in EventSource retry logic (the `retry:` field below sets the delay).
func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
	// retry: 3s = give the browser a bounded reconnect delay; the default
	// implementation-defined one can be seconds longer than we want.
	fmt.Fprintf(w, "retry: 3000\n\n")

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	// On connect, write the current snapshot immediately so the UI never shows
	// an empty list waiting for the next tick. Before the first tick we send an
	// empty array rather than calling gather here: the hub owns permMiss, and
	// a handler-side gather would race with the poller.
	snap, ok := s.hub.snapshot()
	if !ok {
		snap = []apiSession{}
	}
	writeSSE(w, "sessions", snap)
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-ch:
			writeSSE(w, "sessions", payload)
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

// sseHub: one poller fans updates out to every subscriber. One tick wakes the
// loop, which calls session.Gather, diffs the result against the last snapshot,
// and broadcasts on change. A channel-based fan-out keeps the handlers simple
// and lets the hub run on its own context.
//
// permMiss is the per-pane counter PermAlive consults, kept here so it is
// touched by exactly one goroutine (the tick loop); handlers only read the
// post-tick snapshot through the mutex-guarded accessor.
type sseHub struct {
	mu          sync.Mutex
	subscribers map[chan []apiSession]struct{}
	last        []apiSession
	hasLast     bool
	gather      func() []apiSession
	interval    time.Duration
	permMiss    map[string]int
}

func newSSEHub() *sseHub {
	h := &sseHub{
		subscribers: map[chan []apiSession]struct{}{},
		interval:    servePollInterval,
		permMiss:    map[string]int{},
	}
	h.gather = func() []apiSession {
		rows, _ := session.Gather(h.permMiss)
		out := make([]apiSession, len(rows))
		for i, r := range rows {
			out[i] = toAPI(r)
		}
		return out
	}
	return h
}

func (h *sseHub) subscribe() chan []apiSession {
	// Buffered: a slow consumer should not stall the hub, and SSE is
	// drop-the-oldest acceptable since every event is a full snapshot.
	ch := make(chan []apiSession, 4)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) unsubscribe(ch chan []apiSession) {
	h.mu.Lock()
	delete(h.subscribers, ch)
	h.mu.Unlock()
}

func (h *sseHub) snapshot() ([]apiSession, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.hasLast {
		return nil, false
	}
	cp := make([]apiSession, len(h.last))
	copy(cp, h.last)
	return cp, true
}

func (h *sseHub) run(ctx context.Context) {
	t := time.NewTicker(h.interval)
	defer t.Stop()
	h.tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.tick()
		}
	}
}

func (h *sseHub) tick() {
	snap := h.gather()
	h.mu.Lock()
	changed := !h.hasLast || !sameSessions(h.last, snap)
	h.last = snap
	h.hasLast = true
	subs := make([]chan []apiSession, 0, len(h.subscribers))
	for ch := range h.subscribers {
		subs = append(subs, ch)
	}
	h.mu.Unlock()
	if !changed {
		return
	}
	for _, ch := range subs {
		select {
		case ch <- snap:
		default:
			// a stalled subscriber: skip this frame, next poll catches it up.
		}
	}
}

// sameSessions: two snapshots are equal when every field that drives the UI
// matches. Field-level equality (and ignoring field order) is why this exists —
// `reflect.DeepEqual` would be loud about equal slices with different capacity.
func sameSessions(a, b []apiSession) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
