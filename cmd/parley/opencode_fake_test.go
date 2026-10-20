package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ocSession is the id the fake server knows, shaped like the ones OpenCode 1.18
// makes.
const ocSession = "ses_ea8bbe5a9ffePFZ3AOhDdaNgbW"

// ocServer is a stand-in for the OpenCode 1 server a TUI serves, for the tests
// that run the real binary against it. It answers the five calls the front makes
// and models a session whose every prompt is answered at once: the user message
// lands, an assistant message answering it follows, and the status is idle, so a
// delivery completes without the pump waiting out its settle bound.
type ocServer struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	version string
	// page makes the health path answer as OpenCode 2 does, with its web page.
	page     bool
	known    map[string]bool
	user     string
	password string
	msgs     []ocMsg
	seq      int
	posts    [][]byte
	// refused counts the requests turned away for their credentials.
	refused int
}

type ocMsg struct {
	id, role, parent, text string
}

func newOCServer(t *testing.T) *ocServer {
	t.Helper()
	s := &ocServer{t: t, version: "1.18.30", known: map[string]bool{ocSession: true}, user: "opencode"}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *ocServer) port() int {
	_, p, err := net.SplitHostPort(s.srv.Listener.Addr().String())
	if err != nil {
		s.t.Fatalf("split the fake server's address: %v", err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		s.t.Fatalf("the fake server's port: %v", err)
	}
	return n
}

func (s *ocServer) set(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
}

func (s *ocServer) postBodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.posts...)
}

// waitPosts blocks until n prompts have been accepted and returns them.
func (s *ocServer) waitPosts(n int, stderr *syncBuffer) [][]byte {
	s.t.Helper()
	deadline := time.Now().Add(childDeadline)
	for {
		if p := s.postBodies(); len(p) >= n {
			return p
		}
		if !time.Now().Before(deadline) {
			s.t.Fatalf("%d prompt(s) within %s, want %d; pump stderr:\n%s", len(s.postBodies()), childDeadline, n, stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *ocServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.password != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != s.user || p != s.password {
			s.refused++
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}
	path := r.URL.Path
	switch {
	case path == "/global/health" && r.Method == http.MethodGet && s.page:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html><body><div id=root></div></body></html>"))
	case path == "/global/health" && r.Method == http.MethodGet:
		ocJSON(w, http.StatusOK, map[string]any{"healthy": true, "version": s.version})
	case path == "/session/status" && r.Method == http.MethodGet:
		ocJSON(w, http.StatusOK, map[string]any{})
	case strings.HasPrefix(path, "/session/"):
		id, tail, _ := strings.Cut(strings.TrimPrefix(path, "/session/"), "/")
		if !s.known[id] {
			ocJSON(w, http.StatusNotFound, map[string]any{"name": "NotFoundError"})
			return
		}
		switch {
		case tail == "" && r.Method == http.MethodGet:
			ocJSON(w, http.StatusOK, map[string]any{"id": id})
		case tail == "message" && r.Method == http.MethodGet:
			s.listLocked(w, r)
		case tail == "prompt_async" && r.Method == http.MethodPost:
			s.promptLocked(w, r)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *ocServer) listLocked(w http.ResponseWriter, r *http.Request) {
	msgs := s.msgs
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n < len(msgs) {
		msgs = msgs[len(msgs)-n:]
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		info := map[string]any{"id": m.id, "role": m.role}
		if m.parent != "" {
			info["parentID"] = m.parent
		}
		out = append(out, map[string]any{"info": info, "parts": []map[string]any{{"type": "text", "text": m.text}}})
	}
	ocJSON(w, http.StatusOK, out)
}

func (s *ocServer) promptLocked(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.posts = append(s.posts, body)
	var req struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Parts) != 1 {
		ocJSON(w, http.StatusBadRequest, map[string]any{"name": "BadRequest"})
		return
	}
	s.seq++
	user := ocMsg{id: fmt.Sprintf("msg_%012d", s.seq), role: "user", text: req.Parts[0].Text}
	s.seq++
	answer := ocMsg{id: fmt.Sprintf("msg_%012d", s.seq), role: "assistant", parent: user.id, text: "ok"}
	s.msgs = append(s.msgs, user, answer)
	w.WriteHeader(http.StatusNoContent)
}

func ocJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ocPostedText parses a recorded request body as the one shape the front is
// allowed to send, and returns its text.
func ocPostedText(t *testing.T, body []byte) string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || len(top) != 1 || top["parts"] == nil {
		t.Fatalf("the request body is not exactly {\"parts\":[...]}: %s", body)
	}
	var parts []map[string]string
	if err := json.Unmarshal(top["parts"], &parts); err != nil || len(parts) != 1 || len(parts[0]) != 2 || parts[0]["type"] != "text" {
		t.Fatalf("the request is not exactly one text part: %s", body)
	}
	return parts[0]["text"]
}
