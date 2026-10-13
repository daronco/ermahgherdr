package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mkTerminalServer wires a server with a stub pane resolver and recording
// tmux runners so the suite never forks tmux. The returned *outCalls slice
// grows one entry per capture-pane invocation (argv only); the *calls slice
// is the same recorder the conversation tests use (argv + stdin).
func mkTerminalServer(t *testing.T, sidToPane map[string]string, screen []byte) (*server, *[]tmuxCall, *[][]string) {
	t.Helper()
	srv, calls := mkConvServer(t, sidToPane, nil)
	var outCalls [][]string
	srv.conv.tmuxOut = func(argv ...string) ([]byte, error) {
		cp := append([]string(nil), argv...)
		outCalls = append(outCalls, cp)
		if screen == nil {
			return []byte{}, nil
		}
		return screen, nil
	}
	return srv, calls, &outCalls
}

// --- GET /terminal --------------------------------------------------------

func TestTerminalReturnsCaptureBytes(t *testing.T) {
	screen := []byte("\x1b[31mhello\x1b[0m world\n")
	srv, _, outCalls := mkTerminalServer(t, map[string]string{"s1": "%42"}, screen)

	r := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/terminal", nil)
	r.Header.Set(headerLogin, "ok@example.com")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.Bytes(); !bytes.Equal(got, screen) {
		t.Errorf("body = %q, want %q", got, screen)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if len(*outCalls) != 1 {
		t.Fatalf("want 1 capture-pane call, got %d: %+v", len(*outCalls), *outCalls)
	}
	argv := (*outCalls)[0]
	// Expected invocation: capture-pane -e -p -t %42 (order matters only for
	// -t %42; the rest is a stable set of flags we check for presence of -e).
	if argv[0] != "capture-pane" {
		t.Errorf("argv[0] = %q, want capture-pane", argv[0])
	}
	wantFlags := map[string]bool{"-e": false, "-p": false, "-t": false}
	for _, a := range argv {
		if _, ok := wantFlags[a]; ok {
			wantFlags[a] = true
		}
	}
	for f, seen := range wantFlags {
		if !seen {
			t.Errorf("capture-pane missing %s: %+v", f, argv)
		}
	}
	// -t must appear with the pane id as the next argv element — not as a
	// trailing bare flag, not followed by anything else.
	foundT := false
	for i := 0; i < len(argv); i++ {
		if argv[i] != "-t" {
			continue
		}
		if i == len(argv)-1 {
			t.Errorf("-t is the final argv element with no target: %+v", argv)
			continue
		}
		foundT = true
		if argv[i+1] != "%42" {
			t.Errorf("-t target = %q, want %q", argv[i+1], "%42")
		}
	}
	if !foundT {
		t.Errorf("capture-pane missing -t <pane>: %+v", argv)
	}
}

func TestTerminalUnknownSidIs404(t *testing.T) {
	srv, _, outCalls := mkTerminalServer(t, map[string]string{}, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/sessions/missing/terminal", nil)
	r.Header.Set(headerLogin, "ok@example.com")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
	if len(*outCalls) != 0 {
		t.Errorf("capture-pane called for unknown sid: %+v", *outCalls)
	}
}

func TestTerminalMissingIdentityIsRejectedBeforeTmux(t *testing.T) {
	// The auth middleware rejects a request with no Tailscale identity header
	// before serveSessionRoute is reached, so capture-pane never runs. The
	// existing middleware returns 401 for a missing header; we only assert
	// that the request is REJECTED and no tmux call runs — the gate is what
	// the spec cares about.
	srv, _, outCalls := mkTerminalServer(t, map[string]string{"s1": "%42"}, []byte("x"))
	r := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/terminal", nil)
	// no headerLogin
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("missing identity: got %d, want 401", w.Code)
	}
	if len(*outCalls) != 0 {
		t.Errorf("capture-pane called despite missing identity: %+v", *outCalls)
	}
}

func TestTerminalMismatchedIdentityIs403(t *testing.T) {
	srv, _, outCalls := mkTerminalServer(t, map[string]string{"s1": "%42"}, []byte("x"))
	r := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/terminal", nil)
	r.Header.Set(headerLogin, "intruder@example.com")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("mismatched identity: got %d, want 403", w.Code)
	}
	if len(*outCalls) != 0 {
		t.Errorf("capture-pane called despite forbidden identity: %+v", *outCalls)
	}
}

func TestTerminalPostIs405(t *testing.T) {
	srv, _, outCalls := mkTerminalServer(t, map[string]string{"s1": "%42"}, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/terminal", nil)
	r.Header.Set(headerLogin, "ok@example.com")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /terminal: got %d, want 405", w.Code)
	}
	if len(*outCalls) != 0 {
		t.Errorf("capture-pane called on POST: %+v", *outCalls)
	}
}

// --- POST /keys -----------------------------------------------------------

func postKey(t *testing.T, srv *server, sid, tok, token string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(keyBody{Key: token})
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid+"/keys", bytes.NewReader(body))
	r.Header.Set(headerLogin, "ok@example.com")
	r.Header.Set("Origin", "http://"+r.Host)
	r.Header.Set("Content-Type", "application/json")
	if tok != "" {
		r.Header.Set("X-Erma-CSRF", tok)
	}
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	return w
}

