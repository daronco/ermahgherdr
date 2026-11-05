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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
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

// transcriptCwdScanBytes is the fast-path read per transcript. The cwd shows
// up on every attachment / user / assistant row, so a few KB usually covers
// it; header rows without it (queue-operation, last-prompt, …) can still run
// past the budget, and then the rest of the file is scanned line by line.
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

// firstTranscriptCwd returns the first cwd value in a transcript file.
// ok=false when the file records none.
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
	if m == nil && n == len(buf) {
		m = scanCwdByLine(io.MultiReader(bytes.NewReader(buf), f))
	}
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

// scanCwdByLine matches cwdFieldRE one JSONL row at a time, so a match never
// straddles a read boundary and memory stays bounded by the longest row.
func scanCwdByLine(r io.Reader) [][]byte {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if m := cwdFieldRE.FindSubmatch(line); m != nil {
			return m
		}
		if err != nil {
			return nil
		}
	}
}

// Turn: one row the PWA renders. Tool calls fold into a single Tool entry so
// the UI collapses a message that is "assistant thinking + N tool_use blocks"
// to one row by default.
type Turn struct {
	Role    string       `json:"role"`              // "user" | "assistant" | "tool_result" | "system"
	Ts      time.Time    `json:"ts"`                // the transcript's own timestamp
	Text    string       `json:"text"`              // user-visible text; the one-line result head for a tool_result carrier; the full raw body for system
	Summary string       `json:"summary,omitempty"` // one-line label for a collapsed system turn
	Tool    *ToolCall    `json:"tool,omitempty"`    // non-nil for an assistant turn whose body is a tool call
	Pasted  []PastedSpan `json:"pasted,omitempty"`  // byte ranges in Text that were unwrapped from <pasted_content> blocks
}

// ToolCall: the folded summary of an assistant tool_use block.
//
// FilePath and Command hold the raw input value for the tools that care —
// Edit/Write/MultiEdit expose FilePath, Bash exposes Command. DiffPreview is a
// short sample (a handful of − / + lines) the sheet shows above the fold;
// DiffFull is the complete synthesized diff for the in-sheet expander. Both
// are empty for tools that have no diff to render. The fields stay raw: the
// client renders them with textContent, so a payload with "<script>" or any
// other HTML cannot create DOM nodes.
//
// Status, Result, ResultText, Meta and DurationMs are populated when the
// matching tool_result row lands — see attachToolResult. A pending tool_use
// (no result yet) keeps Status empty; the client renders the step with a
// spinner glyph while that is true.
type ToolCall struct {
	Name        string     `json:"name"`                   // "Bash", "Edit", ...
	Args        string     `json:"args"`                   // one-line summary of the tool input
	FilePath    string     `json:"file_path,omitempty"`    // Edit/Write/MultiEdit target path
	Command     string     `json:"command,omitempty"`      // Bash command, verbatim
	DiffPreview []DiffLine `json:"diff_preview,omitempty"` // first few − / + lines (for the sheet's collapsed preview)
	DiffFull    []DiffLine `json:"diff_full,omitempty"`    // full synthesized diff (Edit/Write/MultiEdit)
	Status      string     `json:"status,omitempty"`       // "ok" | "err" | "" for pending
	Result      string     `json:"result,omitempty"`       // one-line head of the tool_result body
	ResultText  string     `json:"result_text,omitempty"`  // full tool_result body (trimmed to toolResultTextCap)
	Meta        string     `json:"meta,omitempty"`         // +N −M, N lines, …
	DurationMs  int64      `json:"duration_ms,omitempty"`  // ts(tool_result) − ts(tool_use)
}

// DiffLine: one row of a synthesized diff. Kind is "ctx" | "del" | "add";
// Text is the line verbatim, with no leading "+" / "−" marker — the client
// renders the marker from Kind so the server never has to escape user text.
type DiffLine struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// PastedSpan marks a slice of a user turn's Text that was originally wrapped
// in <pasted_content id="…">…</pasted_content>. Start is inclusive, End is
// exclusive, both measured in UTF-16 code units so a JS consumer can feed
// them straight to String.prototype.slice. The Text field itself stays
// UTF-8 — only the span offsets are UTF-16 indices.
type PastedSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
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
//
// A pure tool_result-only user row no longer becomes its own Turn. Instead we
// pull its data out via parseToolResultRow and patch it into the preceding
// assistant Turn in this batch that owns a tool_use with no Status yet. When
// no match is found in this batch — the tool_use landed in an earlier batch
// (SSE delta boundary) — we emit an "orphan" Turn carrying just the
// tool_result fields; the client reconciles it into the last pending step
// already on screen without rendering a sibling.
func parseLines(b []byte) []Turn {
	var turns []Turn
	for _, line := range splitLines(b) {
		line = trimTrailingCR(line)
		if len(line) == 0 {
			continue
		}
		if r, ok := parseToolResultRow(line); ok {
			if !attachToolResult(turns, r) {
				turns = append(turns, orphanToolResultTurn(r))
			}
			continue
		}
		if t, ok := parseTurn(line); ok {
			turns = append(turns, t)
		}
	}
	return turns
}

