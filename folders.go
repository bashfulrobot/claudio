package main

import (
	"os"
	"path/filepath"
	"sort"
)

// FolderGroup is one configured root (e.g. ~/git) and its subdirectories
// (recursively, up to maxDepth), for the phone UI's tap-to-pick folder list.
type FolderGroup struct {
	Root string   `json:"root"`
	Dirs []string `json:"dirs"`
}

// maxDepth bounds how many levels below each root get scanned, so a repo
// like ~/dev/kong/customers/nordstrom is reachable (3 levels below ~/dev)
// without walking the whole filesystem.
const maxDepth = 5

// skipDirs are directories never worth offering as a cwd to launch a
// session in -- dependency/build trees that can be huge and would
// otherwise blow up the recursive scan for no benefit.
var skipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"target":       true,
	"dist":         true,
	"build":        true,
	"venv":         true,
	".venv":        true,
	"__pycache__":  true,
	".terraform":   true,
	".direnv":      true,
}

// listFolders scans each configured root for subdirectories up to
// maxDepth levels deep. Hidden directories (leading dot) and skipDirs
// are skipped entirely (not descended into). Returned Dirs are paths
// relative to Root, e.g. "kong/customers/nordstrom".
func listFolders(roots []string) []FolderGroup {
	groups := make([]FolderGroup, 0, len(roots))
	for _, root := range roots {
		var dirs []string
		scanDir(root, "", 1, &dirs)
		sort.Strings(dirs)
		groups = append(groups, FolderGroup{Root: root, Dirs: dirs})
	}
	return groups
}

// scanDir appends every subdirectory of root+relPrefix (relative to root)
// into dirs, then recurses into each one until maxDepth is reached.
func scanDir(root, relPrefix string, depth int, dirs *[]string) {
	entries, err := os.ReadDir(filepath.Join(root, relPrefix))
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if len(name) == 0 || name[0] == '.' || skipDirs[name] || !e.IsDir() {
			continue
		}
		rel := name
		if relPrefix != "" {
			rel = relPrefix + "/" + name
		}
		*dirs = append(*dirs, rel)
		if depth < maxDepth {
			scanDir(root, rel, depth+1, dirs)
		}
	}
}
