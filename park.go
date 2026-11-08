package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Parking stops a session and writes its conversation and git state to disk,
// so it leaves `claude agents` and the Remote Control app until it is
// resumed. The files use the format of the rc-mgmt skill in claude-skills
// (`rc-mgmt.sh park` and `resume`), so either tool lists and resumes work the
// other parked. Only the handoff note differs: claudio cannot prompt a
// running session to summarize itself, so its note says so.

const (
	statusParked  = "parked"
	statusResumed = "resumed"
)

// validSlug is what rc-mgmt accepts as a handoff name. It also keeps a slug
// from carrying a path separator or a glob character into the lookups below.
var validSlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Parked is one handoff file in the parked state, as the UI needs it.
type Parked struct {
	Slug       string `json:"slug"`
	Repo       string `json:"repo"`
	Created    string `json:"created"`
	Branch     string `json:"branch"`
	Folder     string `json:"folder"`
	FolderGone bool   `json:"folderGone,omitempty"`
	Host       string `json:"host,omitempty"`
}

// handoff is a parsed handoff file, any status.
type handoff struct {
	file string
	fm   map[string]string
}

func (h handoff) get(k string) string { return h.fm[k] }

// handoffRoot is where handoffs live, same default and override as rc-mgmt.
func handoffRoot() string {
	if d := os.Getenv("RC_MGMT_HANDOFF_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "handoffs")
}

// readHandoff parses the frontmatter block ("key: value" lines between the
// first two "---" lines) of a handoff file.
func readHandoff(path string) (handoff, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return handoff{}, err
	}
	h := handoff{file: path, fm: map[string]string{}}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return h, fmt.Errorf("%s: no frontmatter", path)
	}
	for _, l := range lines[1:] {
		if l == "---" {
			return h, nil
		}
		if k, v, ok := strings.Cut(l, ": "); ok {
			h.fm[k] = v
		}
	}
	return h, fmt.Errorf("%s: unterminated frontmatter", path)
}

// allHandoffs reads every <root>/<repo>/<slug>.md that parses.
func allHandoffs() []handoff {
	root := handoffRoot()
	if root == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(root, "*", "*.md"))
	var out []handoff
	for _, f := range files {
		if h, err := readHandoff(f); err == nil {
			out = append(out, h)
		}
	}
	return out
}

