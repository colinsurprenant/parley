package opencode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serving starts a server with the given handler, for the answers the fake does
// not give: malformed, oversized, slow, redirecting.
func serving(t *testing.T, h http.HandlerFunc) *HTTPClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewHTTPClient(srv.URL, Auth{}, WithRequestTimeout(2*time.Second))
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

func TestHealth(t *testing.T) {
	f := newFake(t)
	v, err := f.client().Health(context.Background())
	if err != nil || v != "1.18.30" {
		t.Fatalf("Health = %q, %v; want 1.18.30", v, err)
	}
	// A 200 that is not the {healthy, version} object is the answer of a server
	// that is not OpenCode 1's, and says so.
	for name, body := range map[string]string{
		"not json":         `ok`,
		"not an object":    `["healthy"]`,
		"no version":       `{"healthy":true}`,
		"no healthy":       `{"version":"1.18.30"}`,
		"version a number": `{"healthy":true,"version":1}`,
		"two values":       `{"healthy":true,"version":"1.18.30"} {}`,
		"null":             `null`,
	} {
		v, err := serving(t, answering(200, body)).Health(context.Background())
		var us *UnsupportedServerError
		if !errors.As(err, &us) {
			t.Errorf("Health of %s (%s) = %q, %v; want an UnsupportedServerError", name, body, v, err)
		}
	}
	if _, err := serving(t, answering(200, `{"healthy":false,"version":"1.18.30"}`)).Health(context.Background()); err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Errorf("Health of an unhealthy server = %v", err)
	}
	if _, err := serving(t, answering(500, `{"name":"boom"}`)).Health(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("Health of a 500 = %v", err)
	}
}

// TestHealthOfOpenCode2IsARefusalNotADecodeError: on OpenCode 2 the health path
// serves the web page with a 200 (checked live on 2.0.32), a page larger than the
// bound among them, and the front says what that means instead of a JSON error.
func TestHealthOfOpenCode2IsARefusalNotADecodeError(t *testing.T) {
	page := `<!doctype html><html><head><title>OpenCode</title></head><body><div id="root"></div></body></html>`
	for name, h := range map[string]http.HandlerFunc{
		"a page": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(page))
		},
		"a page, JSON claimed": answering(200, page),
		"a big page": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(strings.Repeat("<p>x</p>", 2000)))
		},
		"plain text": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(`{"healthy":true,"version":"1.0.0"}`))
		},
		"no content type": func(w http.ResponseWriter, _ *http.Request) {
			w.Header()["Content-Type"] = nil
			_, _ = w.Write([]byte(`x`))
		},
		"a big JSON body": answering(200, `{"healthy":true,"version":"`+strings.Repeat("x", 5<<10)+`"}`),
	} {
		_, err := serving(t, h).Health(context.Background())
		var us *UnsupportedServerError
		if !errors.As(err, &us) {
			t.Errorf("%s: Health = %v, want an UnsupportedServerError", name, err)
			continue
		}
		for _, want := range []string{"OpenCode 2 is not supported yet", "GET /global/health", "opencode-ai", "opencode.ai/install"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal does not say %q: %v", name, want, err)
			}
		}
		if len(err.Error()) > 600 {
			t.Errorf("%s: the refusal is %d bytes", name, len(err.Error()))
		}
	}
	// Other statuses are not the page: a 404 or a 500 is what it says, and the
	// 401 of a protected server names the variable to set.
	for _, code := range []int{401, 404, 405, 500} {
		_, err := serving(t, answering(code, `x`)).Health(context.Background())
		var us *UnsupportedServerError
		var se *StatusError
		if errors.As(err, &us) || !errors.As(err, &se) || se.Code != code {
			t.Errorf("Health answered %d returned %v, want a StatusError", code, err)
		}
	}
}

