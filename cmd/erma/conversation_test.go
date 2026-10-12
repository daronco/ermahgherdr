package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
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
