// erma serve — path validation for the new-session route.
//
// Every call that invokes tmux to open a window in a user-provided directory
// goes through validateHomePath first: the directory must be cleaned, absolute,
// contained in the user's home, and exist as a directory on disk. We never
// shell out, so an unsafe path only matters because tmux would obediently try
// to open it — and we don't want callers from the loopback perimeter to point
// us at /etc/, /var/, or a sibling account's tree.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// homePathValidator holds the dependencies the real implementation reaches for.
// Tests swap in in-memory stand-ins so the validator can be exercised against
// synthetic filesystems without touching $HOME.
type homePathValidator struct {
	homeDir     func() (string, error)
	evalSymlink func(string) (string, error)
	stat        func(string) (os.FileInfo, error)
}

// defaultHomePathValidator binds the real os/filepath calls.
var defaultHomePathValidator = homePathValidator{
	homeDir:     os.UserHomeDir,
	evalSymlink: filepath.EvalSymlinks,
	stat:        os.Stat,
}

// validateHomePath cleans p, requires it to be an existing directory, and
// requires it to sit inside the user's home directory (post symlink
// resolution). It returns the cleaned path on success and a short error on
// failure — the error reason is suitable for a 400 response body because it
// does not echo the raw input back.
func validateHomePath(p string) (string, error) {
	return defaultHomePathValidator.validate(p)
}

func (v homePathValidator) validate(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is empty")
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("path contains a NUL byte")
	}
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) {
		return "", errors.New("path is not absolute")
	}
	// Resolve the input too, not just home: otherwise /home/alice/link ->
	// /etc would pass the prefix check and tmux would open /etc. On error
	// (e.g. the path does not exist) leave clean as-is and let stat below
	// surface a specific reason.
	if resolved, err := v.evalSymlink(clean); err == nil {
		clean = filepath.Clean(resolved)
	}
	home, err := v.homeDir()
	if err != nil || home == "" {
		return "", errors.New("cannot resolve home directory")
	}
	home = filepath.Clean(home)
	if resolved, err := v.evalSymlink(home); err == nil {
		home = filepath.Clean(resolved)
	}
	sep := string(os.PathSeparator)
	if clean != home && !strings.HasPrefix(clean+sep, home+sep) {
		return "", errors.New("path is not inside the user's home directory")
	}
	info, err := v.stat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errors.New("path does not exist")
		}
		return "", errors.New("cannot stat path")
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return clean, nil
}
