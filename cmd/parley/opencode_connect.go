package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/colinsurprenant/parley/internal/opencode"
)

// opencodeTarget is the OpenCode server a pump delivers into, found and checked:
// the client, the version it reported, and where it is, for the start line.
type opencodeTarget struct {
	client opencode.Client
	// discovery, set with client when the service was found from its file, is what
	// finds it again.
	discovery *opencode.Discovery
	version   string
	where     string
}

// connectOpenCode finds the OpenCode server to deliver into and checks that it
// answers, is a version this front speaks to, and knows the session. Every
// failure is a configuration error that names what to run.
//
// With a port, the server is the one at 127.0.0.1:<port> that `opencode serve
// --port` started, asked GET /api/info with the password the human set for it.
// Without one, the server is OpenCode's background service, whose address and
// password are in its service file.
func connectOpenCode(ctx context.Context, getenv func(string) string, port int, portGiven bool, session string) (*opencodeTarget, error) {
	if portGiven {
		return probePort(ctx, getenv, port, session)
	}
	return discoverService(ctx, getenv, session)
}

// tui is how a session is started in OpenCode, for the errors below: in the
// session's checkout, which starts the background service if none runs.
func tui(session string) string {
	return fmt.Sprintf("`opencode -s %s` in the session's checkout, which starts OpenCode's service if none runs", session)
}

// tuiOn is how a session is started in OpenCode on the server `opencode serve
// --port` started, which the background service's `opencode -s` does not reach: the
// TUI is pointed at the server, with the password it was started with in
// OPENCODE_PASSWORD.
func tuiOn(port int, session string) string {
	return fmt.Sprintf("`opencode --server %s -s %s` in the session's checkout, with OPENCODE_PASSWORD set to the password the server was started with", opencode.LoopbackURL(port), session)
}

// discoverService is connectOpenCode without a port: OpenCode's background
// service, from its service file. The server is asked who it is, and the answer
// is held against the file, before the session is asked for, so that a file left
// behind reads as stale and not as a session the service does not know. The pump
// is given the same Discovery, to find the service again whenever it attaches.
func discoverService(ctx context.Context, getenv func(string) string, session string) (*opencodeTarget, error) {
	path, err := opencode.ServicePath(getenv)
	if err != nil {
		return nil, cfgErrf("%v: pass --port <n> for the server at 127.0.0.1:<n>", err)
	}
	d := &opencode.Discovery{Path: path}
	svc, c, info, err := d.Find(ctx)
	var stale *opencode.StaleServiceError
	switch {
	case errors.Is(err, opencode.ErrNoService):
		return nil, cfgErrf("%v: start OpenCode on the session with %s, and the pump finds the service itself; for a server started with `opencode serve --port <n>` pass --port <n>", err, tui(session))
	case errors.Is(err, opencode.ErrServiceAddress):
		// The address is the service file's to name, and a file is not trusted to name
		// anything but this machine: the credentials in it go to a loopback address
		// alone.
		return nil, cfgErrf("OpenCode's service file %s names an address this front will not send its password to: %v", path, err)
	case errors.Is(err, opencode.ErrServiceFile):
		return nil, cfgErrf("OpenCode's service file: %v", err)
	case errors.As(err, &stale):
		return nil, cfgErrf("%v; stop OpenCode and start it again with %s", err, tui(session))
	case opencode.IsTransport(err):
		return nil, cfgErrf("OpenCode's service file %s names a service at %s, which does not answer, and it may have stopped and left the file behind: start it with %s: %v", path, opencode.RedactURL(svc.URL), tui(session), err)
	case err != nil:
		return nil, cfgErrf("OpenCode's service at %s: %v", opencode.RedactURL(svc.URL), err)
	}
	if _, err := c.Session(ctx, session); err != nil {
		switch {
		case errors.Is(err, opencode.ErrSessionNotFound):
			return nil, cfgErrf("OpenCode's service at %s does not know session %s: start OpenCode on it with %s: %v", opencode.RedactURL(svc.URL), session, tui(session), err)
		case opencode.IsTransport(err):
			return nil, cfgErrf("OpenCode's service at %s stopped answering: start it with %s: %v", opencode.RedactURL(svc.URL), tui(session), err)
		}
		return nil, cfgErrf("OpenCode's service at %s: %v", opencode.RedactURL(svc.URL), err)
	}
	return &opencodeTarget{client: c, discovery: d, version: info.Version, where: "the OpenCode service at " + opencode.RedactURL(svc.URL)}, nil
}

// probePort is connectOpenCode with a port: a server `opencode serve --port`
// started, which wants a password, taken from OPENCODE_PASSWORD, else the older
// OPENCODE_SERVER_PASSWORD, and sent under the user opencode.
func probePort(ctx context.Context, getenv func(string) string, port int, session string) (*opencodeTarget, error) {
	where := fmt.Sprintf("127.0.0.1:%d", port)
	password := getenv(opencodePasswordEnv)
	if password == "" {
		password = getenv(opencodeLegacyPasswordEnv)
	}
	c, err := opencode.NewHTTPClient(opencode.LoopbackURL(port), opencode.Auth{Password: password})
	if err != nil {
		return nil, &configError{err: err}
	}
	info, err := opencode.Probe(ctx, c, session)
	switch {
	case err == nil:
		return &opencodeTarget{client: c, version: info.Version, where: where}, nil
	case errors.Is(err, opencode.ErrSessionNotFound):
		return nil, cfgErrf("OpenCode on %s does not know session %s: start OpenCode on it with %s: %v", where, session, tuiOn(port, session), err)
	case opencode.IsTransport(err):
		return nil, cfgErrf("cannot reach OpenCode on %s: nothing answers there. Start the server with `opencode serve --port %d`, or run the pump without --port to use the background service, which %s: %v", where, port, tui(session), err)
	}
	return nil, cfgErrf("OpenCode on %s: %v", where, err)
}
