package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"unicode/utf8"
)

// ServerInfo is what an OpenCode 2 server says of itself.
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

// Client2 is the HTTP seam for OpenCode 2's API, the second implementation of
// the pump's side of the protocol that Client is for OpenCode 1: everything the
// pump asks of an OpenCode 2 server, and nothing else. HTTPClient2 is the real
// one.
//
// OpenCode 2 differs from 1 where it matters here. A prompt carries an id the
// client chooses and may be posted again under it, so a post that was cut short
// is repeated and not guessed at; it is queued, so it becomes a message of its
// own turn and is never merged into a running one; and a message is read back by
// its id. The session's turn is listed in one place for the whole server, with
// no directory to read it under.
//
// An error from a method is one of the kinds Client's say, with ErrConflict
// besides: a 409.
type Client2 interface {
	// Info returns the server's version and process id.
	Info(ctx context.Context) (ServerInfo, error)
	// Session returns what the server knows of the session, or ErrSessionNotFound.
	Session(ctx context.Context, session string) (SessionInfo, error)
	// Busy reports whether the session is running a turn: OpenCode 2 lists the
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
	// Steer changes an inbox entry from queue to steer, which makes the server
	// take it up at the next boundary, an idle session's included. ErrConflict is
	// the answer for an entry that is no longer in the inbox, or no longer queued.
	Steer(ctx context.Context, session, id string) error
}

// ErrConflict is the server's 409: the request names something that is not in
// the state it needs. It is wrapped with the request; test for it with errors.Is.
var ErrConflict = errors.New("opencode: conflict")

// Hint2 is what a 401 from an OpenCode 2 server is followed by.
const Hint2 = "the server wants its password: the service's own is read from its service file, and for a server started with `opencode serve` set OPENCODE_PASSWORD (or OPENCODE_SERVER_PASSWORD) to the one it was started with"

// ServiceUser is the user name of the HTTP basic credentials of every OpenCode 2
// server.
const ServiceUser = "opencode"

const maxInfoBody = 64 << 10

// HTTPClient2 speaks OpenCode 2's HTTP API (2.0.32 is the version it was
// verified against, in simulation mode) to a server on loopback. It shares the
// transport of HTTPClient and so its bounds: loopback addresses only, no proxy,
// no redirect, a time limit on every request, and a bound on every body read.
type HTTPClient2 struct{ h *HTTPClient }

// NewHTTPClient2 returns a client for the OpenCode 2 server at baseURL, which
// must be an http URL whose host is a loopback IP address and port, as for
// NewHTTPClient. The credentials are sent as HTTP basic ones and to that address
// alone; auth.User should be ServiceUser.
func NewHTTPClient2(baseURL string, auth Auth, opts ...Option) (*HTTPClient2, error) {
	h, err := NewHTTPClient(baseURL, auth, opts...)
	if err != nil {
		return nil, err
	}
	return &HTTPClient2{h: h}, nil
}

// statusError2 is statusError for OpenCode 2, whose 401 has its own advice.
func statusError2(op string, code int, body []byte) error {
	err := statusError(op, code, body)
	if se, ok := err.(*StatusError); ok && code == http.StatusUnauthorized {
		se.Hint = Hint2
	}
	return err
}

// notFoundTag is the _tag of a 404 body, "" if there is none: OpenCode 2 says
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

func session2Path(session, tail string) string {
	return "/api/session/" + url.PathEscape(session) + tail
}