// toolResultTextCap bounds the full result body we carry on the ToolCall. The
// client only reveals the body on a tap; 8 KB keeps the payload small while
// covering a typical Read/Grep output.
const toolResultTextCap = 8 << 10

// toolResultRow is the extracted payload of a tool_result-only user row.
type toolResultRow struct {
	ts       time.Time
	head     string // one-line summary of the body
	fullText string // full body, trimmed to toolResultTextCap
	isError  bool
	rawBody  json.RawMessage // first tool_result block's raw content (for extractToolMeta)
}

// parseToolResultRow returns the tool_result payload for a user row whose
// structured content is nothing but tool_result blocks. Rows that mix text
// and tool_result keep their old behaviour (they come out as user turns via
// parseTurn) and this function returns ok=false for them.
func parseToolResultRow(line []byte) (toolResultRow, bool) {
	var e jsonlEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return toolResultRow{}, false
	}
	if e.Type != "user" {
		return toolResultRow{}, false
	}
	var m msgEnvelope
	if err := json.Unmarshal(e.Message, &m); err != nil {
		return toolResultRow{}, false
	}
	if m.Role != "user" || len(m.Content) == 0 || m.Content[0] == '"' {
		return toolResultRow{}, false
	}
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return toolResultRow{}, false
	}
	r := toolResultRow{ts: parseTS(e.Timestamp)}
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			if b.IsError {
				r.isError = true
			}
			if r.rawBody == nil && len(b.Content) > 0 {
				r.rawBody = append(r.rawBody, b.Content...)
			}
			if s := summarizeToolResult(b.Content); s != "" {
				if r.head != "" {
					r.head += "\n"
				}
				r.head += s
			}
			if s := fullToolResultText(b.Content); s != "" {
				if r.fullText != "" {
					r.fullText += "\n"
				}
				r.fullText += s
			}
		default:
			// Any non-tool_result block (notably text) means this is a mixed
			// row — let parseTurn handle it the old way.
			return toolResultRow{}, false
		}
	}
	if r.rawBody == nil && r.head == "" && r.fullText == "" && !r.isError {
		return toolResultRow{}, false
	}
	return r, true
}

// attachToolResult walks turns from the tail for the first assistant turn
// carrying a tool_use with no Status yet and populates the result fields on
// it. Returns true when a match was found; false when the tool_use is not in
// this batch and the caller must emit an orphan row instead.
//
// TODO: assumes sequential tool execution — walks for the nearest pending
// call. Parallel tool_use blocks with interleaved results need result-id
// correlation (tool_use_id on the tool_result vs id on the tool_use).
func attachToolResult(turns []Turn, r toolResultRow) bool {
	for i := len(turns) - 1; i >= 0; i-- {
		t := &turns[i]
		if t.Tool == nil || t.Tool.Status != "" {
			continue
		}
		applyToolResult(t.Tool, r, t.Ts)
		return true
	}
	return false
}

// applyToolResult writes the result fields onto a ToolCall in place.
// callTs is the timestamp of the matching tool_use row — used to derive
// DurationMs. It may be zero for a tool_use whose row had no timestamp.
func applyToolResult(tc *ToolCall, r toolResultRow, callTs time.Time) {
	if r.isError {
		tc.Status = "err"
	} else {
		tc.Status = "ok"
	}
	tc.Result = r.head
	tc.ResultText = r.fullText
	if !callTs.IsZero() && !r.ts.IsZero() {
		d := r.ts.Sub(callTs).Milliseconds()
		if d < 0 {
			d = 0
		}
		tc.DurationMs = d
	}
	if tc.Status == "ok" {
		tc.Meta = extractToolMeta(tc, r.rawBody, r.fullText)
	}
}

