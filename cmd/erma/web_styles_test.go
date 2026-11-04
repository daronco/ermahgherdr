package main

import (
	"regexp"
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
