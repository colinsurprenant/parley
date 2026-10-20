package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// oc2PID is the process id the fake OpenCode 2 server reports for itself, and
// the service file it is described by holds.
const oc2PID = 31337

// oc2Password is the password of the fake's service. Tests look for it in what
// the pump says, and it must not be there.
const oc2Password = "oc2-service-password-do-not-print"

// oc2Server is a stand-in for the OpenCode 2 server, for the tests that run the
// real binary against it. It answers the calls the front makes, takes a prompt
// into the history at once, and answers the health path of OpenCode 1 as OpenCode
// 2 does, with its web page.
type oc2Server struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	version  string
	pid      int
	password string
	known    map[string]bool
	msgs     []oc2Msg
	posts    [][]byte
	refused  int
	seq      int
	// busy is a turn in progress on every session.
	busy bool
	// down makes every request be answered with a 500, a service that is up and
	// broken.
	down bool
}

type oc2Msg struct{ id, text string }

func newOC2Server(t *testing.T) *oc2Server {
	t.Helper()
	s := &oc2Server{t: t, version: "2.0.32", pid: oc2PID, password: oc2Password, known: map[string]bool{ocSession: true}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *oc2Server) url() string { return s.srv.URL }

func (s *oc2Server) port() int {
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

func (s *oc2Server) set(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
}

func (s *oc2Server) postBodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.posts...)
}

func (s *oc2Server) refusedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

func (s *oc2Server) waitPosts(n int, stderr *syncBuffer) [][]byte {
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

// writeService writes the service file the fake is described by, as OpenCode 2
// writes it: a file only its owner can read, in <stateHome>/opencode/service.json.
// stateHome is what the pump gets as XDG_STATE_HOME. url and pid are the file's
// to name, so a test can make them wrong.
func writeService(t *testing.T, stateHome, url string, pid int, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(stateHome, "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"id": "1b0e1e96-26b8-470c-9aee-f6f58e02b495", "version": "2.0.32", "url": url, "pid": pid, "password": oc2Password})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "service.json")
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func (s *oc2Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		http.Error(w, "broken", http.StatusInternalServerError)
		return
	}
	if s.password != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != "opencode" || p != s.password {
			s.refused++
			ocJSON(w, http.StatusUnauthorized, map[string]any{"_tag": "UnauthorizedError", "message": "Authentication required"})
			return
		}
	}
	path := r.URL.Path
	switch {
	case path == "/global/health":
		// OpenCode 2 serves its web page where OpenCode 1 serves JSON.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html><body><div id=root></div></body></html>"))
	case path == "/api/info" && r.Method == http.MethodGet:
		ocJSON(w, http.StatusOK, map[string]any{"version": s.version, "pid": s.pid})
	case path == "/api/session/active" && r.Method == http.MethodGet:
		data := map[string]any{}
		if s.busy {
			data[ocSession] = map[string]any{"type": "running"}
		}
		ocJSON(w, http.StatusOK, map[string]any{"data": data})
	case strings.HasPrefix(path, "/api/session/"):
		id, tail, _ := strings.Cut(strings.TrimPrefix(path, "/api/session/"), "/")
		if !s.known[id] {
			ocJSON(w, http.StatusNotFound, map[string]any{"_tag": "SessionNotFoundError", "sessionID": id, "message": "Session not found: " + id})
			return
		}
		switch {
		case tail == "" && r.Method == http.MethodGet:
			ocJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"id": id, "location": map[string]any{"directory": ocDir}}})
		case tail == "prompt" && r.Method == http.MethodPost:
			s.promptLocked(w, r, id)
		case strings.HasPrefix(tail, "message/") && r.Method == http.MethodGet:
			mid := strings.TrimPrefix(tail, "message/")
			for _, m := range s.msgs {
				if m.id == mid {
					ocJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"id": m.id, "time": map[string]any{"created": 1}, "text": m.text, "type": "user"}})
					return
				}
			}
			ocJSON(w, http.StatusNotFound, map[string]any{"_tag": "MessageNotFoundError", "sessionID": id, "messageID": mid, "message": "not found"})
		case tail == "inbox" && r.Method == http.MethodGet:
			ocJSON(w, http.StatusOK, map[string]any{"data": []any{}})
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *oc2Server) promptLocked(w http.ResponseWriter, r *http.Request, session string) {
	body, _ := io.ReadAll(r.Body)
	s.posts = append(s.posts, body)
	var req struct {
		ID       string `json:"id"`
		Text     string `json:"text"`
		Delivery string `json:"delivery"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || !strings.HasPrefix(req.ID, "msg_") || req.Delivery != "queue" {
		ocJSON(w, http.StatusBadRequest, map[string]any{"_tag": "InvalidRequestError", "message": "bad prompt"})
		return
	}
	s.seq++
	s.msgs = append(s.msgs, oc2Msg{id: req.ID, text: req.Text})
	ocJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"id": req.ID, "sessionID": session, "time": map[string]any{"created": s.seq}, "type": "user",
		"payload": map[string]any{"text": req.Text}, "delivery": req.Delivery,
	}})
}

// oc2PostedText parses a recorded request body as the one shape the front is
// allowed to send to OpenCode 2, and returns its text.
func oc2PostedText(t *testing.T, body []byte) (id, text string) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || len(top) != 3 || top["id"] == nil || top["text"] == nil || top["delivery"] == nil {
		t.Fatalf("the request body is not exactly {id, text, delivery}: %s", body)
	}
	var delivery string
	if err := json.Unmarshal(top["id"], &id); err != nil || json.Unmarshal(top["text"], &text) != nil || json.Unmarshal(top["delivery"], &delivery) != nil || delivery != "queue" {
		t.Fatalf("the request is not a queued text: %s", body)
	}
	return id, text
}

// oc2Env is ocEnv with the places the pump looks for OpenCode 2 pointed at the
// test's own: XDG_STATE_HOME at stateHome, HOME at a directory of its own, and no
// OpenCode credentials of the developer's shell, whichever version they are for.
func oc2Env(t *testing.T, stateHome string, extra ...string) []string {
	t.Helper()
	var env []string
	for _, kv := range ocEnv() {
		if strings.HasPrefix(kv, "XDG_STATE_HOME=") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, opencodePassword2Env+"=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "XDG_STATE_HOME="+stateHome, "HOME="+t.TempDir())
	return append(env, extra...)
}

// oc2Args is ocArgs without a port: the pump finds the service.
func oc2Args(spec, nick, session, stateDir string, extra ...string) []string {
	return append([]string{"--stream", spec, "--nick", nick, "--session", session,
		"--state-dir", stateDir, "--debounce", "100ms"}, extra...)
}
