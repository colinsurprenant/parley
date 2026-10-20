package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testMsgID = "msg_157612985001ZP97n73ncfxOnr"
	// secret2 is a password that must not show up in anything the client says.
	secret2 = "p4ssw0rd-that-must-stay-secret"
)

// serving2 starts a server with the given handler and returns a client for it.
func serving2(t *testing.T, h http.HandlerFunc) *HTTPClient2 {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewHTTPClient2(srv.URL, Auth{User: ServiceUser, Password: secret2}, WithRequestTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewHTTPClient2: %v", err)
	}
	return c
}

func TestInfo(t *testing.T) {
	f := newFake2(t)
	info, err := f.client().Info(context.Background())
	if err != nil || info.Version != "2.0.32" || info.PID != fake2PID {
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
		"1.x health":    `{"healthy":true,"version":"1.18.30"}`,
	} {
		if info, err := serving2(t, answering(200, body)).Info(context.Background()); err == nil {
			t.Errorf("Info of %s (%s) = %+v, want an error", name, body, info)
		}
	}
	if _, err := serving2(t, answering(404, `{}`)).Info(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("Info of a 404 = %v", err)
	}
}

func TestSession2(t *testing.T) {
	f := newFake2(t)
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
		"1.x shape":       `{"id":"` + testSession + `","directory":"/x"}`,
	} {
		if s, err := serving2(t, answering(200, body)).Session(ctx, testSession); err == nil {
			t.Errorf("Session of %s = %+v, want an error", name, s)
		}
	}
	// A 404 that is not the session's is not "no such session".
	if _, err := serving2(t, answering(404, `{"_tag":"LocationNotFoundError"}`)).Session(ctx, testSession); err == nil || errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Session of another 404 = %v", err)
	}
}

func TestBusy2(t *testing.T) {
	f := newFake2(t)
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
	if busy, err := serving2(t, answering(200, other)).Busy(ctx, testSession); err != nil || busy {
		t.Fatalf("Busy with another session running = %v, %v", busy, err)
	}
	// A type this front has never seen is still a turn in progress.
	if busy, err := serving2(t, answering(200, `{"data":{"`+testSession+`":{"type":"compacting"}}}`)).Busy(ctx, testSession); err != nil || !busy {
		t.Fatalf("Busy of an unknown type = %v, %v", busy, err)
	}
	for name, body := range map[string]string{
		"no data":          `{}`,
		"a list":           `{"data":[]}`,
		"an entry no type": `{"data":{"` + testSession + `":{}}}`,
		"a null entry":     `{"data":{"` + testSession + `":null}}`,
		"not json":         `x`,
	} {
		if busy, err := serving2(t, answering(200, body)).Busy(ctx, testSession); err == nil {
			t.Errorf("Busy of %s = %v, want an error", name, busy)
		}
	}
}

func TestPrompt2SendsTheThreeFieldsAndChecksTheAnswer(t *testing.T) {
	f := newFake2(t)
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
	if err := f.client().Prompt(ctx, testSession, testMsgID, text+"more"); err == nil || !strings.Contains(err.Error(), "other text") {
		t.Fatalf("Prompt of another text under the same id = %v", err)
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
	if err := serving2(t, answering(409, `{"_tag":"ConflictError"}`)).Prompt(ctx, testSession, testMsgID, "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Prompt answered 409 = %v", err)
	}
	if err := serving2(t, answering(400, `{"_tag":"InvalidRequestError"}`)).Prompt(ctx, testSession, testMsgID, "x"); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("Prompt answered 400 = %v", err)
	}
	for name, body := range map[string]string{
		"another id":      `{"data":{"id":"msg_other","sessionID":"` + testSession + `","type":"user","payload":{"text":"x"},"delivery":"queue"}}`,
		"another session": `{"data":{"id":"` + testMsgID + `","sessionID":"ses_other","type":"user","payload":{"text":"x"},"delivery":"queue"}}`,
		"not a user":      `{"data":{"id":"` + testMsgID + `","sessionID":"` + testSession + `","type":"synthetic","payload":{"text":"x"},"delivery":"queue"}}`,
		"no payload":      `{"data":{"id":"` + testMsgID + `","sessionID":"` + testSession + `","type":"user"}}`,
		"no data":         `{}`,
	} {
		if err := serving2(t, answering(200, body)).Prompt(ctx, testSession, testMsgID, "x"); err == nil {
			t.Errorf("Prompt answered with %s was accepted", name)
		}
	}
}

func TestMessage2(t *testing.T) {
	f := newFake2(t)
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
		if m, _, err := serving2(t, answering(200, body)).Message(ctx, testSession, testMsgID); err == nil {
			t.Errorf("Message of %s = %+v, want an error", name, m)
		}
	}
	// A 404 that names no message is not "not there yet".
	if _, found, err := serving2(t, answering(404, `{}`)).Message(ctx, testSession, testMsgID); err == nil || found {
		t.Errorf("Message of an untagged 404 = %v, %v", found, err)
	}
	// An answer over the bound is an error, not a buffer of the whole.
	big := `{"data":{"id":"` + testMsgID + `","type":"user","text":"` + strings.Repeat("x", maxMessageBody) + `"}}`
	if _, _, err := serving2(t, answering(200, big)).Message(ctx, testSession, testMsgID); !IsTooLong(err) {
		t.Errorf("Message over the bound = %v, want a TooLongError", err)
	}
}

