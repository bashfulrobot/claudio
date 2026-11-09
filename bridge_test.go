package main

import (
	"os"
	"path/filepath"
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
