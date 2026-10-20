package opencode

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const fake2PID = 4242

// fake2Msg is a user message the fake's session holds, in the order it was
// promoted.
type fake2Msg struct {
	id, text string
}

// fake2Item is an entry of the fake's inbox.
type fake2Item struct {
	id, text, delivery string
	// lag is how many reads of the message by this id answer 404 before the session
	// takes the entry up.
	lag int
}

type fake2Post struct {
	Body []byte
	// TurnRunning is whether the session was in a turn when the post arrived.
	TurnRunning bool
}

// fakeOC2 is a stand-in for an OpenCode 2 server, over a real listener, with the
// behaviour of 2.0.32 that the pump depends on (read from its source and checked
// against a live server in simulation mode): a prompt with an id is admitted into
// the inbox, a repeat of an id is answered with the first, an idle session takes a
// queued entry up at once and a busy one after its turn, steer entries go first, a
// parked session takes nothing until the human resumes it, and an entry
// leaves the inbox and becomes a message in one step.
//
// A turn is counted in reads of the active list and not in time, as the OpenCode 1
// fake's are, so a test is the same on a loaded machine as on an idle one: after
// an entry is taken up the list reads busy turnReads times.
type fakeOC2 struct {
	t *testing.T

	mu       sync.Mutex
	version  string
	pid      int
	password string
	exists   bool
	dir      string

	msgs  []fake2Msg
	inbox []fake2Item

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

	posts []fake2Post
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

	srv  *httptest.Server
	addr string
}

func newFake2(t *testing.T) *fakeOC2 {
	t.Helper()
	f := &fakeOC2{t: t, version: "2.0.32", pid: fake2PID, exists: true, dir: testDir, turnReads: 1, reads: map[string]int{}}
	f.start("127.0.0.1:0")
	t.Cleanup(f.stop)
	return f
}

func (f *fakeOC2) start(addr string) {
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

func (f *fakeOC2) stop() {
	f.mu.Lock()
	srv := f.srv
	f.srv = nil
	f.mu.Unlock()
	if srv != nil {
		srv.CloseClientConnections()
		srv.Close()
	}
}

func (f *fakeOC2) url() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "http://" + f.addr
}

func (f *fakeOC2) address() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addr
}

func (f *fakeOC2) client() *HTTPClient2 {
	f.t.Helper()
	f.mu.Lock()
	pw := f.password
	f.mu.Unlock()
	c, err := NewHTTPClient2(f.url(), Auth{User: ServiceUser, Password: pw}, WithRequestTimeout(2*time.Second))
	if err != nil {
		f.t.Fatalf("NewHTTPClient2: %v", err)
	}
	return c
}

func (f *fakeOC2) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// release ends a turn the test held open, and lets the session take up what
// waited for it.
func (f *fakeOC2) release() {
	f.set(func() {
		f.manualBusy = false
		f.promoteLocked()
	})
}

// resume is the human resuming a session an interrupt parked: it takes up what
// waited.
func (f *fakeOC2) resume() {
	f.set(func() {
		f.parked = false
		f.promoteLocked()
	})
}

func (f *fakeOC2) postBodies() []fake2Post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fake2Post(nil), f.posts...)
}

func (f *fakeOC2) patched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.patches...)
}

func (f *fakeOC2) read(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[route]
}

func (f *fakeOC2) userTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.msgs {
		out = append(out, m.text)
	}
	return out
}

func (f *fakeOC2) inboxIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, it := range f.inbox {
		out = append(out, it.id)
	}
	return out
}

func (f *fakeOC2) waitPosts(n int) []fake2Post {
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

func (f *fakeOC2) waitReads(route string, n int) {
	f.t.Helper()
	deadline := time.Now().Add(testDeadline)
	for f.read(route) < n {
		if !time.Now().Before(deadline) {
			f.t.Fatalf("%d read(s) of %s within %s, want %d", f.read(route), route, testDeadline, n)
		}
		time.Sleep(pollInterval)
	}
}

func (f *fakeOC2) busyLocked() bool { return f.manualBusy || f.busyLeft > 0 }

// promoteLocked is the session taking up input: nothing while it is in a turn;
// when it is free, the oldest steer entry, else, unless it is parked, the oldest
// queued one. The entry leaves the inbox and becomes a message together, and a
// turn begins.
func (f *fakeOC2) promoteLocked() {
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
	f.msgs = append(f.msgs, fake2Msg{id: it.id, text: text})
	f.busyLeft = f.turnReads
}

func (f *fakeOC2) serve(w http.ResponseWriter, r *http.Request) {
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

func (f *fakeOC2) sessionLocked(w http.ResponseWriter, r *http.Request, tail string) {
	switch {
	case tail == "" && r.Method == http.MethodGet:
		f.reads["session"]++
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"id": testSession, "title": "t", "location": map[string]any{"directory": f.dir}}})
	case tail == "inbox" && r.Method == http.MethodGet:
		f.reads["inbox"]++
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
	case strings.HasPrefix(tail, "message/") && r.Method == http.MethodGet:
		f.reads["message"]++
		f.messageLocked(w, strings.TrimPrefix(tail, "message/"))
	case tail == "prompt" && r.Method == http.MethodPost:
		f.promptLocked(w, r)
	case strings.HasPrefix(tail, "inbox/") && r.Method == http.MethodPatch:
		f.steerLocked(w, r, strings.TrimPrefix(tail, "inbox/"))
	default:
		http.NotFound(w, r)
	}
}

func inboxJSON(it fake2Item) map[string]any {
	return map[string]any{"id": it.id, "sessionID": testSession, "time": map[string]any{"created": 1}, "type": "user", "payload": map[string]any{"text": it.text}, "delivery": it.delivery}
}

func (f *fakeOC2) messageLocked(w http.ResponseWriter, id string) {
	for _, m := range f.msgs {
		if m.id == id {
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

func (f *fakeOC2) promptLocked(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.posts = append(f.posts, fake2Post{Body: body, TurnRunning: f.busyLocked()})
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
	item := fake2Item{id: req.ID, text: req.Text, delivery: req.Delivery}
	known := false
	for _, it := range f.inbox {
		if it.id == req.ID {
			item, known = it, true
		}
	}
	for _, m := range f.msgs {
		if m.id == req.ID {
			item, known = fake2Item{id: m.id, text: m.text, delivery: "queue"}, true
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

func (f *fakeOC2) steerLocked(w http.ResponseWriter, r *http.Request, id string) {
	body, _ := io.ReadAll(r.Body)
	f.patches = append(f.patches, id+" "+string(body))
	w.WriteHeader(http.StatusMethodNotAllowed)
}
