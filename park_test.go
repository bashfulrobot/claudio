package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// parkEnv builds an isolated HOME holding a git repo, a transcript for session
// uuid "uuid-1" and a handoff root, plus a fake claude whose `agents` output
// lists that session. Returns the claude stub path, its call log, and paths.
type parkEnv struct {
	bin, log, repo, root, home string
}

func newParkEnv(t *testing.T, running bool) parkEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("RC_MGMT_HANDOFF_DIR", "")
	// Global git config in the dev environment signs commits and sets hooks;
	// a throwaway repo must not inherit that.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))

	repo := filepath.Join(home, "git", "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Transcript plus one sidecar, where claude keeps them.
	pdir := filepath.Join(home, ".claude", "projects", projectSlug(repo))
	if err := os.MkdirAll(filepath.Join(pdir, "uuid-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "uuid-1.jsonl"), []byte("{\"a\":1}\n{\"b\":2}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "uuid-1", "tool.txt"), []byte("result"), 0o644); err != nil {
		t.Fatal(err)
	}

	pid := "null"
	if running {
		pid = "42"
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, "claude")
	script := `#!/bin/sh
if [ "$1" = agents ]; then
  cat <<JSON
[{"id":"run1","name":"my work","state":"working","status":"busy","pid":` + pid + `,"cwd":"` + repo + `","sessionId":"uuid-1"}]
JSON
  exit 0
fi
echo "$*" >> ` + logPath + `
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return parkEnv{bin: bin, log: logPath, repo: repo, root: filepath.Join(home, ".claude", "handoffs"), home: home}
}

func TestParkStopsSnapshotsAndWritesHandoff(t *testing.T) {
	e := newParkEnv(t, true)

	p, err := parkSession(e.bin, "git", "run1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Slug != "my-work" || p.Repo != "proj" {
		t.Fatalf("parked = %+v", p)
	}
	if c := calls(t, e.log); len(c) != 1 || c[0] != "stop run1" {
		t.Fatalf("claude calls = %v, want one stop", c)
	}

	file := filepath.Join(e.root, "proj", "my-work.md")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"status: parked", "session: run1", "session_uuid: uuid-1", "branch: main", "dirty.txt", "worktree: " + e.repo} {
		if !strings.Contains(string(data), want) {
			t.Errorf("handoff lacks %q:\n%s", want, data)
		}
	}
	snap := filepath.Join(e.root, "proj", "my-work.session")
	for _, f := range []string{"projects/uuid-1.jsonl", "projects/uuid-1/tool.txt"} {
		if _, err := os.Stat(filepath.Join(snap, f)); err != nil {
			t.Errorf("snapshot lacks %s: %v", f, err)
		}
	}
	// Dirty files stay in the folder, and the handoff is private.
	if _, err := os.Stat(filepath.Join(e.repo, "dirty.txt")); err != nil {
		t.Errorf("dirty file touched: %v", err)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("handoff mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestParkRefusesAlreadyParkedSession(t *testing.T) {
	e := newParkEnv(t, false)
	if _, err := parkSession(e.bin, "git", "run1"); err != nil {
		t.Fatal(err)
	}
	if _, err := parkSession(e.bin, "git", "run1"); err == nil {
		t.Fatal("second park accepted")
	}
}

func TestParkOfStoppedSessionDoesNotCallStop(t *testing.T) {
	e := newParkEnv(t, false)
	if _, err := parkSession(e.bin, "git", "run1"); err != nil {
		t.Fatal(err)
	}
	if c := calls(t, e.log); len(c) != 0 {
		t.Fatalf("claude calls = %v, want none", c)
	}
}

func TestParkedSessionIsHiddenFromSessionList(t *testing.T) {
	// A stopped session, as `claude stop` leaves it: listed, but with no process.
	e := newParkEnv(t, false)
	srv := NewServer(e.bin, "git", []string{filepath.Join(e.home, "git")}, "here", nil)

	list := func() string {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
		return rec.Body.String()
	}
	if !strings.Contains(list(), `"run1"`) {
		t.Fatal("session missing before park")
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/run1/park", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("park: %d %s", rec.Code, rec.Body)
	}
	// The fake claude still lists the session (as if stopped); claudio must hide it.
	if strings.Contains(list(), `"run1"`) {
		t.Fatal("parked session still listed")
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/parked", nil))
	if !strings.Contains(rec.Body.String(), `"slug":"my-work"`) || !strings.Contains(rec.Body.String(), `"host":"here"`) {
		t.Fatalf("parked list = %s", rec.Body)
	}
}

func TestResumeRespawnsStoppedSession(t *testing.T) {
	e := newParkEnv(t, false)
	if _, err := parkSession(e.bin, "git", "run1"); err != nil {
		t.Fatal(err)
	}
	if err := resumeParked(e.bin, "git", []string{filepath.Join(e.home, "git")}, "my-work"); err != nil {
		t.Fatal(err)
	}
	if c := calls(t, e.log); len(c) != 1 || c[0] != "respawn run1" {
		t.Fatalf("claude calls = %v, want respawn run1", c)
	}
	data, _ := os.ReadFile(filepath.Join(e.root, "proj", "my-work.md"))
	if !strings.Contains(string(data), "status: resumed") || !strings.Contains(string(data), "successor_branch: main") {
		t.Fatalf("handoff not marked resumed:\n%s", data)
	}
	// Resumed work leaves the parked list and a second resume is refused.
	if got := listParked(); len(got) != 0 {
		t.Fatalf("parked list = %+v", got)
	}
	if err := resumeParked(e.bin, "git", []string{filepath.Join(e.home, "git")}, "my-work"); err == nil {
		t.Fatal("second resume accepted")
	}
}

func TestResumeForksFromSnapshotWhenSessionIsGone(t *testing.T) {
	e := newParkEnv(t, false)
	if _, err := parkSession(e.bin, "git", "run1"); err != nil {
		t.Fatal(err)
	}
	// Simulate `claude rm`: the session and its live transcript are gone, the
	// snapshot remains. Rewrite the stub so `agents` lists nothing.
	script := "#!/bin/sh\nif [ \"$1\" = agents ]; then echo '[]'; exit 0; fi\necho \"$*\" >> " + e.log + "\n"
	if err := os.WriteFile(e.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(e.home, ".claude", "projects", projectSlug(e.repo), "uuid-1.jsonl")
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}

	if err := resumeParked(e.bin, "git", []string{filepath.Join(e.home, "git")}, "my"); err != nil { // prefix
		t.Fatal(err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("transcript not restored from snapshot: %v", err)
	}
	// The fork prompt spans lines, so check the raw log, not one entry per line.
	raw, _ := os.ReadFile(e.log)
	call := string(raw)
	for _, want := range []string{"--bg", "--name my-work", "--remote-control my-work", "--resume uuid-1 --fork-session -- You are the continuation"} {
		if !strings.Contains(call, want) {
			t.Errorf("claude call lacks %q:\n%s", want, call)
		}
	}
	if strings.Count(call, "--bg") != 1 {
		t.Errorf("want exactly one launch:\n%s", call)
	}
	data, _ := os.ReadFile(filepath.Join(e.root, "proj", "my-work.md"))
	if !strings.Contains(string(data), "resumed_via: fork") {
		t.Fatalf("handoff lacks resumed_via: fork:\n%s", data)
	}
}

func TestResumeRefusesFolderOutsideRootsAndGoneFolder(t *testing.T) {
	e := newParkEnv(t, false)
	if _, err := parkSession(e.bin, "git", "run1"); err != nil {
		t.Fatal(err)
	}
	if err := resumeParked(e.bin, "git", []string{t.TempDir()}, "my-work"); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("outside roots: err = %v", err)
	}
	if err := os.RemoveAll(e.repo); err != nil {
		t.Fatal(err)
	}
	if err := resumeParked(e.bin, "git", []string{filepath.Join(e.home, "git")}, "my-work"); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("gone folder: err = %v", err)
	}
	if got := listParked(); len(got) != 1 || !got[0].FolderGone {
		t.Fatalf("parked = %+v, want one with FolderGone", got)
	}
}

func TestSlugLookupRejectsPathTricks(t *testing.T) {
	newParkEnv(t, false)
	for _, bad := range []string{"..", "../x", "a/b", "*", "", ".hidden"} {
		if _, err := findHandoff(bad); err == nil {
			t.Errorf("findHandoff(%q) accepted", bad)
		}
	}
}
