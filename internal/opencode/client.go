package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Message is one entry of a session's history as this front reads it: who wrote
// it, which user message an assistant one answers, and the parts it carries.
// Only what the pump decides on is kept; OpenCode sends a great deal more.
type Message struct {
	ID   string
	Role string // "user" or "assistant"
	// ParentID is the user message an assistant message answers, and empty for
	// a user message.
	ParentID string
	Parts    []Part
}

// Part is one part of a message. Text is the part's text where it has any, and
// only a part of Type "text" is read as text by anything in this package.
type Part struct {
	Type string
	Text string
}

// Client is the HTTP seam of the front: everything the pump asks of an OpenCode
// server, and nothing else. HTTPClient is the real one for OpenCode 1's API; the
// tests swap in their own. OpenCode 2's different API is Client2's.
//
// An error from a method is one of: ErrSessionNotFound; a *TransportError, for a
// request that got no answer (IsDial says that it never reached the server at
// all); a *StatusError, for an answer that was not the expected one; or an
// error that says the answer was malformed. A canceled context is returned as
// the context's own error and is never a *TransportError.
type Client interface {
	// Health returns the server's version string.
	Health(ctx context.Context) (version string, err error)
	// Session returns what the server knows of the session, or
	// ErrSessionNotFound if it does not know it.
	Session(ctx context.Context, session string) (SessionInfo, error)
	// Busy reports whether the session is in the middle of a turn. A session
	// the server does not list is idle: OpenCode lists the sessions that are not.
	// directory is the session's own directory, as Session reported it, and is
	// required: OpenCode keeps a session's status under the directory the session
	// belongs to, and a status read for any other directory, the one the server
	// itself runs in included, lists nothing and so reads every session idle.
	Busy(ctx context.Context, session, directory string) (bool, error)
	// Messages returns one page of the session's history: the newest limit
	// messages of it, oldest first, or with a before cursor, which only a
	// MessagePage.Next is, the limit messages just older than the page that
	// cursor came with. limit must be positive: OpenCode reads zero as "all of
	// them".
	Messages(ctx context.Context, session string, limit int, before string) (MessagePage, error)
	// Prompt appends text to the session as one user message and starts a turn
	// on it, returning once the server has accepted it. Acceptance is not
	// arrival: the message lands in the history a moment later.
	Prompt(ctx context.Context, session, text string) error
}

// MessagePage is one page of a session's history, oldest message first. Next is
// the cursor of the page before it, to be passed as before, and is empty when
// there is nothing older.
type MessagePage struct {
	Messages []Message
	Next     string
}

// SessionInfo is the part of a session the front needs.
type SessionInfo struct {
	ID string
	// Directory is the absolute directory the session belongs to, the checkout
	// it was started in. It need not be the directory the server runs in: a TUI
	// can be started in one directory on a session of another.
	Directory string
}

// ErrSessionNotFound is the server's answer, a 404, for a session it does not
// have. It is wrapped with the id; test for it with errors.Is.
var ErrSessionNotFound = errors.New("opencode: session not found")

// TransportError is a request that got no answer: the connection could not be
// made, was dropped, or timed out. Dial says it never got as far as a
// connection, so the server saw nothing of it and a retry cannot duplicate
// anything; for any other transport failure of a POST, what the server did with
// the request is not known.
type TransportError struct {
	Op   string // the request, e.g. "GET /session/status"
	Err  error
	Dial bool
}

func (e *TransportError) Error() string { return fmt.Sprintf("opencode: %s: %v", e.Op, e.Err) }
func (e *TransportError) Unwrap() error { return e.Err }

// IsTransport reports whether err is a request that got no answer.
func IsTransport(err error) bool {
	var t *TransportError
	return errors.As(err, &t)
}

// IsDial reports whether err is a request that never reached the server.
func IsDial(err error) bool {
	var t *TransportError
	return errors.As(err, &t) && t.Dial
}

