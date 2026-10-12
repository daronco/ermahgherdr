package session

import (
	"strings"
	"testing"
)

// Smoke test: the parser is correct wherever it is called from. cmd/erma holds
// the exhaustive table (see cmd/erma/main_test.go); this proves the package is
// usable without the bar.
func TestReadScreenSmoke(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want Screen
	}{
		{"working", "✢ Reticulating… (15m 52s)\n───\n❯ \n───\n", Screen{Busy: true}},
		{"stopped", "✻ Churned for 15s\n───\n❯ \n───\n", Screen{}},
		{"permission", " Do you want to create X?\n ❯ 1. Yes\n   2. No\n\n Esc to cancel\n",
			Screen{Prompt: true, Form: true}},
	} {
		if got := ReadScreen(c.body); got != c.want {
			t.Errorf("%s: ReadScreen = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestStatusOfSmoke(t *testing.T) {
	for _, c := range []struct {
		wait string
		sc   Screen
		seen bool
		want State
	}{
		{"perm", Screen{Prompt: true}, true, Urgent},
		{"", Screen{Form: true}, true, Urgent},
		{"working", Screen{Busy: true}, true, Proc},
		{"done", Screen{}, true, Done},
		{"", Screen{}, true, Idle},
	} {
		if got := StatusOf(c.wait, c.sc, c.seen); got != c.want {
			t.Errorf("StatusOf(%q, %+v, %v) = %v, want %v",
				c.wait, c.sc, c.seen, got, c.want)
		}
	}
}

func TestArrangeSmoke(t *testing.T) {
	rows := []Session{
		{Name: "a", Flag: "prio"}, {Name: "b"}, {Name: "c", Flag: "paused"},
		{Name: "dai", Dai: true},
	}
	Arrange(rows, false)
	if rows[0].Name != "dai" {
		t.Errorf("Dai not first: %v", rows)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if got := strings.Join(names, " "); got != "dai b a c" {
		t.Errorf("order bottom-up: %q", got)
	}
}

func TestBandOf(t *testing.T) {
	for _, c := range []struct {
		fl   string
		want int
	}{{"", 0}, {"prio", 1}, {"paused", 2}, {"other", 0}} {
		if got := BandOf(c.fl); got != c.want {
			t.Errorf("BandOf(%q) = %d, want %d", c.fl, got, c.want)
		}
	}
}
