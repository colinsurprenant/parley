package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testMsgID = "msg_157612985001ZP97n73ncfxOnr"
	// testPassword is a password that must not show up in anything the client says.
	testPassword = "p4ssw0rd-that-must-stay-secret"
)

// serving starts a server with the given handler and returns a client for it.
func serving(t *testing.T, h http.HandlerFunc) *HTTPClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewHTTPClient(srv.URL, Auth{Password: testPassword}, WithRequestTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	return c
}

func answering(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

func TestInfo(t *testing.T) {
	f := newFake(t)
	info, err := f.client().Info(context.Background())
	if err != nil || info.Version != "2.0.32" || info.PID != fakePID {
		t.Fatalf("Info = %+v, %v; want 2.0.32 and the pid", info, err)
	}
	// A 200 that is not what an OpenCode 2 server says of itself is not one.
	for name, body := range map[string]string{
		"not json":      `ok`,
		"a list":        `[]`,
		"no version":    `{"pid":4}`,
		"no pid":        `{"version":"2.0.32"}`,
		"a zero pid":    `{"version":"2.0.32","pid":0}`,
		"empty version": `{"version":"","pid":4}`,
		"two values":    `{"version":"2.0.32","pid":4} {}`,
	} {
		if info, err := serving(t, answering(200, body)).Info(context.Background()); err == nil {
			t.Errorf("Info of %s (%s) = %+v, want an error", name, body, info)
		}
	}
	if _, err := serving(t, answering(404, `{}`)).Info(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("Info of a 404 = %v", err)
	}
}

func TestSession(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	s, err := f.client().Session(ctx, testSession)
	if err != nil || s.ID != testSession || s.Directory != testDir {
		t.Fatalf("Session = %+v, %v", s, err)
	}
	if _, err := f.client().Session(ctx, "ses_000000000000000000000000AA"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Session of an unknown id = %v, want ErrSessionNotFound", err)
	}
	for name, body := range map[string]string{
		"another session": `{"data":{"id":"ses_000000000000000000000000AA","location":{"directory":"/x"}}}`,
		"no directory":    `{"data":{"id":"` + testSession + `","location":{}}}`,
		"no location":     `{"data":{"id":"` + testSession + `"}}`,
		"no data":         `{}`,
	} {
		if s, err := serving(t, answering(200, body)).Session(ctx, testSession); err == nil {
			t.Errorf("Session of %s = %+v, want an error", name, s)
		}
	}
	// A 404 that is not the session's is not "no such session".
	if _, err := serving(t, answering(404, `{"_tag":"LocationNotFoundError"}`)).Session(ctx, testSession); err == nil || errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Session of another 404 = %v", err)
	}
}

func TestBusy(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	if busy, err := f.client().Busy(ctx, testSession); err != nil || busy {
		t.Fatalf("Busy of an idle server = %v, %v", busy, err)
	}
	f.set(func() { f.manualBusy = true })
	if busy, err := f.client().Busy(ctx, testSession); err != nil || !busy {
		t.Fatalf("Busy of a session in a turn = %v, %v", busy, err)
	}
	// Another session's turn is not this session's.
	other := `{"data":{"ses_000000000000000000000000AA":{"type":"running"}}}`
	if busy, err := serving(t, answering(200, other)).Busy(ctx, testSession); err != nil || busy {
		t.Fatalf("Busy with another session running = %v, %v", busy, err)
	}
	// A type this front has never seen is still a turn in progress.
	if busy, err := serving(t, answering(200, `{"data":{"`+testSession+`":{"type":"compacting"}}}`)).Busy(ctx, testSession); err != nil || !busy {
		t.Fatalf("Busy of an unknown type = %v, %v", busy, err)
	}
	for name, body := range map[string]string{
		"no data":          `{}`,
		"a list":           `{"data":[]}`,
		"an entry no type": `{"data":{"` + testSession + `":{}}}`,
		"a null entry":     `{"data":{"` + testSession + `":null}}`,
		"not json":         `x`,
	} {
		if busy, err := serving(t, answering(200, body)).Busy(ctx, testSession); err == nil {
			t.Errorf("Busy of %s = %v, want an error", name, busy)
		}
	}
}