func TestSession(t *testing.T) {
	f := newFake(t)
	c := f.client()
	ctx := context.Background()
	info, err := c.Session(ctx, testSession)
	if err != nil || info.ID != testSession || info.Directory != testDir {
		t.Fatalf("Session of a known session = %+v, %v; want its id and directory %s", info, err, testDir)
	}
	if _, err := c.Session(ctx, "ses_000000000000000000000000AA"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Session of an unknown one = %v, want ErrSessionNotFound", err)
	}
	// An answer for another session is not an answer for this one.
	other := serving(t, answering(200, `{"id":"ses_000000000000000000000000AA","directory":"/x"}`))
	if _, err := other.Session(ctx, testSession); err == nil {
		t.Fatal("Session accepted the answer for another session")
	}
	// The directory is what the status is read under: a session without one is
	// one this front cannot watch.
	for name, body := range map[string]string{
		"nothing":          `{}`,
		"a list":           `[]`,
		"id a number":      `{"id":7,"directory":"/x"}`,
		"no directory":     `{"id":"` + testSession + `"}`,
		"empty directory":  `{"id":"` + testSession + `","directory":""}`,
		"directory a list": `{"id":"` + testSession + `","directory":["/x"]}`,
		"not json":         `x`,
	} {
		if info, err := serving(t, answering(200, body)).Session(ctx, testSession); err == nil {
			t.Errorf("Session accepted %s (%s): %+v", name, body, info)
		}
	}
	f.set(func() { f.noDirectory = true })
	if _, err := f.client().Session(ctx, testSession); err == nil || !strings.Contains(err.Error(), "no directory") {
		t.Errorf("Session of one with no directory = %v", err)
	}
}

// TestBusyIsReadUnderTheSessionsDirectory is the scoping OpenCode 1.18.30 has:
// the fake, like the server, lists a turn under the session's directory and
// answers {} to any other, so a client that left the directory out, or asked for
// the server's own, would read a session in the middle of a turn as idle.
func TestBusyIsReadUnderTheSessionsDirectory(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	f.set(func() { f.manualBusy = true })
	c := f.client()
	if busy, err := c.Busy(ctx, testSession, testDir); err != nil || !busy {
		t.Fatalf("Busy under the session's directory = %v, %v; want busy", busy, err)
	}
	for _, other := range []string{"/srv/where-the-server-runs", testDir + "/sub"} {
		if busy, err := c.Busy(ctx, testSession, other); err != nil || busy {
			t.Fatalf("the fake listed the turn under %s: %v, %v", other, busy, err)
		}
	}
	reads := len(f.statusDirsSeen())
	if _, err := c.Busy(ctx, testSession, ""); err == nil {
		t.Fatal("Busy with no directory was accepted")
	}
	if n := len(f.statusDirsSeen()); n != reads {
		t.Fatalf("Busy with no directory made a request anyway")
	}
	// A directory with characters a query string treats as its own arrives whole.
	odd := "/work/my checkout&x=1/é"
	seen := ""
	srv := serving(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("directory")
		answering(200, `{}`)(w, r)
	})
	if _, err := srv.Busy(ctx, testSession, odd); err != nil || seen != odd {
		t.Fatalf("the server saw directory %q (err %v), want %q", seen, err, odd)
	}
}

func TestBusy(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		body string
		busy bool
	}{
		"nobody is busy":         {`{}`, false},
		"another session busy":   {`{"ses_000000000000000000000000AA":{"type":"busy"}}`, false},
		"listed idle":            {`{"` + testSession + `":{"type":"idle"}}`, false},
		"busy":                   {`{"` + testSession + `":{"type":"busy"}}`, true},
		"retry":                  {`{"` + testSession + `":{"type":"retry","attempt":2,"message":"x","next":9}}`, true},
		"a type from the future": {`{"` + testSession + `":{"type":"compacting"}}`, true},
	} {
		got, err := serving(t, answering(200, tc.body)).Busy(ctx, testSession, testDir)
		if err != nil || got != tc.busy {
			t.Errorf("%s: Busy = %v, %v; want %v", name, got, err, tc.busy)
		}
	}
	for name, body := range map[string]string{
		"a list":        `[]`,
		"null":          `null`,
		"null status":   `{"` + testSession + `":null}`,
		"no type":       `{"` + testSession + `":{}}`,
		"empty type":    `{"` + testSession + `":{"type":""}}`,
		"type a number": `{"` + testSession + `":{"type":1}}`,
		"not json":      `busy`,
	} {
		if got, err := serving(t, answering(200, body)).Busy(ctx, testSession, testDir); err == nil {
			t.Errorf("Busy accepted %s (%s): %v", name, body, got)
		}
	}
}

