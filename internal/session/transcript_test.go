package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTranscript builds a Claude-shaped projects root at tmp/.claude/projects/
// and writes one jsonl file. The returned sid is the file's basename (minus
// .jsonl), suitable for a direct ReadConversation call.
func writeTranscript(t *testing.T, lines []string) (home, sid string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	proj := filepath.Join(home, ".claude", "projects", "-proj-a")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	sid = "aaaa1111-2222-3333-4444-555566667777"
	path := filepath.Join(proj, sid+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, sid
}

func TestReadConversationHappyPath(t *testing.T) {
	user, _ := json.Marshal(map[string]any{
		"type":      "user",
		"timestamp": "2026-10-12T20:00:00.000Z",
		"message": map[string]any{
			"role":    "user",
			"content": "hello world",
		},
	})
	asst, _ := json.Marshal(map[string]any{
		"type":      "assistant",
		"timestamp": "2026-10-12T20:00:01.000Z",
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "hi back"},
			},
		},
	})
	tool, _ := json.Marshal(map[string]any{
		"type":      "assistant",
		"timestamp": "2026-10-12T20:00:02.000Z",
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "thinking", "thinking": "..."},
				map[string]any{
					"type":  "tool_use",
					"name":  "Bash",
					"input": map[string]any{"command": "ls -la /tmp"},
				},
			},
		},
	})
	_, sid := writeTranscript(t, []string{string(user), string(asst), string(tool)})
	turns, cur, err := ReadConversation(sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 {
		t.Fatalf("want 3 turns, got %d: %+v", len(turns), turns)
	}
	if turns[0].Role != "user" || !strings.Contains(turns[0].Text, "hello") {
		t.Errorf("user turn: %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Tool != nil {
		t.Errorf("assistant text turn: %+v", turns[1])
	}
	if turns[2].Tool == nil || turns[2].Tool.Name != "Bash" {
		t.Fatalf("tool turn: %+v", turns[2])
	}
	if !strings.Contains(turns[2].Tool.Args, "ls -la") {
		t.Errorf("tool args not summarised: %q", turns[2].Tool.Args)
	}
	if cur <= 0 {
		t.Errorf("cursor did not advance: %d", cur)
	}

	// A second read from the advanced cursor should return nothing new.
	turns, _, err = ReadConversation(sid, cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 0 {
		t.Errorf("tail with no new lines: want 0 turns, got %d", len(turns))
	}
}

func TestReadConversationUnknownSid(t *testing.T) {
	writeTranscript(t, nil) // just to set CLAUDE_CONFIG_DIR
	if _, _, err := ReadConversation("does-not-exist", 0); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("want ErrUnknownSession, got %v", err)
	}
}

// Path traversal: the sid must not be interpretable as a path. A literal
// "../" or an absolute path is rejected without a file-system call, so the
// test asserts via a sentinel file: if it got opened, we'd read a turn.
func TestReadConversationRejectsPathTraversal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	proj := filepath.Join(home, ".claude", "projects", "-ok")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a sibling file outside the projects root; any traversal attempt
	// that reaches it would turn into a non-nil turn list.
	stray := filepath.Join(home, ".claude", "nope.jsonl")
	entry, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": "SECRET"},
	})
	if err := os.WriteFile(stray, append([]byte{}, append(entry, '\n')...), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"../nope",
		"../../nope", // the one that would reach the stray file through the glob
		"../../etc/passwd",
		"/etc/passwd",
		".",
		"..",
		"",
		"a/b",
		"a\\b",
	} {
		turns, _, err := ReadConversation(bad, 0)
		if !errors.Is(err, ErrUnknownSession) {
			t.Errorf("sid=%q: want ErrUnknownSession, got err=%v, turns=%d", bad, err, len(turns))
		}
	}
}

