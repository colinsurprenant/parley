package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ServerInfo is what an OpenCode server says of itself.
type ServerInfo struct {
	Version string
	// PID is the server's process id, which the service file also records: the
	// two agreeing is what shows that the file describes the server that answered.
	PID int
}

// UserMessage is a user message of a session as the server holds it, once it is
// a message and no longer an entry in the inbox.
type UserMessage struct {
	ID   string
	Text string
}

// InboxItem is one entry of a session's inbox: input the server has accepted
// and not yet turned into a message. Delivery is "queue" or "steer".
type InboxItem struct {
	ID       string
	Delivery string
}

// Client is the HTTP seam of the front: everything the pump asks of an OpenCode
// server, and nothing else. HTTPClient is the real one; the tests swap in their
// own.
//
// A prompt carries an id the client chooses and may be posted again under it, so
// a post that was cut short is repeated and not guessed at; it is queued, so it
// becomes a message of its own turn and is never merged into a running one; and a
// message is read back by its id. The sessions in a turn are listed in one place
// for the whole server.
//
// An error from a method is one of: ErrSessionNotFound; a *TransportError, for a
// request that got no answer (IsDial says that it never reached the server at
// all); a *StatusError, for an answer that was not the expected one, with
// ErrConflict wrapped for a 409 on a post; a *TooLongError, for an answer over the
// bound it is read under; or an error that says the answer was malformed. A
// canceled context is returned as the context's own error and is never a
// *TransportError.
type Client interface {
	// Info returns the server's version and process id.
	Info(ctx context.Context) (ServerInfo, error)
	// Session returns what the server knows of the session, or ErrSessionNotFound.
	Session(ctx context.Context, session string) (SessionInfo, error)
	// Busy reports whether the session is running a turn: OpenCode lists the
	// sessions that are, and a session it does not list is idle.
	Busy(ctx context.Context, session string) (bool, error)
	// Prompt posts text to the session as the user message id, queued: it waits in
	// the session's inbox and becomes a message of its own turn. The server
	// admits the first post of an id and answers a repeat of it with the first, so
	// a post that got no answer can be sent again. Acceptance is not arrival: the
	// message exists once the turn that promotes it has begun.
	Prompt(ctx context.Context, session, id, text string) error
	// Message returns the user message id, and found false if the session has no
	// message by that id (yet).
	Message(ctx context.Context, session, id string) (msg UserMessage, found bool, err error)
	// Inbox returns the session's inbox, oldest first.
	Inbox(ctx context.Context, session string) ([]InboxItem, error)
}

// ErrConflict is the server's 409: the request names something that is not in
// the state it needs. It is wrapped with the request; test for it with errors.Is.
var ErrConflict = errors.New("opencode: conflict")

// PasswordHint is what a 401 from an OpenCode server is followed by.
const PasswordHint = "the server wants its password: the service's own is read from its service file, and for a server started with `opencode serve` set OPENCODE_PASSWORD (or OPENCODE_SERVER_PASSWORD) to the one it was started with"

// ServiceUser is the user name of the HTTP basic credentials of every OpenCode
// server.
const ServiceUser = "opencode"

// SessionInfo is the part of a session the front needs.
type SessionInfo struct {
	ID string
	// Directory is the absolute directory the session belongs to: the checkout
	// it was started in.
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
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("opencode: %s: %s", e.Op, e.Status)
	if e.Body != "" {
		s += ": " + e.Body
	}
	if e.Code == http.StatusUnauthorized {
		s += " (" + PasswordHint + ")"
	}
	return s
}

// InstallHint says how to get an OpenCode this front speaks to. A package manager
// or an installer may hand out the legacy 1.x, which has no API this front can
// speak to: the legacy npm package opencode-ai and the installer at
// opencode.ai/install give 1.x.
const InstallHint = "install OpenCode 2: `brew install opencode` (Homebrew core), or `npm i -g @opencode/cli`"

// Auth is the HTTP basic credentials of an OpenCode server, whose user name is
// always ServiceUser. An empty Password sends none. It never prints its password,
// whatever verb formats it.
type Auth struct {
	Password string
}

func (a Auth) String() string   { return "opencode.Auth{Password:<redacted>}" }
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
	// buffered whole. The message and inbox replies are the large ones: they carry
	// the whole text of a delivery, and the inbox every queued entry's payload.
	maxInfoBody    = 64 << 10
	maxSessionBody = 1 << 20
	maxStatusBody  = 1 << 20
	maxMessageBody = 16 << 20
	maxPromptBody  = 64 << 10
	maxErrorBody   = 4 << 10

	// errorExcerpt is how much of an error body is quoted.
	errorExcerpt = 200
)

