package opencode

import (
	"bytes"
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

const (
	testSession = "ses_ea8bbe5a9ffePFZ3AOhDdaNgbW"
	// testDir is the directory the fake's session belongs to.
	testDir = "/work/checkout"

	fakePID = 4242
)

// fakeMsg is a message the fake's session holds, in the order it was added: a user
// message promoted from the inbox (kind ""), or, added by a test, an assistant
// message that finished as finish, or an idle marker with an outcome.
type fakeMsg struct {
	id, text string
	kind     string
	finish   string
	outcome  string
}

// fakeItem is an entry of the fake's inbox.
type fakeItem struct {
	id, text, delivery string
	// lag is how many reads of the message by this id answer 404 before the session
	// takes the entry up.
	lag int
}

type fakePost struct {
	Body []byte
	// TurnRunning is whether the session was in a turn when the post arrived.
	TurnRunning bool
}

// fakeOpenCode is a stand-in for an OpenCode 2 server, over a real listener, with the
// behaviour of 2.0.32 that the pump depends on (read from its source and checked
// against a live server in simulation mode): a prompt with an id is admitted into
// the inbox, a repeat of an id is answered with the first, an idle session takes a
// queued entry up at once and a busy one after its turn, steer entries go first, a
// parked session takes nothing until the human resumes it, and an entry
// leaves the inbox and becomes a message in one step.
//
// A turn is counted in reads of the active list and not in time, so a test is the
// same on a loaded machine as on an idle one: after an entry is taken up the list
// reads busy turnReads times.
type fakeOpenCode struct {
	t *testing.T

	mu       sync.Mutex
	version  string
	pid      int
	password string
	exists   bool
	dir      string

	msgs  []fakeMsg
	inbox []fakeItem

	// manualBusy is a turn the test holds open, as a human's own prompt would.
	manualBusy bool
	// turnReads is how many reads of the active list a turn the fake starts is
	// busy for; busyLeft is what is left of the one in progress.
	turnReads int
	busyLeft  int
	// parked makes the session take nothing up from the queue until resume: the
	// state after an interrupt.
	parked bool

	// lagReads is the lag given to an entry on admission.
	lagReads int
	// loseResponses is how many prompt answers are swallowed, the connection
	// closed after the entry was admitted.
	loseResponses int
	// onLose, if set, runs (with the fake's lock held) as an answer is swallowed,
	// before the connection is closed: the moment a test makes the world change
	// under the pump, such as a service file that has gone stale.
	onLose func()
	// cancelOnPost removes an entry from the inbox as it is admitted: the human
	// cancelled it.
	cancelOnPost bool
	// busyOnPost starts a turn of the human's own as the entry is admitted, so the
	// entry waits behind it until the test ends it.
	busyOnPost bool
	// corrupt makes the message that an entry becomes hold other text.
	corrupt bool
	// deleteOnPost makes the session unknown once an entry has been admitted.
	deleteOnPost bool

	posts []fakePost
	// patches are the requests to change an inbox entry. The pump makes none: it
	// never steers, and the fake answers 405, so one is a failure of the test.
	patches []string
	// echoOther makes the answer to a prompt carry other text than was posted, as a
	// plugin that rewrites prompts would leave it; the entry is admitted as posted.
	echoOther bool
	// info503 is how many reads of /api/info are answered 503, a service that is
	// starting.
	info503 int
	// inboxTooLong makes the inbox read answer a body past the bound the front
	// reads it under, as an inbox full of attachments does.
	inboxTooLong bool
	reads        map[string]int
	// cursorAlways keeps the next cursor of the history list non-null at its start,
	// where the live server leaves it and the page after is empty.
	cursorAlways bool
	// failNext is, per route ("active", "inbox", "message", "recent", "session",
	// "prompt"), the statuses the next
	// reads are answered with in place of their answers: a service that is starting
	// or in trouble.
	failNext map[string][]int

	srv  *httptest.Server
	addr string
}

func newFake(t *testing.T) *fakeOpenCode {
	t.Helper()
	f := &fakeOpenCode{t: t, version: "2.0.32", pid: fakePID, exists: true, dir: testDir, turnReads: 1, reads: map[string]int{}}
	f.start("127.0.0.1:0")
	t.Cleanup(f.stop)
	return f
}

func (f *fakeOpenCode) start(addr string) {
	f.t.Helper()
	var l net.Listener
	var err error
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
	f.mu.Lock()
	pw := f.password
	f.mu.Unlock()
	c, err := NewHTTPClient(f.url(), Auth{Password: pw}, WithRequestTimeout(2*time.Second))
	if err != nil {
		f.t.Fatalf("NewHTTPClient: %v", err)
	}
	return c
}

func (f *fakeOpenCode) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// release ends a turn the test held open, and lets the session take up what
// waited for it.
func (f *fakeOpenCode) release() {
	f.set(func() {
		f.manualBusy = false
		f.promoteLocked()
	})
}

// resume is the human resuming a session an interrupt parked: it takes up what
// waited.
func (f *fakeOpenCode) resume() {
	f.set(func() {
		f.parked = false
		f.promoteLocked()
	})
}

func (f *fakeOpenCode) postBodies() []fakePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePost(nil), f.posts...)
}

