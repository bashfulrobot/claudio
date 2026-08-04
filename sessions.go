package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// spawnGrace bounds how long spawnSession waits for `claude --bg` to either
// exit (an argv/git error) or settle into the background before the HTTP
// handler gives up and treats it as a successful launch. `claude --bg`
// itself returns almost immediately once it has forked; this window exists
// to catch the failure case, not to slow down the success case.
const spawnGrace = 3 * time.Second

// maxPromptBytes caps the initial-prompt field -- long prompts belong in the
// session itself, not the spawn call, and this keeps argv well inside ARG_MAX.
const maxPromptBytes = 4000

// worktreeCwdMarker is the path segment Claude Code's own -w/--worktree flag
// uses for every worktree it creates: <repo>/.claude/worktrees/<name>.
const worktreeCwdMarker = "/.claude/worktrees/"

var (
	allowedModels = map[string]bool{
		"sonnet": true, "opus": true, "fable": true, "haiku": true,
	}
	allowedEfforts = map[string]bool{
		"low": true, "medium": true, "high": true, "xhigh": true, "max": true,
	}
	// bypassPermissions is included deliberately: a spawned session can
	// already reach it by being instructed to add
	// --dangerously-skip-permissions itself, so this isn't a new capability,
	// and spawn options never persist across sessions (see index.html).
	allowedPermissionModes = map[string]bool{
		"acceptEdits": true, "auto": true, "bypassPermissions": true,
		"manual": true, "dontAsk": true, "plan": true,
	}
)