func TestPromptSendsTheThreeFieldsAndChecksTheAnswer(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.parked = true })
	ctx := context.Background()
	text := "[parley] from=claude-b id=1\n  héllo\n"
	if err := f.client().Prompt(ctx, testSession, testMsgID, text); err != nil {
		t.Fatalf("Prompt = %v", err)
	}
	posts := f.postBodies()
	if len(posts) != 1 {
		t.Fatalf("%d posts", len(posts))
	}
	id, got, delivery := post(t, posts[0].Body)
	if id != testMsgID || got != text || delivery != "queue" {
		t.Fatalf("posted %q %q %q", id, got, delivery)
	}
	// A repeat is answered with the first, and the entry is there once.
	if err := f.client().Prompt(ctx, testSession, testMsgID, text); err != nil {
		t.Fatalf("Prompt again = %v", err)
	}
	if ids := f.inboxIDs(); len(ids) != 1 || ids[0] != testMsgID {
		t.Fatalf("inbox %v", ids)
	}
	// The same id with other text is answered with the first, which is not the text
	// that was asked to be posted: the client says so and does not take it for a
	// delivery.
	err := f.client().Prompt(ctx, testSession, testMsgID, text+"more")
	for _, want := range []string{"admitted the prompt under message id " + testMsgID + " with other text", "a plugin may have rewritten it", "will probably be taken up as it is", "nothing was committed", "a restart sends the delivery again under a new id"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Prompt of another text under the same id = %v, want it to say %q", err, want)
		}
	}
	// Text JSON cannot carry is refused before anything is sent.
	if err := f.client().Prompt(ctx, testSession, "msg_x", "bad \xff text"); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("Prompt of invalid UTF-8 = %v", err)
	}
	if n := len(f.postBodies()); n != 3 {
		t.Fatalf("%d posts reached the server, want only the three valid ones", n)
	}
	// Errors by status.
	if err := f.client().Prompt(ctx, "ses_000000000000000000000000AA", testMsgID, "x"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Prompt into an unknown session = %v", err)
	}
	if err := serving(t, answering(409, `{"_tag":"ConflictError"}`)).Prompt(ctx, testSession, testMsgID, "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Prompt answered 409 = %v", err)
	}
	if err := serving(t, answering(400, `{"_tag":"InvalidRequestError"}`)).Prompt(ctx, testSession, testMsgID, "x"); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("Prompt answered 400 = %v", err)
	}
	for name, body := range map[string]string{
		"another id":      `{"data":{"id":"msg_other","sessionID":"` + testSession + `","type":"user","payload":{"text":"x"},"delivery":"queue"}}`,
		"another session": `{"data":{"id":"` + testMsgID + `","sessionID":"ses_other","type":"user","payload":{"text":"x"},"delivery":"queue"}}`,
		"not a user":      `{"data":{"id":"` + testMsgID + `","sessionID":"` + testSession + `","type":"synthetic","payload":{"text":"x"},"delivery":"queue"}}`,
		"no payload":      `{"data":{"id":"` + testMsgID + `","sessionID":"` + testSession + `","type":"user"}}`,
		"no data":         `{}`,
	} {
		if err := serving(t, answering(200, body)).Prompt(ctx, testSession, testMsgID, "x"); err == nil {
			t.Errorf("Prompt answered with %s was accepted", name)
		}
	}
}

