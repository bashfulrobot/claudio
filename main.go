package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address (bind to your Tailscale IP to expose over the tailnet)")
	claudeBin := flag.String("claude-bin", "claude", "path to the claude CLI binary")
	gitBin := flag.String("git-bin", "git", "path to the git binary, used to decide whether a new session can be worktree-isolated")
	rootsFlag := flag.String("roots", defaultRoots(), "comma-separated project root directories to offer in the folder picker")
	flag.Parse()

	var roots []string
	for _, r := range strings.Split(*rootsFlag, ",") {
		r = strings.TrimSpace(r)
		if r != "" {
			roots = append(roots, r)
		}
	}

	srv := NewServer(*claudeBin, *gitBin, roots)

	log.Printf("claudio listening on %s (roots: %v)", *addr, roots)
	if err := http.ListenAndServe(*addr, srv); err != nil {
		log.Fatal(err)
	}
}

func defaultRoots() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return strings.Join([]string{
		filepath.Join(home, "git"),
		filepath.Join(home, "dev"),
	}, ",")
}
