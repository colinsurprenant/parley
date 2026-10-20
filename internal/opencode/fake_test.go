package opencode

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testSession = "ses_ea8bbe5a9ffePFZ3AOhDdaNgbW"
	// testDir is the directory the fake's session belongs to.
	testDir = "/work/checkout"
)

// fakeMsg is one message in the fake's history, in the shape OpenCode 1.18.30
// lists it: info with the id, role and, for an assistant message, the parent,
// and the parts.
type fakeMsg struct {
	ID       string
	Role     string
	ParentID string
	Parts    []map[string]any
}

// plantedMsg is a message the fake adds when a prompt is accepted. partType, if
// set, replaces the type of its text part.
type plantedMsg struct {
	role, partType, text string
}

type fakePost struct {
	Body []byte
	// TurnRunning is whether a turn the fake itself started was still running
	// when the post arrived, which is what a delivery into a busy session is.
	TurnRunning bool
}

// fakeOpenCode is a stand-in for the OpenCode 1 server a TUI serves, over a
// real listener, with the knobs the pump's tests turn: whether a session is busy
// and for how long, what the status reads right after a prompt, whether a prompt
// is accepted and then lost, and whether the server answers at all.
//
// A turn the fake starts is counted in polls of the status endpoint and not in
// time, so a test of the status race is the same on a loaded machine as on an
// idle one: after a prompt the status reads idle raceIdle times, busy busyPolls
// times, and the assistant message that ends the turn appears with the last busy
// read.
type fakeOpenCode struct {
	t *testing.T

	mu       sync.Mutex
	version  string
	exists   bool
	password string
	msgs     []fakeMsg
	seq      int

	// dir is the directory the session belongs to. The server itself is taken
	// to run somewhere else, as a TUI started in another checkout does: like
	// OpenCode 1.18.30, the fake lists a session's status only under the
	// session's own directory, and answers {} for a request with none or another.
	dir         string
	noDirectory bool
	statusDirs  []string
	// nextUserID, if set, is the id the next accepted prompt's message is given
	// instead of the next in sequence: the server assigns ids when it creates a
	// message, which is not the order prompts were posted in.
	nextUserID string
	// stallMessages and stallStatus make those reads hang, once a prompt has been
	// accepted, until the client gives up on the request: a server that has
	// stopped answering without closing anything.
	stallMessages bool
	stallStatus   bool
	// burst is how many assistant messages the turn has written by the time the
	// first read after an accepted prompt is answered, each answering burstParent
	// (the prompt's own message when that is empty).
	burst       int
	burstParent string
	// plant is put in the history when a prompt is accepted, after the prompt's own
	// message if there is one: what a read-back must not take for the delivery.
	plant []plantedMsg

	// manualBusy is a turn the test holds open, as a human's own prompt would.
	manualBusy bool

	// The turn each accepted prompt starts: the status reads idle turnRace
	// times, as it can right after a 204, then busy turnPolls times, and the
	// last busy read is the one that ends the turn. turnPolls of zero is a prompt
	// that starts no turn at all.
	turnRace  int
	turnPolls int
	// raceIdle and busyPolls are what is left of the turn in progress.
	raceIdle  int
	busyPolls int
	// hold makes the server accept a prompt, 204 and all, and lose it.
	hold bool
	// userLag is how many reads of the history pass before an accepted prompt's
	// message shows in it.
	userLag int
	// promptCode, if set, is the status the prompt endpoint answers with.
	promptCode int
	// page makes the health path answer as OpenCode 2 does, with its web page.
	page bool

	running   bool
	lagLeft   int
	pending   []fakeMsg
	pendingID string

	posts        []fakePost
	statusReads  int
	messageReads int
	limits       []string

	srv  *httptest.Server
	addr string
}

func newFake(t *testing.T) *fakeOpenCode {
	t.Helper()
	f := &fakeOpenCode{t: t, version: "1.18.30", exists: true, turnPolls: 1, dir: testDir}
	f.start("127.0.0.1:0")
	t.Cleanup(f.stop)
	return f
}