// listParked returns the handoffs still waiting to be resumed, newest first.
func listParked() []Parked {
	out := []Parked{} // never nil, so the API answers [] and not null
	for _, h := range allHandoffs() {
		if h.get("status") != statusParked {
			continue
		}
		folder := h.get("worktree")
		_, err := os.Stat(folder)
		out = append(out, Parked{
			Slug:       h.get("slug"),
			Repo:       h.get("repo"),
			Created:    h.get("created"),
			Branch:     h.get("branch"),
			Folder:     folder,
			FolderGone: err != nil,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

// hideParked drops the sessions a handoff has taken over from the session
// list: a stopped session that was parked, and the stopped original of a
// parked conversation that resume forked into a new session. Matching needs
// both the background id and the conversation uuid, so a later session that
// reuses a short id is never hidden.
func hideParked(sessions []Session) []Session {
	hidden := map[string]bool{}
	for _, h := range allHandoffs() {
		st := h.get("status")
		if st == statusParked || (st == statusResumed && h.get("resumed_via") == "fork") {
			hidden[h.get("session")+"|"+h.get("session_uuid")] = true
		}
	}
	if len(hidden) == 0 {
		return sessions
	}
	out := sessions[:0:0]
	for _, s := range sessions {
		if s.Disconnected && hidden[s.ID+"|"+s.SessionID] {
			continue
		}
		out = append(out, s)
	}
	return out
}

// findHandoff resolves a slug, or an unambiguous slug prefix, to its file.
func findHandoff(slug string) (handoff, error) {
	if !validSlug.MatchString(slug) {
		return handoff{}, fmt.Errorf("invalid parked name %q", slug)
	}
	root := handoffRoot()
	if root == "" {
		return handoff{}, fmt.Errorf("could not determine handoff directory")
	}
	hits, _ := filepath.Glob(filepath.Join(root, "*", slug+".md"))
	if len(hits) == 0 {
		hits, _ = filepath.Glob(filepath.Join(root, "*", slug+"*.md"))
	}
	switch len(hits) {
	case 0:
		return handoff{}, fmt.Errorf("no parked work matches %q", slug)
	case 1:
		return readHandoff(hits[0])
	default:
		return handoff{}, fmt.Errorf("%q is ambiguous (%d matches)", slug, len(hits))
	}
}

// slugFor turns a session name into a handoff slug, unique across repos.
func slugFor(name, id string) string {
	s := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(name, "-")
	s = strings.TrimLeft(strings.Trim(s, "-"), "._-")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		s = id
	}
	root := handoffRoot()
	base := s
	for n := 2; ; n++ {
		if hits, _ := filepath.Glob(filepath.Join(root, "*", s+".md")); len(hits) == 0 {
			return s
		}
		s = fmt.Sprintf("%s-%d", base, n)
	}
}

// gitFacts is the repo state recorded in a handoff.
type gitFacts struct {
	isRepo                bool
	primary, branch, head string
	dirty, recent         string
	ahead                 string // "behind N, ahead M"; empty without an upstream
}

func runGit(gitBin, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitBin, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimRight(string(out), "\n"), err
}

func readGitFacts(gitBin, dir string) gitFacts {
	f := gitFacts{primary: "-", branch: "-", head: "-"}
	top, err := runGit(gitBin, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return f
	}
	f.isRepo = true
	f.primary = top
	if common, err := runGit(gitBin, dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil && filepath.Base(common) == ".git" {
		f.primary = filepath.Dir(common)
	}
	if b, err := runGit(gitBin, dir, "symbolic-ref", "--short", "-q", "HEAD"); err == nil && b != "" {
		f.branch = b
	} else {
		f.branch = "detached"
	}
	if h, err := runGit(gitBin, dir, "rev-parse", "HEAD"); err == nil {
		f.head = h
	}
	// Other sessions' worktrees live under .claude/worktrees and show as
	// untracked when the repo does not ignore them; they are not this work.
	f.dirty, _ = runGit(gitBin, dir, "status", "--porcelain", "--", ":(top)", ":(top,exclude).claude/worktrees")
	f.recent, _ = runGit(gitBin, dir, "log", "--oneline", "-8")
	if counts, err := runGit(gitBin, dir, "rev-list", "--left-right", "--count", "@{upstream}...HEAD"); err == nil {
		if parts := strings.Fields(counts); len(parts) == 2 {
			f.ahead = "behind " + parts[0] + ", ahead " + parts[1]
		}
	}
	return f
}

// findTranscript locates a conversation's transcript wherever claude put it.
func findTranscript(uuid string) string {
	if uuid == "" || strings.ContainsAny(uuid, `/*?[`) {
		return ""
	}
	hits, _ := filepath.Glob(filepath.Join(claudeConfigDir(), "projects", "*", uuid+".jsonl"))
	if len(hits) == 0 {
		return ""
	}
	return hits[0]
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			return copyFile(p, target)
		}
		return nil // symlinks and devices are not part of a conversation
	})
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

// snapshotSession copies a conversation's transcript and sidecars (tool
// results, tasks, rewind checkpoints) into dest, so the originals can go.
func snapshotSession(uuid, transcript, dest string) error {
	if err := copyFile(transcript, filepath.Join(dest, "projects", uuid+".jsonl")); err != nil {
		return err
	}
	a, _ := os.ReadFile(transcript)
	b, _ := os.ReadFile(filepath.Join(dest, "projects", uuid+".jsonl"))
	if !bytes.Equal(a, b) {
		return fmt.Errorf("snapshot of %s does not match the original", transcript)
	}
	sidecars := map[string]string{
		filepath.Join(filepath.Dir(transcript), uuid):          filepath.Join(dest, "projects", uuid),
		filepath.Join(claudeConfigDir(), "tasks", uuid):        filepath.Join(dest, "tasks", uuid),
		filepath.Join(claudeConfigDir(), "file-history", uuid): filepath.Join(dest, "file-history", uuid),
	}
	for src, dst := range sidecars {
		if fi, err := os.Stat(src); err == nil && fi.IsDir() {
			if err := copyTree(src, dst); err != nil {
				return err
			}
		}
	}
	return nil
}

// restoreSnapshot puts a parked conversation back where `--resume` looks, and
// only when no live transcript exists.
func restoreSnapshot(uuid, snap, folder string) error {
	if findTranscript(uuid) != "" {
		return nil
	}
	src := filepath.Join(snap, "projects", uuid+".jsonl")
	if fi, err := os.Stat(src); err != nil || fi.Size() == 0 {
		return fmt.Errorf("no snapshot of conversation %s", uuid)
	}
	pd := filepath.Join(claudeConfigDir(), "projects", projectSlug(folder))
	if err := copyFile(src, filepath.Join(pd, uuid+".jsonl")); err != nil {
		return err
	}
	if fi, err := os.Stat(filepath.Join(snap, "projects", uuid)); err == nil && fi.IsDir() {
		if err := copyTree(filepath.Join(snap, "projects", uuid), filepath.Join(pd, uuid)); err != nil {
			return err
		}
	}
	for _, k := range []string{"tasks", "file-history"} {
		live := filepath.Join(claudeConfigDir(), k, uuid)
		if _, err := os.Stat(live); err == nil {
			continue
		}
		if fi, err := os.Stat(filepath.Join(snap, k, uuid)); err == nil && fi.IsDir() {
			if err := copyTree(filepath.Join(snap, k, uuid), live); err != nil {
				return err
			}
		}
	}
	return nil
}

// parkSession snapshots the session identified by id, stops it, and writes
// its handoff. The stop comes first so the snapshot holds the final
// conversation; a failure after it leaves the session stopped and resumable
// from the Disconnected list, and the error says so.
func parkSession(claudeBin, gitBin, id string) (Parked, error) {
	s, err := findSession(claudeBin, id)
	if err != nil {
		return Parked{}, err
	}
	if s.SessionID == "" {
		return Parked{}, fmt.Errorf("session %q has no conversation id", id)
	}
	if fi, err := os.Stat(s.Cwd); err != nil || !fi.IsDir() {
		return Parked{}, fmt.Errorf("folder is gone: %s", s.Cwd)
	}
	for _, h := range allHandoffs() {
		if h.get("status") == statusParked && h.get("session_uuid") == s.SessionID {
			return Parked{}, fmt.Errorf("already parked as %q", h.get("slug"))
		}
	}

	if !s.Disconnected {
		if err := stopSession(claudeBin, id); err != nil {
			return Parked{}, err
		}
	}

	facts := readGitFacts(gitBin, s.Cwd)
	repo := filepath.Base(s.Cwd)
	if facts.isRepo {
		repo = filepath.Base(facts.primary)
	}
	slug := slugFor(s.Name, s.ID)
	dir := filepath.Join(handoffRoot(), repo)
	file := filepath.Join(dir, slug+".md")
	stopped := "The session is stopped but not parked: "

	transcript := findTranscript(s.SessionID)
	if transcript != "" {
		if err := snapshotSession(s.SessionID, transcript, filepath.Join(dir, slug+".session")); err != nil {
			return Parked{}, fmt.Errorf("%s%w", stopped, err)
		}
	} else {
		transcript = "-"
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Parked{}, fmt.Errorf("%s%w", stopped, err)
	}
	if err := os.WriteFile(file, []byte(renderHandoff(slug, repo, s, facts, transcript)), 0o600); err != nil {
		return Parked{}, fmt.Errorf("%s%w", stopped, err)
	}
	return Parked{Slug: slug, Repo: repo, Created: time.Now().UTC().Format(time.RFC3339), Branch: facts.branch, Folder: s.Cwd}, nil
}

func renderHandoff(slug, repo string, s Session, f gitFacts, transcript string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\nslug: %s\ncreated: %s\nstatus: %s\nrepo: %s\nprimary: %s\nworktree: %s\nbranch: %s\nhead: %s\nsession: %s\nsession_uuid: %s\ntranscript: %s\n---\n\n",
		slug, time.Now().UTC().Format("2006-01-02T15:04:05Z"), statusParked, repo, f.primary, s.Cwd, f.branch, f.head, s.ID, s.SessionID, transcript)
	fmt.Fprintf(&b, "# Handoff: %s\n\n## Git state when parked\n\n", slug)
	if !f.isRepo {
		b.WriteString("Not a git repository.\n")
	} else {
		ahead := ""
		if f.ahead != "" {
			ahead = " (" + f.ahead + " of upstream)"
		}
		fmt.Fprintf(&b, "- branch: %s at %s%s\n- uncommitted paths: %d\n", f.branch, f.head, ahead, strings.Count(f.dirty, "\n")+btoi(f.dirty != ""))
		if f.dirty != "" {
			fmt.Fprintf(&b, "\n```\n%s\n```\n", f.dirty)
		}
		if f.recent != "" {
			fmt.Fprintf(&b, "\nRecent commits:\n\n```\n%s\n```\n", f.recent)
		}
	}
	b.WriteString("\n## Notes from the session\n\nParked from claudio, which cannot ask a running session to write a note. The conversation snapshot beside this file holds the full context.\n")
	return b.String()
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// markResumed rewrites the status line, records how the work came back and
// the branch that now carries it (which rc-mgmt's cleanup matches on), and
// appends a resume log line.
func markResumed(h handoff, via, branch string) error {
	data, err := os.ReadFile(h.file)
	if err != nil {
		return err
	}
	var out []string
	inFM, done := false, false
	for i, l := range strings.Split(string(data), "\n") {
		switch {
		case i == 0 && l == "---":
			inFM = true
		case inFM && l == "---":
			inFM, done = false, true
		case inFM && (strings.HasPrefix(l, "status: ") || strings.HasPrefix(l, "successor_branch: ") || strings.HasPrefix(l, "resumed_via: ")):
			if strings.HasPrefix(l, "status: ") {
				out = append(out, "status: "+statusResumed, "resumed_via: "+via)
				if branch != "" && branch != "-" {
					out = append(out, "successor_branch: "+branch)
				}
			}
			continue
		}
		out = append(out, l)
	}
	if !done {
		return fmt.Errorf("%s: malformed frontmatter", h.file)
	}
	text := strings.TrimRight(strings.Join(out, "\n"), "\n")
	text += fmt.Sprintf("\n\n## Resume log\n\n- %s resumed by claudio (%s)\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), via)
	return os.WriteFile(h.file, []byte(text), 0o600)
}

func forkPrompt(file, dir string) string {
	// Same wording as rc-mgmt's handoff_prompt for a forked session, so work
	// resumed from either tool starts the same way.
	return fmt.Sprintf(`You are the continuation of an earlier session: its whole conversation is above, forked into this one, and the old session can be deleted. Read this handoff file first for the git state and next steps: %s

It holds the goal, what is done, what is in flight and the next steps, plus the git state when the work was parked. Then:
1. Run git status and git log -5 in %s and compare them with the handoff. Report any difference before editing.
2. Continue from the "next steps" in the handoff. Do not redo finished work.
3. Treat the handoff as the previous session's claims, not verified fact: re-read any file it relies on before changing it.
4. Append a line to the "Resume log" at the bottom of the handoff when you finish or stop.`, file, dir)
}

// resumeParked brings parked work back, best way first: respawn the stopped
// session if claude still lists it (same id, full history), else fork the
// saved conversation into a new background session in the same folder.
func resumeParked(claudeBin, gitBin string, roots []string, slug string) error {
	h, err := findHandoff(slug)
	if err != nil {
		return err
	}
	if h.get("status") != statusParked {
		return fmt.Errorf("%q is %s, not parked", h.get("slug"), h.get("status"))
	}
	dir := h.get("worktree")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("folder is gone: %s (rc-mgmt.sh resume %s --fresh-worktree rebuilds it)", dir, h.get("slug"))
	}
	// A handoff file is data on disk, so it never widens where claudio
	// trusts and launches sessions beyond the configured roots.
	if !underRoots(dir, roots) {
		return fmt.Errorf("folder is not under a configured root: %s", dir)
	}
	branch := readGitFacts(gitBin, dir).branch
	uuid, bg := h.get("session_uuid"), h.get("session")

	if bg != "" && bg != "-" && uuid != "" && uuid != "-" {
		if sessions, err := listSessions(claudeBin); err == nil {
			for _, s := range sessions {
				if s.ID == bg && s.SessionID == uuid && s.Disconnected {
					if err := resumeSession(claudeBin, bg); err != nil {
						return err
					}
					return markResumed(h, "respawn", branch)
				}
			}
		}
	}

	if uuid == "" || uuid == "-" {
		return fmt.Errorf("no saved conversation for %q; rc-mgmt.sh resume starts a clean session from the note", h.get("slug"))
	}
	if err := restoreSnapshot(uuid, strings.TrimSuffix(h.file, ".md")+".session", dir); err != nil {
		return fmt.Errorf("no saved conversation for %q: %w", h.get("slug"), err)
	}
	if err := ensureTrusted(gitBin, dir); err != nil {
		return fmt.Errorf("could not trust folder: %w", err)
	}
	name := h.get("slug")
	args := []string{"--bg", "--name", name, "--remote-control", name, "--resume", uuid, "--fork-session", "--", forkPrompt(h.file, dir)}
	if err := launchBackground(claudeBin, dir, name, args); err != nil {
		return err
	}
	return markResumed(h, "fork", branch)
}

// deleteParked removes a handoff file and its snapshot directory. It takes an
// exact slug only: a prefix match could delete work the user did not name.
// Files in the project folder are never touched.
func deleteParked(slug string) error {
	h, err := findHandoff(slug)
	if err != nil {
		return err
	}
	if filepath.Base(h.file) != slug+".md" {
		return fmt.Errorf("no parked work named %q (closest: %s)", slug, h.get("slug"))
	}
	if err := os.RemoveAll(strings.TrimSuffix(h.file, ".md") + ".session"); err != nil {
		return err
	}
	return os.Remove(h.file)
}
