package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakePeer is a stand-in remote claudio. It records the last request it saw.
func fakePeer(t *testing.T, sessionsJSON string) (*httptest.Server, *http.Request, *string) {
	t.Helper()
	var last http.Request
	var body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r.Clone(r.Context())
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/sessions" {
			_, _ = w.Write([]byte(sessionsJSON))
			return
		}
		_, _ = w.Write([]byte(`{"status":"peer-ok"}`))
	}))
	t.Cleanup(ts.Close)
	return ts, &last, &body
}

func newMultiServer(t *testing.T, peers []Peer) *Server {
	t.Helper()
	bin, _ := stubClaude(t)
	return NewServer(bin, "git", nil, "here", peers)
}

func TestListMergesPeerSessionsAndTagsHost(t *testing.T) {
	peer, last, _ := fakePeer(t, `[{"id":"rem1","name":"remote","pid":7,"cwd":"/m/x"}]`)
	srv := newMultiServer(t, []Peer{{Name: "mac", URL: peer.URL}})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got []Session
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	hosts := map[string]string{}
	for _, s := range got {
		hosts[s.ID] = s.Host
	}
	if hosts["run1"] != "here" {
		t.Errorf("local session host = %q, want here", hosts["run1"])
	}
	if hosts["rem1"] != "mac" {
		t.Errorf("peer session host = %q, want mac", hosts["rem1"])
	}
	// A peer must be asked for its own sessions only, or two hosts that list
	// each other would recurse.
	if last.URL.Query().Get("local") != "1" {
		t.Errorf("peer list request %q lacks local=1", last.URL.String())
	}
}

func TestListSurvivesDeadPeer(t *testing.T) {
	peer, _, _ := fakePeer(t, `[]`)
	url := peer.URL
	peer.Close()
	srv := newMultiServer(t, []Peer{{Name: "mac", URL: url}})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"run1"`) {
		t.Fatalf("local sessions missing: %s", rec.Body)
	}
}

func TestLocalQueryIgnoresPeers(t *testing.T) {
	hit := false
	peer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	t.Cleanup(peer.Close)
	srv := newMultiServer(t, []Peer{{Name: "mac", URL: peer.URL}})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions?local=1", nil))
	if rec.Code != http.StatusOK || hit {
		t.Fatalf("status = %d, peer contacted = %v", rec.Code, hit)
	}
}

func TestHostParamProxiesToPeer(t *testing.T) {
	peer, last, body := fakePeer(t, `[]`)
	srv := newMultiServer(t, []Peer{{Name: "mac", URL: peer.URL}})

	req := httptest.NewRequest(http.MethodPost, "/api/sessions?host=mac", strings.NewReader(`{"cwd":"/m/x"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "peer-ok") {
		t.Fatalf("not proxied: %d %s", rec.Code, rec.Body)
	}
	if last.Method != http.MethodPost || last.URL.Path != "/api/sessions" {
		t.Errorf("peer saw %s %s", last.Method, last.URL)
	}
	if last.URL.Query().Has("host") {
		t.Errorf("host param leaked to peer: %s", last.URL)
	}
	if *body != `{"cwd":"/m/x"}` {
		t.Errorf("body = %q", *body)
	}
}

func TestHostParamUnknownIsRejected(t *testing.T) {
	srv := newMultiServer(t, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/run1?host=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHostParamLocalNameRunsLocally(t *testing.T) {
	bin, logPath := stubClaude(t)
	srv := NewServer(bin, "git", nil, "here", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/run1/stop?host=here", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if c := calls(t, logPath); len(c) != 1 || c[0] != "stop run1" {
		t.Fatalf("calls = %v", c)
	}
}

func TestHostsEndpoint(t *testing.T) {
	up, _, _ := fakePeer(t, `[]`)
	down, _, _ := fakePeer(t, `[]`)
	downURL := down.URL
	down.Close()
	srv := newMultiServer(t, []Peer{{Name: "mac", URL: up.URL}, {Name: "gone", URL: downURL}})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/hosts", nil))
	var got []HostInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []HostInfo{{Name: "here", Local: true, Online: true}, {Name: "mac", Online: true}, {Name: "gone", Online: false}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("host %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParsePeers(t *testing.T) {
	got, err := parsePeers("mac=http://100.1.2.3:8787, nix=http://nix:8787/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "mac" || got[1].URL != "http://nix:8787" {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []string{"mac", "=http://x", "mac=ftp://x", "a=http://x,a=http://y"} {
		if _, err := parsePeers(bad); err == nil {
			t.Errorf("parsePeers(%q) accepted", bad)
		}
	}
}
