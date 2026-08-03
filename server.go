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
	roots     []string
	mux       *http.ServeMux
}

func NewServer(claudeBin string, roots []string) *Server {
	s := &Server{claudeBin: claudeBin, roots: roots, mux: http.NewServeMux()}
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
// POST /api/sessions  -> spawn {cwd, name}
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
		var body struct {
			Cwd  string `json:"cwd"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(body.Cwd) == "" {
			writeError(w, http.StatusBadRequest, "cwd is required")
			return
		}
		if err := spawnSession(s.claudeBin, body.Cwd, body.Name); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "launching"})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// DELETE /api/sessions/{id} — id is the short `id` field from
// `claude agents --json`, e.g. "5aad2c1c", not a pid.
func (s *Server) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id is required")
		return
	}
	if err := killSession(s.claudeBin, id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// GET /api/folders
func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, listFolders(s.roots))
}
