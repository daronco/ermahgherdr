// Transcript reader: parses the Claude Code JSONL at
// ~/.claude/projects/*/<sid>.jsonl into the compact Turn stream the PWA
// renders. The file is append-only — the reader keeps a byte-offset cursor so
// a tail only pays for the new lines.
//
// Confinement: the sid is NEVER a path. Every call resolves the file through
// a glob under the Claude Code projects root, and the match is checked to live
// inside that root. A client-controlled sid with "..", a leading "/" or any
// other separator cannot escape (os.ReadFile also refuses "..", but the glob
// alone would quietly miss and 404 — explicit rejection is clearer).

package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Turn: one row the PWA renders. Tool calls fold into a single Tool entry so
// the UI collapses a message that is "assistant thinking + N tool_use blocks"
// to one row by default.
type Turn struct {
	Role string    `json:"role"`           // "user" | "assistant" | "tool_result"
	Ts   time.Time `json:"ts"`             // the transcript's own timestamp
	Text string    `json:"text"`           // user-visible text; a short summary for tool_result
	Tool *ToolCall `json:"tool,omitempty"` // non-nil for an assistant turn whose body is a tool call
}

// ToolCall: the folded summary of an assistant tool_use block.
type ToolCall struct {
	Name string `json:"name"` // "Bash", "Edit", ...
	Args string `json:"args"` // one-line summary of the tool input
}

// Cursor: a byte offset into the JSONL file. The client treats it as opaque.
// A new session (first read) starts at 0.
type Cursor int64

// ErrUnknownSession: the sid is not currently tracked. 404 at the HTTP layer.
var ErrUnknownSession = errors.New("session: unknown session id")

// claudeProjectsRoot: the directory tree Claude Code puts every project
// transcript under. CLAUDE_CONFIG_DIR overrides $HOME/.claude so a test can
// point the whole thing at a temp dir.
func claudeProjectsRoot() string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	return filepath.Join(dir, "projects")
}

// validSid: the sid may only be the base name of a .jsonl file. A real Claude
// sid is a UUID, but we don't want to tie the parser to the exact shape;
// instead reject anything that could ever leave the projects root.
func validSid(sid string) bool {
	if sid == "" || sid == "." || sid == ".." {
		return false
	}
	if strings.ContainsAny(sid, "/\\") {
		return false
	}
	if strings.Contains(sid, "..") {
		return false
	}
	return true
}

// transcriptPath finds the .jsonl for sid under the projects root. Returns
// ErrUnknownSession when no file exists.
func transcriptPath(sid string) (string, error) {
	if !validSid(sid) {
		return "", ErrUnknownSession
	}
	root := claudeProjectsRoot()
	matches, err := filepath.Glob(filepath.Join(root, "*", sid+".jsonl"))
	if err != nil || len(matches) == 0 {
		return "", ErrUnknownSession
	}
	// Defense in depth: a glob that for any reason returned a path outside the
	// root must not open. filepath.Rel + ".." is the standard safe check.
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", ErrUnknownSession
	}
	for _, m := range matches {
		mAbs, err := filepath.Abs(m)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootAbs, mAbs)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		return mAbs, nil
	}
	return "", ErrUnknownSession
}

// ReadConversation reads every complete JSONL line after `after` from the
// session's transcript, parses them into Turns, and returns the next cursor
// (the byte offset where the next read should start — the end of the last
// complete line consumed).
//
// A partial line at the end of the file is deliberately left for the next
// read. Claude Code writes the JSONL in append mode; tailing it in the middle
// of a write would otherwise miss or corrupt the newest turn.
func ReadConversation(sid string, after Cursor) ([]Turn, Cursor, error) {
	path, err := transcriptPath(sid)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, ErrUnknownSession
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	// Cursor past end: file was truncated or rotated. Start over so the client
	// is not stuck on a stale offset.
	if int64(after) > st.Size() {
		after = 0
	}
	if _, err := f.Seek(int64(after), io.SeekStart); err != nil {
		return nil, 0, err
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}
	// Trim to the last complete line. A write in flight would otherwise land as
	// a half-parsed turn.
	nl := bytesLastIndexByte(buf, '\n')
	if nl < 0 {
		return nil, after, nil
	}
	consumed := buf[:nl+1]
	nextCursor := Cursor(int64(after) + int64(len(consumed)))
	var turns []Turn
	for _, line := range splitLines(consumed) {
		line = trimTrailingCR(line)
		if len(line) == 0 {
			continue
		}
		if t, ok := parseTurn(line); ok {
			turns = append(turns, t)
		}
	}
	return turns, nextCursor, nil
}

