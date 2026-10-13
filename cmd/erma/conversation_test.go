package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daronco/ermahgherdr/internal/session"
)

// tmuxCall records one tmux invocation the handler attempted. The test stub
// captures argv (whole slice) and the stdin payload, so the asserts can prove:
//   - argv[0]="tmux" is only reached via exec.Command (we never see sh here),
//   - the user text payload travels on stdin ONLY, never on argv.
type tmuxCall struct {
	Argv  []string
	Stdin string
}

// recordingTmux: a tmuxRunner that appends calls to a slice.
func recordingTmux(dst *[]tmuxCall) tmuxRunner {
	return func(stdin io.Reader, argv ...string) error {
		var buf bytes.Buffer
		if stdin != nil {
			_, _ = io.Copy(&buf, stdin)
		}
		cp := append([]string(nil), argv...)
		*dst = append(*dst, tmuxCall{Argv: cp, Stdin: buf.String()})
		return nil
	}
}

// mkConvServer wires a server with the auth/CSRF plumbing in place but no
// real tmux and a stub pane resolver. The seed sid→pane maps everything the
// test needs; the recorder lets us assert the tmux invocations after the fact.
func mkConvServer(t *testing.T, sidToPane map[string]string, markCount *int32) (*server, *[]tmuxCall) {
	t.Helper()
	srv := newServer(serveFlags{tailscaleLogin: "ok@example.com"})
	srv.conv.paneResolver = func() map[string]string {
		// paneResolver returns pane → sid, the inverse of our test map.
		out := map[string]string{}
		for sid, pane := range sidToPane {
			out[pane] = sid
		}
		return out
	}
	var calls []tmuxCall
	srv.conv.tmux = recordingTmux(&calls)
	srv.conv.mark = func(pane string) {
		if markCount != nil {
			atomic.AddInt32(markCount, 1)
		}
	}
	return srv, &calls
}

// mintCSRF: call GET /conversation once to seed the CSRF token for sid.
// Returns the token the server minted. Needs a jsonl file in place, so the
// handler returns 200.
func mintCSRF(t *testing.T, srv *server, sid string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	proj := filepath.Join(home, ".claude", "projects", "-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, sid+".jsonl"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid+"/conversation", nil)
	r.Header.Set(headerLogin, "ok@example.com")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /conversation: got %d: %s", w.Code, w.Body.String())
	}
	tok := w.Header().Get("X-Erma-CSRF")
	if tok == "" {
		var body conversationBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		tok = body.Csrf
	}
	if tok == "" {
		t.Fatal("no CSRF token returned")
	}
	return tok
}

// --- Unknown id -----------------------------------------------------------

func TestConversationUnknownSid(t *testing.T) {
	srv, _ := mkConvServer(t, map[string]string{}, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/sessions/does-not-exist/conversation", nil)
	r.Header.Set(headerLogin, "ok@example.com")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown sid: got %d, want 404", w.Code)
	}
}

