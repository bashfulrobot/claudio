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

func TestConnectRestartsSessionWithRemoteControl(t *testing.T) {
	e := newParkEnv(t, true) // run1, pid 42, no bridge file
	if err := connectSession(e.bin, "git", "run1"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(e.log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	want := []string{"stop run1", "--bg --name my work --remote-control my work --resume uuid-1"}
	if len(lines) != len(want) || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("claude calls = %q, want %q", lines, want)
	}
}

func TestConnectRefusesSessionThatAlreadyHasRemoteControl(t *testing.T) {
	e := newParkEnv(t, true)
	writeSessionFile(t, filepath.Join(e.home, ".claude"), "42.json", `{"sessionId":"uuid-1","bridgeSessionId":"session_abc"}`)
	if err := connectSession(e.bin, "git", "run1"); err == nil {
		t.Fatal("connected a session that already has a bridge")
	}
	if raw, _ := os.ReadFile(e.log); len(raw) != 0 {
		t.Errorf("claude was called: %s", raw)
	}
}

func TestConnectRefusesStoppedSession(t *testing.T) {
	e := newParkEnv(t, false)
	if err := connectSession(e.bin, "git", "run1"); err == nil {
		t.Fatal("connected a session that is not running")
	}
}