func TestMessage(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	if _, found, err := f.client().Message(ctx, testSession, testMsgID); err != nil || found {
		t.Fatalf("Message of an id nobody posted = %v, %v", found, err)
	}
	if err := f.client().Prompt(ctx, testSession, testMsgID, "text"); err != nil {
		t.Fatal(err)
	}
	m, found, err := f.client().Message(ctx, testSession, testMsgID)
	if err != nil || !found || m.ID != testMsgID || m.Text != "text" {
		t.Fatalf("Message = %+v, %v, %v", m, found, err)
	}
	if _, _, err := f.client().Message(ctx, "ses_000000000000000000000000AA", testMsgID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Message of an unknown session = %v", err)
	}
	for name, body := range map[string]string{
		"another id":  `{"data":{"id":"msg_other","type":"user","text":"x"}}`,
		"not a user":  `{"data":{"id":"` + testMsgID + `","type":"assistant","text":"x"}}`,
		"no text":     `{"data":{"id":"` + testMsgID + `","type":"user"}}`,
		"no data":     `{}`,
		"not json":    `x`,
		"text number": `{"data":{"id":"` + testMsgID + `","type":"user","text":1}}`,
	} {
		if m, _, err := serving(t, answering(200, body)).Message(ctx, testSession, testMsgID); err == nil {
			t.Errorf("Message of %s = %+v, want an error", name, m)
		}
	}
	// A 404 that names no message is not "not there yet".
	if _, found, err := serving(t, answering(404, `{}`)).Message(ctx, testSession, testMsgID); err == nil || found {
		t.Errorf("Message of an untagged 404 = %v, %v", found, err)
	}
	// An answer over the bound is an error, not a buffer of the whole.
	big := `{"data":{"id":"` + testMsgID + `","type":"user","text":"` + strings.Repeat("x", maxMessageBody) + `"}}`
	if _, _, err := serving(t, answering(200, big)).Message(ctx, testSession, testMsgID); !IsTooLong(err) {
		t.Errorf("Message over the bound = %v, want a TooLongError", err)
	}
}

func TestInbox(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.parked = true })
	ctx := context.Background()
	if items, err := f.client().Inbox(ctx, testSession); err != nil || len(items) != 0 {
		t.Fatalf("Inbox of a quiet session = %v, %v", items, err)
	}
	for _, id := range []string{testMsgID, "msg_157612985002ZP97n73ncfxOnr"} {
		if err := f.client().Prompt(ctx, testSession, id, "t"); err != nil {
			t.Fatal(err)
		}
	}
	items, err := f.client().Inbox(ctx, testSession)
	if err != nil || len(items) != 2 || items[0] != (InboxItem{ID: testMsgID, Delivery: "queue"}) {
		t.Fatalf("Inbox = %v, %v", items, err)
	}
	for name, body := range map[string]string{
		"no data":              `{}`,
		"an entry no id":       `{"data":[{"delivery":"queue"}]}`,
		"an entry no delivery": `{"data":[{"id":"x"}]}`,
		"a null entry":         `{"data":[null]}`,
		"an object":            `{"data":{}}`,
	} {
		if items, err := serving(t, answering(200, body)).Inbox(ctx, testSession); err == nil {
			t.Errorf("Inbox of %s = %v, want an error", name, items)
		}
	}
}

