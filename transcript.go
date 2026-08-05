package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// transcriptTailBytes bounds how much of a session's transcript claudio reads
// to build the "recent output" panel. Transcripts grow unboundedly over a
// long session (this file writes one line per message/tool-call/result), but
// the web UI only ever needs the last few turns.
const transcriptTailBytes = 200_000

// maxOutputEvents caps how many recent activity entries the output panel
// shows -- older entries are silently dropped, not summarized.
const maxOutputEvents = 12

// maxEventDetailLen caps how much of a tool's input or a text turn gets sent
// to the browser -- a Write with a large file body or a long thinking block
// would otherwise blow up the response for no benefit on a phone screen.
const maxEventDetailLen = 300

// TranscriptEvent is claudio's simplified view of one entry from a Claude
// Code transcript line. A transcript line is either plain assistant/user
// text, or a tool call (and, once claude has seen the result, its
// resolution).
type TranscriptEvent struct {
	Role     string `json:"role"` // "assistant" or "user"
	Text     string `json:"text,omitempty"`
	ToolName string `json:"toolName,omitempty"`
	Detail   string `json:"detail,omitempty"` // best-effort summary of tool input (command, file path, ...)
	Resolved bool   `json:"resolved"`         // false only for a tool_use with no tool_result seen yet
	IsError  bool   `json:"isError,omitempty"`

	// toolUseID correlates a tool_use event with its later tool_result line.
	// Unexported: internal bookkeeping only, never sent to the browser.
	toolUseID string
}

// SessionOutput is the response body for GET /api/sessions/{id}/output.
type SessionOutput struct {
	Events []*TranscriptEvent `json:"events"`
	// PendingToolUse is the oldest tool_use in the tail that has no matching
	// tool_result yet -- the closest claudio gets to "what is this agent
	// actually asking me right now" without speaking the daemon's private
	// per-session pty/rendezvous socket (see killSession's doc comment in
	// sessions.go for why that channel isn't something claudio should
	// touch: Anthropic's own Remote Control already covers "answer this
	// prompt remotely", since every claudio-spawned session is launched
	// with --remote-control).
	PendingToolUse *TranscriptEvent `json:"pendingToolUse,omitempty"`
}

// claudeConfigDir is where claude keeps everything under $HOME/.claude,
// unless overridden -- same variable claude itself honors.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// projectSlug mirrors Claude Code's own transcript-directory naming
// convention: cwd with every "/" and "." replaced by "-". Confirmed against
// this repo's own live transcript directory. This is an internal convention
// Anthropic controls and could change without notice -- same caveat as
// splitWorktreeCwd in sessions.go -- so a lookup miss must degrade to "no
// output available", never an error surfaced as if claudio were broken.
func projectSlug(cwd string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
}

func transcriptPath(cwd, sessionID string) (string, error) {
	dir := claudeConfigDir()
	if dir == "" {
		return "", fmt.Errorf("could not determine claude config directory")
	}
	return filepath.Join(dir, "projects", projectSlug(cwd), sessionID+".jsonl"), nil
}

// readSessionOutput tails a session's transcript and reduces it to a short,
// UI-friendly activity feed plus whatever tool call is still awaiting a
// result.
func readSessionOutput(cwd, sessionID string) (SessionOutput, error) {
	path, err := transcriptPath(cwd, sessionID)
	if err != nil {
		return SessionOutput{}, err
	}

	lines, err := tailLines(path, transcriptTailBytes)
	if err != nil {
		return SessionOutput{}, fmt.Errorf("reading transcript: %w", err)
	}

	var events []*TranscriptEvent
	pending := map[string]*TranscriptEvent{} // tool_use id -> event, removed once resolved
	var order []string                       // tool_use ids in first-seen order

	for _, line := range lines {
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue // a partial first line from the tail seek, or a non-message entry (attachment, summary, ...)
		}
		if entry.Type != "assistant" && entry.Type != "user" || len(entry.Message.Content) == 0 {
			continue
		}

		// message.content is either a bare string (plain user text) or an
		// array of typed blocks (text / tool_use / tool_result).
		var blocks []json.RawMessage
		if err := json.Unmarshal(entry.Message.Content, &blocks); err != nil {
			var text string
			if json.Unmarshal(entry.Message.Content, &text) == nil && strings.TrimSpace(text) != "" {
				events = append(events, &TranscriptEvent{Role: entry.Message.Role, Text: truncateRunes(text, maxEventDetailLen), Resolved: true})
			}
			continue
		}

		for _, b := range blocks {
			var block struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				IsError   bool            `json:"is_error"`
			}
			if json.Unmarshal(b, &block) != nil {
				continue
			}
			switch block.Type {
			case "text":
				if strings.TrimSpace(block.Text) == "" {
					continue
				}
				events = append(events, &TranscriptEvent{Role: entry.Message.Role, Text: truncateRunes(block.Text, maxEventDetailLen), Resolved: true})
			case "tool_use":
				ev := &TranscriptEvent{
					Role:      entry.Message.Role,
					ToolName:  block.Name,
					Detail:    summarizeToolInput(block.Input),
					toolUseID: block.ID,
				}
				events = append(events, ev)
				pending[block.ID] = ev
				order = append(order, block.ID)
			case "tool_result":
				if ev, ok := pending[block.ToolUseID]; ok {
					ev.Resolved = true
					ev.IsError = block.IsError
					delete(pending, block.ToolUseID)
				}
			}
		}
	}

	if len(events) > maxOutputEvents {
		events = events[len(events)-maxOutputEvents:]
	}

	out := SessionOutput{Events: events}
	// The oldest still-unresolved tool_use, in file order, is the one the
	// agent is actually stuck on -- a later tool_use in the tail can't have
	// started before an earlier one in the same turn was resolved.
	for _, id := range order {
		if ev, ok := pending[id]; ok {
			out.PendingToolUse = ev
			break
		}
	}
	return out, nil
}

// summarizeToolInput picks out whichever field of a tool call's input is
// most useful to show at a glance (the shell command, the file path, ...)
// rather than dumping the whole JSON object. Falls back to a capped raw-JSON
// preview for tool shapes claudio doesn't specifically recognize.
func summarizeToolInput(input json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(input, &m) == nil {
		for _, key := range []string{"command", "file_path", "path", "pattern", "url", "prompt", "description"} {
			raw, ok := m[key]
			if !ok {
				continue
			}
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				return truncateRunes(s, maxEventDetailLen)
			}
		}
	}
	return truncateRunes(string(input), maxEventDetailLen)
}

// truncateRunes trims s to at most n runes, appending an ellipsis -- a plain
// byte slice would risk cutting a multi-byte rune in half.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// tailLines returns path's non-empty lines, reading at most the last
// maxBytes of the file. The first line after a seek into the middle of the
// file may be a truncated fragment of a longer line, so it's dropped rather
// than fed to json.Unmarshal as if it were whole.
func tailLines(path string, maxBytes int64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	var start int64
	truncated := false
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
		truncated = true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			first = false
			if truncated {
				continue
			}
		}
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}
