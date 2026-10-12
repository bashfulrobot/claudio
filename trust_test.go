package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func readProjects(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	return top.Projects
}

func TestSetTrustedPreservesOtherFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	orig := `{"theme":"dark","numStartups":7,"projects":{"/a":{"allowedTools":["x"]},"/b":{"hasTrustDialogAccepted":false}}}`
	if err := os.WriteFile(path, []byte(orig), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := setTrusted(path, "/b"); err != nil {
		t.Fatal(err)
	}
	if err := setTrusted(path, "/c"); err != nil {
		t.Fatal(err)
	}

	p := readProjects(t, path)
	if p["/b"]["hasTrustDialogAccepted"] != true || p["/c"]["hasTrustDialogAccepted"] != true {
		t.Fatalf("trust not set: %v", p)
	}
	if _, ok := p["/a"]["allowedTools"]; !ok {
		t.Fatalf("/a lost fields: %v", p["/a"])
	}
	data, _ := os.ReadFile(path)
	var top map[string]any
	_ = json.Unmarshal(data, &top)
	if top["theme"] != "dark" || top["numStartups"] != float64(7) {
		t.Fatalf("top-level fields lost: %v", top)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed: %v", fi.Mode().Perm())
	}
}

func TestSetTrustedCreatesMissingFileAndSkipsRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	if err := setTrusted(path, "/a"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if err := setTrusted(path, "/a"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("already-trusted path was rewritten")
	}
}

func TestSetTrustedRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setTrusted(path, "/a"); err == nil {
		t.Fatal("expected error on corrupt file")
	}
	if data, _ := os.ReadFile(path); string(data) != "{not json" {
		t.Fatal("corrupt file was modified")
	}
}

func TestUnderRoots(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "proj")
	other := t.TempDir()
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{
		root:                        true,
		sub:                         true,
		other:                       false,
		link:                        false,
		filepath.Join(root, "nope"): false,
	} {
		if got := underRoots(dir, []string{root}); got != want {
			t.Errorf("underRoots(%s) = %v, want %v", dir, got, want)
		}
	}
}

func TestTrustKeyUsesMainCheckoutForWorktree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(repo, "init", "-q")
	run(repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(t.TempDir(), "wt")
	run(repo, "worktree", "add", "-q", "-b", "b", wt)

	want, _ := filepath.EvalSymlinks(repo)
	for _, dir := range []string{repo, wt} {
		got, err := trustKey("git", dir)
		if err != nil || got != want {
			t.Errorf("trustKey(%s) = %q, %v; want %q", dir, got, err, want)
		}
	}

	plain := t.TempDir()
	wantPlain, _ := filepath.EvalSymlinks(plain)
	if got, err := trustKey("git", plain); err != nil || got != wantPlain {
		t.Errorf("non-repo trustKey = %q, %v; want %q", got, err, wantPlain)
	}
}

func TestTrustKeyRejectsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := trustKey("git", home); err == nil {
		t.Fatal("expected error for home directory")
	}
}