func (f *fakeOpenCode) patched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.patches...)
}

func (f *fakeOpenCode) read(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[route]
}

func (f *fakeOpenCode) userTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.msgs {
		if m.kind == "" {
			out = append(out, m.text)
		}
	}
	return out
}

func (f *fakeOpenCode) inboxIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, it := range f.inbox {
		out = append(out, it.id)
	}
	return out
}

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

func (f *fakeOpenCode) waitReads(route string, n int) {
	f.t.Helper()
	deadline := time.Now().Add(testDeadline)
	for f.read(route) < n {
		if !time.Now().Before(deadline) {
			f.t.Fatalf("%d read(s) of %s within %s, want %d", f.read(route), route, testDeadline, n)
		}
		time.Sleep(pollInterval)
	}
}

func (f *fakeOpenCode) busyLocked() bool { return f.manualBusy || f.busyLeft > 0 }

// promoteLocked is the session taking up input: nothing while it is in a turn;
// when it is free, the oldest steer entry, else, unless it is parked, the oldest
// queued one. The entry leaves the inbox and becomes a message together, and a
// turn begins.
func (f *fakeOpenCode) promoteLocked() {
	if f.busyLocked() {
		return
	}
	pick := -1
	for i, it := range f.inbox {
		if it.delivery == "steer" && it.lag == 0 {
			pick = i
			break
		}
	}
	if pick < 0 && !f.parked {
		for i, it := range f.inbox {
			if it.delivery == "queue" && it.lag == 0 {
				pick = i
				break
			}
		}
	}
	if pick < 0 {
		return
	}
	it := f.inbox[pick]
	f.inbox = append(f.inbox[:pick:pick], f.inbox[pick+1:]...)
	text := it.text
	if f.corrupt {
		text += "!"
	}
	f.msgs = append(f.msgs, fakeMsg{id: it.id, text: text})
	f.busyLeft = f.turnReads
}