func TestMessages(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	f.seed("user", "one")
	f.seed("user", "two")
	f.seed("assistant", "three")
	c := f.client()

	got, err := c.Messages(ctx, testSession, 2)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	// The newest two, oldest first.
	if len(got) != 2 || got[0].Role != "user" || got[1].Role != "assistant" || got[0].ID >= got[1].ID {
		t.Fatalf("Messages(2) = %+v, want the newest two in order", got)
	}
	if got[0].Parts[0].Type != "text" || got[0].Parts[0].Text != "two" || got[1].Parts[0].Type != "step-start" || got[1].Parts[1].Text != "three" {
		t.Fatalf("parts = %+v", got)
	}
	f.mu.Lock()
	lastLimit := f.limits[len(f.limits)-1]
	f.mu.Unlock()
	if lastLimit != "2" {
		t.Fatalf("the request carried limit=%q, want 2", lastLimit)
	}

	for _, n := range []int{0, -1} {
		if _, err := c.Messages(ctx, testSession, n); err == nil {
			t.Errorf("Messages accepted limit %d, which OpenCode reads as no limit", n)
		}
	}
	if _, err := c.Messages(ctx, "ses_000000000000000000000000AA", 1); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Messages of an unknown session = %v", err)
	}

	empty := serving(t, answering(200, `[]`))
	if msgs, err := empty.Messages(ctx, testSession, 1); err != nil || len(msgs) != 0 {
		t.Errorf("Messages of an empty history = %v, %v", msgs, err)
	}
	for name, body := range map[string]string{
		"an object":         `{}`,
		"null":              `null`,
		"a null entry":      `[null]`,
		"no info":           `[{"parts":[]}]`,
		"no id":             `[{"info":{"role":"user"},"parts":[]}]`,
		"id a number":       `[{"info":{"id":3,"role":"user"},"parts":[]}]`,
		"empty id":          `[{"info":{"id":"","role":"user"},"parts":[]}]`,
		"no role":           `[{"info":{"id":"m"},"parts":[]}]`,
		"parts an object":   `[{"info":{"id":"m","role":"user"},"parts":{}}]`,
		"part without type": `[{"info":{"id":"m","role":"user"},"parts":[{"text":"x"}]}]`,
		"text a number":     `[{"info":{"id":"m","role":"user"},"parts":[{"type":"text","text":5}]}]`,
		"trailing garbage":  `[] x`,
	} {
		if msgs, err := serving(t, answering(200, body)).Messages(ctx, testSession, 1); err == nil {
			t.Errorf("Messages accepted %s (%s): %+v", name, body, msgs)
		}
	}
	// A message with no parts at all, and a part with no text, are well formed.
	ok := serving(t, answering(200, `[{"info":{"id":"m","role":"assistant","parentID":"p","extra":{"a":1}}},{"info":{"id":"n","role":"user"},"parts":[{"type":"file","url":"x"}]}]`))
	msgs, err := ok.Messages(ctx, testSession, 2)
	if err != nil || len(msgs) != 2 || msgs[0].ParentID != "p" || msgs[1].Parts[0].Type != "file" || msgs[1].Parts[0].Text != "" {
		t.Errorf("Messages of unusual but well formed entries = %+v, %v", msgs, err)
	}
}

func TestPromptSendsExactlyOneTextPart(t *testing.T) {
	f := newFake(t)
	c := f.client()
	text := "[parley] from=claude-b id=0 ts=t — x\n  <b>&amp;</b> \"q\"   😀\n\n"
	if err := c.Prompt(context.Background(), testSession, text); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	posts := f.postBodies()
	if len(posts) != 1 {
		t.Fatalf("%d posts, want 1", len(posts))
	}
	if got := postedText(t, posts[0].Body); got != text {
		t.Fatalf("the server decoded\n%q\nwant\n%q", got, text)
	}
	if users := f.userTexts(); len(users) != 1 || users[0] != text {
		t.Fatalf("the session holds %q, want the text byte for byte", users)
	}
}