// StatusError is an answer with a status other than the one the request
// expects. Body is a bounded, printable excerpt of what came with it.
type StatusError struct {
	Op     string
	Code   int
	Status string
	Body   string
	// Hint replaces the advice that follows a 401, which is OpenCode 1's.
	Hint string
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("opencode: %s: %s", e.Op, e.Status)
	if e.Body != "" {
		s += ": " + e.Body
	}
	if e.Code == http.StatusUnauthorized {
		if e.Hint != "" {
			s += " (" + e.Hint + ")"
		} else {
			s += " (the server wants the password it was started with: set OPENCODE_SERVER_PASSWORD, and OPENCODE_SERVER_USERNAME if it is not opencode)"
		}
	}
	return s
}

// Install1Hint, Install2Hint and InstallHint say how to get an OpenCode this
// front speaks to: either version. A package manager may hand out the other one
// than the human meant, and Homebrew core did, from 2026-09-28: it moved to
// OpenCode 2.
const (
	Install1Hint = "OpenCode 1.x: install the npm package opencode-ai, or use the installer at opencode.ai/install (Homebrew core moved to OpenCode 2 on 2026-09-28)"
	Install2Hint = "OpenCode 2: use the installer at opencode.ai/v2/install, or Homebrew core"
	InstallHint  = "install OpenCode, " + Install1Hint + "; " + Install2Hint
)

// UnsupportedServerError is a server that answered the health check as something
// other than OpenCode 1 does. OpenCode 2 serves its web page with a 200 at the
// paths OpenCode 1 serves JSON at, so the answer that gives it away is a page, or
// any body that is not the {healthy, version} object.
type UnsupportedServerError struct {
	Op  string
	Why string
}

func (e *UnsupportedServerError) Error() string {
	return fmt.Sprintf("%s was not answered as OpenCode 1's server answers it (%s): this is not the OpenCode 1 server this client speaks to; if it is OpenCode 2, the pump finds its protocol when it starts, so start it again", e.Op, e.Why)
}

// Auth is the HTTP basic credentials OpenCode's server takes: OpenCode 1's when
// it is started with OPENCODE_SERVER_PASSWORD, and OpenCode 2's always. An empty
// Password sends none. It never prints its password, whatever verb formats it.
type Auth struct {
	User     string
	Password string
}

func (a Auth) String() string {
	return "opencode.Auth{User:" + strconv.Quote(a.User) + ", Password:<redacted>}"
}
func (a Auth) GoString() string { return a.String() }

const (
	// DefaultRequestTimeout bounds one request, response body included. The
	// server is the human's own on loopback: a request that has not been answered
	// in this long is a server that is not coming back, and the pump must not be
	// held by it.
	DefaultRequestTimeout = 10 * time.Second

	// dialTimeout bounds the connect alone. A loopback connect that is not
	// refused at once is not going to succeed.
	dialTimeout = 5 * time.Second

	// The bounds on a response body, per endpoint. Each is far above what its
	// endpoint sends and exists so that a server that floods a reply is not
	// buffered whole. A message reply is the large one: it can carry a tool's
	// whole output in every message of the window.
	maxHealthBody  = 4 << 10
	maxSessionBody = 1 << 20
	maxStatusBody  = 1 << 20
	maxMessageBody = 16 << 20
	maxPromptBody  = 64 << 10
	maxErrorBody   = 4 << 10

	// errorExcerpt is how much of an error body is quoted.
	errorExcerpt = 200
)

// HTTPClient speaks OpenCode 1's HTTP API (1.18.30 is the version it was
// verified against) to a server on loopback.
type HTTPClient struct {
	base    string
	auth    Auth
	hc      *http.Client
	timeout time.Duration
}

// Option configures an HTTPClient.
type Option func(*HTTPClient)

// WithRequestTimeout replaces DefaultRequestTimeout.
func WithRequestTimeout(d time.Duration) Option {
	return func(c *HTTPClient) { c.timeout = d }
}