func (f *fakeOpenCode) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.password != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != ServiceUser || p != f.password {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"_tag": "UnauthorizedError", "message": "Authentication required"})
			return
		}
	}
	path := r.URL.Path
	switch {
	case path == "/api/info" && r.Method == http.MethodGet:
		f.reads["info"]++
		if f.info503 > 0 {
			f.info503--
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"_tag": "ServiceUnavailable", "message": "starting"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"version": f.version, "pid": f.pid, "urls": []string{"http://" + f.addr}})
	case path == "/api/session/active" && r.Method == http.MethodGet:
		f.reads["active"]++
		if f.failedLocked(w, "active") {
			return
		}
		busy := f.busyLocked()
		if f.busyLeft > 0 {
			// The turn ends with this read: the session is free after it, and takes
			// up the next entry the way it does at the end of a turn that succeeded.
			f.busyLeft--
			if f.busyLeft == 0 {
				f.promoteLocked()
			}
		}
		data := map[string]any{}
		if busy {
			data[testSession] = map[string]any{"type": "running"}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data})
	case strings.HasPrefix(path, "/api/session/"):
		rest := strings.TrimPrefix(path, "/api/session/")
		id, tail, _ := strings.Cut(rest, "/")
		if id != testSession || !f.exists {
			writeJSON(w, http.StatusNotFound, map[string]any{"_tag": "SessionNotFoundError", "sessionID": id, "message": "Session not found: " + id})
			return
		}
		f.sessionLocked(w, r, tail)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeOpenCode) sessionLocked(w http.ResponseWriter, r *http.Request, tail string) {
	switch {
	case tail == "" && r.Method == http.MethodGet:
		f.reads["session"]++
		if f.failedLocked(w, "session") {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"id": testSession, "title": "t", "location": map[string]any{"directory": f.dir}}})
	case tail == "inbox" && r.Method == http.MethodGet:
		f.reads["inbox"]++
		if f.failedLocked(w, "inbox") {
			return
		}
		if f.inboxTooLong {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"msg_x","delivery":"queue","pad":"` + strings.Repeat("x", maxMessageBody) + `"}]}`))
			return
		}
		out := make([]map[string]any, 0, len(f.inbox))
		for _, it := range f.inbox {
			out = append(out, inboxJSON(it))
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	case tail == "message" && r.Method == http.MethodGet:
		f.reads["recent"]++
		if f.failedLocked(w, "recent") {
			return
		}
		f.recentLocked(w, r)
	case strings.HasPrefix(tail, "message/") && r.Method == http.MethodGet:
		f.reads["message"]++
		if f.failedLocked(w, "message") {
			return
		}
		f.messageLocked(w, strings.TrimPrefix(tail, "message/"))
	case tail == "prompt" && r.Method == http.MethodPost:
		f.promptLocked(w, r)
	case strings.HasPrefix(tail, "inbox/") && r.Method == http.MethodPatch:
		f.steerLocked(w, r, strings.TrimPrefix(tail, "inbox/"))
	default:
		http.NotFound(w, r)
	}
}

// failedLocked answers a read of route with the next status the test queued for
// it, and says whether it did.
func (f *fakeOpenCode) failedLocked(w http.ResponseWriter, route string) bool {
	q := f.failNext[route]
	if len(q) == 0 {
		return false
	}
	f.failNext[route] = q[1:]
	writeJSON(w, q[0], map[string]any{"_tag": "ServiceUnavailable", "message": "the service is in trouble"})
	return true
}

// recentLocked answers a page of the history, newest first: the newest messages for
// order=desc and a limit, and the ones after a page for a cursor and a limit (the
// server's rule is that a cursor is not combined with an order). The cursor is the
// index of the next older message. A page that leaves none behind has a null next
// cursor, unless the test wants the cursor kept at the start of the history, as the
// live server keeps it.
func (f *fakeOpenCode) recentLocked(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := strconv.Atoi(q.Get("limit"))
	bad := func() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"_tag": "InvalidRequestError", "message": "bad query"})
	}
	if err != nil || limit < 1 || len(q) != 2 {
		bad()
		return
	}
	start := len(f.msgs) - 1
	switch {
	case q.Has("cursor"):
		n, err := strconv.Atoi(strings.TrimPrefix(q.Get("cursor"), "c"))
		if err != nil || !strings.HasPrefix(q.Get("cursor"), "c") || n >= len(f.msgs) {
			bad()
			return
		}
		start = n
	case q.Get("order") != "desc":
		bad()
		return
	}
	out := []map[string]any{}
	i := start
	for ; i >= 0 && len(out) < limit; i-- {
		m := f.msgs[i]
		e := map[string]any{"id": m.id, "time": map[string]any{"created": i + 1}, "metadata": map[string]any{}}
		switch m.kind {
		case "":
			e["type"], e["text"], e["files"] = "user", m.text, []any{}
		case "assistant":
			e["type"], e["agent"], e["content"], e["finish"] = "assistant", "build", []any{}, m.finish
		case "idle":
			e["type"], e["outcome"] = "idle", m.outcome
		default:
			e["type"] = m.kind
		}
		out = append(out, e)
	}
	var next any
	if i >= 0 || f.cursorAlways {
		next = "c" + strconv.Itoa(i)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "cursor": map[string]any{"previous": nil, "next": next}})
}

// addOther puts a message of a kind the front reads past (a skill, a shell) into the
// history.
func (f *fakeOpenCode) addOther(kind string) {
	f.set(func() {
		f.msgs = append(f.msgs, fakeMsg{id: fmt.Sprintf("msg_o%d", len(f.msgs)), kind: kind})
	})
}

// addAssistant puts an assistant message that finished as finish into the history,
// as a step of a turn does.
func (f *fakeOpenCode) addAssistant(finish string) {
	f.set(func() {
		f.msgs = append(f.msgs, fakeMsg{id: fmt.Sprintf("msg_a%d", len(f.msgs)), kind: "assistant", finish: finish})
	})
}

// addIdle puts an idle marker with outcome into the history, as the end of a turn
// does.
func (f *fakeOpenCode) addIdle(outcome string) {
	f.set(func() {
		f.msgs = append(f.msgs, fakeMsg{id: fmt.Sprintf("msg_i%d", len(f.msgs)), kind: "idle", outcome: outcome})
	})
}

// addUser puts a user message into the history, the human's own prompt.
func (f *fakeOpenCode) addUser(text string) {
	f.set(func() {
		f.msgs = append(f.msgs, fakeMsg{id: fmt.Sprintf("msg_u%d", len(f.msgs)), text: text})
	})
}

func inboxJSON(it fakeItem) map[string]any {
	return map[string]any{"id": it.id, "sessionID": testSession, "time": map[string]any{"created": 1}, "type": "user", "payload": map[string]any{"text": it.text}, "delivery": it.delivery}
}

func (f *fakeOpenCode) messageLocked(w http.ResponseWriter, id string) {
	for _, m := range f.msgs {
		if m.id == id && m.kind == "" {
			writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"id": m.id, "time": map[string]any{"created": 1}, "text": m.text, "type": "user"}})
			return
		}
	}
	for i := range f.inbox {
		if f.inbox[i].id == id && f.inbox[i].lag > 0 {
			f.inbox[i].lag--
			if f.inbox[i].lag == 0 {
				f.promoteLocked()
			}
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"_tag": "MessageNotFoundError", "sessionID": testSession, "messageID": id, "message": "Message not found: " + id})
}

func (f *fakeOpenCode) promptLocked(w http.ResponseWriter, r *http.Request) {
	if f.failedLocked(w, "prompt") {
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.posts = append(f.posts, fakePost{Body: body, TurnRunning: f.busyLocked()})
	// The body is held to the three fields this front sends: anything else is a
	// request the front is not allowed to make.
	var req struct {
		ID       string `json:"id"`
		Text     string `json:"text"`
		Delivery string `json:"delivery"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || !strings.HasPrefix(req.ID, "msg_") || req.Delivery != "queue" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"_tag": "InvalidRequestError", "message": "bad prompt"})
		return
	}
	item := fakeItem{id: req.ID, text: req.Text, delivery: req.Delivery}
	known := false
	for _, it := range f.inbox {
		if it.id == req.ID {
			item, known = it, true
		}
	}
	for _, m := range f.msgs {
		if m.id == req.ID && m.kind == "" {
			item, known = fakeItem{id: m.id, text: m.text, delivery: "queue"}, true
		}
	}
	if !known {
		item.lag = f.lagReads
		f.inbox = append(f.inbox, item)
		switch {
		case f.cancelOnPost:
			f.inbox = f.inbox[:len(f.inbox)-1]
		case f.busyOnPost:
			f.manualBusy = true
		default:
			f.promoteLocked()
		}
		if f.deleteOnPost {
			f.exists = false
		}
	}
	if f.loseResponses > 0 {
		f.loseResponses--
		if f.onLose != nil {
			f.onLose()
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
	}
	echo := inboxJSON(item)
	if f.echoOther {
		echo["payload"] = map[string]any{"text": item.text + " (rewritten)"}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": echo})
}

func (f *fakeOpenCode) steerLocked(w http.ResponseWriter, r *http.Request, id string) {
	body, _ := io.ReadAll(r.Body)
	f.patches = append(f.patches, id+" "+string(body))
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