// HTTPClient speaks OpenCode 2's HTTP API (2.0.32 is the version it was verified
// against, in simulation mode) to a server on loopback: loopback addresses only,
// no proxy, no redirect, a time limit on every request, and a bound on every body
// read.
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

// LoopbackURL is the base URL of the server `opencode serve --port` serves:
// OpenCode's default hostname, which is loopback.
func LoopbackURL(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }

// NewHTTPClient returns a client for the server at baseURL, which must be an
// http URL whose host is a loopback IP address and port, with no path, query or
// credentials. The check is the front's whole trust boundary on this side: the
// basic credentials go over plaintext, so they are only ever sent to a loopback
// address, and a name such as localhost, which a resolver could point anywhere,
// is refused for the same reason `parley server --listen` refuses it. A client's
// credentials are sent to that address alone.
func NewHTTPClient(baseURL string, auth Auth, opts ...Option) (*HTTPClient, error) {
	// Every message below names the URL as RedactURL spells it: it may have come
	// from a file, and a URL with credentials in it must not be echoed with them,
	// least of all in the error that refuses it, and url.Parse's own error quotes the
	// raw string.
	shown := RedactURL(baseURL)
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("opencode: server URL %q is not a URL", shown)
	}
	if u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("opencode: server URL %q must be http://<loopback ip>:<port> and nothing else", shown)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return nil, fmt.Errorf("opencode: server URL %q has no port", shown)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("opencode: server URL %q is not a loopback IP address", shown)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("opencode: server URL %q has no valid port", shown)
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