// LoopbackURL is the base URL of the server a TUI started with --port serves:
// OpenCode's default hostname, which is loopback.
func LoopbackURL(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }

// NewHTTPClient returns a client for the server at baseURL, which must be an
// http URL whose host is a loopback IP address and port, with no path, query or
// credentials. The check is the front's whole trust boundary on this side: the
// basic credentials go over plaintext, so they are only ever sent to a loopback
// address, and a name such as localhost, which a resolver could point anywhere,
// is refused for the same reason `parley server --listen` refuses it.
func NewHTTPClient(baseURL string, auth Auth, opts ...Option) (*HTTPClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("opencode: server URL: %w", err)
	}
	if u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("opencode: server URL %q must be http://<loopback ip>:<port> and nothing else", baseURL)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return nil, fmt.Errorf("opencode: server URL %q has no port: %w", baseURL, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("opencode: server URL %q is not a loopback IP address", baseURL)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("opencode: server URL %q has no valid port", baseURL)
	}
	c := &HTTPClient{
		base:    "http://" + u.Host,
		auth:    auth,
		timeout: DefaultRequestTimeout,
	}
	for _, o := range opts {
		o(c)
	}
	if c.timeout <= 0 {
		return nil, errors.New("opencode: request timeout must be positive")
	}
	c.hc = &http.Client{
		Transport: &http.Transport{
			// No proxy: this is a loopback server and the credentials must not
			// be handed to anything on the way to it. No keep-alive: a pump makes
			// a few requests a minute at most, and a connection kept across a
			// restart of the TUI would only fail on its first reuse.
			Proxy:             nil,
			DialContext:       (&net.Dialer{Timeout: dialTimeout}).DialContext,
			DisableKeepAlives: true,
		},
		// A redirect is not part of this API, and following one would send the
		// credentials wherever it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c, nil
}

// reply is what came back from one request: the status, the body read up to the
// bound, and the media type the server gave it. tooLong says the body went over
// the bound, in which case it is not kept.
type reply struct {
	code        int
	body        []byte
	contentType string
	nextCursor  string
	tooLong     bool
}

// do makes one request and returns the status and the bounded body. It does not
// judge the status: each method knows which one it expects. A body over the
// bound is an error.
func (c *HTTPClient) do(ctx context.Context, method, path string, query url.Values, body []byte, limit int64) (int, []byte, error) {
	r, err := c.exchange(ctx, method, path, query, body, limit)
	if err != nil {
		return 0, nil, err
	}
	if r.tooLong {
		return 0, nil, &TooLongError{Op: method + " " + path, Limit: limit}
	}
	return r.code, r.body, nil
}

// TooLongError is an answer longer than the bound this front reads it under. It
// is its own type because the one caller that can do something about it, the
// history read, asks again for less.
type TooLongError struct {
	Op    string
	Limit int64
}

func (e *TooLongError) Error() string {
	return fmt.Sprintf("opencode: %s: response is longer than the %d bytes this front reads", e.Op, e.Limit)
}

// IsTooLong reports whether err is an answer over the bound.
func IsTooLong(err error) bool {
	var t *TooLongError
	return errors.As(err, &t)
}

