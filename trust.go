package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// trustMu serialises claudio's own read-modify-write of the claude config
// file. Running claude processes rewrite the same file without taking it, so
// the rename in setTrusted narrows that race but cannot close it.
var trustMu sync.Mutex

// claudeConfigPath is where claude keeps per-project state, including
// projects["<path>"].hasTrustDialogAccepted. CLAUDE_CONFIG_DIR moves it.
func claudeConfigPath() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate home directory: %w", err)
	}
	return filepath.Join(home, ".claude.json"), nil
}

// trustKey returns the path claude keys workspace trust on: the main
// checkout's root for any git repo or linked worktree, otherwise the folder
// itself. Trust never persists for the home directory, so that is an error
// instead of a silent no-op.
func trustKey(gitBin, cwd string) (string, error) {
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s: %w", cwd, err)
	}
	if home, herr := os.UserHomeDir(); herr == nil {
		if h, e := filepath.EvalSymlinks(home); e == nil && h == resolved {
			return "", errors.New("claude never saves trust for the home directory; pick a project folder")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitBin, "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Dir = resolved
	out, err := cmd.Output()
	if err != nil {
		return resolved, nil // not a repo: trust keys on the folder itself
	}
	common := strings.TrimSpace(string(out))
	if filepath.Base(common) != ".git" {
		return resolved, nil // bare or unusual layout: fall back to the folder
	}
	return filepath.Dir(common), nil
}

// ensureTrusted marks the trust key for cwd as accepted in the claude config
// so `claude --bg` does not fail with "Workspace not trusted" when no dialog
// can appear. It leaves every other field in the file untouched and skips the
// write when trust is already recorded.
func ensureTrusted(gitBin, cwd string) error {
	key, err := trustKey(gitBin, cwd)
	if err != nil {
		return err
	}
	path, err := claudeConfigPath()
	if err != nil {
		return err
	}
	trustMu.Lock()
	defer trustMu.Unlock()
	return setTrusted(path, key)
}

func setTrusted(path, key string) error {
	top := map[string]json.RawMessage{}
	mode := os.FileMode(0o600)
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &top); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	projects := map[string]json.RawMessage{}
	if raw, ok := top["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return fmt.Errorf("parsing projects in %s: %w", path, err)
		}
	}
	entry := map[string]json.RawMessage{}
	if raw, ok := projects[key]; ok {
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("parsing project entry %s: %w", key, err)
		}
	}
	if string(entry["hasTrustDialogAccepted"]) == "true" {
		return nil
	}
	entry["hasTrustDialogAccepted"] = json.RawMessage("true")

	var err error
	if projects[key], err = json.Marshal(entry); err != nil {
		return err
	}
	if top["projects"], err = json.Marshal(projects); err != nil {
		return err
	}
	data, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.claudio-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// underRoots reports whether dir resolves to a path inside one of roots, so a
// request cannot make claudio trust an arbitrary folder.
func underRoots(dir string, roots []string) bool {
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	for _, r := range roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		if d == rr || strings.HasPrefix(d, rr+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