// Info implements Client2: GET /api/info, which answers {"version", "pid", ...}
// without an envelope.
func (c *HTTPClient2) Info(ctx context.Context) (ServerInfo, error) {
	const op = "GET /api/info"
	code, body, err := c.h.do(ctx, http.MethodGet, "/api/info", nil, nil, maxInfoBody)
	if err != nil {
		return ServerInfo{}, err
	}
	if code != http.StatusOK {
		return ServerInfo{}, statusError2(op, code, body)
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

// Session implements Client2: GET /api/session/{id}. The directory is the
// session's location, kept for the report of where the session lives.
func (c *HTTPClient2) Session(ctx context.Context, session string) (SessionInfo, error) {
	path := session2Path(session, "")
	op := "GET " + path
	code, body, err := c.h.do(ctx, http.MethodGet, path, nil, nil, maxSessionBody)
	if err != nil {
		return SessionInfo{}, err
	}
	switch {
	case code == http.StatusOK:
	case code == http.StatusNotFound && notFoundTag(body) == "SessionNotFoundError":
		return SessionInfo{}, fmt.Errorf("%w: %s", ErrSessionNotFound, session)
	default:
		return SessionInfo{}, statusError2(op, code, body)
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

// Busy implements Client2: GET /api/session/active, which answers
// {"data":{"<id>":{"type":"running"}}} for the sessions that are in a turn and
// {"data":{}} when none is. Any entry is a turn in progress: delivering into a
// session that might be mid-turn is the mistake that cannot be taken back, as
// for OpenCode 1.
func (c *HTTPClient2) Busy(ctx context.Context, session string) (bool, error) {
	const op = "GET /api/session/active"
	code, body, err := c.h.do(ctx, http.MethodGet, "/api/session/active", nil, nil, maxStatusBody)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, statusError2(op, code, body)
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

// Prompt implements Client2: POST /api/session/{id}/prompt with exactly
// {"id":<id>,"text":<text>,"delivery":"queue"}, which answers 200 with the
// inbox entry it admitted. The text must be valid UTF-8, for the reason Client's
// Prompt gives.
//
// The entry in the answer is checked to be the one asked for, in this session,
// with these bytes: the server answers a repeat of an id with the first post of
// it, and an id that was used for other text would otherwise be taken for the
// delivery.
func (c *HTTPClient2) Prompt(ctx context.Context, session, id, text string) error {
	if !utf8.ValidString(text) {
		return errors.New("opencode: the text to send is not valid UTF-8, which JSON cannot carry unaltered")
	}
	b, err := json.Marshal(queuedPromptBody{ID: id, Text: text, Delivery: "queue"})
	if err != nil {
		return fmt.Errorf("opencode: encode the prompt: %w", err)
	}
	path := session2Path(session, "/prompt")
	op := "POST " + path
	code, body, err := c.h.do(ctx, http.MethodPost, path, nil, b, maxMessageBody)
	if err != nil {
		return err
	}
	switch {
	case code == http.StatusOK:
	case code == http.StatusNotFound && notFoundTag(body) == "SessionNotFoundError":
		return fmt.Errorf("%w: %s", ErrSessionNotFound, session)
	case code == http.StatusConflict:
		return fmt.Errorf("%w: %w", ErrConflict, statusError2(op, code, body))
	default:
		return statusError2(op, code, body)
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
		return fmt.Errorf("opencode: %s: the server holds other text under message id %s than the text posted", op, id)
	}
	return nil
}

// Message implements Client2: GET /api/session/{id}/message/{messageID}, which
// answers {"data":{"id","time","text","type":"user"}} once the message exists
// and 404 MessageNotFoundError until then, an inbox entry included.
func (c *HTTPClient2) Message(ctx context.Context, session, id string) (UserMessage, bool, error) {
	path := session2Path(session, "/message/"+url.PathEscape(id))
	op := "GET " + path
	code, body, err := c.h.do(ctx, http.MethodGet, path, nil, nil, maxMessageBody)
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
		return UserMessage{}, false, statusError2(op, code, body)
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

// Inbox implements Client2: GET /api/session/{id}/inbox, which answers
// {"data":[{"id","delivery",...}]} for the input the server has accepted and not
// yet made a message of, oldest first.
func (c *HTTPClient2) Inbox(ctx context.Context, session string) ([]InboxItem, error) {
	path := session2Path(session, "/inbox")
	op := "GET " + path
	code, body, err := c.h.do(ctx, http.MethodGet, path, nil, nil, maxMessageBody)
	if err != nil {
		return nil, err
	}
	switch {
	case code == http.StatusOK:
	case code == http.StatusNotFound && notFoundTag(body) == "SessionNotFoundError":
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, session)
	default:
		return nil, statusError2(op, code, body)
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

// Steer implements Client2: PATCH /api/session/{id}/inbox/{inboxID} with
// {"delivery":"steer"}, which answers 204.
func (c *HTTPClient2) Steer(ctx context.Context, session, id string) error {
	path := session2Path(session, "/inbox/"+url.PathEscape(id))
	op := "PATCH " + path
	code, body, err := c.h.do(ctx, http.MethodPatch, path, nil, []byte(`{"delivery":"steer"}`), maxPromptBody)
	if err != nil {
		return err
	}
	switch {
	case code == http.StatusNoContent:
		return nil
	case code == http.StatusNotFound && notFoundTag(body) == "SessionNotFoundError":
		return fmt.Errorf("%w: %s", ErrSessionNotFound, session)
	case code == http.StatusConflict:
		return fmt.Errorf("%w: %w", ErrConflict, statusError2(op, code, body))
	default:
		return statusError2(op, code, body)
	}
}