// exchange is do with the reply whole, for the caller that reads more than the
// status and the body into its decision.
func (c *HTTPClient) exchange(ctx context.Context, method, path string, query url.Values, body []byte, limit int64) (reply, error) {
	op := method + " " + path
	if err := ctx.Err(); err != nil {
		return reply{}, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, target, rd)
	if err != nil {
		return reply{}, fmt.Errorf("opencode: %s: %w", op, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.auth.Password != "" {
		req.SetBasicAuth(c.auth.User, c.auth.Password)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// The caller's own cancel is the shutdown path and says nothing about
		// the server; the per-request deadline is the server not answering.
		if cerr := ctx.Err(); cerr != nil {
			return reply{}, cerr
		}
		return reply{}, &TransportError{Op: op, Err: err, Dial: isDial(err)}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return reply{}, cerr
		}
		return reply{}, &TransportError{Op: op, Err: fmt.Errorf("read the response: %w", err)}
	}
	r := reply{code: resp.StatusCode, body: b, nextCursor: resp.Header.Get("X-Next-Cursor")}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil {
		r.contentType = mt
	}
	if int64(len(b)) > limit {
		r.body, r.tooLong = nil, true
	}
	return r, nil
}

// isDial reports whether err is the failure to connect, as distinct from a
// failure on a connection that was made.
func isDial(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// statusError builds the error for an answer that was not the expected one, with
// a bounded printable excerpt of its body.
func statusError(op string, code int, body []byte) error {
	if len(body) > maxErrorBody {
		body = body[:maxErrorBody]
	}
	text := strings.ToValidUTF8(strings.Join(strings.Fields(string(body)), " "), "?")
	if r := []rune(text); len(r) > errorExcerpt {
		text = string(r[:errorExcerpt]) + "…"
	}
	return &StatusError{Op: op, Code: code, Status: strconv.Itoa(code) + " " + http.StatusText(code), Body: text}
}

// decode reads body as exactly one JSON value of v's shape. A trailing value or
// stray bytes are an error, as is a value of the wrong type.
func decode(op string, body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("opencode: %s: the answer is not the shape this front reads: %w", op, err)
	}
	return nil
}

// Health implements Client: GET /global/health, which answers
// {"healthy":true,"version":"1.18.30"} as JSON on OpenCode 1. OpenCode 2 answers
// 200 with its web page there, so a 200 that is not JSON of that shape, whatever
// its content type claims, is not an error in decoding but the answer of a server
// this front does not speak to, and is reported as *UnsupportedServerError.
func (c *HTTPClient) Health(ctx context.Context) (string, error) {
	const op = "GET /global/health"
	r, err := c.exchange(ctx, http.MethodGet, "/global/health", nil, nil, maxHealthBody)
	if err != nil {
		return "", err
	}
	if r.code != http.StatusOK {
		return "", statusError(op, r.code, r.body)
	}
	if r.contentType != "application/json" {
		return "", &UnsupportedServerError{Op: op, Why: "content type " + quoteBounded(r.contentType)}
	}
	if r.tooLong {
		return "", &UnsupportedServerError{Op: op, Why: fmt.Sprintf("a body of more than %d bytes", maxHealthBody)}
	}
	var h struct {
		Healthy *bool   `json:"healthy"`
		Version *string `json:"version"`
	}
	if err := json.Unmarshal(r.body, &h); err != nil || h.Healthy == nil || h.Version == nil {
		return "", &UnsupportedServerError{Op: op, Why: "a body that is not {healthy, version}"}
	}
	if !*h.Healthy {
		return "", fmt.Errorf("opencode: %s: the server reports itself unhealthy", op)
	}
	return *h.Version, nil
}

// sessionPath is the path of a session's resource. The id is escaped, though a
// caller has checked its shape: it is the one caller-supplied part of any path.
func sessionPath(session, tail string) string {
	return "/session/" + url.PathEscape(session) + tail
}

// Session implements Client: GET /session/{id}. The session's directory is
// required by OpenCode's own schema and by this front: without it the status of
// the session cannot be read, and a front that guessed would deliver into a turn
// it could not see.
func (c *HTTPClient) Session(ctx context.Context, session string) (SessionInfo, error) {
	path := sessionPath(session, "")
	op := "GET " + path
	code, body, err := c.do(ctx, http.MethodGet, path, nil, nil, maxSessionBody)
	if err != nil {
		return SessionInfo{}, err
	}
	switch code {
	case http.StatusOK:
	case http.StatusNotFound:
		return SessionInfo{}, fmt.Errorf("%w: %s", ErrSessionNotFound, session)
	default:
		return SessionInfo{}, statusError(op, code, body)
	}
	var s struct {
		ID        *string `json:"id"`
		Directory *string `json:"directory"`
	}
	if err := decode(op, body, &s); err != nil {
		return SessionInfo{}, err
	}
	// The server answered for the session it was asked for, not another.
	if s.ID == nil || *s.ID != session {
		return SessionInfo{}, fmt.Errorf("opencode: %s: the answer is for another session", op)
	}
	if s.Directory == nil || *s.Directory == "" {
		return SessionInfo{}, fmt.Errorf("opencode: %s: the session has no directory, which its status is read under", op)
	}
	return SessionInfo{ID: *s.ID, Directory: *s.Directory}, nil
}

// Busy implements Client: GET /session/status?directory=<the session's>, which
// answers a map of the sessions of that directory that are not idle, each
// {"type":"busy"}, {"type":"retry",...} or {"type":"idle"}. Anything but idle is
// a turn in progress, a type this front has never seen included: delivering into
// a session that might be mid-turn is the one mistake here that cannot be taken
// back.
//
// The directory is not optional. Checked on 1.18.30 with a server running in one
// directory and a session of another: the status of a turn started on that
// session, however the prompt was posted (with no directory, or with the
// server's), is listed under the session's directory and nowhere else, and the
// same request without the parameter, or with the server's own directory, answers
// {} for the whole turn. The other session routes this front uses (the session
// itself, its messages, prompt_async) find the session by id and take no
// directory.
func (c *HTTPClient) Busy(ctx context.Context, session, directory string) (bool, error) {
	const op = "GET /session/status"
	if directory == "" {
		return false, errors.New("opencode: the status of a session is read under its directory, and none was given")
	}
	code, body, err := c.do(ctx, http.MethodGet, "/session/status", url.Values{"directory": {directory}}, nil, maxStatusBody)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, statusError(op, code, body)
	}
	var all map[string]*struct {
		Type *string `json:"type"`
	}
	if err := decode(op, body, &all); err != nil {
		return false, err
	}
	if all == nil {
		return false, fmt.Errorf("opencode: %s: the answer is not an object", op)
	}
	st, listed := all[session]
	if !listed {
		return false, nil
	}
	if st == nil || st.Type == nil || *st.Type == "" {
		return false, fmt.Errorf("opencode: %s: the session's status has no type", op)
	}
	return *st.Type != "idle", nil
}

