package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

// stubValidator wires validateHomePath around an in-memory filesystem so the
// table-driven cases below don't depend on anything on disk.
func stubValidator(home string, dirs, files map[string]bool) homePathValidator {
	return homePathValidator{
		homeDir:     func() (string, error) { return home, nil },
		evalSymlink: func(p string) (string, error) { return p, nil },
		stat: func(p string) (os.FileInfo, error) {
			if dirs[p] {
				return fakeFileInfo{name: filepath.Base(p), dir: true}, nil
			}
			if files[p] {
				return fakeFileInfo{name: filepath.Base(p), dir: false}, nil
			}
			return nil, os.ErrNotExist
		},
	}
}

func TestValidateHomePathAccepts(t *testing.T) {
	home := "/home/alice"
	v := stubValidator(home, map[string]bool{
		"/home/alice":              true,
		"/home/alice/projects":     true,
		"/home/alice/projects/foo": true,
	}, nil)
	for _, in := range []string{
		"/home/alice",
		"/home/alice/projects",
		"/home/alice/projects/foo",
		"/home/alice/projects/foo/", // trailing slash — cleaned to the same path
		"/home/alice/./projects/foo",
		"/home/alice/projects/bar/../foo",
	} {
		got, err := v.validate(in)
		if err != nil {
			t.Errorf("validate(%q) unexpected error: %v", in, err)
			continue
		}
		if !strings.HasPrefix(got, home) {
			t.Errorf("validate(%q) = %q, want path inside %q", in, got, home)
		}
	}
}

func TestValidateHomePathRejects(t *testing.T) {
	home := "/home/alice"
	v := stubValidator(home, map[string]bool{
		"/home/alice":          true,
		"/home/alice/projects": true,
		"/etc":                 true,
		"/home/bob":            true,
	}, map[string]bool{
		"/home/alice/README.md": true,
	})
	for _, c := range []struct {
		name, in, contains string
	}{
		{"empty", "", "empty"},
		{"relative", "projects/foo", "not absolute"},
		{"dot-prefix", "./projects", "not absolute"},
		{"parent escape via dotdot", "/home/alice/../bob", "not inside"},
		{"outside home absolute", "/etc", "not inside"},
		{"sibling home", "/home/bob", "not inside"},
		{"nonexistent inside home", "/home/alice/does-not-exist", "does not exist"},
		{"file not directory", "/home/alice/README.md", "not a directory"},
		{"nul byte", "/home/alice/\x00evil", "NUL"},
		{"home-prefix lookalike", "/home/alice-evil", "not inside"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := v.validate(c.in)
			if err == nil {
				t.Fatalf("validate(%q): want error, got nil", c.in)
			}
			if c.contains != "" && !strings.Contains(err.Error(), c.contains) {
				t.Errorf("validate(%q) err = %q, want contains %q", c.in, err.Error(), c.contains)
			}
		})
	}
}

func TestValidateHomePathHomeResolveFailure(t *testing.T) {
	v := homePathValidator{
		homeDir:     func() (string, error) { return "", errors.New("boom") },
		evalSymlink: func(p string) (string, error) { return p, nil },
		stat:        func(p string) (os.FileInfo, error) { return fakeFileInfo{dir: true}, nil },
	}
	_, err := v.validate("/anywhere")
	if err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("expected home-resolve error, got %v", err)
	}
}

// A symlinked home still contains paths given under its canonical name.
func TestValidateHomePathFollowsSymlinks(t *testing.T) {
	v := homePathValidator{
		homeDir: func() (string, error) { return "/home/alice", nil },
		evalSymlink: func(p string) (string, error) {
			if p == "/home/alice" {
				return "/mnt/users/alice", nil
			}
			return p, nil
		},
		stat: func(p string) (os.FileInfo, error) {
			if p == "/mnt/users/alice/projects/foo" {
				return fakeFileInfo{dir: true}, nil
			}
			return nil, os.ErrNotExist
		},
	}
	got, err := v.validate("/mnt/users/alice/projects/foo")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "/mnt/users/alice/projects/foo" {
		t.Errorf("got %q", got)
	}
	// A path given under the pre-resolve name must NOT slip through, because
	// filepath.Clean does not follow symlinks and the containment check uses
	// the resolved home.
	if _, err := v.validate("/home/alice/projects/foo"); err == nil {
		t.Errorf("validate on pre-resolve name should fail")
	}
}

// An input path that is itself a symlink pointing outside $HOME must be
// rejected: filepath.Clean does not follow symlinks, so without resolving
// the input side the prefix check would pass and tmux would open the target.
func TestValidateHomePathRejectsInputSymlinkOutsideHome(t *testing.T) {
	v := homePathValidator{
		homeDir: func() (string, error) { return "/home/alice", nil },
		evalSymlink: func(p string) (string, error) {
			if p == "/home/alice/link" {
				return "/etc", nil
			}
			return p, nil
		},
		stat: func(p string) (os.FileInfo, error) {
			if p == "/etc" || p == "/home/alice/link" {
				return fakeFileInfo{dir: true}, nil
			}
			return nil, os.ErrNotExist
		},
	}
	_, err := v.validate("/home/alice/link")
	if err == nil {
		t.Fatalf("expected rejection for input symlink pointing outside $HOME")
	}
	if !strings.Contains(err.Error(), "not inside") {
		t.Errorf("err = %q, want contains 'not inside'", err.Error())
	}
}
