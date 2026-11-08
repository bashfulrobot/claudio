package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// peerTimeout bounds every call to another claudio so a dead host cannot
// stall the merged session list.
const peerTimeout = 3 * time.Second

// Peer is another claudio instance reachable over the tailnet. Requests carry
// no credentials: access control is the tailnet ACL, same as for this server.
type Peer struct {
	Name string
	URL  string // no trailing slash
}

// HostInfo is one entry of GET /api/hosts.
type HostInfo struct {
	Name   string `json:"name"`
	Local  bool   `json:"local,omitempty"`
	Online bool   `json:"online"`
}

// parsePeers reads "name=http://host:port,name2=http://..." from the -peers flag.
func parsePeers(s string) ([]Peer, error) {
	var peers []Peer
	seen := map[string]bool{}
	for item := range strings.SplitSeq(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, raw, ok := strings.Cut(item, "=")
		name, raw = strings.TrimSpace(name), strings.TrimSpace(raw)
		if !ok || name == "" || raw == "" {
			return nil, fmt.Errorf("peer %q: want name=http://host:port", item)
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("peer %q: url must be http(s)://host[:port]", item)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate peer name %q", name)
		}
		seen[name] = true
		peers = append(peers, Peer{Name: name, URL: strings.TrimRight(raw, "/")})
	}
	return peers, nil
}

func (s *Server) peerByName(name string) (Peer, bool) {
	for _, p := range s.peers {
		if p.Name == name {
			return p, true
		}
	}
	return Peer{}, false
}

// forwardToPeer proxies r to the peer named by ?host=. It reports whether the
// request was handled; false means the request is for this host. The target
// URL comes only from the configured peer list, never from the request, so
// the param cannot be used to reach arbitrary addresses.
func (s *Server) forwardToPeer(w http.ResponseWriter, r *http.Request) bool {
	host := r.URL.Query().Get("host")
	if host == "" || host == s.hostName {
		return false
	}
	p, ok := s.peerByName(host)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown host %q", host))
		return true
	}
	target, _ := url.Parse(p.URL)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			q := pr.In.URL.Query()
			q.Del("host")
			pr.Out.URL.RawQuery = q.Encode()
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("peer %s: %v", p.Name, err)
			writeError(w, http.StatusBadGateway, fmt.Sprintf("host %q unreachable", p.Name))
		},
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	proxy.ServeHTTP(w, r.WithContext(ctx))
	return true
}

// listAllSessions returns local sessions tagged with this host's name, plus
// every reachable peer's local sessions. An unreachable peer is skipped and
// logged, so one dead host never blanks the list; /api/hosts reports it.
func (s *Server) listAllSessions(includePeers bool) ([]Session, error) {
	local, err := listSessions(s.claudeBin)
	if err != nil {
		return nil, err
	}
	for i := range local {
		local[i].Host = s.hostName
	}
	local = hideParked(local)
	if !includePeers || len(s.peers) == 0 {
		return local, nil
	}

	results := make([][]Session, len(s.peers))
	var wg sync.WaitGroup
	for i, p := range s.peers {
		wg.Go(func() {
			var got []Session
			if err := getPeerJSON(p, "/api/sessions?local=1", &got); err != nil {
				log.Printf("peer %s: %v", p.Name, err)
				return
			}
			for j := range got {
				got[j].Host = p.Name
			}
			results[i] = got
		})
	}
	wg.Wait()
	for _, r := range results {
		local = append(local, r...)
	}
	return local, nil
}

var peerClient = &http.Client{Timeout: peerTimeout}

func getPeerJSON(p Peer, path string, v any) error {
	resp, err := peerClient.Get(p.URL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v)
}

// listAllParked returns this host's parked work plus every reachable
// peer's, each tagged with its host. A dead peer is skipped like in
// listAllSessions.
func (s *Server) listAllParked(includePeers bool) []Parked {
	out := listParked()
	for i := range out {
		out[i].Host = s.hostName
	}
	if !includePeers || len(s.peers) == 0 {
		return out
	}
	results := make([][]Parked, len(s.peers))
	var wg sync.WaitGroup
	for i, p := range s.peers {
		wg.Go(func() {
			var got []Parked
			if err := getPeerJSON(p, "/api/parked?local=1", &got); err != nil {
				log.Printf("peer %s: %v", p.Name, err)
				return
			}
			for j := range got {
				got[j].Host = p.Name
			}
			results[i] = got
		})
	}
	wg.Wait()
	for _, r := range results {
		out = append(out, r...)
	}
	return out
}

// hostsStatus pings every peer concurrently. The local host is always online.
func (s *Server) hostsStatus() []HostInfo {
	out := make([]HostInfo, 1+len(s.peers))
	out[0] = HostInfo{Name: s.hostName, Local: true, Online: true}
	var wg sync.WaitGroup
	for i, p := range s.peers {
		out[i+1].Name = p.Name
		wg.Go(func() {
			var ignored []Session
			out[i+1].Online = getPeerJSON(p, "/api/sessions?local=1", &ignored) == nil
		})
	}
	wg.Wait()
	return out
}
