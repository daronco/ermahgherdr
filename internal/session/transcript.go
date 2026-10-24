// Transcript reader: parses the Claude Code JSONL at
// ~/.claude/projects/*/<sid>.jsonl into the compact Turn stream the PWA
// renders. The file is append-only — the reader keeps a byte-offset cursor so
// a tail only pays for the new lines.
//
// Confinement: the sid is NEVER a path. Every call resolves the file through
// a glob under the Claude Code projects root, and the match is checked to live
// inside that root. A sid with "..", a leading "/" or any other separator is
// refused up front: the glob alone would follow "*/../.." out of the root.

package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// TranscriptCwd: one project's recorded working directory, carried by the
// Claude Code JSONL transcript. MTime is the file's modification time — the
// moment Claude last appended a turn — and is what the recent-dirs list sorts
// by. Transcripts outlive the session itself: the pid-keyed file at
// ~/.claude/sessions/<pid>.json is deleted on /exit and when the tmux pane
// closes, but the transcript stays under ~/.claude/projects/.
type TranscriptCwd struct {
	Cwd   string
	MTime time.Time
}

// cwdFieldRE picks up a top-level cwd:"..." pair from a JSONL row. Avoiding a
// full JSON decode keeps the read to one small buffer per transcript — the
// recent-dirs page can touch hundreds of files, and parsing the whole first
// line each time was measurable at that fan-out.
var cwdFieldRE = regexp.MustCompile(`"cwd"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// transcriptCwdScanBytes bounds the read per transcript. The cwd shows up on
// every attachment / user / assistant row Claude Code writes, so a few KB is
// plenty — the only rows without it are a handful of header entries
// (queue-operation, mode, atis-latch, last-prompt).
const transcriptCwdScanBytes = 16 << 10

// TranscriptCwds walks every Claude Code transcript under the projects root
// and returns the cwd each one records, newest first by file mtime. A
// transcript that carries no cwd (killed before the first attachment or turn
// landed) is skipped.
func TranscriptCwds() []TranscriptCwd {
	root := claudeProjectsRoot()
	files, _ := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	out := make([]TranscriptCwd, 0, len(files))
	for _, f := range files {
		cwd, ok := firstTranscriptCwd(f)
		if !ok {
			continue
		}
		var mt time.Time
		if fi, err := os.Stat(f); err == nil {
			mt = fi.ModTime()
		}
		out = append(out, TranscriptCwd{Cwd: cwd, MTime: mt})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].MTime.After(out[j].MTime)
	})
	return out
}

// firstTranscriptCwd reads the leading chunk of a transcript file and returns
// the first cwd value found. ok=false when none appears within the budget.
func firstTranscriptCwd(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	buf := make([]byte, transcriptCwdScanBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", false
	}
	m := cwdFieldRE.FindSubmatch(buf[:n])
	if m == nil {
		return "", false
	}
	var s string
	if err := json.Unmarshal([]byte(`"`+string(m[1])+`"`), &s); err != nil {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

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

// Transcript: one emission from WatchTranscript. Reset=true means the file was
// rotated/truncated (or this is the first dump), and the consumer should
// discard whatever it had and replace it with Turns.
type Transcript struct {
	Turns  []Turn
	Cursor Cursor
	Start  Cursor // on a Reset: where its turns begin, for paging back
	Reset  bool
}

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

// sidShape: a Claude session id is a UUID. Letters, digits and dashes cover it
// without tying the reader to one version, and leave nothing a path or a glob
// pattern could use — no separator, no dot, no * ? [.
var sidShape = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

func validSid(sid string) bool { return sidShape.MatchString(sid) }

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
	return parseLines(consumed), Cursor(int64(after) + int64(len(consumed))), nil
}

// TailBytes: how far back the conversation view reaches when it opens, and
// how much each "load earlier" page adds. A few dozen exchanges, and a
// response of a few hundred KB however large the transcript has grown.
const TailBytes = 256 << 10

// ReadConversationBefore reads the complete lines that end at or before
// `before` — the end of the file when before <= 0 or past it — reaching back
// about max bytes, further only to finish a line longer than that. It returns
// the turns, the offset of the first line read (0 once the start of the file
// is reached; the next page's `before`) and the offset after the last complete
// line (where a forward tail picks up).
func ReadConversationBefore(sid string, before Cursor, max int64) ([]Turn, Cursor, Cursor, error) {
	path, err := transcriptPath(sid)
	if err != nil {
		return nil, 0, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, ErrUnknownSession
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, 0, err
	}
	end := int64(before)
	if end <= 0 || end > st.Size() {
		end = st.Size()
	}
	for lo := end - max; ; lo -= max {
		if lo < 0 {
			lo = 0
		}
		from := lo
		if lo > 0 {
			from = lo - 1 // one byte back: is lo already the start of a line?
		}
		buf := make([]byte, end-from)
		n, err := f.ReadAt(buf, from)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, 0, err
		}
		buf = buf[:n]
		start := 0
		if lo > 0 {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				continue // inside one long line: reach further back
			}
			start = i + 1
		}
		stop := bytesLastIndexByte(buf, '\n') + 1
		if stop <= start {
			if lo > 0 {
				continue
			}
			return nil, 0, 0, nil // not one complete line yet
		}
		return parseLines(buf[start:stop]), Cursor(from + int64(start)), Cursor(from + int64(stop)), nil
	}
}

// parseLines folds newline-terminated JSONL into the turns the UI renders.
func parseLines(b []byte) []Turn {
	var turns []Turn
	for _, line := range splitLines(b) {
		line = trimTrailingCR(line)
		if len(line) == 0 {
			continue
		}
		if t, ok := parseTurn(line); ok {
			turns = append(turns, t)
		}
	}
	return turns
}

// transcriptSize: the current size of sid's transcript.
func transcriptSize(sid string) (int64, error) {
	path, err := transcriptPath(sid)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
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

// WatchTranscript tails sid's JSONL file at interval, emitting a Transcript on
// the returned channel every time new turns show up. With from < 0 the first
// emission is a Reset carrying the tail (ReadConversationBefore, TailBytes);
// with from >= 0 it resumes there and sends only what is new. A file that
// shrinks below the cursor (rotation, truncation) sends a fresh tail as a
// Reset. The goroutine ends — and the channel closes — when ctx is cancelled.
//
// A tick that reads nothing new emits nothing: the caller gets at most one
// frame per real change, so a quiet session does not wake its consumers.
func WatchTranscript(ctx context.Context, sid string, interval time.Duration, from Cursor) <-chan Transcript {
	out := make(chan Transcript)
	tail := func() Transcript {
		// A missing file still yields an empty Reset, so a subscriber knows it is
		// connected; the next tick picks the file up when it appears.
		turns, start, end, _ := ReadConversationBefore(sid, 0, TailBytes)
		return Transcript{Turns: turns, Cursor: end, Start: start, Reset: true}
	}
	go func() {
		defer close(out)
		send := func(tr Transcript) bool {
			select {
			case <-ctx.Done():
				return false
			case out <- tr:
				return true
			}
		}
		cursor := from
		if from < 0 {
			tr := tail()
			cursor = tr.Cursor
			if !send(tr) {
				return
			}
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			size, err := transcriptSize(sid)
			if err != nil {
				continue // the transcript may vanish mid-stream; the next tick may recover
			}
			if size < int64(cursor) {
				tr := tail()
				cursor = tr.Cursor
				if !send(tr) {
					return
				}
				continue
			}
			turns, next, err := ReadConversation(sid, cursor)
			if err != nil || next == cursor {
				continue
			}
			cursor = next
			if len(turns) > 0 && !send(Transcript{Turns: turns, Cursor: cursor}) {
				return
			}
		}
	}()
	return out
}