// orphanToolResultTurn builds the carrier Turn the server emits when a
// tool_result arrives in a batch whose matching tool_use already shipped in
// an earlier delta. The client recognises Role="tool_result" + Tool filled,
// does not render it as a sibling, and patches the result onto the last
// pending step in its DOM.
func orphanToolResultTurn(r toolResultRow) Turn {
	tc := &ToolCall{}
	applyToolResult(tc, r, time.Time{})
	return Turn{Role: "tool_result", Ts: r.ts, Text: r.head, Tool: tc}
}

// extractToolMeta derives the right-hand meta label from a successful
// tool_result. Edit/Write read the add/del counts off the already-synthesized
// diff (the call's input is the source of truth for what changed, and the
// result body does not reliably carry a +N −M header). Read/Grep count the
// newlines in the body. Everything else returns empty — the client falls
// back to the step's elapsed time.
func extractToolMeta(tc *ToolCall, rawBody json.RawMessage, fullText string) string {
	switch tc.Name {
	case "Edit", "Write", "MultiEdit":
		added, removed := 0, 0
		for _, d := range tc.DiffFull {
			switch d.Kind {
			case "add":
				added++
			case "del":
				removed++
			}
		}
		if added == 0 && removed == 0 {
			return ""
		}
		return fmt.Sprintf("+%d −%d", added, removed)
	case "Read", "Grep":
		n := countLines(fullText)
		if n == 0 {
			return ""
		}
		return fmt.Sprintf("%d lines", n)
	}
	return ""
}

// countLines returns the number of lines in s. A body with no trailing
// newline still counts its final line.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// fullToolResultText dumps the whole tool_result body (bounded to
// toolResultTextCap) so the client can expand the nested step output with
// "Show all N lines". Shares its block-walking shape with summarizeToolResult
// but keeps every line instead of collapsing to one.
func fullToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return capBody(s)
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var buf strings.Builder
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(b.Text)
		if buf.Len() >= toolResultTextCap {
			break
		}
	}
	return capBody(buf.String())
}

func capBody(s string) string {
	if len(s) <= toolResultTextCap {
		return s
	}
	// Back up to the nearest rune boundary so the cut never lands inside a
	// multi-byte UTF-8 sequence.
	cut := toolResultTextCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…[truncated]"
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
//
// IsCompactSummary marks the "user" row Claude Code writes when the previous
// context was auto-compacted. The marker lives on the envelope; a transcript
// that pre-dates it still gets caught by the leading-sentence fallback in
// parseTurn.
type jsonlEntry struct {
	Type             string          `json:"type"`
	Timestamp        string          `json:"timestamp"`
	Message          json.RawMessage `json:"message"`
	IsCompactSummary bool            `json:"isCompactSummary"`
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
		if m.Role == "user" {
			if isCompactionSummary(e.IsCompactSummary, s) {
				return Turn{Role: "system", Ts: ts, Text: s, Summary: compactionSummaryLabel}, true
			}
			if role, summary, ok := classifyHarnessText(s); ok {
				return Turn{Role: role, Ts: ts, Text: s, Summary: summary}, true
			}
			text, pasted := unwrapPastedBlocks(s)
			return Turn{Role: "user", Ts: ts, Text: text, Pasted: pasted}, true
		}
		return Turn{Role: m.Role, Ts: ts, Text: s}, true
	}
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return Turn{}, false
	}
	var text strings.Builder
	var tool *ToolCall
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
			tool = buildToolCall(b.Name, b.Input)
			// tool_result blocks on a user row are claimed upstream by
			// parseLines → parseToolResultRow before parseTurn ever sees them.
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
			s := text.String()
			if isCompactionSummary(e.IsCompactSummary, s) {
				return Turn{Role: "system", Ts: ts, Text: s, Summary: compactionSummaryLabel}, true
			}
			if role, summary, ok := classifyHarnessText(s); ok {
				return Turn{Role: role, Ts: ts, Text: s, Summary: summary}, true
			}
			cleaned, pasted := unwrapPastedBlocks(s)
			return Turn{Role: "user", Ts: ts, Text: cleaned, Pasted: pasted}, true
		}
		// Pure tool_result rows are claimed by parseLines before they ever
		// reach parseTurn, so a user row with no text has nothing left to say.
		return Turn{}, false
	}
	return Turn{}, false
}

