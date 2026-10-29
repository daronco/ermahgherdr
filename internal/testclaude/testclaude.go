// Package testclaude lets a test binary stand in for claude in a tmux pane.
// tmux names a pane after its process's argv[0], and a coreutils binary
// symlinked as "claude" will not do: uutils (Ubuntu 26.04+) refuses to run
// under a name that is not its own.
package testclaude

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Main turns the process into the fake claude when it was started under that
// name — it copies stdin to stdout until stdin closes. Call it first in TestMain.
func Main() {
	if filepath.Base(os.Args[0]) != "claude" {
		return
	}
	_, _ = io.Copy(os.Stdout, os.Stdin)
	os.Exit(0)
}

// Link puts a "claude" in dir that runs the current test binary as the fake.
func Link(t testing.TB, dir string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "claude")
	if err := os.Symlink(self, p); err != nil {
		t.Fatal(err)
	}
	return p
}
