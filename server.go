package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"strings"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	claudeBin string
	gitBin    string
	roots     []string
	mux       *http.ServeMux
}

func NewServer(claudeBin, gitBin string, roots []string) *Server {
	s := &Server{claudeBin: claudeBin, gitBin: gitBin, roots: roots, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("embedding static assets: %v", err)
	}
	s.mux.Handle("/", http.FileServer(http.FS(sub)))

	s.mux.HandleFunc("/api/sessions", s.handleSessions)
	s.mux.HandleFunc("/api/sessions/", s.handleSessionByID)
	s.mux.HandleFunc("/api/folders", s.handleFolders)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// GET  /api/sessions  -> list
// POST /api/sessions  -> spawn (see SpawnOptions in sessions.go for the full body)
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sessions, err := listSessions(s.claudeBin)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, sessions)

	case http.MethodPost:
		// Model/Effort/PermissionMode/Prompt/Worktree are all optional --
		// see SpawnOptions in sessions.go for why a blank value is omitted
		// from the claude invocation entirely rather than sent as some
		// "default", and why Worktree is a pointer.
		var body struct {
			Cwd            string `json:"cwd"`
			Name           string `json:"name"`
			Model          string `json:"model"`
			Effort         string `json:"effort"`
			PermissionMode string `json:"permissionMode"`
			Prompt         string `json:"prompt"`
			Worktree       *bool  `json:"worktree"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(body.Cwd) == "" {
			writeError(w, http.StatusBadRequest, "cwd is required")
			return
		}
		// Spawning trusts the folder, so only folders under a configured root
		// are allowed, never an arbitrary path from the request.
		if !underRoots(body.Cwd, s.roots) {
			writeError(w, http.StatusBadRequest, "folder is not under a configured root")
			return
		}
		opts := SpawnOptions{
			Model:          body.Model,
			Effort:         body.Effort,
			PermissionMode: body.PermissionMode,
			Prompt:         body.Prompt,
			Worktree:       body.Worktree,
		}
		result, err := spawnSession(s.claudeBin, s.gitBin, body.Cwd, body.Name, opts)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, result)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleSessionByID serves two routes under one prefix registration:
//
//	DELETE /api/sessions/{id}        permanently remove (id is the short "id"
//	                                 field from `claude agents --json`, e.g.
//	                                 "5aad2c1c" -- not a pid)
//	POST   /api/sessions/{id}/stop   stop but keep the conversation, resumable
//	                                 later via `claude attach <id>`
//	GET    /api/sessions/{id}/output recent transcript activity + any tool
//	                                 call still awaiting a result (see
//	                                 SessionOutput in transcript.go)
//
// Both share one "/api/sessions/" prefix handler (see routes()) rather than
// Go 1.22 method-scoped patterns: the catch-all file server mounted at "/"
// means a method-scoped "GET /api/sessions/{id}" would let an unmatched GET
// fall through to it and return an HTML 200 instead of a JSON 405/404, which
// is worse than the manual path-splitting below.
func (s *Server) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/")
	if rest == "" {
		writeError(w, http.StatusBadRequest, "session id is required")
		return
	}
	parts := strings.Split(rest, "/")

	switch {
	case len(parts) == 1:
		if r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if err := killSession(s.claudeBin, parts[0]); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})

	case len(parts) == 2 && parts[1] == "stop":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if err := stopSession(s.claudeBin, parts[0]); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})

	case len(parts) == 2 && parts[1] == "output":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		sessions, err := listSessions(s.claudeBin)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		var target *Session
		for i := range sessions {
			if sessions[i].ID == parts[0] {
				target = &sessions[i]
				break
			}
		}
		if target == nil {
			writeError(w, http.StatusNotFound, "no known claude session with that id")
			return
		}
		out, err := readSessionOutput(target.Cwd, target.SessionID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)

	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// GET /api/folders
func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, listFolders(s.roots))
}
