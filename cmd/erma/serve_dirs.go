// erma serve — recent directory discovery for the "New session" sheet.
//
// The source is the cwd each Claude Code session recorded in its JSONL
// transcript under ~/.claude/projects/, ordered by file mtime. That file
// outlives the session itself: the pid-keyed registry at
// ~/.claude/sessions/<pid>.json is deleted on /exit and when the tmux pane
// closes (SIGHUP), so a directory the user visited yesterday would otherwise
// vanish from the list the next day. The transcript is the only history we
// get for a session the user walked away from — we deliberately do not
// persist a separate recents list on disk.
//
// We stat each cwd (via validateHomePath) before showing it so the sheet
// only lists directories the launcher could actually open.
package main

import (
	"sort"

	"github.com/daronco/ermahgherdr/internal/session"
)

// recentDir is the on-the-wire shape of GET /api/dirs/recent. Open is the
// number of live Claude sessions whose pane_current_path resolves to this
// directory — the sheet renders it as an "N open" badge.
type recentDir struct {
	Path     string `json:"path"`
	LastUsed int64  `json:"lastUsed"` // unix seconds, transcript file mtime
	Open     int    `json:"open,omitempty"`
}

// recentDirsConfig lets tests swap the cwd source, the path validator, and the
// live-session dir snapshot the Open count joins against.
type recentDirsConfig struct {
	cwds     func() []session.TranscriptCwd
	validate func(string) (string, error)
	liveDirs func() []string
	limit    int
}

func defaultRecentDirsConfig() recentDirsConfig {
	return recentDirsConfig{
		cwds:     session.TranscriptCwds,
		validate: validateHomePath,
		liveDirs: defaultLiveDirs,
		limit:    30,
	}
}

// defaultLiveDirs reads the raw pane_current_path of every live Claude pane the
// tmux poller knows about. These are the symlink-unresolved, non-home-abbrev'd
// dirs: validateHomePath canonicalises them inside recentDirs so a session in
// /var/x and a transcript in /private/var/x count together on macOS.
func defaultLiveDirs() []string {
	// GatherReadOnly errors are non-fatal; the badge degrades to 0.
	rows, _ := session.GatherReadOnly(nil)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Dir != "" {
			out = append(out, r.Dir)
		}
	}
	return out
}

// recentDirs collates unique cwds from the Claude transcripts, newest first.
// A cwd that no longer passes validateHomePath (missing on disk, outside
// $HOME) is dropped silently — this list is only ever shown as a suggestion.
// Entries with no cwd field are skipped rather than guessed at.
//
// Each returned row's Open is the number of live Claude sessions whose
// pane_current_path validates to the same canonical path.
func recentDirs(cfg recentDirsConfig) []recentDir {
	// Count live sessions per canonical directory so a recent-dir row can
	// carry the "N open" badge. Validation runs on both sides of the compare
	// so paths that differ only in symlink resolution still match.
	openCounts := map[string]int{}
	if cfg.liveDirs != nil {
		for _, d := range cfg.liveDirs() {
			if d == "" {
				continue
			}
			clean, err := cfg.validate(d)
			if err != nil {
				continue
			}
			openCounts[clean]++
		}
	}
	rows := cfg.cwds()
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].MTime.After(rows[j].MTime)
	})
	seen := map[string]bool{}
	out := make([]recentDir, 0, len(rows))
	for _, r := range rows {
		if r.Cwd == "" {
			continue
		}
		clean, err := cfg.validate(r.Cwd)
		if err != nil {
			continue
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, recentDir{Path: clean, LastUsed: r.MTime.Unix(), Open: openCounts[clean]})
		if cfg.limit > 0 && len(out) >= cfg.limit {
			break
		}
	}
	return out
}