func TestPromptAnswers(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	if err := f.client().Prompt(ctx, "ses_000000000000000000000000AA", "x"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Prompt into an unknown session = %v, want ErrSessionNotFound", err)
	}
	// Only 204 is acceptance: a 200 is a server that did something else.
	for _, code := range []int{200, 202, 400, 401, 403, 500, 302} {
		err := serving(t, answering(code, `{"name":"x"}`)).Prompt(ctx, testSession, "x")
		var se *StatusError
		if !errors.As(err, &se) || se.Code != code {
			t.Errorf("Prompt answered %d returned %v, want a StatusError", code, err)
		}
	}
	// Invalid UTF-8 would be silently replaced on its way into JSON, and what
	// landed would not be what was sent.
	called := false
	c := serving(t, func(http.ResponseWriter, *http.Request) { called = true })
	if err := c.Prompt(ctx, testSession, "bad \xff byte"); err == nil || called {
		t.Errorf("Prompt of invalid UTF-8 = %v (request made: %v)", err, called)
	}
}

func TestBasicAuth(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	f.set(func() { f.password = "s3cret-pw" })

	good, err := NewHTTPClient(f.url(), Auth{User: "opencode", Password: "s3cret-pw"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := good.Health(ctx); err != nil {
		t.Fatalf("Health with the right credentials: %v", err)
	}
	if err := good.Prompt(ctx, testSession, "x"); err != nil {
		t.Fatalf("Prompt with the right credentials: %v", err)
	}

	for name, a := range map[string]Auth{
		"no credentials": {},
		"wrong password": {User: "opencode", Password: "nope"},
		"wrong user":     {User: "bob", Password: "s3cret-pw"},
	} {
		c, err := NewHTTPClient(f.url(), a)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Health(ctx)
		var se *StatusError
		if !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
			t.Errorf("%s: Health = %v, want a 401", name, err)
			continue
		}
		if !strings.Contains(err.Error(), "OPENCODE_SERVER_PASSWORD") || strings.Contains(err.Error(), "s3cret-pw") {
			t.Errorf("%s: the 401 should say which variable to set and never quote a password: %v", name, err)
		}
	}
}

func TestResponseBodiesAreBounded(t *testing.T) {
	// Within the bound, a long answer is still read: a key of 5 KiB is no limit.
	long := strings.Repeat("x", 5<<10)
	c := serving(t, answering(200, `{"`+long+`":{"type":"busy"}}`))
	if busy, err := c.Busy(context.Background(), testSession, testDir); err != nil || busy {
		t.Fatalf("Busy of a %d byte answer = %v, %v", len(long), busy, err)
	}
	huge := strings.Repeat("x", maxStatusBody+1)
	c = serving(t, answering(200, `{"`+huge+`":{"type":"busy"}}`))
	if _, err := c.Busy(context.Background(), testSession, testDir); err == nil || !strings.Contains(err.Error(), "longer than") || !IsTooLong(err) {
		t.Fatalf("Busy of a %d byte answer = %v, want the bound", len(huge), err)
	}
	// An error body is quoted bounded and printable.
	c = serving(t, answering(500, strings.Repeat("e\n", 2000)+"\x00\xff"))
	_, err := c.Health(context.Background())
	if err == nil || len(err.Error()) > 600 || strings.ContainsAny(err.Error(), "\x00\xff\n") {
		t.Fatalf("a 500 with a large body = %q", err)
	}
}

// TestAHistoryReplyOverTheBoundIsATooLongError: the history read is the one that
// can be heavy, and the pump asks again for less when it is, so the over-the-bound
// answer is a type of its own that carries the bound.
func TestAHistoryReplyOverTheBoundIsATooLongError(t *testing.T) {
	c := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("["))
		chunk := []byte(strings.Repeat(" ", 1<<20))
		for i := 0; i < maxMessageBody>>20+1; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte("]"))
	})
	_, err := c.Messages(context.Background(), testSession, 10)
	var tl *TooLongError
	if !errors.As(err, &tl) || !IsTooLong(err) || tl.Limit != maxMessageBody || IsTransport(err) {
		t.Fatalf("Messages of a reply over the bound = %v, want a TooLongError for %d", err, maxMessageBody)
	}
	// It is not a general-purpose error: a reply that fits is read as before.
	if msgs, err := serving(t, answering(200, `[]`)).Messages(context.Background(), testSession, 10); err != nil || len(msgs) != 0 {
		t.Fatalf("Messages of an empty history = %v, %v", msgs, err)
	}
}

