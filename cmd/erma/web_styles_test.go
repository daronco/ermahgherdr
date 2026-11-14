package main

import (
	"regexp"
	"strings"
	"testing"
)

// A var() with no fallback whose custom property is never declared renders
// as if the rule were absent. Text merges of styles.css can produce this
// silently (a rule added on main using a token another branch renamed).
func TestStylesReferenceOnlyDeclaredProperties(t *testing.T) {
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(--[\w-]+)\s*:`).FindAllStringSubmatch(string(css), -1) {
		declared[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`var\(\s*(--[\w-]+)\s*\)`).FindAllStringSubmatch(string(css), -1) {
		if !declared[m[1]] {
			t.Errorf("styles.css uses var(%s) without a fallback, but never declares it", m[1])
		}
	}
}

// Inline code in an assistant turn must wrap long unbreakable tokens in place
// so a ~70-char span can't blow out the 390 px viewport. Fenced blocks keep
// `white-space: pre` and scroll inside their <pre>.
func TestInlineCodeWrapsAndFencedCodePreserves(t *testing.T) {
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	src := string(css)

	inline := extractRuleBody(t, src, ".turn-body code")
	if strings.Contains(inline, "white-space: nowrap") {
		t.Errorf(".turn-body code must NOT declare `white-space: nowrap` — long inline spans would push the page past the 390 px viewport. Got:\n%s", inline)
	}
	hasWrap := strings.Contains(inline, "overflow-wrap: anywhere") ||
		strings.Contains(inline, "overflow-wrap: break-word") ||
		strings.Contains(inline, "word-break: break-all") ||
		strings.Contains(inline, "word-break: break-word")
	if !hasWrap {
		t.Errorf(".turn-body code must declare a breaking rule (overflow-wrap: anywhere / word-break: break-word) so long tokens wrap inside the turn. Got:\n%s", inline)
	}
	hasClone := strings.Contains(inline, "box-decoration-break: clone")
	if !hasClone {
		t.Errorf(".turn-body code must declare `box-decoration-break: clone` so the pill chip keeps its padding + background on every wrapped line. Got:\n%s", inline)
	}

	fenced := extractRuleBody(t, src, ".turn-body pre code")
	if !strings.Contains(fenced, "white-space: pre") {
		t.Errorf(".turn-body pre code must keep `white-space: pre` so fenced code continues to scroll inside the <pre> box. Got:\n%s", fenced)
	}
}

// extractRuleBody returns the body (between { and }) of the first rule in src
// whose selector list matches selector exactly. Strips /* ... */ comments
// first so a `{` or `}` inside one cannot confuse the scanner. Nested braces
// are not expected in this stylesheet.
func extractRuleBody(t *testing.T, src, selector string) string {
	t.Helper()
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	i := 0
	for i < len(src) {
		open := strings.IndexByte(src[i:], '{')
		if open < 0 {
			break
		}
		selEnd := i + open
		closeRel := strings.IndexByte(src[selEnd+1:], '}')
		if closeRel < 0 {
			break
		}
		body := src[selEnd+1 : selEnd+1+closeRel]
		// Selector text is from the last `}` (end of previous rule) or start
		// of file up to this `{`. Everything in between is whitespace plus
		// the selector list.
		selStart := strings.LastIndexByte(src[:selEnd], '}') + 1
		sel := strings.TrimSpace(src[selStart:selEnd])
		for _, part := range strings.Split(sel, ",") {
			if strings.TrimSpace(part) == selector {
				return body
			}
		}
		i = selEnd + 1 + closeRel + 1
	}
	t.Fatalf("styles.css does not define a rule for selector %q", selector)
	return ""
}