// Messages implements Client: GET /session/{id}/message?limit=N[&before=C].
// Verified on OpenCode 1.18.30: limit=N returns the newest N messages, oldest of
// them first (by message id, which the server assigns when it creates the
// message, so the order is the server's and not the order the prompts were posted
// in), and limit=0 returns every message. The reply names the page before it in
// an X-Next-Cursor header, present while anything older remains, and
// before=<that cursor> returns the N messages older than the page it came with.
// The cursor is opaque to this front and is not a message id: a bare id as
// before is a 400. It is passed back as it came, after a check that it is the
// short token it looks like.
func (c *HTTPClient) Messages(ctx context.Context, session string, limit int, before string) (MessagePage, error) {
	if limit < 1 {
		return MessagePage{}, fmt.Errorf("opencode: message limit %d: OpenCode reads a limit below 1 as no limit", limit)
	}
	if before != "" && !validCursor(before) {
		return MessagePage{}, errors.New("opencode: the cursor to read before is not one the server gave")
	}
	path := sessionPath(session, "/message")
	op := "GET " + path
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if before != "" {
		q.Set("before", before)
	}
	r, err := c.exchange(ctx, http.MethodGet, path, q, nil, maxMessageBody)
	if err != nil {
		return MessagePage{}, err
	}
	if r.tooLong {
		return MessagePage{}, &TooLongError{Op: op, Limit: maxMessageBody}
	}
	body := r.body
	switch r.code {
	case http.StatusOK:
	case http.StatusNotFound:
		return MessagePage{}, fmt.Errorf("%w: %s", ErrSessionNotFound, session)
	default:
		return MessagePage{}, statusError(op, r.code, body)
	}
	if r.nextCursor != "" && !validCursor(r.nextCursor) {
		return MessagePage{}, fmt.Errorf("opencode: %s: the X-Next-Cursor header is not a cursor this front passes back", op)
	}
	var wire []*struct {
		Info *struct {
			ID       *string `json:"id"`
			Role     *string `json:"role"`
			ParentID *string `json:"parentID"`
		} `json:"info"`
		Parts []*struct {
			Type *string `json:"type"`
			Text *string `json:"text"`
		} `json:"parts"`
	}
	if err := decode(op, body, &wire); err != nil {
		return MessagePage{}, err
	}
	if wire == nil {
		return MessagePage{}, fmt.Errorf("opencode: %s: the answer is not a list", op)
	}
	out := make([]Message, 0, len(wire))
	for i, w := range wire {
		if w == nil || w.Info == nil || w.Info.ID == nil || *w.Info.ID == "" || w.Info.Role == nil || *w.Info.Role == "" {
			return MessagePage{}, fmt.Errorf("opencode: %s: message %d has no info.id and info.role", op, i)
		}
		m := Message{ID: *w.Info.ID, Role: *w.Info.Role}
		if w.Info.ParentID != nil {
			m.ParentID = *w.Info.ParentID
		}
		for j, p := range w.Parts {
			if p == nil || p.Type == nil || *p.Type == "" {
				return MessagePage{}, fmt.Errorf("opencode: %s: message %d part %d has no type", op, i, j)
			}
			part := Part{Type: *p.Type}
			if p.Text != nil {
				part.Text = *p.Text
			}
			m.Parts = append(m.Parts, part)
		}
		out = append(out, m)
	}
	return MessagePage{Messages: out, Next: r.nextCursor}, nil
}

