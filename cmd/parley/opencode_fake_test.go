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

// ocPID is the process id the fake OpenCode server reports for itself, and
// the service file it is described by holds.
const ocPID = 31337

// ocPassword is the password of the fake's service. Tests look for it in what
// the pump says, and it must not be there.
const ocPassword = "oc2-service-password-do-not-print"

// ocSession is the id the fake server knows, shaped like the ones OpenCode makes.
const ocSession = "ses_ea8bbe5a9ffePFZ3AOhDdaNgbW"

// ocDir is the directory the fake's session belongs to.
const ocDir = "/work/checkout"

// ocServer is a stand-in for the OpenCode server, for the tests that run the real
// binary against it. It answers the calls the front makes and takes a prompt into
// the history at once.
type ocServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	version  string
	pid      int
	password string
	known    map[string]bool
	msgs     []ocMsg
	posts    [][]byte
	refused  int
	seq      int
	// activeReads is how many times the pump asked which sessions are in a turn.
	activeReads int
	// busy is a turn in progress on every session.
	busy bool
	// down makes every request be answered with a 500, a service that is up and
	// broken.
	down bool
	// anySession makes every session id known, for the tests of `line add`, whose
	// session id is parley's own and is not known to the test ahead of the run.
	anySession bool
	// history is the session's history the message list answers from, oldest first;
	// historyReads is how many times it was asked for.
	history      []ocHist
	historyReads int
}

type ocMsg struct{ id, text string }

// ocHist is one message of the history the fake lists: a user message, an assistant
// message that finished as finish, or an idle marker with an outcome.
type ocHist struct{ kind, finish, outcome string }

// ocLiveTurn is the history of a real primer turn on OpenCode 2.1.0: the primer, two
// steps of the assistant (the first went on to a tool, the last stopped), and the
// idle marker with the outcome succeeded.
var ocLiveTurn = []ocHist{{kind: "user"}, {kind: "assistant", finish: "tool-calls"}, {kind: "assistant", finish: "stop"}, {kind: "idle", outcome: "succeeded"}}

// newOCServer is a server that wants no password, as a test of --port, or of a
// recorded session, has no use for one.
func newOCServer(t *testing.T) *ocServer {
	t.Helper()
	s := newOCServiceServer(t)
	s.set(func() { s.password = "" })
	return s
}

// newOCServiceServer is a server with the password of a service, which is what the
// service file the tests write holds.
func newOCServiceServer(t *testing.T) *ocServer {
	t.Helper()
	s := &ocServer{t: t, version: "2.0.32", pid: ocPID, password: ocPassword, known: map[string]bool{ocSession: true}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *ocServer) url() string { return s.srv.URL }

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

func (s *ocServer) refusedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

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
	b, err := json.Marshal(map[string]any{"id": "1b0e1e96-26b8-470c-9aee-f6f58e02b495", "version": "2.0.32", "url": url, "pid": pid, "password": ocPassword})
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

func (s *ocServer) serve(w http.ResponseWriter, r *http.Request) {
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
	case path == "/api/info" && r.Method == http.MethodGet:
		ocJSON(w, http.StatusOK, map[string]any{"version": s.version, "pid": s.pid})
	case path == "/api/session/active" && r.Method == http.MethodGet:
		s.activeReads++
		data := map[string]any{}
		if s.busy {
			data[ocSession] = map[string]any{"type": "running"}
		}
		ocJSON(w, http.StatusOK, map[string]any{"data": data})
	case strings.HasPrefix(path, "/api/session/"):
		id, tail, _ := strings.Cut(strings.TrimPrefix(path, "/api/session/"), "/")
		if !s.known[id] && !s.anySession {
			ocJSON(w, http.StatusNotFound, map[string]any{"_tag": "SessionNotFoundError", "sessionID": id, "message": "Session not found: " + id})
			return
		}
		switch {
		case tail == "" && r.Method == http.MethodGet:
			ocJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"id": id, "location": map[string]any{"directory": ocDir}}})
		case tail == "prompt" && r.Method == http.MethodPost:
			s.promptLocked(w, r, id)
		case tail == "message" && r.Method == http.MethodGet:
			s.historyLocked(w, r)
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