// start serves on addr, which is "127.0.0.1:0" for a new port or the address of
// an earlier run to come back on it.
func (f *fakeOpenCode) start(addr string) {
	f.t.Helper()
	var l net.Listener
	var err error
	// A port just released can take a moment to be bindable again.
	for i := 0; i < 50; i++ {
		if l, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		f.t.Fatalf("listen on %s: %v", addr, err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	f.mu.Lock()
	f.srv, f.addr = srv, l.Addr().String()
	f.mu.Unlock()
}

// stop closes the listener and every connection; it is safe to call twice.
func (f *fakeOpenCode) stop() {
	f.mu.Lock()
	srv := f.srv
	f.srv = nil
	f.mu.Unlock()
	if srv != nil {
		srv.CloseClientConnections()
		srv.Close()
	}
}

// url is the base URL of the server as the front is given it.
func (f *fakeOpenCode) url() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "http://" + f.addr
}

func (f *fakeOpenCode) address() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addr
}

func (f *fakeOpenCode) client() *HTTPClient {
	f.t.Helper()
	c, err := NewHTTPClient(f.url(), f.auth(), WithRequestTimeout(2*time.Second))
	if err != nil {
		f.t.Fatalf("NewHTTPClient: %v", err)
	}
	return c
}

func (f *fakeOpenCode) auth() Auth {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Auth{User: "opencode", Password: f.password}
}

func (f *fakeOpenCode) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// seed puts a message in the history as if it had been there all along.
func (f *fakeOpenCode) seed(role, text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.newMsgLocked(role, "", text)
	f.msgs = append(f.msgs, m)
	return m.ID
}

// seedID is seed with an id of the test's choosing.
func (f *fakeOpenCode) seedID(id, role, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.newMsgLocked(role, "", text)
	m.ID = id
	f.msgs = append(f.msgs, m)
}

func (f *fakeOpenCode) newMsgLocked(role, parent, text string) fakeMsg {
	f.seq++
	id := fmt.Sprintf("msg_%012d", f.seq)
	parts := []map[string]any{{"type": "text", "text": text, "id": "prt_" + strconv.Itoa(f.seq), "messageID": id}}
	if role == "assistant" {
		parts = append([]map[string]any{{"type": "step-start"}}, parts...)
	}
	return fakeMsg{ID: id, Role: role, ParentID: parent, Parts: parts}
}

// userTexts is the text of every user message in the history, in id order.
func (f *fakeOpenCode) userTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	msgs := append([]fakeMsg(nil), f.msgs...)
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
	for _, m := range msgs {
		if m.Role == "user" {
			out = append(out, m.Parts[0]["text"].(string))
		}
	}
	return out
}

// statusDirsSeen is the directory parameter of every status request so far.
func (f *fakeOpenCode) statusDirsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.statusDirs...)
}

func (f *fakeOpenCode) postBodies() []fakePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePost(nil), f.posts...)
}

// waitPosts blocks until n prompts have been accepted and returns them.
func (f *fakeOpenCode) waitPosts(n int) []fakePost {
	f.t.Helper()
	deadline := time.Now().Add(testDeadline)
	for {
		if p := f.postBodies(); len(p) >= n {
			return p
		}
		if !time.Now().Before(deadline) {
			f.t.Fatalf("%d prompt(s) within %s, want %d", len(f.postBodies()), testDeadline, n)
		}
		time.Sleep(pollInterval)
	}
}