// Session mirrors one entry of `claude agents --json`, plus two
// claudio-computed fields.
type Session struct {
	PID       int    `json:"pid"`
	ID        string `json:"id"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"`
	StartedAt int64  `json:"startedAt"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	State     string `json:"state"`

	// Repo and Worktree do NOT come from `claude agents --json` -- they're
	// derived by splitting Cwd on the "/.claude/worktrees/" convention
	// Claude Code's own -w/--worktree flag uses, so the phone UI can show
	// "myrepo ⑂ my-worktree" instead of a full path that just ellipsizes.
	// Display-only: this is a heuristic on a path convention Anthropic
	// controls and could change, so nothing here may branch on it.
	Repo     string `json:"repo,omitempty"`
	Worktree string `json:"worktree,omitempty"`
}

// SpawnOptions are the optional per-session overrides the web UI's
// collapsed "Options" panel can set. Every field left at its zero value is
// omitted from the claude invocation entirely, so claude falls through to
// whatever ~/.claude/settings.json already says -- there is no sentinel
// "default" value threaded through this struct.
type SpawnOptions struct {
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	PermissionMode string `json:"permissionMode"`
	Prompt         string `json:"prompt"`
	// Worktree is *bool, not bool: the default is true (worktree-by-default),
	// and a plain bool zero-values to false when the JSON key is absent,
	// which would silently invert that default for any client that omits
	// it. nil means "use the default"; only an explicit false disables it.
	Worktree *bool `json:"worktree"`
}

// SpawnResult reports what spawnSession actually did, beyond the plain
// success/error today's callers expect -- worktree-by-default is only
// honest if the caller is told when isolation silently didn't happen.
type SpawnResult struct {
	Name         string `json:"name"`
	Worktree     string `json:"worktree,omitempty"`        // name actually passed to --worktree
	WorktreeSkip string `json:"worktreeSkipped,omitempty"` // why -w was omitted, if it was
}

// validateSpawnOptions rejects anything that could be parsed as a new CLI
// flag instead of the value it's meant to be. exec.Command takes an argv
// slice with no shell involved, so there's no quoting/metacharacter
// injection -- but claude's OWN argument parser is still an injection
// surface: a model value of "--dangerously-skip-permissions" or
// "--settings /tmp/evil.json" is parsed by claude as a brand new flag on a
// process holding the caller's real subscription credentials and tool
// permissions. claudio binds to a Tailscale IP, so anything on the tailnet
// can POST this JSON directly -- the <select> elements in the web UI are a
// convenience, not the security boundary; this allowlist is.
func validateSpawnOptions(name string, opts SpawnOptions) error {
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("session name must not start with '-'")
	}
	if opts.Model != "" && !allowedModels[opts.Model] {
		return fmt.Errorf("unknown model %q", opts.Model)
	}
	if opts.Effort != "" && !allowedEfforts[opts.Effort] {
		return fmt.Errorf("unknown effort level %q", opts.Effort)
	}
	if opts.PermissionMode != "" && !allowedPermissionModes[opts.PermissionMode] {
		return fmt.Errorf("unknown permission mode %q", opts.PermissionMode)
	}
	if len(opts.Prompt) > maxPromptBytes {
		return fmt.Errorf("initial prompt too long (max %d bytes)", maxPromptBytes)
	}
	if strings.ContainsRune(opts.Prompt, 0) {
		return fmt.Errorf("initial prompt must not contain NUL bytes")
	}
	return nil
}

// capWriter is a size-bounded, concurrency-safe io.Writer for capturing a
// spawned child's stderr. A plain *bytes.Buffer isn't safe here: assigning
// any non-*os.File to cmd.Stderr makes os/exec spawn its own copier
// goroutine, and spawnSession's bounded-wait timeout path reads the buffer
// from the HTTP handler's goroutine concurrently with that copier -- a data
// race. The cap also keeps a chatty or looping child from growing this
// unboundedly while claudio waits it out.
type capWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if remain := w.max - w.buf.Len(); remain > 0 {
		if remain > len(p) {
			remain = len(p)
		}
		w.buf.Write(p[:remain])
	}
	// Always report the full length written, even though it was truncated:
	// returning a short count would make os/exec treat this as a write
	// error and tear down the child's stderr pipe early.
	return len(p), nil
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// splitWorktreeCwd derives claudio's display-only Repo/Worktree labels from
// a session's cwd, per the Session.Worktree doc comment above.
func splitWorktreeCwd(cwd string) (repo, worktree string, ok bool) {
	repo, worktree, found := strings.Cut(cwd, worktreeCwdMarker)
	if !found || repo == "" || worktree == "" {
		return "", "", false
	}
	return repo, worktree, true
}

// listSessions shells out to `claude agents --json`, which lists every
// interactive and background Claude Code session currently running for
// this user (no TTY required).
func listSessions(claudeBin string) ([]Session, error) {
	// Deliberately omit --all: that flag also returns completed background
	// sessions, which would leave killed sessions lingering in the "Running"
	// list. Default --json is already scoped to active sessions only.
	cmd := exec.Command(claudeBin, "agents", "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("claude agents --json: %w (stderr: %s)", err, stderr.String())
	}

	var sessions []Session
	if err := json.Unmarshal(stdout.Bytes(), &sessions); err != nil {
		return nil, fmt.Errorf("parsing claude agents output: %w", err)
	}

	for i := range sessions {
		if repo, wt, ok := splitWorktreeCwd(sessions[i].Cwd); ok {
			sessions[i].Repo = repo
			sessions[i].Worktree = wt
		}
	}

	return sessions, nil
}

// spawnSession launches a new detached, backgrounded Claude Code session in
// cwd, registered under name for both `claude agents` and Anthropic's
// Remote Control (claude.ai/code, iOS app). It intentionally does not use
// -p/--print: this is a full interactive-equivalent session, not a one-shot.
//
// Unless opts.Worktree is explicitly false, the session is isolated into a
// git worktree by default (see gitWorktreeSupport in git.go) so that
// concurrent sessions spawned from the phone never share a checkout and
// stomp on each other's uncommitted work.
func spawnSession(claudeBin, gitBin, cwd, name string, opts SpawnOptions) (SpawnResult, error) {
	res := SpawnResult{}

	info, err := os.Stat(cwd)
	if err != nil {
		return res, fmt.Errorf("folder does not exist: %s", cwd)
	}
	if !info.IsDir() {
		return res, fmt.Errorf("not a directory: %s", cwd)
	}
	if name == "" {
		name = filepath.Base(cwd)
	}
	res.Name = name

	if err := validateSpawnOptions(name, opts); err != nil {
		return res, err
	}

	args := []string{"--bg", "--name", name, "--remote-control", name}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	if opts.PermissionMode != "" {
		args = append(args, "--permission-mode", opts.PermissionMode)
	}

	wantWorktree := opts.Worktree == nil || *opts.Worktree
	if wantWorktree {
		if ok, repoRoot, reason := gitWorktreeSupport(gitBin, cwd); ok {
			wt := worktreeName(gitBin, repoRoot, name)
			// Always the long form with an explicit value: -w/--worktree
			// takes an OPTIONAL value, so a bare -w immediately followed by
			// the positional prompt below would have claude's parser
			// swallow the prompt text as the worktree name instead.
			args = append(args, "--worktree", wt)
			res.Worktree = wt
		} else {
			res.WorktreeSkip = reason
		}
	} else {
		res.WorktreeSkip = "disabled"
	}

	if opts.Prompt != "" {
		// "--" must come last, after every flag including --worktree, so a
		// prompt that happens to start with "-" is taken as the positional
		// prompt argument rather than parsed as yet another flag.
		args = append(args, "--", opts.Prompt)
	}

	cmd := exec.Command(claudeBin, args...)
	cmd.Dir = cwd
	// Detach into its own session/process group so it survives past the
	// lifetime of this HTTP request and isn't killed if claudio restarts.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	stderr := &capWriter{max: 4096}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("failed to launch claude: %w", err)
	}

	// Bounded wait, not fire-and-forget: worktree-by-default adds a git
	// mutation (worktree add) to every spawn that can fail for reasons
	// unrelated to what the user typed -- a name collision the pre-check in
	// worktreeName missed, a repo with no commits yet, a detached HEAD, an
	// unwritable .claude/worktrees. `claude --bg` returns almost immediately
	// once it has successfully forked into the background, and an argv/git
	// error exits fast too, so waiting a few seconds turns "spawn sometimes
	// silently does nothing" into a real error message without meaningfully
	// slowing down the success path.
	done := make(chan error, 1) // buffered: the timeout branch below must not leak a blocked sender
	go func() { done <- cmd.Wait() }()

	select {
	case werr := <-done:
		if werr != nil {
			return res, fmt.Errorf("claude exited immediately: %w (stderr: %s)", werr, stderr.String())
		}
	case <-time.After(spawnGrace):
		// Still running past the grace period: treat it as a successful
		// background launch and keep reaping in a detached goroutine so it
		// doesn't become a zombie. Do not touch stderr from here once the
		// HTTP handler may have already returned and read it -- log only.
		go func() {
			if werr := <-done; werr != nil {
				log.Printf("claude --bg for %q exited after the grace period: %v (stderr: %s)", name, werr, stderr.String())
			}
		}()
	}

	return res, nil
}

// killSession permanently deletes the background session identified by id
// (the short `id` field from `claude agents --json`, not its pid), including
// its worktree if it has one.
//
// This uses `claude rm <id>`, a real subcommand that isn't listed in
// `claude --help` but is documented at code.claude.com/docs/en/agent-view
// and confirmed working via `claude rm --help` on this install. It's the
// same primitive the interactive agent view's Ctrl+X-twice binding uses.
//
// Signaling the worker process directly does NOT work: background sessions
// run under a `claude bg-pty-host` supervisor owned by a long-lived daemon
// that transparently relaunches a killed worker (new pid, same session)
// within seconds — confirmed by hand, SIGTERM'ing both the worker and its
// immediate pty-host parent were each silently undone by a respawn. `rm`
// is the actual sanctioned way to make a session go away for good.
func killSession(claudeBin string, id string) error {
	sessions, err := listSessions(claudeBin)
	if err != nil {
		return err
	}

	found := false
	for _, s := range sessions {
		if s.ID == id {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no known claude session with id %q", id)
	}

	var stderr bytes.Buffer
	cmd := exec.Command(claudeBin, "rm", id)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude rm %s: %w (stderr: %s)", id, err, stderr.String())
	}
	return nil
}

// stopSession stops the background session identified by id, keeping its
// conversation intact so it can be resumed later with `claude attach <id>`.
// Unlike killSession/`claude rm`, this does NOT delete the session's
// worktree, and the session drops out of listSessions' output afterward
// (claude agents --json without --all only reports active sessions -- see
// the comment there).
func stopSession(claudeBin string, id string) error {
	sessions, err := listSessions(claudeBin)
	if err != nil {
		return err
	}

	found := false
	for _, s := range sessions {
		if s.ID == id {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no known claude session with id %q", id)
	}

	var stderr bytes.Buffer
	cmd := exec.Command(claudeBin, "stop", id)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude stop %s: %w (stderr: %s)", id, err, stderr.String())
	}
	return nil
}
