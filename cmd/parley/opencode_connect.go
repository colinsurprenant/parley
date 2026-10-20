package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/colinsurprenant/parley/internal/opencode"
)

// opencodeTarget is the OpenCode server a pump delivers into, found and checked:
// the client for whichever version it is, the version it reported, and where it
// is, for the start line. Exactly one of the clients is set.
type opencodeTarget struct {
	client  opencode.Client
	client2 opencode.Client2
	version string
	where   string
}

// connectOpenCode finds the OpenCode server to deliver into and checks that it
// answers, is a version this front speaks to, and knows the session. Every
// failure is a configuration error that names what to run, for each version where
// the cause could be either.
//
// With a port, the server is the one at 127.0.0.1:<port>, and the version is told
// by asking it: OpenCode 1 answers GET /global/health with {healthy, version};
// anything else is asked GET /api/info as OpenCode 2, with the password the human
// set for it. Without one, the server is OpenCode 2's background service, whose
// address and password are in its service file.
func connectOpenCode(ctx context.Context, getenv func(string) string, port int, portGiven bool, session string) (*opencodeTarget, error) {
	if portGiven {
		return probePort(ctx, getenv, port, session)
	}
	return discoverService(ctx, getenv, session)
}

// How to start each version on a session, for the errors below.
func tui1(port, session string) string {
	return fmt.Sprintf("`opencode --port %s --session %s` in the session's checkout", port, session)
}

func tui2(session string) string {
	return fmt.Sprintf("`opencode -s %s` in the session's checkout, which starts OpenCode 2's service if none runs", session)
}

// discoverService is connectOpenCode without a port: OpenCode 2's background
// service, from its service file.
func discoverService(ctx context.Context, getenv func(string) string, session string) (*opencodeTarget, error) {
	path, err := opencode.ServicePath(getenv)
	if err != nil {
		return nil, cfgErrf("%v: pass --port <n> for the server at 127.0.0.1:<n>", err)
	}
	svc, err := opencode.ReadService(path)
	switch {
	case errors.Is(err, opencode.ErrNoService):
		return nil, cfgErrf("%v: start OpenCode 2 on the session with %s, and the pump finds the service itself; for OpenCode 1.x pass --port <n>, the port of %s", err, tui2(session), tui1("<n>", session))
	case err != nil:
		return nil, cfgErrf("OpenCode 2's service file: %v", err)
	}
	// The address is the service file's to name, and a file is not trusted to name
	// anything but this machine: the credentials in it go to a loopback address
	// alone.
	c, err := svc.Client()
	if err != nil {
		return nil, cfgErrf("OpenCode 2's service file %s names an address this front will not send its password to: %v", path, err)
	}
	info, err := opencode.Probe2(ctx, c, session)
	switch {
	case errors.Is(err, opencode.ErrSessionNotFound):
		return nil, cfgErrf("OpenCode 2's service at %s does not know session %s: start OpenCode on it with %s: %v", svc.URL, session, tui2(session), err)
	case opencode.IsTransport(err):
		return nil, cfgErrf("OpenCode 2's service file %s names a service at %s, which does not answer, and it may have stopped and left the file behind: start it with %s: %v", path, svc.URL, tui2(session), err)
	case err != nil:
		return nil, cfgErrf("OpenCode 2's service at %s: %v", svc.URL, err)
	}
	// The file is what the service wrote about itself, and the server that answers
	// at its address has to be that service: a file left by a service that has
	// gone, whose address something else now has, would otherwise be taken for it.
	if info.PID != svc.PID || (svc.Version != "" && info.Version != svc.Version) {
		return nil, cfgErrf("the service file %s describes pid %d at %s, and the server that answers there is pid %d (OpenCode %s): the file is stale or the address belongs to another server; stop OpenCode 2 and start it again with %s",
			path, svc.PID, svc.URL, info.PID, info.Version, tui2(session))
	}
	return &opencodeTarget{client2: c, version: info.Version, where: "the OpenCode 2 service at " + svc.URL}, nil
}

// probePort is connectOpenCode with a port. OpenCode 1 is tried first, because
// its health path is the answer that is not mistaken for anything: a JSON object
// with healthy and version. OpenCode 2 serves its web page there.
func probePort(ctx context.Context, getenv func(string) string, port int, session string) (*opencodeTarget, error) {
	url := opencode.LoopbackURL(port)
	where := fmt.Sprintf("127.0.0.1:%d", port)
	user := getenv(opencodeUserEnv)
	if user == "" {
		user = opencodeDefaultUser
	}
	c1, err := opencode.NewHTTPClient(url, opencode.Auth{User: user, Password: getenv(opencodePasswordEnv)})
	if err != nil {
		return nil, &configError{err: err}
	}
	portText := strconv.Itoa(port)
	_, err1 := c1.Health(ctx)
	switch {
	case err1 == nil:
		version, err := opencode.Probe(ctx, c1, session)
		if err != nil {
			return nil, probeFailure1(port, session, err)
		}
		return &opencodeTarget{client: c1, version: version, where: where}, nil
	case opencode.IsTransport(err1):
		return nil, cfgErrf("cannot reach OpenCode on %s: nothing answers there. OpenCode 1 serves its API only while the TUI runs with --port: start it with %s. OpenCode 2 has no --port: start %s, and run the pump without --port, or run `opencode serve --port %s` yourself: %v",
			where, tui1(portText, session), tui2(session), portText, err1)
	}
	// Not OpenCode 1's answer: a page, a 401 for a password, or anything else.
	// Ask as OpenCode 2, with its own variable and then the old one.
	password := getenv(opencodePassword2Env)
	if password == "" {
		password = getenv(opencodePasswordEnv)
	}
	c2, err := opencode.NewHTTPClient2(url, opencode.Auth{User: opencode.ServiceUser, Password: password})
	if err != nil {
		return nil, &configError{err: err}
	}
	info, err2 := opencode.Probe2(ctx, c2, session)
	switch {
	case err2 == nil:
		return &opencodeTarget{client2: c2, version: info.Version, where: where}, nil
	case errors.Is(err2, opencode.ErrSessionNotFound):
		return nil, cfgErrf("OpenCode 2 on %s does not know session %s: start OpenCode on it with %s: %v", where, session, tui2(session), err2)
	}
	return nil, cfgErrf("OpenCode on %s answered as neither version. As OpenCode 1: %v. As OpenCode 2: %v. OpenCode 1 is started with %s; OpenCode 2 with %s, and the pump run without --port, or with `opencode serve --port %s`",
		where, err1, err2, tui1(portText, session), tui2(session), portText)
}

// probeFailure1 turns a failed startup probe of an OpenCode 1 server into the
// configuration error it is, and says what the human runs to make it right when
// that is the TUI: the server is only there while the TUI runs, and only for the
// session it was given.
func probeFailure1(port int, session string, err error) error {
	switch {
	case errors.Is(err, opencode.ErrSessionNotFound):
		return cfgErrf("OpenCode on 127.0.0.1:%d does not know session %s: start the TUI on it with %s: %v", port, session, tui1(strconv.Itoa(port), session), err)
	default:
		return cfgErrf("OpenCode on 127.0.0.1:%d: %v", port, err)
	}
}