// historyLocked answers a page of the history, newest first, for the two queries the
// front is allowed to make: order=desc and a limit for the newest, and a cursor and a
// limit (no order) for the page after one. The cursor is the index of the next older
// message, and null when there is none.
func (s *ocServer) historyLocked(w http.ResponseWriter, r *http.Request) {
	s.historyReads++
	q := r.URL.Query()
	limit, err := strconv.Atoi(q.Get("limit"))
	start := len(s.history) - 1
	switch {
	case err != nil || limit < 1 || len(q) != 2:
		ocJSON(w, http.StatusBadRequest, map[string]any{"_tag": "InvalidRequestError", "message": "bad query"})
		return
	case q.Has("cursor"):
		n, err := strconv.Atoi(strings.TrimPrefix(q.Get("cursor"), "c"))
		if err != nil || n >= len(s.history) {
			ocJSON(w, http.StatusBadRequest, map[string]any{"_tag": "InvalidRequestError", "message": "bad cursor"})
			return
		}
		start = n
	case q.Get("order") != "desc":
		ocJSON(w, http.StatusBadRequest, map[string]any{"_tag": "InvalidRequestError", "message": "bad query"})
		return
	}
	out := []map[string]any{}
	i := start
	for ; i >= 0 && len(out) < limit; i-- {
		h := s.history[i]
		e := map[string]any{"id": "msg_h" + strconv.Itoa(i), "time": map[string]any{"created": i + 1}}
		switch h.kind {
		case "user":
			e["type"], e["text"], e["files"] = "user", "the primer", []any{}
		case "assistant":
			e["type"], e["agent"], e["content"], e["finish"] = "assistant", "build", []any{}, h.finish
		case "idle":
			e["type"], e["outcome"] = "idle", h.outcome
		default:
			e["type"] = h.kind
		}
		out = append(out, e)
	}
	var next any
	if i >= 0 {
		next = "c" + strconv.Itoa(i)
	}
	ocJSON(w, http.StatusOK, map[string]any{"data": out, "cursor": map[string]any{"previous": nil, "next": next}})
}

func (s *ocServer) promptLocked(w http.ResponseWriter, r *http.Request, session string) {
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
	s.msgs = append(s.msgs, ocMsg{id: req.ID, text: req.Text})
	ocJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"id": req.ID, "sessionID": session, "time": map[string]any{"created": s.seq}, "type": "user",
		"payload": map[string]any{"text": req.Text}, "delivery": req.Delivery,
	}})
}

// ocPosted parses a recorded request body as the one shape the front is allowed
// to send, and returns its id and its text.
func ocPosted(t *testing.T, body []byte) (id, text string) {
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

// ocStateEnv is ocEnv with the places the pump looks for OpenCode 2 pointed at the
// test's own: XDG_STATE_HOME at stateHome, HOME at a directory of its own, and no
// OpenCode credentials of the developer's shell, whichever version they are for.
func ocStateEnv(t *testing.T, stateHome string, extra ...string) []string {
	t.Helper()
	var env []string
	for _, kv := range ocEnv() {
		if strings.HasPrefix(kv, "XDG_STATE_HOME=") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, opencodePasswordEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "XDG_STATE_HOME="+stateHome, "HOME="+t.TempDir())
	return append(env, extra...)
}

// ocServiceArgs is ocArgs without a port: the pump finds the service.
func ocServiceArgs(spec, nick, session, stateDir string, extra ...string) []string {
	return append([]string{"--stream", spec, "--nick", nick, "--session", session,
		"--state-dir", stateDir, "--debounce", "100ms"}, extra...)
}

// ocPostedText is the text of a recorded request body, parsed as ocPosted does.
func ocPostedText(t *testing.T, body []byte) string {
	t.Helper()
	_, text := ocPosted(t, body)
	return text
}

func ocJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