func TestTheInstallHintSaysWhenHomebrewMoved(t *testing.T) {
	for _, want := range []string{"npm package opencode-ai", "opencode.ai/install", "Homebrew core moved to OpenCode 2 on 2026-09-28"} {
		if !strings.Contains(Install1Hint, want) {
			t.Errorf("the install hint does not say %q: %s", want, Install1Hint)
		}
	}
	if strings.Contains(Install1Hint, "may be") {
		t.Errorf("the install hint still hedges: %s", Install1Hint)
	}
}

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
	start := time.Now()
	_, err = c.Health(context.Background())
	if !IsTransport(err) || IsDial(err) {
		t.Fatalf("Health of a server that never answers = %v, want a transport error that is not a dial", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the request took %s against an 80ms timeout", time.Since(start))
	}
}

func TestAServerThatIsDownIsADialError(t *testing.T) {
	f := newFake(t)
	c := f.client()
	f.stop()
	_, err := c.Health(context.Background())
	if !IsDial(err) || !IsTransport(err) {
		t.Fatalf("Health of a closed port = %v, want a dial error", err)
	}
}

func TestACanceledContextIsNotATransportError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := newFake(t)
	if _, err := f.client().Health(ctx); !errors.Is(err, context.Canceled) || IsTransport(err) {
		t.Fatalf("Health on a canceled context = %v", err)
	}
	// And one canceled while the request is in flight.
	release := make(chan struct{})
	started := make(chan struct{})
	c := serving(t, func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})
	t.Cleanup(func() { close(release) })
	ctx, cancel = context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := c.Health(ctx); errc <- err }()
	<-started
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) || IsTransport(err) {
			t.Fatalf("Health canceled in flight = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a canceled request did not return")
	}
}

func TestARedirectIsNotFollowed(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the redirect was followed")
	}))
	t.Cleanup(target.Close)
	c := serving(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	_, err := c.Health(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusTemporaryRedirect {
		t.Fatalf("Health of a redirect = %v, want a StatusError", err)
	}
}

func TestNewHTTPClientOnlyEverTalksToLoopback(t *testing.T) {
	for _, ok := range []string{"http://127.0.0.1:4096", "http://127.0.0.1:4096/", "http://[::1]:4096", "http://127.0.0.2:80", LoopbackURL(4096)} {
		if _, err := NewHTTPClient(ok, Auth{User: "u", Password: "p"}); err != nil {
			t.Errorf("NewHTTPClient(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "127.0.0.1:4096", "https://127.0.0.1:4096", "http://localhost:4096", "http://example.com:4096",
		"http://10.0.0.5:4096", "http://0.0.0.0:4096", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:70000",
		"http://user:pw@127.0.0.1:4096", "http://127.0.0.1:4096/api", "http://127.0.0.1:4096?x=1", "http://127.0.0.1:4096#f",
		"ftp://127.0.0.1:4096",
	} {
		if _, err := NewHTTPClient(bad, Auth{User: "u", Password: "p"}); err == nil {
			t.Errorf("NewHTTPClient(%q) was accepted", bad)
		}
	}
	if _, err := NewHTTPClient(LoopbackURL(4096), Auth{}, WithRequestTimeout(0)); err == nil {
		t.Error("a zero request timeout was accepted")
	}
	if got := LoopbackURL(4096); got != "http://127.0.0.1:4096" {
		t.Errorf("LoopbackURL = %q", got)
	}
}