// Every allowlisted token maps to the exact tmux argument expected.
func TestSendKeyAllowlistMapsToTmux(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%7"}, nil)
	tok := mintCSRF(t, srv, "s1")

	for token, want := range terminalKeyAllowlist {
		*calls = (*calls)[:0]
		w := postKey(t, srv, "s1", tok, token)
		if w.Code != http.StatusNoContent {
			t.Errorf("token %q: got %d, want 204 (%s)", token, w.Code, w.Body.String())
			continue
		}
		if len(*calls) != 1 {
			t.Errorf("token %q: want 1 tmux call, got %d: %+v", token, len(*calls), *calls)
			continue
		}
		argv := (*calls)[0].Argv
		if argv[0] != "send-keys" {
			t.Errorf("token %q: argv[0] = %q, want send-keys", token, argv[0])
		}
		if argv[len(argv)-1] != want {
			t.Errorf("token %q: send-keys final arg = %q, want %q", token, argv[len(argv)-1], want)
		}
		// -t must be followed by the pane id.
		foundT := false
		for i := 0; i < len(argv)-1; i++ {
			if argv[i] == "-t" && argv[i+1] == "%7" {
				foundT = true
			}
		}
		if !foundT {
			t.Errorf("token %q: send-keys missing -t %%7: %+v", token, argv)
		}
	}
}

// Tokens not in the allowlist get 400 and never reach the tmux exec path.
// This is the "free-text send-keys -l is not accepted" guarantee. The
// allowlist is case-sensitive, so lower-case variants of real tokens are
// also rejected — the client must use the exact spelling.
func TestSendKeyRejectsUnknownToken(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%7"}, nil)
	tok := mintCSRF(t, srv, "s1")
	rejected := []string{
		"rm -rf /",
		"escape", // lower-case: not in the allowlist
		"ctrl-c", // the conversation /key spelling; /keys uses C-c
		"F1",
		"a",
		"",
		"C-x", // Ctrl combos outside the allowlist
	}
	for _, token := range rejected {
		*calls = (*calls)[:0]
		w := postKey(t, srv, "s1", tok, token)
		if w.Code != http.StatusBadRequest {
			t.Errorf("token %q: got %d, want 400", token, w.Code)
		}
		if len(*calls) != 0 {
			t.Errorf("token %q: tmux called despite rejection: %+v", token, *calls)
		}
	}
}

func TestSendKeyUnknownSidIs404BeforeExec(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%7"}, nil)
	tok := mintCSRF(t, srv, "s1")
	srv.conv.paneResolver = func() map[string]string { return map[string]string{} }
	w := postKey(t, srv, "s1", tok, "Escape")
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("tmux called for unknown sid: %+v", *calls)
	}
}

func TestSendKeyRequiresCSRF(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%7"}, nil)
	mintCSRF(t, srv, "s1")
	w := postKey(t, srv, "s1", "", "Escape")
	if w.Code != http.StatusForbidden {
		t.Errorf("no CSRF: got %d, want 403", w.Code)
	}
	w = postKey(t, srv, "s1", strings.Repeat("0", 64), "Escape")
	if w.Code != http.StatusForbidden {
		t.Errorf("wrong CSRF: got %d, want 403", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("tmux called despite CSRF rejection: %+v", *calls)
	}
}

func TestSendKeyRequiresOrigin(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%7"}, nil)
	tok := mintCSRF(t, srv, "s1")
	body, _ := json.Marshal(keyBody{Key: "Escape"})
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/keys", bytes.NewReader(body))
	r.Header.Set(headerLogin, "ok@example.com")
	r.Header.Set("X-Erma-CSRF", tok)
	// no Origin
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("no Origin: got %d, want 403", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("tmux called despite missing Origin: %+v", *calls)
	}
}

func TestSendKeyMissingIdentityIsRejectedBeforeTmux(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%7"}, nil)
	tok := mintCSRF(t, srv, "s1")
	body, _ := json.Marshal(keyBody{Key: "Escape"})
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/keys", bytes.NewReader(body))
	r.Header.Set("Origin", "http://"+r.Host)
	r.Header.Set("X-Erma-CSRF", tok)
	// no headerLogin
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("missing identity: got %d, want 401", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("tmux called despite missing identity: %+v", *calls)
	}
}

func TestSendKeyGetIs405(t *testing.T) {
	srv, calls := mkConvServer(t, map[string]string{"s1": "%7"}, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/keys", nil)
	r.Header.Set(headerLogin, "ok@example.com")
	w := httptest.NewRecorder()
	srv.mux().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /keys: got %d, want 405", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("tmux called on GET: %+v", *calls)
	}
}

// --- Allowlist content ----------------------------------------------------

// The spec names an explicit list of allowed tokens. Lock it down so a drift
// here (a key dropped, an extra key added) fails a test instead of silently
// changing the exposed surface.
func TestAllowlistMatchesSpec(t *testing.T) {
	want := []string{
		"Escape", "Tab", "Up", "Down", "Left", "Right", "Enter", "BSpace",
		"C-c", "C-d", "C-r", "C-z", "C-l", "C-u", "C-w", "C-a", "C-e",
	}
	if len(terminalKeyAllowlist) != len(want) {
		t.Fatalf("allowlist size = %d, want %d: %+v", len(terminalKeyAllowlist), len(want), terminalKeyAllowlist)
	}
	for _, k := range want {
		got, ok := terminalKeyAllowlist[k]
		if !ok {
			t.Errorf("allowlist missing %q", k)
			continue
		}
		if got != k {
			t.Errorf("allowlist[%q] = %q, want %q (one-to-one)", k, got, k)
		}
	}
}
