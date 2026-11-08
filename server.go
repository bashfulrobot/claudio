package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
)

//go:embed static
var staticFS embed.FS

// maxBodyBytes caps every JSON request body. The largest legitimate one is a
// spawn with a 4000-byte prompt or a bulk call with 50 short ids.
const maxBodyBytes = 64 << 10

type Server struct {
	claudeBin string
	gitBin    string
	roots     []string
	hostName  string // label for this instance in the merged session list
	peers     []Peer // other claudio instances, see peers.go
	mux       *http.ServeMux
}

func NewServer(claudeBin, gitBin string, roots []string, hostName string, peers []Peer) *Server {
	s := &Server{claudeBin: claudeBin, gitBin: gitBin, roots: roots, hostName: hostName, peers: peers, mux: http.NewServeMux()}
	s.routes()
	return s
}

// ServeHTTP sends any /api request carrying ?host=<peer> to that peer before
// the mux sees it, so every per-session route works remotely unchanged.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if !originAllowed(r) {
			writeError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		if s.forwardToPeer(w, r) {
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// originAllowed refuses a state-changing request that a browser tagged with
// another site's Origin or a cross-site Sec-Fetch-Site. Without it any web page the phone visits could POST
// to claudio (the JSON decoder ignores Content-Type, so a "simple" cross-site
// request is enough) and spawn or delete sessions. Requests with no Origin,
// such as curl or a peer claudio, pass: only a browser sets one. The peer hop
// drops Origin after this check ran here, see forwardToPeer.
func originAllowed(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	// Browsers set Sec-Fetch-Site on every request and page script cannot
	// change it, so it also covers requests that carry no Origin (a ping
	// link, for one). It does not stop DNS rebinding: a rebound page is
	// same-origin to itself. See the README limits.
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
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
	s.mux.HandleFunc("/api/hosts", s.handleHosts)
	s.mux.HandleFunc("/api/parked", s.handleParked)
	s.mux.HandleFunc("/api/parked/", s.handleParkedBySlug)
}

// GET /api/parked -> parked work on this host and every reachable peer
func (s *Server) handleParked(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.listAllParked(r.URL.Query().Get("local") != "1"))
}

// POST /api/parked/{slug}/resume -> bring parked work back (see resumeParked)
func (s *Server) handleParkedBySlug(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/parked/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "resume" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := resumeParked(s.claudeBin, s.gitBin, s.roots, parts[0]); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

// GET /api/hosts -> this host plus each configured peer with an online flag
func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.hostsStatus())
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
		// local=1 is what a peer sends us: it wants only our own sessions, so
		// two hosts configured as each other's peers never recurse.
		sessions, err := s.listAllSessions(r.URL.Query().Get("local") != "1")
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
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
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
//	POST   /api/sessions/{id}/resume restart a disconnected session
//	POST   /api/sessions/bulk        remove or resume many (see handleBulk)
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
	case len(parts) == 1 && parts[0] == "bulk":
		s.handleBulk(w, r)

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

	case len(parts) == 2 && parts[1] == "park":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		p, err := parkSession(s.claudeBin, s.gitBin, parts[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p)

	case len(parts) == 2 && parts[1] == "resume":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if err := resumeSession(s.claudeBin, parts[0]); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})

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

// POST /api/sessions/bulk  {"action": "remove"|"resume", "ids": ["5aad2c1c", ...]}
// -> {"results": [{"id", "ok", "error"?}, ...]}, one entry per distinct id.
func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Action string   `json:"action"`
		IDs    []string `json:"ids"`
	}
	// Bound the body before decoding: the maxBulkIDs cap in bulkAction only
	// applies after the whole ids array is already in memory.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	results, err := bulkAction(s.claudeBin, body.Action, body.IDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// GET /api/folders
func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, listFolders(s.roots))
}
