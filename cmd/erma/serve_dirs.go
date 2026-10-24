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

// recentDir is the on-the-wire shape of GET /api/dirs/recent.
type recentDir struct {
	Path     string `json:"path"`
	LastUsed int64  `json:"lastUsed"` // unix seconds, transcript file mtime
}

// recentDirsConfig lets tests swap the cwd source and the path validator.
type recentDirsConfig struct {
	cwds     func() []session.TranscriptCwd
	validate func(string) (string, error)
	limit    int
}

func defaultRecentDirsConfig() recentDirsConfig {
	return recentDirsConfig{
		cwds:     session.TranscriptCwds,
		validate: validateHomePath,
		limit:    30,
	}
}

// recentDirs collates unique cwds from the Claude transcripts, newest first.
// A cwd that no longer passes validateHomePath (missing on disk, outside
// $HOME) is dropped silently — this list is only ever shown as a suggestion.
// Entries with no cwd field are skipped rather than guessed at.
func recentDirs(cfg recentDirsConfig) []recentDir {
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
		out = append(out, recentDir{Path: clean, LastUsed: r.MTime.Unix()})
		if cfg.limit > 0 && len(out) >= cfg.limit {
			break
		}
	}
	return out
}