// compactionSummaryLabel: the one-line chip the PWA shows for a user row that
// is in fact the auto-compaction summary Claude Code wrote in place of the
// previous conversation.
const compactionSummaryLabel = "📦 conversation compacted"

// compactionLeadingSentence: the opening of the compaction summary Claude
// Code emits — used as a fallback for transcripts written before the
// isCompactSummary envelope marker existed.
const compactionLeadingSentence = "This session is being continued from a previous conversation"

// isCompactionSummary decides whether a user row is the auto-compaction
// summary. The envelope marker wins; the leading-sentence fallback catches
// older transcripts.
func isCompactionSummary(marker bool, body string) bool {
	if marker {
		return true
	}
	return strings.HasPrefix(strings.TrimLeft(body, " \t\r\n"), compactionLeadingSentence)
}

// pastedBlockRE matches a complete <pasted_content id="…">…</pasted_content>
// wrapper. The id attribute is required so a bare mention of the tag — the
// kind you'd see in a message that discusses the feature — is left alone.
// The id value itself is not captured: it's just a disambiguator so two
// pasted blocks in one message can be told apart; the content we care about
// is the inner body (capture group 1). (?s) lets `.` cross newlines; the
// inner match is non-greedy so two blocks in one message don't collapse.
var pastedBlockRE = regexp.MustCompile(`(?s)<pasted_content id="[^"]*">(.*?)</pasted_content>`)

// unwrapPastedBlocks strips every full <pasted_content id="…">…</pasted_content>
// wrapper from s, keeps the inner text verbatim in place, and returns the
// UTF-16 code-unit ranges each pasted block now occupies in the cleaned
// string — JS String.prototype.slice indexes in UTF-16 code units, so we
// pre-convert here to keep the client oblivious to the encoding mismatch.
// Surrounding text is left exactly as it was. A string without any complete
// wrapper comes back unchanged with a nil span list.
func unwrapPastedBlocks(s string) (string, []PastedSpan) {
	matches := pastedBlockRE.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	spans := make([]PastedSpan, 0, len(matches))
	prev := 0
	u16 := 0
	for _, m := range matches {
		// m[0], m[1] — full match; m[2], m[3] — inner (capture group 1).
		pre := s[prev:m[0]]
		b.WriteString(pre)
		u16 += utf16Len(pre)
		start := u16
		inner := s[m[2]:m[3]]
		b.WriteString(inner)
		u16 += utf16Len(inner)
		spans = append(spans, PastedSpan{Start: start, End: u16})
		prev = m[1]
	}
	b.WriteString(s[prev:])
	return b.String(), spans
}

// utf16Len returns the length of s measured in UTF-16 code units — the unit
// JS String.length / String.prototype.slice uses. BMP code points cost one
// unit, supplementary-plane ones (U+10000 and above) cost two (surrogate
// pair). Invalid UTF-8 bytes decode to U+FFFD, which costs one.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// harnessTags: non-task-notification harness wrappers Claude Code injects as
// user-role rows. The PWA labels these "YOU" by default; the parser reclaims
// them as "system" so the UI can render a compact chip instead.
var harnessTags = []string{
	"<system-reminder>",
	"<command-name>",
	"<command-message>",
	"<local-command-stdout>",
}

// taskNotificationSummaryRE pulls the inner summary out of a <task-notification>
// envelope. The opening tag may carry attributes (type="…"), so a prefix match
// is enough — the summary is what we actually show.
var taskNotificationSummaryRE = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)

// classifyHarnessText decides whether a user-role content string is a harness
// injection and, if so, returns the role to reclassify it as plus a one-line
// summary. A plain prompt returns ok=false.
func classifyHarnessText(s string) (role, summary string, ok bool) {
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if strings.HasPrefix(trimmed, "<task-notification") {
		head := strings.TrimSpace(firstNonEmptyLine(trimmed))
		if m := taskNotificationSummaryRE.FindStringSubmatch(trimmed); m != nil {
			head = strings.TrimSpace(html.UnescapeString(m[1]))
		}
		return "system", oneLine(head), true
	}
	for _, tag := range harnessTags {
		if strings.HasPrefix(trimmed, tag) {
			name := tag[1 : len(tag)-1] // strip "<" and ">"
			body := strings.TrimPrefix(trimmed, tag)
			line := firstNonEmptyLine(body)
			line = strings.TrimSuffix(strings.TrimSpace(line), "</"+name+">")
			line = strings.TrimSpace(html.UnescapeString(line))
			if line == "" {
				return "system", name, true
			}
			return "system", oneLine(name + ": " + line), true
		}
	}
	return "", "", false
}

