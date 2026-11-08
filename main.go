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
	hostFlag := flag.String("host-name", defaultHostName(), "label for this host in the session list")
	peersFlag := flag.String("peers", "", "other claudio instances as name=http://tailscale-ip:8787, comma-separated")
	flag.Parse()

	peers, err := parsePeers(*peersFlag)
	if err != nil {
		log.Fatalf("-peers: %v", err)
	}

	var roots []string
	for r := range strings.SplitSeq(*rootsFlag, ",") {
		r = strings.TrimSpace(r)
		if r != "" {
			roots = append(roots, r)
		}
	}

	srv := NewServer(*claudeBin, *gitBin, roots, *hostFlag, peers)

	log.Printf("claudio %q listening on %s (roots: %v, peers: %v)", *hostFlag, *addr, roots, peers)
	if err := http.ListenAndServe(*addr, srv); err != nil {
		log.Fatal(err)
	}
}

func defaultHostName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "local"
	}
	return strings.SplitN(h, ".", 2)[0]
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
