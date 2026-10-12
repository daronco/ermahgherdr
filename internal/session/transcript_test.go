package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