// firstNonEmptyLine returns the first line of s that has any non-whitespace
// character. An empty string comes back when every line is blank.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// diffPreviewMaxLines caps how many − / + lines the sheet shows above its
// Full-diff expander. A single tap must still be enough to tell what is being
// approved; anything beyond this count is only reachable through the expander.
const diffPreviewMaxLines = 6

// buildToolCall folds a tool_use block into the row the client renders. It
// always sets Name and Args (the one-line summary the existing UI shows);
// Edit/Write/MultiEdit and Bash also populate the structured preview fields
// the permission sheet needs.
func buildToolCall(name string, input json.RawMessage) *ToolCall {
	tc := &ToolCall{Name: name, Args: summarizeToolInput(input)}
	if len(input) == 0 {
		return tc
	}
	switch name {
	case "Edit":
		populateEdit(tc, input)
	case "Write":
		populateWrite(tc, input)
	case "MultiEdit":
		populateMultiEdit(tc, input)
	case "Bash":
		populateBash(tc, input)
	}
	return tc
}

// populateEdit reads {file_path, old_string, new_string} and synthesizes a
// diff: every line of old_string is a "del", every line of new_string is an
// "add". There is no shared-context detection — Claude's own "Edit" semantics
// are "replace this block with that block", and the user only needs to see
// the two sides to decide.
func populateEdit(tc *ToolCall, raw json.RawMessage) {
	var in struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return
	}
	tc.FilePath = in.FilePath
	tc.DiffFull = synthesizeEditDiff(in.OldString, in.NewString)
	tc.DiffPreview = headDiffLines(tc.DiffFull, diffPreviewMaxLines)
}

// populateWrite treats a Write as an Edit whose old side is empty: every line
// of content is an "add". file_path drives the preview label.
func populateWrite(tc *ToolCall, raw json.RawMessage) {
	var in struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return
	}
	tc.FilePath = in.FilePath
	tc.DiffFull = synthesizeEditDiff("", in.Content)
	tc.DiffPreview = headDiffLines(tc.DiffFull, diffPreviewMaxLines)
}

// populateMultiEdit concatenates the synthesized diff of each edit in input
// order, so one tool call reads as one diff — the sheet already has to be
// compact, a per-edit header would make the preview noisier for no gain.
func populateMultiEdit(tc *ToolCall, raw json.RawMessage) {
	var in struct {
		FilePath string `json:"file_path"`
		Edits    []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return
	}
	tc.FilePath = in.FilePath
	var full []DiffLine
	for _, e := range in.Edits {
		full = append(full, synthesizeEditDiff(e.OldString, e.NewString)...)
	}
	tc.DiffFull = full
	tc.DiffPreview = headDiffLines(full, diffPreviewMaxLines)
}

// populateBash just lifts the command string. There is no diff — the sheet
// renders Command as a single mono block instead.
func populateBash(tc *ToolCall, raw json.RawMessage) {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return
	}
	tc.Command = in.Command
}

// synthesizeEditDiff builds the DiffLine slice an Edit / Write / MultiEdit
// replacement represents: each line of old is a "del", each line of new is an
// "add". Trailing newlines are stripped before splitting so the diff does not
// carry a phantom blank line at the end of each side.
func synthesizeEditDiff(oldSide, newSide string) []DiffLine {
	var out []DiffLine
	if s := strings.TrimRight(oldSide, "\n"); s != "" {
		for _, line := range strings.Split(s, "\n") {
			out = append(out, DiffLine{Kind: "del", Text: line})
		}
	}
	if s := strings.TrimRight(newSide, "\n"); s != "" {
		for _, line := range strings.Split(s, "\n") {
			out = append(out, DiffLine{Kind: "add", Text: line})
		}
	}
	return out
}

// headDiffLines returns the first n lines of a diff, or the whole slice when
// it's shorter. Used for the preview; the client checks len(DiffFull) >
// len(DiffPreview) to decide whether the Full-diff expander is worth showing.
func headDiffLines(full []DiffLine, n int) []DiffLine {
	if len(full) <= n {
		if len(full) == 0 {
			return nil
		}
		out := make([]DiffLine, len(full))
		copy(out, full)
		return out
	}
	out := make([]DiffLine, n)
	copy(out, full[:n])
	return out
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
