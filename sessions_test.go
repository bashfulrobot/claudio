package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubClaude writes a fake claude binary. `agents --json --all` prints the
// fixture, every other call is appended to a log, and `rm` for id "bad1"
// fails the way claude refuses a worktree with unpushed commits.
func stubClaude(t *testing.T) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "claude")
	script := `#!/bin/sh
if [ "$1" = agents ]; then
  case "$*" in *--all*) ;; *) echo "agents called without --all" >&2; exit 2;; esac
  cat <<'JSON'
[
 {"id":"run1","name":"live","state":"working","status":"busy","pid":42,"cwd":"/r/live","sessionId":"s1"},
 {"id":"stp1","name":"stopped","state":"stopped","status":null,"pid":null,"cwd":"/r/a","sessionId":"s2"},
 {"id":"don1","name":"done","state":"done","status":null,"pid":null,"cwd":"/r/b/.claude/worktrees/w","sessionId":"s3"},
 {"id":"bad1","name":"stuck","state":"blocked","status":null,"pid":null,"cwd":"/r/c","sessionId":"s4"}
]
JSON
  exit 0
fi
echo "$*" >> ` + logPath + `
if [ "$1" = rm ] && [ "$2" = bad1 ]; then echo "unpushed commits" >&2; exit 1; fi
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func calls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestListSessionsFlagsDisconnected(t *testing.T) {
	bin, _ := stubClaude(t)
	got, err := listSessions(bin)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"run1": false, "stp1": true, "don1": true, "bad1": true}
	if len(got) != len(want) {
		t.Fatalf("got %d sessions, want %d", len(got), len(want))
	}
	for _, s := range got {
		if s.Disconnected != want[s.ID] {
			t.Errorf("%s Disconnected = %v, want %v", s.ID, s.Disconnected, want[s.ID])
		}
	}
}

func TestResumeSession(t *testing.T) {
	bin, logPath := stubClaude(t)
	if err := resumeSession(bin, "stp1"); err != nil {
		t.Fatal(err)
	}
	if err := resumeSession(bin, "run1"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("running session: err = %v", err)
	}
	if err := resumeSession(bin, "nope"); err == nil {
		t.Fatal("unknown id accepted")
	}
	if c := calls(t, logPath); len(c) != 1 || c[0] != "respawn stp1" {
		t.Fatalf("calls = %v", c)
	}
}

func TestBulkRemoveReportsPerIDAndDedupes(t *testing.T) {
	bin, logPath := stubClaude(t)
	res, err := bulkAction(bin, "remove", []string{"stp1", "bad1", "stp1", "ghost"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("got %d results, want 3 (deduped): %+v", len(res), res)
	}
	byID := map[string]BulkResult{}
	for _, r := range res {
		byID[r.ID] = r
	}
	if !byID["stp1"].OK {
		t.Errorf("stp1 should succeed: %+v", byID["stp1"])
	}
	if byID["bad1"].OK || !strings.Contains(byID["bad1"].Error, "unpushed commits") {
		t.Errorf("bad1 should carry claude's refusal: %+v", byID["bad1"])
	}
	if byID["ghost"].OK {
		t.Errorf("unknown id should fail: %+v", byID["ghost"])
	}
	if c := calls(t, logPath); len(c) != 2 || c[0] != "rm stp1" || c[1] != "rm bad1" {
		t.Fatalf("calls = %v", c)
	}
}

func TestBulkActionValidation(t *testing.T) {
	bin, _ := stubClaude(t)
	if _, err := bulkAction(bin, "stop", []string{"stp1"}); err == nil {
		t.Error("unknown action accepted")
	}
	if _, err := bulkAction(bin, "remove", nil); err == nil {
		t.Error("empty ids accepted")
	}
	many := make([]string, maxBulkIDs+1)
	for i := range many {
		many[i] = "x"
	}
	if _, err := bulkAction(bin, "remove", many); err == nil {
		t.Error("over-limit ids accepted")
	}
}