// reply is what came back from one request: the status and the body read up to the
// bound. tooLong says the body went over the bound, in which case it is not kept.
type reply struct {
	code    int
	body    []byte
	tooLong bool
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
// is its own type because a caller can tell it from a failure: the inbox, which
// carries every queued entry's payload, can pass the bound on the human's own
// queued prompts, and the pump then waits on the message itself.
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
		req.SetBasicAuth(ServiceUser, c.auth.Password)
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
	r := reply{code: resp.StatusCode, body: b}
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

// notFoundTag is the _tag of a 404 body, "" if there is none: OpenCode says
// which thing it did not find.
func notFoundTag(body []byte) string {
	var e struct {
		Tag string `json:"_tag"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	return e.Tag
}

func sessionPath(session, tail string) string {
	return "/api/session/" + url.PathEscape(session) + tail
}

// Info implements Client: GET /api/info, which answers {"version", "pid", ...}
// without an envelope.
func (c *HTTPClient) Info(ctx context.Context) (ServerInfo, error) {
	const op = "GET /api/info"
	code, body, err := c.do(ctx, http.MethodGet, "/api/info", nil, nil, maxInfoBody)
	if err != nil {
		return ServerInfo{}, err
	}
	if code != http.StatusOK {
		return ServerInfo{}, statusError(op, code, body)
	}
	var i struct {
		Version *string `json:"version"`
		PID     *int    `json:"pid"`
	}
	if err := decode(op, body, &i); err != nil {
		return ServerInfo{}, err
	}
	if i.Version == nil || *i.Version == "" || i.PID == nil || *i.PID < 1 {
		return ServerInfo{}, fmt.Errorf("opencode: %s: the answer has no version and process id, so it is not an OpenCode 2 server's", op)
	}
	return ServerInfo{Version: *i.Version, PID: *i.PID}, nil
}

// Session implements Client: GET /api/session/{id}. The directory is the
// session's location, kept for the report of where the session lives.
func (c *HTTPClient) Session(ctx context.Context, session string) (SessionInfo, error) {
	path := sessionPath(session, "")
	op := "GET " + path
	code, body, err := c.do(ctx, http.MethodGet, path, nil, nil, maxSessionBody)
	if err != nil {
		return SessionInfo{}, err
	}
	switch {
	case code == http.StatusOK:
	case code == http.StatusNotFound && notFoundTag(body) == "SessionNotFoundError":
		return SessionInfo{}, fmt.Errorf("%w: %s", ErrSessionNotFound, session)
	default:
		return SessionInfo{}, statusError(op, code, body)
	}
	var s struct {
		Data *struct {
			ID       *string `json:"id"`
			Location *struct {
				Directory *string `json:"directory"`
			} `json:"location"`
		} `json:"data"`
	}
	if err := decode(op, body, &s); err != nil {
		return SessionInfo{}, err
	}
	if s.Data == nil || s.Data.ID == nil || *s.Data.ID != session {
		return SessionInfo{}, fmt.Errorf("opencode: %s: the answer is not for the session asked for", op)
	}
	if s.Data.Location == nil || s.Data.Location.Directory == nil || *s.Data.Location.Directory == "" {
		return SessionInfo{}, fmt.Errorf("opencode: %s: the session has no directory", op)
	}
	return SessionInfo{ID: *s.Data.ID, Directory: *s.Data.Location.Directory}, nil
}

// Busy implements Client: GET /api/session/active, which answers
// {"data":{"<id>":{"type":"running"}}} for the sessions that are in a turn and
// {"data":{}} when none is. Any entry is a turn in progress: delivering into a
// session that might be mid-turn is the mistake that cannot be taken back.
func (c *HTTPClient) Busy(ctx context.Context, session string) (bool, error) {
	const op = "GET /api/session/active"
	code, body, err := c.do(ctx, http.MethodGet, "/api/session/active", nil, nil, maxStatusBody)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, statusError(op, code, body)
	}
	var a struct {
		Data map[string]*struct {
			Type *string `json:"type"`
		} `json:"data"`
	}
	if err := decode(op, body, &a); err != nil {
		return false, err
	}
	if a.Data == nil {
		return false, fmt.Errorf("opencode: %s: the answer has no data object", op)
	}
	st, listed := a.Data[session]
	if !listed {
		return false, nil
	}
	if st == nil || st.Type == nil || *st.Type == "" {
		return false, fmt.Errorf("opencode: %s: the session's entry has no type", op)
	}
	return true, nil
}

// queuedPromptBody is the whole of what this front ever POSTs to OpenCode 2: the
// id, the text and the delivery. There are no files, agents or skills, no
// metadata and no resume: the model, the agent and the permissions are the
// session's own, and the text is the delivery.
type queuedPromptBody struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Delivery string `json:"delivery"`
}

// Prompt implements Client: POST /api/session/{id}/prompt with exactly
// {"id":<id>,"text":<text>,"delivery":"queue"}, which answers 200 with the
// inbox entry it admitted. The text must be valid UTF-8, for the reason Client's
// Prompt gives.
//
// The entry in the answer is checked to be the one asked for, in this session,
// with these bytes: the server answers a repeat of an id with the first post of
// it, and an id that was used for other text would otherwise be taken for the
// delivery.
func (c *HTTPClient) Prompt(ctx context.Context, session, id, text string) error {
	if !utf8.ValidString(text) {
		return errors.New("opencode: the text to send is not valid UTF-8, which JSON cannot carry unaltered")
	}
	b, err := json.Marshal(queuedPromptBody{ID: id, Text: text, Delivery: "queue"})
	if err != nil {
		return fmt.Errorf("opencode: encode the prompt: %w", err)
	}
	path := sessionPath(session, "/prompt")
	op := "POST " + path
	code, body, err := c.do(ctx, http.MethodPost, path, nil, b, maxMessageBody)
	if err != nil {
		return err
	}
	switch {
	case code == http.StatusOK:
	case code == http.StatusNotFound && notFoundTag(body) == "SessionNotFoundError":
		return fmt.Errorf("%w: %s", ErrSessionNotFound, session)
	case code == http.StatusConflict:
		return fmt.Errorf("%w: %w", ErrConflict, statusError(op, code, body))
	default:
		return statusError(op, code, body)
	}
	var a struct {
		Data *struct {
			ID        *string `json:"id"`
			SessionID *string `json:"sessionID"`
			Type      *string `json:"type"`
			Payload   *struct {
				Text *string `json:"text"`
			} `json:"payload"`
		} `json:"data"`
	}
	if err := decode(op, body, &a); err != nil {
		return err
	}
	d := a.Data
	switch {
	case d == nil || d.ID == nil || *d.ID != id || d.SessionID == nil || *d.SessionID != session:
		return fmt.Errorf("opencode: %s: the answer is not for the prompt posted", op)
	case d.Type == nil || *d.Type != "user" || d.Payload == nil || d.Payload.Text == nil:
		return fmt.Errorf("opencode: %s: the answer is not a user input", op)
	case *d.Payload.Text != text:
		// The prompt was admitted, so the entry is in the inbox and will be taken up as
		// the server holds it. A plugin that rewrites prompts (OpenCode 2 runs them
		// on every prompt) is the likely cause. Posting again would be a second entry.
		return fmt.Errorf("opencode: %s: the server admitted the prompt under message id %s with other text than was posted (%d bytes against %d; a plugin may have rewritten it): the entry is in the session's inbox and will probably be taken up as it is, and nothing was committed, so a restart sends the delivery again under a new id", op, id, len(*d.Payload.Text), len(text))
	}
	return nil
}

// Message implements Client: GET /api/session/{id}/message/{messageID}, which
// answers {"data":{"id","time","text","type":"user"}} once the message exists
// and 404 MessageNotFoundError until then, an inbox entry included.
func (c *HTTPClient) Message(ctx context.Context, session, id string) (UserMessage, bool, error) {
	path := sessionPath(session, "/message/"+url.PathEscape(id))
	op := "GET " + path
	code, body, err := c.do(ctx, http.MethodGet, path, nil, nil, maxMessageBody)
	if err != nil {
		return UserMessage{}, false, err
	}
	switch {
	case code == http.StatusOK:
	case code == http.StatusNotFound && notFoundTag(body) == "MessageNotFoundError":
		return UserMessage{}, false, nil
	case code == http.StatusNotFound && notFoundTag(body) == "SessionNotFoundError":
		return UserMessage{}, false, fmt.Errorf("%w: %s", ErrSessionNotFound, session)
	default:
		return UserMessage{}, false, statusError(op, code, body)
	}
	var m struct {
		Data *struct {
			ID   *string `json:"id"`
			Type *string `json:"type"`
			Text *string `json:"text"`
		} `json:"data"`
	}
	if err := decode(op, body, &m); err != nil {
		return UserMessage{}, false, err
	}
	d := m.Data
	switch {
	case d == nil || d.ID == nil || *d.ID != id:
		return UserMessage{}, false, fmt.Errorf("opencode: %s: the answer is for another message", op)
	case d.Type == nil || *d.Type != "user" || d.Text == nil:
		return UserMessage{}, false, fmt.Errorf("opencode: %s: message %s is not a user message with text", op, id)
	}
	return UserMessage{ID: *d.ID, Text: *d.Text}, true, nil
}

// Inbox implements Client: GET /api/session/{id}/inbox, which answers
// {"data":[{"id","delivery",...}]} for the input the server has accepted and not
// yet made a message of, oldest first.
func (c *HTTPClient) Inbox(ctx context.Context, session string) ([]InboxItem, error) {
	path := sessionPath(session, "/inbox")
	op := "GET " + path
	code, body, err := c.do(ctx, http.MethodGet, path, nil, nil, maxMessageBody)
	if err != nil {
		return nil, err
	}
	switch {
	case code == http.StatusOK:
	case code == http.StatusNotFound && notFoundTag(body) == "SessionNotFoundError":
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, session)
	default:
		return nil, statusError(op, code, body)
	}
	var in struct {
		Data []*struct {
			ID       *string `json:"id"`
			Delivery *string `json:"delivery"`
		} `json:"data"`
	}
	if err := decode(op, body, &in); err != nil {
		return nil, err
	}
	if in.Data == nil {
		return nil, fmt.Errorf("opencode: %s: the answer is not a list", op)
	}
	out := make([]InboxItem, 0, len(in.Data))
	for i, it := range in.Data {
		if it == nil || it.ID == nil || *it.ID == "" || it.Delivery == nil || *it.Delivery == "" {
			return nil, fmt.Errorf("opencode: %s: entry %d has no id and delivery", op, i)
		}
		out = append(out, InboxItem{ID: *it.ID, Delivery: *it.Delivery})
	}
	return out, nil
}

// userinfoRE is the userinfo of a URL: whatever sits between the scheme's "//" and
// the "@" that ends it, before the first "/", "?" or "#".
var userinfoRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://)[^/?#]*@`)

// RedactURL is raw with any userinfo taken out ("http://user:secret@host:1" is
// "http://host:1"), for a URL that is about to be printed. It works on the text,
// not on a parse, so that a string that does not parse is redacted as well. A URL
// that came out of a file is the case: it is printed to say what was wrong with it.
func RedactURL(raw string) string { return userinfoRE.ReplaceAllString(raw, "$1") }
