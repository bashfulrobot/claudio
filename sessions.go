package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Session mirrors one entry of `claude agents --json`.
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
	return sessions, nil
}

// spawnSession launches a new detached, backgrounded Claude Code session in
// cwd, registered under name for both `claude agents` and Anthropic's
// Remote Control (claude.ai/code, iOS app). It intentionally does not use
// -p/--print: this is a full interactive-equivalent session, not a one-shot.
func spawnSession(claudeBin, cwd, name string) error {
	info, err := os.Stat(cwd)
	if err != nil {
		return fmt.Errorf("folder does not exist: %s", cwd)
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", cwd)
	}
	if name == "" {
		name = filepath.Base(cwd)
	}

	cmd := exec.Command(claudeBin, "--bg", "--name", name, "--remote-control", name)
	cmd.Dir = cwd
	// Detach into its own session/process group so it survives past the
	// lifetime of this HTTP request and isn't killed if claudio restarts.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch claude: %w", err)
	}
	// Reap this specific child in the background so it doesn't become a
	// zombie once it exits; we don't wait on it before responding.
	go func(c *exec.Cmd) {
		_ = c.Wait()
	}(cmd)

	return nil
}

// killSession permanently deletes the background session identified by id
// (the short `id` field from `claude agents --json`, not its pid).
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
