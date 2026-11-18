package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSessionFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBridgeURL(t *testing.T) {
	dir := t.TempDir()
	writeSessionFile(t, dir, "100.json", `{"sessionId":"uuid-1","bridgeSessionId":"session_abc123"}`)
	writeSessionFile(t, dir, "200.json", `{"sessionId":"uuid-2"}`)
	writeSessionFile(t, dir, "300.json", `{"sessionId":"uuid-3","bridgeSessionId":"session_x/../evil"}`)
	writeSessionFile(t, dir, "400.json", `{"sessionId":"uuid-4","bridgeSessionId":"other_abc"}`)
	writeSessionFile(t, dir, "500.json", `not json`)

	cases := []struct {
		name string
		pid  int
		uuid string
		want string
	}{
		{"running session", 100, "uuid-1", "https://claude.ai/code/session_abc123"},
		{"no bridge", 200, "uuid-2", ""},
		{"path trick in id", 300, "uuid-3", ""},
		{"wrong id prefix", 400, "uuid-4", ""},
		{"reused pid, other session", 100, "uuid-9", ""},
		{"broken file", 500, "x", ""},
		{"no file", 999, "uuid-1", ""},
		{"no pid", 0, "uuid-1", ""},
	}
	for _, c := range cases {
		if got := bridgeURL(dir, c.pid, c.uuid); got != c.want {
			t.Errorf("%s: bridgeURL = %q, want %q", c.name, got, c.want)
		}
	}
	if got := bridgeURL("", 100, "uuid-1"); got != "" {
		t.Errorf("empty config dir: %q", got)
	}
}

const connUUID = "6bc80ce3-09d9-4481-99ad-adaa3328b667"

// connectEnv stubs claude with one session (id run1, pid 42 unless stopped)
// in a real folder under a temp HOME, and returns the stub, its call log, the
// folder and the HOME.
func connectEnv(t *testing.T, running bool, name, sessionID string) (bin, log, repo, home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	repo = filepath.Join(home, "git", "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := "null"
	if running {
		pid = "42"
	}
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "claude")
	script := `#!/bin/sh
if [ "$1" = agents ]; then
  cat <<'JSON'
[{"id":"run1","name":"` + name + `","state":"working","status":"idle","pid":` + pid + `,"cwd":"` + repo + `","sessionId":"` + sessionID + `"}]
JSON
  exit 0
fi
echo "$*" >> ` + log + `
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log, repo, home
}

func TestConnectRestartsSessionWithRemoteControl(t *testing.T) {
	bin, log, repo, _ := connectEnv(t, true, "my work", connUUID)
	if err := connectSession(bin, "git", []string{filepath.Dir(repo)}, "run1"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	want := []string{"stop run1", "--bg --name my work --remote-control my work --resume " + connUUID}
	if len(lines) != len(want) || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("claude calls = %q, want %q", lines, want)
	}
}

func TestConnectRefusalsCallNothing(t *testing.T) {
	cases := []struct {
		name    string
		running bool
		sid     string
		roots   func(repo string) []string
		bridge  bool
	}{
		{"not running", false, connUUID, func(r string) []string { return []string{filepath.Dir(r)} }, false},
		{"already has a bridge", true, connUUID, func(r string) []string { return []string{filepath.Dir(r)} }, true},
		{"session id is a flag", true, "--dangerously-skip-permissions", func(r string) []string { return []string{filepath.Dir(r)} }, false},
		{"folder outside roots", true, connUUID, func(r string) []string { return []string{"/nonexistent-root"} }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin, log, repo, home := connectEnv(t, c.running, "my work", c.sid)
			if c.bridge {
				writeSessionFile(t, filepath.Join(home, ".claude"), "42.json", `{"sessionId":"`+c.sid+`","bridgeSessionId":"session_abc"}`)
			}
			if err := connectSession(bin, "git", c.roots(repo), "run1"); err == nil {
				t.Fatal("connect succeeded, want refusal")
			}
			if raw, _ := os.ReadFile(log); len(raw) != 0 {
				t.Errorf("claude was called: %s", raw)
			}
		})
	}
}

func TestConnectRenamesSessionWhoseNameLooksLikeAFlag(t *testing.T) {
	bin, log, repo, _ := connectEnv(t, true, "--model", connUUID)
	if err := connectSession(bin, "git", []string{filepath.Dir(repo)}, "run1"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(log)
	if !strings.Contains(string(raw), "--bg --name run1 --remote-control run1 --resume "+connUUID) {
		t.Fatalf("calls = %q, want the id used as the name", raw)
	}
}