func TestInbox2AndSteer(t *testing.T) {
	f := newFake2(t)
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
	if err := f.client().Steer(ctx, testSession, testMsgID); err != nil {
		t.Fatalf("Steer = %v", err)
	}
	if got := f.patched(); len(got) != 1 || got[0] != testMsgID+` {"delivery":"steer"}` {
		t.Fatalf("patches %q", got)
	}
	// Nothing is left to steer once it is taken up, and the server says so.
	if err := f.client().Steer(ctx, testSession, testMsgID); !errors.Is(err, ErrConflict) {
		t.Fatalf("Steer of an entry that is no longer queued = %v, want ErrConflict", err)
	}
	if err := f.client().Steer(ctx, "ses_000000000000000000000000AA", testMsgID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Steer in an unknown session = %v", err)
	}
	for name, body := range map[string]string{
		"no data":              `{}`,
		"an entry no id":       `{"data":[{"delivery":"queue"}]}`,
		"an entry no delivery": `{"data":[{"id":"x"}]}`,
		"a null entry":         `{"data":[null]}`,
		"an object":            `{"data":{}}`,
	} {
		if items, err := serving2(t, answering(200, body)).Inbox(ctx, testSession); err == nil {
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
	c := serving2(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"_tag":"UnauthorizedError","message":"Authentication required"}`)
	})
	_, err := c.Info(context.Background())
	if !ok || user != ServiceUser || pass != secret2 {
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
	if strings.Contains(err.Error(), "OPENCODE_SERVER_USERNAME") {
		t.Errorf("the 401 gives OpenCode 1's advice: %v", err)
	}
	auth := Auth{User: ServiceUser, Password: secret2}
	for _, out := range []string{
		err.Error(),
		fmt.Sprintf("%v %+v %#v %s", auth, auth, auth, auth),
		fmt.Sprintf("%v %+v %#v", &auth, &auth, &auth),
	} {
		if strings.Contains(out, secret2) {
			t.Errorf("the password is in %q", out)
		}
	}

	// A host that is not a loopback IP literal is refused before anything is sent.
	for _, u := range []string{"http://example.com:80", "http://localhost:80", "http://10.0.0.1:80", "https://127.0.0.1:80", "http://127.0.0.1", "http://user@127.0.0.1:80", "http://0.0.0.0:80"} {
		if _, err := NewHTTPClient2(u, auth); err == nil {
			t.Errorf("NewHTTPClient2 accepted %s", u)
		} else if strings.Contains(err.Error(), secret2) {
			t.Errorf("the refusal of %s holds the password: %v", u, err)
		}
	}
	for _, u := range []string{"http://127.0.0.1:49374", "http://[::1]:49374"} {
		if _, err := NewHTTPClient2(u, auth); err != nil {
			t.Errorf("NewHTTPClient2 refused %s: %v", u, err)
		}
	}
}

func TestTheClientDoesNotFollowARedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect was followed, with credentials: %v", r.Header.Get("Authorization") != "")
	}))
	t.Cleanup(target.Close)
	c := serving2(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/info", http.StatusFound)
	})
	if _, err := c.Info(context.Background()); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("Info of a redirect = %v", err)
	}
}

func TestAnUnreachableServerIsADialError2(t *testing.T) {
	f := newFake2(t)
	c := f.client()
	f.stop()
	if _, err := c.Info(context.Background()); !IsDial(err) {
		t.Fatalf("Info of a server that is down = %v, want a dial error", err)
	}
}

func TestCheckVersion2(t *testing.T) {
	for _, v := range []string{"2.0.32", "2.0.0", "2", "2.1.0-rc.1"} {
		if err := CheckVersion2(v); err != nil {
			t.Errorf("CheckVersion2(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"1.18.30", "3.0.0", "0.9.1"} {
		if err := CheckVersion2(v); err == nil || !strings.Contains(err.Error(), "not OpenCode 2") {
			t.Errorf("CheckVersion2(%q) = %v, want the refusal", v, err)
		}
	}
	for _, v := range []string{"", "latest", "v2.0.32", "opencode v2.0.32", "+2.0.0"} {
		if err := CheckVersion2(v); err == nil || !strings.Contains(err.Error(), "cannot read an OpenCode version") {
			t.Errorf("CheckVersion2(%q) = %v, want the unreadable-version refusal", v, err)
		}
	}
	if err := CheckVersion2(strings.Repeat("x", 10_000)); err == nil || len(err.Error()) > 500 {
		t.Errorf("CheckVersion2 of a long string = %v", err)
	}
}

func TestProbe2(t *testing.T) {
	f := newFake2(t)
	ctx := context.Background()
	if info, err := Probe2(ctx, f.client(), testSession); err != nil || info.Version != "2.0.32" || info.PID != fake2PID {
		t.Fatalf("Probe2 = %+v, %v", info, err)
	}
	if _, err := Probe2(ctx, f.client(), "ses_000000000000000000000000AA"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Probe2 of an unknown session = %v, want ErrSessionNotFound", err)
	}
	f.set(func() { f.version = "1.18.30" })
	if _, err := Probe2(ctx, f.client(), testSession); err == nil || !strings.Contains(err.Error(), "not OpenCode 2") {
		t.Fatalf("Probe2 of another version = %v", err)
	}
	f.stop()
	if _, err := Probe2(ctx, f.client(), testSession); err == nil {
		t.Fatalf("Probe2 of a server that is down succeeded")
	}
}
