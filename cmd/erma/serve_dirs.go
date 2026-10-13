// erma serve — recent directory discovery for the "New session" sheet.
//
// The server never trusts cwds blindly: a directory the registry remembers may
// have been deleted, and a leftover file from a crashed claude is pruned by
// session.Registry before we even see it. We stat each path (via validateHomePath)
// before showing it so the sheet only lists directories the launcher could
// actually open.
package main

import (
	"sort"

	"github.com/daronco/ermahgherdr/internal/session"
)

// recentDir is the on-the-wire shape of GET /api/dirs/recent.
type recentDir struct {
	Path     string `json:"path"`
	LastUsed int64  `json:"lastUsed"` // unix seconds, registry file mtime
}

// recentDirsConfig lets tests swap the registry source and the path validator.
type recentDirsConfig struct {
	registry func() []session.RegistryEntry
	validate func(string) (string, error)
	limit    int
}

func defaultRecentDirsConfig() recentDirsConfig {
	return recentDirsConfig{
		registry: session.Registry,
		validate: validateHomePath,
		limit:    30,
	}
}

// recentDirs collates unique cwds from the Claude registry, newest first. A
// cwd that no longer passes validateHomePath (missing on disk, outside $HOME)
// is dropped silently — this list is only ever shown as a suggestion. Entries
// with no cwd field are skipped rather than guessed at.
//
// Only directories of *currently running* claude sessions appear here:
// session.Registry drops rows whose pid is gone, so a directory the user
// visited yesterday and closed is not resurfaced. The registry file is the
// liveness signal; we deliberately do not keep a separate history on disk.
func recentDirs(cfg recentDirsConfig) []recentDir {
	rows := cfg.registry()
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
