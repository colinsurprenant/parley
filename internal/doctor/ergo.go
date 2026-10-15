package doctor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/colinsurprenant/parley/internal/stream/irc"
)

// checkErgo checks that ergo is installed and runs, and its version against the
// one the embedded configuration was written for. The bool is whether it was
// found and runs, which is what the live check needs.
func checkErgo(ctx context.Context, bin string) ([]Result, bool) {
	path, err := Resolve(bin)
	if err != nil {
		return []Result{{Fail, "ergo", fmt.Sprintf("%v: %s", err, ErgoInstallHint)}}, false
	}
	version := checkVersion(ctx, "ergo", path, ErgoVerified, ergoRemedy)
	return []Result{{OK, "ergo", "found " + path}, version}, version.Level != Fail
}

const (
	dialTimeout  = 3 * time.Second
	capsDeadline = 5 * time.Second
	// maxCapsLines bounds how much of a server's answer is read: a CAP LS reply
	// is a handful of lines, and a socket that talks on is not an IRC server.
	maxCapsLines = 50
	// maxCapsLine bounds one line of it: an IRCv3 message is at most 8191 bytes
	// of tags and 512 of the rest. With maxCapsLines it bounds the whole read.
	maxCapsLine = 8704
)

// checkServer asks the IRC server at addr for its capabilities and checks that
// it offers every one the adapter requires. Nothing is registered: CAP LS is
// answered before a connection has a nick, so the check leaves no account and
// no presence behind. Nothing listening is a warning, because the server may
// just not be started; something that answers and lacks a capability, or is not
// an IRC server at all, is a failure, because a table would go to it.
func checkServer(ctx context.Context, addr string) Result {
	const surface = "server"
	addr, err := irc.LoopbackAddr(addr)
	if err != nil {
		return Result{Fail, surface, err.Error()}
	}
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return Result{Warn, surface, fmt.Sprintf("nothing answers at %s: start the server with parley server", addr)}
	}
	defer conn.Close()
	// The deadline is the bound on every read, and a cancelled context closes
	// the connection under a read that is blocked.
	_ = conn.SetDeadline(time.Now().Add(capsDeadline))
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if _, err := conn.Write([]byte("CAP LS 302\r\n")); err != nil {
		return Result{Fail, surface, fmt.Sprintf("%s: write CAP LS: %v", addr, err)}
	}
	offered, err := readCaps(bufio.NewReaderSize(conn, maxCapsLine))
	_, _ = conn.Write([]byte("QUIT\r\n"))
	if err != nil {
		return Result{Fail, surface, fmt.Sprintf("%s did not answer CAP LS 302 as an IRCv3 server does (%v): parley server runs the configuration parley needs", addr, err)}
	}
	var missing []string
	required := irc.RequiredCaps()
	for _, c := range required {
		if _, ok := offered[c]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return Result{Fail, surface, fmt.Sprintf("%s does not offer %s: parley server runs the configuration parley needs", addr, strings.Join(missing, ", "))}
	}
	// A sasl that lists its mechanisms is a statement about which are there, and
	// parley authenticates with PLAIN alone (AUTHENTICATE PLAIN, in the adapter).
	// A bare sasl lists none, and says nothing against it.
	if mechs := offered["sasl"]; mechs != "" && !hasMechanism(mechs, "PLAIN") {
		return Result{Fail, surface, fmt.Sprintf("%s offers sasl with the mechanisms %q, and PLAIN is not among them: parley authenticates with AUTHENTICATE PLAIN, and parley server runs the configuration that allows it", addr, mechs)}
	}
	return Result{OK, surface, fmt.Sprintf("%s offers all %d capabilities parley requires", addr, len(required))}
}

// hasMechanism reports whether a sasl capability's comma-separated mechanisms
// include mech.
func hasMechanism(list, mech string) bool {
	for _, m := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(m), mech) {
			return true
		}
	}
	return false
}

// readCaps reads a server's reply to CAP LS 302 and returns the capabilities it
// lists, each name with its value ("" for one that has none). A name is kept
// verbatim, as the irc adapter keeps it: a leading `-` is part of it, so `-sasl`
// is a capability of its own and not `sasl`. The reply is one
// line, or several with `*` before the last one's list. A line is read only up
// to r's buffer size, so a peer that never ends one cannot grow the read.
func readCaps(r *bufio.Reader) (map[string]string, error) {
	offered := map[string]string{}
	for i := 0; i < maxCapsLines; i++ {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("a line longer than %d bytes: not an IRC server", r.Size())
		}
		if err != nil {
			return nil, fmt.Errorf("no CAP LS reply: %w", err)
		}
		params := capParams(strings.TrimRight(string(line), "\r\n"))
		// CAP <target> LS [*] :<caps>
		if len(params) < 3 || !strings.EqualFold(params[0], "CAP") || !strings.EqualFold(params[2], "LS") {
			continue
		}
		list, more := "", false
		switch {
		case len(params) >= 5 && params[3] == "*":
			list, more = params[4], true
		case len(params) >= 4:
			list = params[3]
		}
		for _, c := range strings.Fields(list) {
			name, value, _ := strings.Cut(c, "=")
			offered[name] = value
		}
		if !more {
			return offered, nil
		}
	}
	return nil, fmt.Errorf("no end of CAP LS in %d lines", maxCapsLines)
}

// capParams splits an IRC line into its command and parameters: tags and prefix
// dropped, a parameter beginning with ':' taking the rest of the line.
func capParams(line string) []string {
	if strings.HasPrefix(line, "@") {
		_, line, _ = strings.Cut(line, " ")
	}
	line = strings.TrimLeft(line, " ")
	if strings.HasPrefix(line, ":") {
		_, line, _ = strings.Cut(line, " ")
	}
	var params []string
	for line != "" {
		line = strings.TrimLeft(line, " ")
		if strings.HasPrefix(line, ":") {
			params = append(params, line[1:])
			break
		}
		var p string
		p, line, _ = strings.Cut(line, " ")
		if p != "" {
			params = append(params, p)
		}
	}
	return params
}