func (f *fakeOpenCode) serve(w http.ResponseWriter, r *http.Request) {
	// A stall waits outside the lock, so that the other requests are answered
	// while it hangs.
	f.mu.Lock()
	stall := len(f.posts) > 0 && ((f.stallMessages && strings.HasSuffix(r.URL.Path, "/message")) || (f.stallStatus && r.URL.Path == "/session/status"))
	f.mu.Unlock()
	if stall {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.password != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != "opencode" || p != f.password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Secure Area"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}
	path := r.URL.Path
	switch {
	case path == "/global/health" && r.Method == http.MethodGet && f.page:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html><body><div id=root></div></body></html>"))
	case path == "/global/health" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"healthy": true, "version": f.version})
	case path == "/session/status" && r.Method == http.MethodGet:
		f.statusReads++
		dir := r.URL.Query().Get("directory")
		f.statusDirs = append(f.statusDirs, dir)
		if dir != f.dir {
			writeJSON(w, http.StatusOK, map[string]any{})
			return
		}
		if f.statusBusyLocked() {
			writeJSON(w, http.StatusOK, map[string]any{testSession: map[string]any{"type": "busy"}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{})
	case strings.HasPrefix(path, "/session/"):
		rest := strings.TrimPrefix(path, "/session/")
		id, tail, _ := strings.Cut(rest, "/")
		if id != testSession || !f.exists {
			writeJSON(w, http.StatusNotFound, map[string]any{"name": "NotFoundError", "data": map[string]any{"message": "Session not found: " + id}})
			return
		}
		switch {
		case tail == "" && r.Method == http.MethodGet:
			info := map[string]any{"id": id, "title": "t", "directory": f.dir}
			if f.noDirectory {
				delete(info, "directory")
			}
			writeJSON(w, http.StatusOK, info)
		case tail == "message" && r.Method == http.MethodGet:
			f.messagesLocked(w, r)
		case tail == "prompt_async" && r.Method == http.MethodPost:
			f.promptLocked(w, r)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

// statusBusyLocked is one read of the status endpoint, advancing the turn a
// prompt started.
func (f *fakeOpenCode) statusBusyLocked() bool {
	if f.manualBusy {
		return true
	}
	if f.raceIdle > 0 {
		f.raceIdle--
		return false
	}
	if f.busyPolls > 0 {
		f.busyPolls--
		if f.busyPolls == 0 {
			// The turn ends with this read: the answer is in the history, and
			// the next read of the status is idle.
			f.running = false
			f.msgs = append(f.msgs, f.newMsgLocked("assistant", f.pendingID, "done"))
		}
		return true
	}
	return false
}

func (f *fakeOpenCode) messagesLocked(w http.ResponseWriter, r *http.Request) {
	f.messageReads++
	f.limits = append(f.limits, r.URL.Query().Get("limit"))
	if f.lagLeft > 0 {
		f.lagLeft--
		if f.lagLeft == 0 {
			f.msgs = append(f.msgs, f.pending...)
			f.pending = nil
		}
	}
	// The server lists by id, whatever order the messages were created in.
	msgs := append([]fakeMsg(nil), f.msgs...)
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
	q := r.URL.Query()
	if c := q.Get("before"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		var cur struct {
			ID string `json:"id"`
		}
		if err != nil || json.Unmarshal(raw, &cur) != nil || cur.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"_tag": "BadRequest"})
			return
		}
		i := sort.Search(len(msgs), func(i int) bool { return msgs[i].ID >= cur.ID })
		msgs = msgs[:i]
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"name": "BadRequest"})
			return
		}
		if n > 0 && n < len(msgs) {
			// The cursor of the page before this one, which exists while anything
			// older is left.
			oldest := msgs[len(msgs)-n]
			cur, _ := json.Marshal(map[string]any{"id": oldest.ID, "time": 1})
			w.Header().Set("X-Next-Cursor", base64.RawURLEncoding.EncodeToString(cur))
			msgs = msgs[len(msgs)-n:]
		}
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		info := map[string]any{"id": m.ID, "sessionID": testSession, "role": m.Role, "time": map[string]any{"created": 1}}
		if m.ParentID != "" {
			info["parentID"] = m.ParentID
		}
		out = append(out, map[string]any{"info": info, "parts": m.Parts})
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *fakeOpenCode) promptLocked(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if f.promptCode != 0 {
		writeJSON(w, f.promptCode, map[string]any{"name": "Nope"})
		return
	}
	f.posts = append(f.posts, fakePost{Body: body, TurnRunning: f.running})
	var req struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Parts) != 1 || req.Parts[0].Type != "text" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"name": "BadRequest"})
		return
	}
	user := ""
	if !f.hold {
		m := f.newMsgLocked("user", "", req.Parts[0].Text)
		user = m.ID
		if f.nextUserID != "" {
			m.ID, f.nextUserID = f.nextUserID, ""
		}
		f.pendingID = m.ID
		if f.userLag > 0 {
			f.pending, f.lagLeft = append(f.pending, m), f.userLag
		} else {
			f.msgs = append(f.msgs, m)
		}
		f.raceIdle, f.busyPolls = f.turnRace, f.turnPolls
		f.running = f.busyPolls > 0
	}
	for _, pl := range f.plant {
		m := f.newMsgLocked(pl.role, "", pl.text)
		if pl.partType != "" {
			m.Parts[len(m.Parts)-1]["type"] = pl.partType
		}
		f.msgs = append(f.msgs, m)
	}
	parent := user
	if f.burstParent != "" {
		parent = f.burstParent
	}
	for i := 0; i < f.burst; i++ {
		f.msgs = append(f.msgs, f.newMsgLocked("assistant", parent, fmt.Sprintf("step %d", i)))
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