// jsonlEntry: the subset of a JSONL row ReadConversation needs. Everything
// else is passed through to the raw message decoder.
type jsonlEntry struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// msgEnvelope: the "message" sub-document. User and assistant both use it;
// their content is either a plain string or a list of structured blocks.
type msgEnvelope struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentBlock: one block of the structured content list.
type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Content  json.RawMessage `json:"content"`
	IsError  bool            `json:"is_error"`
	Thinking string          `json:"thinking"`
}

// parseTurn: fold one JSONL row into (at most) one Turn. Returns ok=false for
// rows the UI does not render: queue-operation, atis-latch, last-prompt,
// attachment, and a user turn whose content is nothing but tool_result blocks
// (those render as the tool's own row in the preceding assistant turn — the
// PWA already shows the tool, the result is noise unless it errored).
func parseTurn(line []byte) (Turn, bool) {
	var e jsonlEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return Turn{}, false
	}
	switch e.Type {
	case "user", "assistant":
		// fall through
	default:
		return Turn{}, false
	}
	var m msgEnvelope
	if err := json.Unmarshal(e.Message, &m); err != nil {
		return Turn{}, false
	}
	ts := parseTS(e.Timestamp)
	// content can be either a bare string or a JSON array of blocks.
	if len(m.Content) > 0 && m.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			return Turn{}, false
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return Turn{}, false
		}
		return Turn{Role: m.Role, Ts: ts, Text: s}, true
	}
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return Turn{}, false
	}
	var text strings.Builder
	var tool *ToolCall
	var toolResult string
	var toolErrored bool
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if s := strings.TrimSpace(b.Text); s != "" {
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString(s)
			}
		case "tool_use":
			tool = &ToolCall{Name: b.Name, Args: summarizeToolInput(b.Input)}
		case "tool_result":
			toolErrored = toolErrored || b.IsError
			if s := summarizeToolResult(b.Content); s != "" {
				if toolResult != "" {
					toolResult += "\n"
				}
				toolResult += s
			}
			// Keep walking: a tool_result row may also carry a trailing text block.
		}
	}
	switch m.Role {
	case "assistant":
		if tool != nil {
			// Fold the assistant's thinking/text (if any) into the tool row's args
			// summary so the UI shows a single compact entry.
			return Turn{Role: "assistant", Ts: ts, Text: text.String(), Tool: tool}, true
		}
		if text.Len() == 0 {
			return Turn{}, false
		}
		return Turn{Role: "assistant", Ts: ts, Text: text.String()}, true
	case "user":
		if text.Len() > 0 {
			return Turn{Role: "user", Ts: ts, Text: text.String()}, true
		}
		// Nothing but tool_result blocks. Render only when it errored OR carried
		// a short result — the common success case is already implied by the
		// preceding tool_use row.
		if toolErrored || toolResult != "" {
			role := "tool_result"
			head := toolResult
			if toolErrored {
				head = "[error] " + head
			}
			return Turn{Role: role, Ts: ts, Text: head}, true
		}
		return Turn{}, false
	}
	return Turn{}, false
}

// summarizeToolInput folds a tool_use input blob into one line. The exact
// keys differ per tool; we pick the most informative one when we can.
func summarizeToolInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "query", "url", "description", "prompt"} {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return oneLine(k + "=" + s)
			}
		}
	}
	// Fallback: space-joined "k=v" of the top-level scalar keys, truncated.
	// Keys are sorted so the summary is deterministic across runs — Go map
	// iteration order would otherwise vary each call.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch vv := m[k].(type) {
		case string:
			parts = append(parts, k+"="+vv)
		case float64:
			parts = append(parts, fmt.Sprintf("%s=%v", k, vv))
		case bool:
			parts = append(parts, fmt.Sprintf("%s=%v", k, vv))
		}
		if len(parts) >= 4 {
			break
		}
	}
	return oneLine(strings.Join(parts, " "))
}

// summarizeToolResult: the "content" of a tool_result can be a bare string or
// a list of {type: text, text}. Fold either into one short line.
func summarizeToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return oneLine(s)
		}
		return ""
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var buf strings.Builder
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				if buf.Len() > 0 {
					buf.WriteByte(' ')
				}
				buf.WriteString(b.Text)
			}
		}
		return oneLine(buf.String())
	}
	return ""
}

// oneLine squashes whitespace and truncates to a terminal-friendly length.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	// Collapse runs of whitespace to one space.
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	s = b.String()
	const max = 180
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// parseTS: the JSONL timestamp is RFC3339Nano; a missing or malformed one
// returns the zero value, which the front-end treats as "no timestamp".
func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// bytesLastIndexByte: avoids a `bytes` import for one call.
func bytesLastIndexByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// splitLines splits a buffer that ends on a newline into one []byte per line
// (without the trailing newline). The buffer must be newline-terminated.
func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	return out
}

func trimTrailingCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}