func TestPostToUnknownSid(t *testing.T) {
	// A sid that is not in the registry 404s even with a valid-looking token,
	// because the resolver fails first. We can only test this by giving the
	// POST a token for one sid and then switching the pane resolver — but
	// easier: register a sid, mint, then clear the resolver and POST to it.
	srv, calls := mkConvServer(t, map[string]string{"s1": "%1"}, nil)
	tok := mintCSRF(t, srv, "s1")
	srv.conv.paneResolver = func() map[string]string { return map[string]string{} }
	body := bytes.NewBufferString(`{"text":"hi"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/text", body)
	r.Header.Set(headerLogin, "ok@example.com")
	r.Header.Set("Origin", "http://"+r.Host)
	r.Header.Set("X-Erma-CSRF", tok)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown post sid: got %d, want 404", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("tmux called for unknown sid: %+v", *calls)
	}
}

// --- Method check ---------------------------------------------------------

func TestGetOnPostPathIs405(t *testing.T) {
	srv, _ := mkConvServer(t, map[string]string{"s1": "%1"}, nil)
	for _, path := range []string{
		"/api/sessions/s1/text",
		"/api/sessions/s1/key",
		"/api/sessions/s1/read",
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(headerLogin, "ok@example.com")
		w := httptest.NewRecorder()
		srv.mux().ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: GET got %d, want 405", path, w.Code)
		}
	}
}

// --- Origin + CSRF gates --------------------------------------------------

func TestPostWithoutOriginRejected(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%1"}, nil)
	tok := mintCSRF(t, srv, "s1")
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/text",
		bytes.NewBufferString(`{"text":"x"}`))
	r.Header.Set(headerLogin, "ok@example.com")
	r.Header.Set("X-Erma-CSRF", tok)
	// no Origin
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("no-Origin: got %d, want 403", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("tmux called despite failed Origin: %+v", *calls)
	}
}

func TestPostMismatchedOriginRejected(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%1"}, nil)
	tok := mintCSRF(t, srv, "s1")
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/text",
		bytes.NewBufferString(`{"text":"x"}`))
	r.Header.Set(headerLogin, "ok@example.com")
	r.Header.Set("Origin", "http://evil.example")
	r.Header.Set("X-Erma-CSRF", tok)
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("mismatched Origin: got %d, want 403", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("tmux called despite mismatched Origin: %+v", *calls)
	}
}

func TestPostWithoutCSRFRejected(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%1"}, nil)
	mintCSRF(t, srv, "s1")
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/text",
		bytes.NewBufferString(`{"text":"x"}`))
	r.Header.Set(headerLogin, "ok@example.com")
	r.Header.Set("Origin", "http://"+r.Host)
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("no-CSRF: got %d, want 403", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("tmux called despite missing CSRF: %+v", *calls)
	}
}

func TestPostWithWrongCSRFRejected(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%1"}, nil)
	mintCSRF(t, srv, "s1")
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/text",
		bytes.NewBufferString(`{"text":"x"}`))
	r.Header.Set(headerLogin, "ok@example.com")
	r.Header.Set("Origin", "http://"+r.Host)
	r.Header.Set("X-Erma-CSRF", strings.Repeat("0", 64))
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("wrong CSRF: got %d, want 403", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("tmux called despite wrong CSRF: %+v", *calls)
	}
}

// --- Text ------------------------------------------------------------------

func TestTextHappyPathArgvOnlyAndStdin(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%1"}, nil)
	tok := mintCSRF(t, srv, "s1")
	payload := "hello\nmulti line body"
	body, _ := json.Marshal(textBody{Text: payload})
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/text", bytes.NewReader(body))
	r.Header.Set(headerLogin, "ok@example.com")
	r.Header.Set("Origin", "http://"+r.Host)
	r.Header.Set("X-Erma-CSRF", tok)
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if len(*calls) != 3 {
		t.Fatalf("want 3 tmux calls (load-buffer, paste-buffer, send-keys), got %d: %+v", len(*calls), *calls)
	}
	lb := (*calls)[0]
	if lb.Argv[0] != "load-buffer" || lb.Argv[len(lb.Argv)-1] != "-" {
		t.Errorf("load-buffer argv: %+v", lb.Argv)
	}
	if lb.Stdin != payload {
		t.Errorf("load-buffer stdin: got %q, want %q", lb.Stdin, payload)
	}
	// The payload must NOT be on argv anywhere.
	for i, c := range *calls {
		for _, a := range c.Argv {
			if strings.Contains(a, "multi line body") || strings.Contains(a, "hello") {
				t.Errorf("call[%d] argv leaked payload: %+v", i, c.Argv)
			}
		}
		// And no `sh -c` ever.
		if len(c.Argv) > 0 && (c.Argv[0] == "sh" || c.Argv[0] == "-c") {
			t.Errorf("call[%d] used sh: %+v", i, c.Argv)
		}
	}
	pb := (*calls)[1]
	if pb.Argv[0] != "paste-buffer" {
		t.Errorf("second call not paste-buffer: %+v", pb.Argv)
	}
	// -p is required (bracketed paste).
	foundP := false
	for _, a := range pb.Argv {
		if a == "-p" {
			foundP = true
		}
	}
	if !foundP {
		t.Errorf("paste-buffer missing -p: %+v", pb.Argv)
	}
	sk := (*calls)[2]
	if sk.Argv[0] != "send-keys" || sk.Argv[len(sk.Argv)-1] != "Enter" {
		t.Errorf("third call not send-keys Enter: %+v", sk.Argv)
	}
}

// --- Keys ------------------------------------------------------------------

func TestKeyAllowlistAccepted(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%1"}, nil)
	tok := mintCSRF(t, srv, "s1")
	for name, tmuxName := range keyAllowlist {
		*calls = (*calls)[:0]
		body, _ := json.Marshal(keyBody{Key: name})
		r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/key", bytes.NewReader(body))
		r.Header.Set(headerLogin, "ok@example.com")
		r.Header.Set("Origin", "http://"+r.Host)
		r.Header.Set("X-Erma-CSRF", tok)
		w := httptest.NewRecorder()
		srv.mux().ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Errorf("key %q: got %d, want 204 (%s)", name, w.Code, w.Body.String())
			continue
		}
		if len(*calls) != 1 || (*calls)[0].Argv[0] != "send-keys" {
			t.Errorf("key %q: wrong call: %+v", name, *calls)
			continue
		}
		last := (*calls)[0].Argv[len((*calls)[0].Argv)-1]
		if last != tmuxName {
			t.Errorf("key %q: send-keys final arg = %q, want %q", name, last, tmuxName)
		}
	}
}

func TestKeyRejectedWhenNotInAllowlist(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%1"}, nil)
	tok := mintCSRF(t, srv, "s1")
	for _, bad := range []string{"C-x", "4", "F1", "rm -rf /", "", "escape"} {
		body, _ := json.Marshal(keyBody{Key: bad})
		r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/key", bytes.NewReader(body))
		r.Header.Set(headerLogin, "ok@example.com")
		r.Header.Set("Origin", "http://"+r.Host)
		r.Header.Set("X-Erma-CSRF", tok)
		w := httptest.NewRecorder()
		srv.mux().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("bad key %q: got %d, want 400", bad, w.Code)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("tmux called for rejected key: %+v", *calls)
	}
}

// --- Read ack -------------------------------------------------------------

func TestReadFiresMarkOnce(t *testing.T) {
	var marks int32
	srv, _ := mkConvServer(t, map[string]string{"s1": "%1"}, &marks)
	tok := mintCSRF(t, srv, "s1")
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/read", bytes.NewBufferString(`{}`))
	r.Header.Set(headerLogin, "ok@example.com")
	r.Header.Set("Origin", "http://"+r.Host)
	r.Header.Set("X-Erma-CSRF", tok)
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if n := atomic.LoadInt32(&marks); n != 1 {
		t.Errorf("MarkRead calls: got %d, want 1", n)
	}
}

// --- Path traversal end-to-end --------------------------------------------

func TestConversationPathTraversalIs404(t *testing.T) {
	// A sid that would try to walk out of the projects root must 404 at the
	// HTTP boundary — the registry doesn't know it, and the transcript reader
	// refuses the shape before touching the file system.
	srv, _ := mkConvServer(t, map[string]string{"s1": "%1"}, nil)
	for _, bad := range []string{"../etc/passwd", "..", "a/b", "s1/../s1"} {
		r := httptest.NewRequest(http.MethodGet,
			"/api/sessions/"+bad+"/conversation", nil)
		r.Header.Set(headerLogin, "ok@example.com")
		w := httptest.NewRecorder()
		srv.mux().ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("sid=%q: got %d, want 404", bad, w.Code)
		}
	}
}

// --- extractSid -----------------------------------------------------------

func TestExtractSid(t *testing.T) {
	for _, c := range []struct {
		path, wantSid, wantTail string
		wantOK                  bool
	}{
		{"/api/sessions/abc/conversation", "abc", "/conversation", true},
		{"/api/sessions/abc/text", "abc", "/text", true},
		{"/api/sessions/abc", "abc", "", true},
		{"/api/sessions/", "", "", false},
		{"/api/sessions", "", "", false},
	} {
		sid, tail, ok := extractSid(c.path)
		if sid != c.wantSid || tail != c.wantTail || ok != c.wantOK {
			t.Errorf("extractSid(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.path, sid, tail, ok, c.wantSid, c.wantTail, c.wantOK)
		}
	}
}

// Every state-changing endpoint, not just /text, enforces Origin, CSRF and the
// registry lookup — and a rejected request reaches neither tmux nor MarkRead.
func TestStateChangingGatesOnEveryEndpoint(t *testing.T) {
	endpoints := map[string]string{
		"text": `{"text":"hi"}`,
		"key":  `{"key":"esc"}`,
		"read": `{}`,
	}
	gates := []struct {
		name     string
		origin   func(host string) string
		token    func(tok string) string
		unknown  bool
		wantCode int
	}{
		{"no Origin", func(string) string { return "" }, func(t string) string { return t }, false, http.StatusForbidden},
		{"foreign Origin", func(string) string { return "http://evil.example" }, func(t string) string { return t }, false, http.StatusForbidden},
		{"no CSRF", func(h string) string { return "http://" + h }, func(string) string { return "" }, false, http.StatusForbidden},
		{"wrong CSRF", func(h string) string { return "http://" + h }, func(string) string { return strings.Repeat("0", 64) }, false, http.StatusForbidden},
		{"unknown sid", func(h string) string { return "http://" + h }, func(t string) string { return t }, true, http.StatusNotFound},
	}
	for ep, payload := range endpoints {
		for _, g := range gates {
			var marks int32
			srv, calls := mkConvServer(t, map[string]string{"s1": "%1"}, &marks)
			tok := mintCSRF(t, srv, "s1")
			if g.unknown {
				srv.conv.paneResolver = func() map[string]string { return map[string]string{} }
			}
			r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/"+ep, bytes.NewBufferString(payload))
			r.Header.Set(headerLogin, "ok@example.com")
			if o := g.origin(r.Host); o != "" {
				r.Header.Set("Origin", o)
			}
			if tk := g.token(tok); tk != "" {
				r.Header.Set("X-Erma-CSRF", tk)
			}
			w := httptest.NewRecorder()
			srv.mux().ServeHTTP(w, r)
			if w.Code != g.wantCode {
				t.Errorf("%s / %s: got %d, want %d", ep, g.name, w.Code, g.wantCode)
			}
			if len(*calls) != 0 || atomic.LoadInt32(&marks) != 0 {
				t.Errorf("%s / %s: side effect despite rejection: tmux=%+v marks=%d", ep, g.name, *calls, marks)
			}
		}
	}
}

// The registry is the gate for reads too: a transcript on disk for a sid the
// registry does not know is still a 404, on both GET routes.
func TestReadRoutesRequireRegistry(t *testing.T) {
	srv, _ := mkConvServer(t, map[string]string{"ghost": "%9"}, nil)
	mintCSRF(t, srv, "ghost") // writes ghost.jsonl; then the registry forgets it
	srv.conv.paneResolver = func() map[string]string { return map[string]string{"%1": "s1"} }
	for _, tail := range []string{"/conversation", "/stream"} {
		// The deadline only matters if the gate regresses: an open stream then
		// ends with a 200 instead of hanging the test.
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		r := httptest.NewRequest(http.MethodGet, "/api/sessions/ghost"+tail, nil).WithContext(ctx)
		r.Header.Set(headerLogin, "ok@example.com")
		w := httptest.NewRecorder()
		srv.mux().ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET ghost%s: got %d, want 404", tail, w.Code)
		}
	}
}

// writeLongTranscript replaces s1's transcript (set up by mintCSRF) with n
// user turns, enough to run past session.TailBytes.
func writeLongTranscript(t *testing.T, n int) int64 {
	t.Helper()
	var b strings.Builder
	for i := 0; i < n; i++ {
		line, _ := json.Marshal(map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": fmt.Sprintf("turn-%05d %s", i, strings.Repeat("x", 80))},
		})
		b.Write(append(line, '\n'))
	}
	path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-proj", "s1.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return int64(b.Len())
}

func getConversation(t *testing.T, srv *server, query string) conversationBody {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/conversation"+query, nil)
	r.Header.Set(headerLogin, "ok@example.com")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET conversation%s: %d", query, w.Code)
	}
	var body conversationBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// Opening a long transcript sends only its tail; ?before= pages back from it.
func TestConversationOpensWithTail(t *testing.T) {
	srv, _ := mkConvServer(t, map[string]string{"s1": "%1"}, nil)
	mintCSRF(t, srv, "s1")
	size := writeLongTranscript(t, 6000)

	tail := getConversation(t, srv, "")
	if int64(tail.Cursor) != size || tail.Start == 0 || int64(tail.Cursor-tail.Start) > session.TailBytes {
		t.Fatalf("tail: start=%d cursor=%d size=%d", tail.Start, tail.Cursor, size)
	}
	if last := tail.Turns[len(tail.Turns)-1].Text; !strings.HasPrefix(last, "turn-05999") {
		t.Errorf("tail does not end at the newest turn: %q", last)
	}
	page := getConversation(t, srv, fmt.Sprintf("?before=%d", tail.Start))
	if page.Cursor != tail.Start || page.Start >= tail.Start || len(page.Turns) == 0 {
		t.Fatalf("page: start=%d cursor=%d, tail start=%d", page.Start, page.Cursor, tail.Start)
	}
	first := strings.Fields(tail.Turns[0].Text)[0]
	lastOfPage := strings.Fields(page.Turns[len(page.Turns)-1].Text)[0]
	var a, b int
	fmt.Sscanf(lastOfPage, "turn-%d", &a)
	fmt.Sscanf(first, "turn-%d", &b)
	if b != a+1 {
		t.Errorf("gap between page and tail: %s then %s", lastOfPage, first)
	}
}

// A stream told where the client already is sends no transcript dump — only
// what is appended — and tags it with the cursor as the SSE id.
func TestStreamResumesFromCursor(t *testing.T) {
	srv, _ := mkConvServer(t, map[string]string{"s1": "%1"}, nil)
	mintCSRF(t, srv, "s1")
	size := writeLongTranscript(t, 10)
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()

	for _, c := range []struct{ name, query, lastID string }{
		{"?after", fmt.Sprintf("?after=%d", size), ""},
		{"Last-Event-ID", "", fmt.Sprint(size)},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/sessions/s1/stream"+c.query, nil)
		req.Header.Set(headerLogin, "ok@example.com")
		if c.lastID != "" {
			req.Header.Set("Last-Event-ID", c.lastID)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-proj", "s1.jsonl")
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		_, _ = f.WriteString(`{"type":"user","message":{"role":"user","content":"fresh"}}` + "\n")
		f.Close()

		var events []string
		var id string
		br := bufio.NewReader(resp.Body)
		for len(events) == 0 || events[len(events)-1] != "delta" {
			line, err := br.ReadString('\n')
			if err != nil {
				break
			}
			if strings.HasPrefix(line, "event: ") {
				events = append(events, strings.TrimSpace(strings.TrimPrefix(line, "event: ")))
			}
			if strings.HasPrefix(line, "id: ") {
				id = strings.TrimSpace(strings.TrimPrefix(line, "id: "))
			}
		}
		resp.Body.Close()
		cancel()
		for _, e := range events {
			if e == "conversation" {
				t.Errorf("%s: stream re-sent the transcript: %v", c.name, events)
			}
		}
		if len(events) == 0 || events[len(events)-1] != "delta" {
			t.Errorf("%s: no delta for the appended turn: %v", c.name, events)
		}
		if want := fmt.Sprint(size + int64(len(`{"type":"user","message":{"role":"user","content":"fresh"}}`)+1)); id != want {
			t.Errorf("%s: delta id = %q, want %s", c.name, id, want)
		}
		if c, ok := cursorParam(id); ok {
			size = int64(c) // the next case resumes after this append
		}
	}
}