// maxCursor is the longest cursor this front passes back. The server's is a
// base64 of an id and a time, about a hundred bytes.
const maxCursor = 512

// validCursor reports whether c is a token of the alphabet a base64 cursor is
// written in, short enough to be one. It goes into a query string, so nothing a
// query treats as its own gets in.
func validCursor(c string) bool {
	if c == "" || len(c) > maxCursor {
		return false
	}
	for i := 0; i < len(c); i++ {
		switch b := c[i]; {
		case '0' <= b && b <= '9', 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z', b == '-', b == '_', b == '=':
		default:
			return false
		}
	}
	return true
}

// promptBody is the whole of what this front ever POSTs: one text part, and no
// other field. There is no system field, no agent, no model and no second part:
// the model, the agent and the permissions are the session's own, and the text
// is the delivery.
type promptBody struct {
	Parts []promptPart `json:"parts"`
}

type promptPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Prompt implements Client: POST /session/{id}/prompt_async with exactly
// {"parts":[{"type":"text","text":<text>}]}, which answers 204 with no body.
//
// The text must be valid UTF-8, because JSON can only carry that: encoding
// anything else would substitute U+FFFD for the bad bytes and the message that
// landed would not be the text that was sent. Render never produces any, and a
// text that did would fail here, loudly, rather than arrive altered.
func (c *HTTPClient) Prompt(ctx context.Context, session, text string) error {
	if !utf8.ValidString(text) {
		return errors.New("opencode: the text to send is not valid UTF-8, which JSON cannot carry unaltered")
	}
	b, err := json.Marshal(promptBody{Parts: []promptPart{{Type: "text", Text: text}}})
	if err != nil {
		return fmt.Errorf("opencode: encode the prompt: %w", err)
	}
	path := sessionPath(session, "/prompt_async")
	op := "POST " + path
	code, body, err := c.do(ctx, http.MethodPost, path, nil, b, maxPromptBody)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrSessionNotFound, session)
	default:
		return statusError(op, code, body)
	}
}
