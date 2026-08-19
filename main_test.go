package main

import (
	"os"
	"testing"
)

// The state table is where the bug lived: a mark alone could pin a session on
// red forever. These cases pin down who wins between the mark and the screen.
func TestStatusOf(t *testing.T) {
	for _, c := range []struct {
		name string
		wait string
		sc   screen
		seen bool
		want state
	}{
		{"permission up", "perm", screen{prompt: true}, true, urgent},
		{"perm, pane unread", "perm", screen{}, false, urgent},
		{"approved, now working", "perm", screen{busy: true}, true, proc},
		{"working", "working", screen{busy: true}, true, proc},
		{"working, stopped without a Stop hook", "working", screen{}, true, done},
		{"working, pane unread", "working", screen{}, false, proc},
		{"finished", "done", screen{}, true, done},
		{"finished, then off again", "done", screen{busy: true}, true, proc},
		{"read", "waiting", screen{}, true, idle},
		{"no hook, quiet", "", screen{}, true, idle},
		{"no hook, working", "", screen{busy: true}, true, proc},
	} {
		if got := statusOf(c.wait, c.sc, c.seen); got != c.want {
			t.Errorf("%s: statusOf(%q, %+v, %v) = %v, want %v",
				c.name, c.wait, c.sc, c.seen, got, c.want)
		}
	}
}

func TestPermAlive(t *testing.T) {
	for _, c := range []struct {
		name   string
		sc     screen
		seen   bool
		age    int64
		misses int
		want   bool
	}{
		{"prompt on screen", screen{prompt: true}, true, 999, 9, true},
		{"pane could not be read", screen{}, false, 999, 9, true},
		{"still within the grace", screen{}, true, 1, 1, true},
		{"first miss", screen{}, true, 999, 1, true},
		{"second miss", screen{}, true, 999, 2, false},
	} {
		if got := permAlive(c.sc, c.seen, c.age, c.misses); got != c.want {
			t.Errorf("%s: permAlive(%+v, %v, %d, %d) = %v, want %v",
				c.name, c.sc, c.seen, c.age, c.misses, got, c.want)
		}
	}
}

func TestReadScreen(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want screen
	}{
		{"working", "● Read(main.go)\n✢ Reticulating… (15m 52s · ↓ 45.1k tokens)\n───\n❯ \n───\n  ⏵⏵ auto mode on\n",
			screen{busy: true}},
		{"stopped", "● all set\n✻ Churned for 15s\n─── a session ──\n❯ \n───\n  ⏵⏵ auto mode on\n",
			screen{}},
		// verbatim from Claude Code 2.1.236: the question is phrased per tool,
		// so "Do you want to proceed?" is not the string to look for.
		{"permission", " Create file\n permprobe-xyz.txt\n╌╌╌\n  1 hello\n╌╌╌\n" +
			" Do you want to create permprobe-xyz.txt?\n ❯ 1. Yes\n" +
			"   2. Yes, and switch to accept edits for this session (shift+tab)\n   3. No\n\n" +
			" Esc to cancel · Tab to amend\n",
			screen{prompt: true}},
		{"question", "Choose an option:\n❯ 1. Keep it\n  2. Drop it\n", screen{prompt: true}},
		{"typed at the input box", "─── a session ──\n❯ 2. also check the other one\n───\n", screen{}},
		{"a numbered list Claude wrote", "● Steps:\n  1. build\n  2. test\n  3. ship\n─── a session ──\n❯ \n",
			screen{}},
	} {
		if got := readScreen(c.body); got != c.want {
			t.Errorf("%s: readScreen = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// testdata/screen-*.txt are whole panes captured off a live Claude Code, before
// and after answering a real permission prompt.
func TestReadScreenCaptures(t *testing.T) {
	for _, c := range []struct {
		file string
		want screen
	}{
		{"testdata/screen-permission.txt", screen{prompt: true}},
		{"testdata/screen-answered.txt", screen{}},
	} {
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		if got := readScreen(string(b)); got != c.want {
			t.Errorf("%s: readScreen = %+v, want %+v", c.file, got, c.want)
		}
	}
}