// After-cursor past the end of the file resets to zero so the caller is not
// stuck on a stale offset (file rotation or truncation).
func TestReadConversationCursorResetOnTruncation(t *testing.T) {
	line, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": "a"},
	})
	_, sid := writeTranscript(t, []string{string(line)})
	turns, _, err := ReadConversation(sid, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Errorf("truncation-reset: want 1 turn after reset, got %d", len(turns))
	}
}

// Partial trailing line is not consumed. The cursor should stop at the last
// complete line's end, so once the line finishes (newline arrives) the next
// read picks up exactly one new turn.
func TestReadConversationPartialLine(t *testing.T) {
	first, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": "complete"},
	})
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	proj := filepath.Join(home, ".claude", "projects", "-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "aaaa1111-2222-3333-4444-555566667777"
	path := filepath.Join(proj, sid+".jsonl")
	// Write the complete line plus the opening of a second one, no trailing \n.
	partialPrefix := `{"type":"user","message":{"role":"user","content":"ne`
	content := string(first) + "\n" + partialPrefix
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	turns, cur, err := ReadConversation(sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("want 1 turn (only the complete one), got %d", len(turns))
	}
	// Finish the partial line: the writer appends the remaining bytes + \n.
	finish := `xt"}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(finish); err != nil {
		t.Fatal(err)
	}
	f.Close()
	turns2, _, err := ReadConversation(sid, cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns2) != 1 {
		t.Fatalf("want 1 new turn after append, got %d (cur=%d)", len(turns2), cur)
	}
	if turns2[0].Text != "next" {
		t.Errorf("completion lost: %+v", turns2[0])
	}
}

// WatchTranscript emits a Reset carrying the current transcript on connect,
// then one delta per append — nothing when the file is quiet.
func TestWatchTranscriptEmitsResetThenDelta(t *testing.T) {
	first, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": "one"},
	})
	home, sid := writeTranscript(t, []string{string(first)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := WatchTranscript(ctx, sid, 20*time.Millisecond, -1)

	// Initial Reset.
	select {
	case tr, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before initial Reset")
		}
		if !tr.Reset || len(tr.Turns) != 1 || tr.Turns[0].Text != "one" {
			t.Fatalf("initial frame: %+v", tr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no initial frame within 2s")
	}

	// Append a second line.
	path := filepath.Join(home, ".claude", "projects", "-proj-a", sid+".jsonl")
	second, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": "two"},
	})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(second, '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Delta carrying just the new line.
	select {
	case tr, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before delta")
		}
		if tr.Reset {
			t.Errorf("second frame should be a delta, not a reset: %+v", tr)
		}
		if len(tr.Turns) != 1 || tr.Turns[0].Text != "two" {
			t.Errorf("delta turns: %+v", tr.Turns)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delta within 2s of append")
	}

	// Cancelling the context closes the channel.
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			// Drain one extra frame then expect close.
			select {
			case _, ok := <-ch:
				if ok {
					t.Error("channel not closed after cancel")
				}
			case <-time.After(2 * time.Second):
				t.Error("channel not closed within 2s of cancel")
			}
		}
	case <-time.After(2 * time.Second):
		t.Error("channel not closed within 2s of cancel")
	}
}

// extractPromptText pulls the asked-for question out of a pane body so the
// permission banner carries the real text rather than a generic placeholder.
func TestExtractPromptText(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want string
	}{
		{
			"do-you-want-to",
			" Create file\n permprobe-xyz.txt\n╌╌╌\n  1 hello\n╌╌╌\n" +
				" Do you want to create permprobe-xyz.txt?\n ❯ 1. Yes\n   2. No\n\n" +
				" Esc to cancel\n",
			"Do you want to create permprobe-xyz.txt?",
		},
		{
			"choose-an-option",
			"Choose an option:\n❯ 1. Keep it\n  2. Drop it\n",
			"Choose an option:",
		},
		{
			"mcp-elicitation",
			"───\n MCP server \"notion\" requests your input\n ❯ Accept\n   Decline\n Esc to cancel\n",
			"MCP server \"notion\" requests your input",
		},
	} {
		if got := extractPromptText(c.body); got != c.want {
			t.Errorf("%s: extractPromptText = %q, want %q", c.name, got, c.want)
		}
	}
}

// validSid on its own, so dropping it is caught even though transcriptPath's
// Rel check would still stop an escape.
func TestValidSid(t *testing.T) {
	for _, c := range []struct {
		sid  string
		want bool
	}{
		{"aaaa1111-2222-3333-4444-555566667777", true},
		{"", false}, {".", false}, {"..", false},
		{"../x", false}, {"a/b", false}, {"a\\b", false}, {"/etc/passwd", false}, {"a..b", false},
		{"*", false}, {"a?", false}, {"[ab]", false}, {"a.b", false}, {"a b", false},
		{strings.Repeat("a", 129), false},
	} {
		if got := validSid(c.sid); got != c.want {
			t.Errorf("validSid(%q) = %v, want %v", c.sid, got, c.want)
		}
	}
}

func userLine(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func texts(turns []Turn) []string {
	var out []string
	for _, t := range turns {
		out = append(out, t.Text)
	}
	return out
}

// Opening reads only the tail, and paging back by `start` walks the rest of the
// file without gaps or repeats, down to offset 0.
func TestReadConversationBeforePagesBack(t *testing.T) {
	var lines, want []string
	for i := 0; i < 40; i++ {
		s := fmt.Sprintf("turn-%02d", i)
		lines, want = append(lines, userLine(t, s)), append(want, s)
	}
	home, sid := writeTranscript(t, lines)
	path := filepath.Join(home, ".claude", "projects", "-proj-a", sid+".jsonl")
	st, _ := os.Stat(path)

	turns, start, end, err := ReadConversationBefore(sid, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if int64(end) != st.Size() || start == 0 || len(turns) == 0 || len(turns) > 5 {
		t.Fatalf("tail: %d turns, start=%d end=%d (size %d)", len(turns), start, end, st.Size())
	}
	got := texts(turns)
	for start > 0 {
		var page []Turn
		page, start, _, err = ReadConversationBefore(sid, start, 200)
		if err != nil || len(page) == 0 {
			t.Fatalf("page before %d: %d turns, err %v", start, len(page), err)
		}
		got = append(texts(page), got...)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("paged turns:\n got %v\nwant %v", got, want)
	}
}

// A line longer than the window is still read whole, and a line still being
// written at the end of the file is left for the tail.
func TestReadConversationBeforeLongAndPartialLines(t *testing.T) {
	long := userLine(t, strings.Repeat("x", 5000))
	home, sid := writeTranscript(t, []string{userLine(t, "a"), long})
	path := filepath.Join(home, ".claude", "projects", "-proj-a", sid+".jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(`{"type":"user","mess`)
	f.Close()
	st, _ := os.Stat(path)

	turns, start, end, err := ReadConversationBefore(sid, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || len(turns[0].Text) != 5000 || start == 0 {
		t.Fatalf("long line: %d turns, start=%d", len(turns), start)
	}
	if int64(end) != st.Size()-int64(len(`{"type":"user","mess`)) {
		t.Errorf("end=%d includes the partial line (size %d)", end, st.Size())
	}
}

// Resuming from a cursor sends no initial dump, only what is appended later.
func TestWatchTranscriptResumesFromCursor(t *testing.T) {
	home, sid := writeTranscript(t, []string{userLine(t, "old")})
	path := filepath.Join(home, ".claude", "projects", "-proj-a", sid+".jsonl")
	st, _ := os.Stat(path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := WatchTranscript(ctx, sid, 20*time.Millisecond, Cursor(st.Size()))

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(userLine(t, "new") + "\n")
	f.Close()
	select {
	case tr := <-ch:
		if tr.Reset || strings.Join(texts(tr.Turns), ",") != "new" {
			t.Errorf("first frame: %+v", tr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delta within 2s")
	}
}
