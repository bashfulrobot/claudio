package main

import (
	"os"
	"sort"
)

// FolderGroup is one configured root (e.g. ~/git) and its immediate
// subdirectories, for the phone UI's tap-to-pick folder list.
type FolderGroup struct {
	Root string   `json:"root"`
	Dirs []string `json:"dirs"`
}

// listFolders scans each configured root for immediate subdirectories.
// Hidden directories (leading dot) are skipped.
func listFolders(roots []string) []FolderGroup {
	groups := make([]FolderGroup, 0, len(roots))
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		dirs := make([]string, 0, len(entries))
		for _, e := range entries {
			name := e.Name()
			if len(name) == 0 || name[0] == '.' {
				continue
			}
			if e.IsDir() {
				dirs = append(dirs, name)
			}
		}
		sort.Strings(dirs)
		groups = append(groups, FolderGroup{Root: root, Dirs: dirs})
	}
	return groups
}