// TestTheClientSendsTheCredentialsToLoopbackAndSaysNothingOfThem: the password is
// sent as HTTP basic credentials under the user opencode, to a loopback address
// alone, and it is in no error, whatever the server answers and whatever formats
// the client.
func TestTheClientSendsTheCredentialsToLoopbackAndSaysNothingOfThem(t *testing.T) {
	var user, pass string
	var ok bool
	c := serving(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"_tag":"UnauthorizedError","message":"Authentication required"}`)
	})
	_, err := c.Info(context.Background())
	if !ok || user != ServiceUser || pass != testPassword {
		t.Fatalf("credentials %q %q %v", user, pass, ok)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 401 {
		t.Fatalf("Info of a 401 = %v", err)
	}
	for _, want := range []string{"401", "OPENCODE_PASSWORD", "service file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the 401 does not say %q: %v", want, err)
		}
	}
	auth := Auth{Password: testPassword}
	for _, out := range []string{
		err.Error(),
		fmt.Sprintf("%v %+v %#v %s", auth, auth, auth, auth),
		fmt.Sprintf("%v %+v %#v", &auth, &auth, &auth),
	} {
		if strings.Contains(out, testPassword) {
			t.Errorf("the password is in %q", out)
		}
	}

	// A host that is not a loopback IP literal is refused before anything is sent.
	for _, u := range []string{"http://example.com:80", "http://localhost:80", "http://10.0.0.1:80", "https://127.0.0.1:80", "http://127.0.0.1", "http://user@127.0.0.1:80", "http://0.0.0.0:80"} {
		if _, err := NewHTTPClient(u, auth); err == nil {
			t.Errorf("NewHTTPClient accepted %s", u)
		} else if strings.Contains(err.Error(), testPassword) {
			t.Errorf("the refusal of %s holds the password: %v", u, err)
		}
	}
	for _, u := range []string{"http://127.0.0.1:49374", "http://[::1]:49374"} {
		if _, err := NewHTTPClient(u, auth); err != nil {
			t.Errorf("NewHTTPClient refused %s: %v", u, err)
		}
	}
}

func TestTheClientDoesNotFollowARedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect was followed, with credentials: %v", r.Header.Get("Authorization") != "")
	}))
	t.Cleanup(target.Close)
	c := serving(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/info", http.StatusFound)
	})
	if _, err := c.Info(context.Background()); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("Info of a redirect = %v", err)
	}
}

func TestAnUnreachableServerIsADialError(t *testing.T) {
	f := newFake(t)
	c := f.client()
	f.stop()
	if _, err := c.Info(context.Background()); !IsDial(err) {
		t.Fatalf("Info of a server that is down = %v, want a dial error", err)
	}
}

// TestATimeoutIsATransportErrorButNotADial: a server that accepts the connection
// and never answers is a transport failure, and not a failure to connect. The
// difference is the pump's rule for a post: one that never connected sent nothing
// and is started over under a fresh id, while one that got no answer may have been
// admitted and is posted again under the same id.
func TestATimeoutIsATransportErrorButNotADial(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	// Cleanups run last in, first out: the handler is let go before the server
	// waits for it.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	c, err := NewHTTPClient(srv.URL, Auth{}, WithRequestTimeout(80*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"Info":   func() error { _, err := c.Info(context.Background()); return err },
		"Prompt": func() error { return c.Prompt(context.Background(), testSession, testMsgID, "hello") },
	} {
		start := time.Now()
		err := call()
		if !IsTransport(err) || IsDial(err) {
			t.Errorf("%s of a server that never answers = %v, want a transport error that is not a dial", name, err)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("%s took %s against an 80ms timeout", name, time.Since(start))
		}
	}
}

// TestACanceledContextIsNotATransportError: the caller's cancel is the shutdown, and
// says nothing about the server, so it is not a failure to hold for.
func TestACanceledContextIsNotATransportError(t *testing.T) {
	calls := map[string]func(*HTTPClient, context.Context) error{
		"Info":   func(c *HTTPClient, ctx context.Context) error { _, err := c.Info(ctx); return err },
		"Prompt": func(c *HTTPClient, ctx context.Context) error { return c.Prompt(ctx, testSession, testMsgID, "hello") },
	}
	for name, call := range calls {
		// Canceled before the request is made.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := call(newFake(t).client(), ctx); !errors.Is(err, context.Canceled) || IsTransport(err) {
			t.Errorf("%s on a canceled context = %v", name, err)
		}

		// And canceled while the request is in flight.
		release := make(chan struct{})
		started := make(chan struct{})
		var once sync.Once
		c := serving(t, func(http.ResponseWriter, *http.Request) {
			once.Do(func() { close(started) })
			<-release
		})
		t.Cleanup(func() { close(release) })
		ctx, cancel = context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- call(c, ctx) }()
		<-started
		cancel()
		select {
		case err := <-errc:
			if !errors.Is(err, context.Canceled) || IsTransport(err) {
				t.Errorf("%s canceled in flight = %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: a canceled request did not return", name)
		}
	}
}
