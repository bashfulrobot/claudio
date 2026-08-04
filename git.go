package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// gitTimeout bounds every git invocation from the spawn path -- a stale NFS
// mount or a hung git process must degrade the worktree decision, never wedge
// the HTTP handler.
const gitTimeout = 5 * time.Second

// gitWorktreeSupport reports whether `claude --worktree` can safely be used
// in dir, and if not, a short human-readable reason fit for a phone toast.
// Any failure to run git at all (missing binary, timeout, not a repo) returns
// (false, "", reason) rather than an error: a git problem must never block
// the primary spawn path, only skip the -w flag.
//
// repoRoot is the resolved common .git directory's parent (the main
// worktree's top level), used by worktreeName to check for existing
// worktrees/branches. It is only meaningful when ok is true.
func gitWorktreeSupport(gitBin, dir string) (ok bool, repoRoot, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, gitBin, "rev-parse", "--git-dir", "--git-common-dir", "--is-bare-repository")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false, "", "not a git repo"
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) < 3 {
		return false, "", "unexpected git output"
	}
	if lines[2] == "true" {
		return false, "", "bare repo"
	}

	gitDir := resolveGitPath(dir, lines[0])
	commonDir := resolveGitPath(dir, lines[1])
	if gitDir == "" || commonDir == "" {
		return false, "", "unexpected git output"
	}
	if gitDir != commonDir {
		// A linked worktree's --git-dir (.../.git/worktrees/<name>) always
		// differs from its --git-common-dir (the main repo's .git). Equal
		// paths mean this IS the main checkout, at any depth.
		return false, "", "already a git worktree"
	}

	return true, filepath.Dir(commonDir), ""
}

// resolveGitPath turns one line of `git rev-parse --git-dir`/`--git-common-dir`
// output into a clean, symlink-resolved absolute path. git may print either
// form depending on cwd depth (absolute at a repo root, relative in a
// subdirectory), so a raw string compare between the two flags is unreliable
// -- deliberately NOT filepath.Abs, which would resolve against claudio's own
// process cwd rather than dir. Falls back to the cleaned, non-symlink-resolved
// path if the target doesn't exist yet (defensive; rev-parse output should
// always exist).
func resolveGitPath(dir, p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// worktreeNamePattern matches the characters git worktree add and branch
// names both tolerate; enforced by validateSpawnOptions before a name ever
// reaches worktreeName, but re-checked here too since this is also where an
// auto-suffix gets appended.
var worktreeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// worktreeName returns a worktree name free of collisions with both an
// existing worktree directory under repoRoot/.claude/worktrees and an
// existing git branch, starting from base. On collision it suffixes -HHMM,
// then -HHMMSS if that's still taken -- spawning twice in the same repo is
// the common case, since the name defaults to the folder's basename.
func worktreeName(gitBin, repoRoot, base string) string {
	if !worktreeNamePattern.MatchString(base) {
		base = "session"
	}

	candidates := []string{base}
	now := time.Now()
	candidates = append(candidates, base+"-"+now.Format("1504"))
	candidates = append(candidates, base+"-"+now.Format("150405"))

	for _, c := range candidates {
		if !worktreeCollides(gitBin, repoRoot, c) {
			return c
		}
	}
	// Every candidate collided (pathological, e.g. rapid repeated spawns
	// within the same second) -- fall back to a name git itself will reject
	// on genuine collision, surfaced via the bounded-wait error path.
	return candidates[len(candidates)-1]
}

func worktreeCollides(gitBin, repoRoot, name string) bool {
	if _, err := os.Stat(filepath.Join(repoRoot, ".claude", "worktrees", name)); err == nil {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitBin, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	cmd.Dir = repoRoot
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// Exit 0 means the ref exists (a collision); any non-zero exit
	// (including "not found") means it doesn't. Errors starting the
	// process itself are treated as "no collision" -- worktreeCollides is
	// advisory, the bounded wait in spawnSession is the real backstop.
	return cmd.Run() == nil
}
