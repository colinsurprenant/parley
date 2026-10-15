package doctor

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/colinsurprenant/parley/internal/codex"
	"github.com/colinsurprenant/parley/internal/ergoconf"
	"github.com/colinsurprenant/parley/internal/stream"
	"github.com/colinsurprenant/parley/internal/stream/irc"
)

const (
	ergoSurface    = "ergo live"
	ergoLiveBudget = 90 * time.Second
	ergoStartup    = 20 * time.Second
	ergoStopGrace  = 5 * time.Second
	// The nicks of the two clients, and the channel they meet in. Nobody else
	// is on the throwaway server, so nothing here can collide with a real table.
	liveSender   = "doctor-a"
	liveReceiver = "doctor-b"
	liveChannel  = "doctor"
	liveBody     = "parley doctor --live: a message through the throwaway server"
)

// liveErgo runs a throwaway ergo from the embedded configuration, on a free
// loopback port with a datastore in a temp directory, and sends one message
// through it between two parley IRC clients — each its own account, registered
// on the way — read back through the same stream layer a seat uses. The server
// is stopped and the directory removed whatever the outcome. Nothing of the user's
// running server is touched: the port is the kernel's pick, never the one a
// table uses.
func liveErgo(ctx context.Context, bin string) []Result {
	path, err := Resolve(bin)
	if err != nil {
		return []Result{{Fail, ergoSurface, err.Error()}}
	}
	ctx, cancel := context.WithTimeout(ctx, ergoLiveBudget)
	defer cancel()

	tmp, err := os.MkdirTemp("", "parley-doctor-ergo-")
	if err != nil {
		return []Result{{Fail, ergoSurface, fmt.Sprintf("make a directory for the server: %v", err)}}
	}
	defer os.RemoveAll(tmp)

	addr, err := freeLoopbackAddr()
	if err != nil {
		return []Result{{Fail, ergoSurface, fmt.Sprintf("find a free loopback port: %v", err)}}
	}
	datastore := filepath.Join(tmp, "ergo")
	if err := os.Mkdir(datastore, 0o700); err != nil {
		return []Result{{Fail, ergoSurface, err.Error()}}
	}
	conf, err := ergoconf.Install(datastore, addr)
	if err != nil {
		return []Result{{Fail, ergoSurface, err.Error()}}
	}

	srv, err := startErgo(path, conf, datastore)
	if err != nil {
		return []Result{{Fail, ergoSurface, fmt.Sprintf("start ergo: %v", err)}}
	}
	defer srv.stop()
	if err := srv.waitListening(ctx, addr); err != nil {
		return []Result{{Fail, ergoSurface, fmt.Sprintf("ergo did not come up on %s from the embedded configuration: %v", addr, err)}}
	}

	sent, err := roundTrip(ctx, addr, filepath.Join(tmp, "state"))
	if err != nil {
		return []Result{{Fail, ergoSurface, fmt.Sprintf("a throwaway ergo on %s: %v", addr, err)}}
	}
	return []Result{{OK, ergoSurface, fmt.Sprintf("a throwaway ergo on %s took a message from %s and %s read it back (id %s); stopped and removed", addr, liveSender, liveReceiver, sent.ID)}}
}

// roundTrip has one client send and another read the message back, through
// Deliverers over the IRC adapter. Each registers its account on first use,
// with a password kept under stateDir.
func roundTrip(ctx context.Context, addr, stateDir string) (stream.Message, error) {
	spec, err := irc.ParseSpec("irc://" + addr + "/" + liveChannel)
	if err != nil {
		return stream.Message{}, err
	}
	open := func(nick string) (*stream.Deliverer, func(), error) {
		s, err := irc.Open(spec, nick, irc.NewStateCredentials(stateDir, nick))
		if err != nil {
			return nil, nil, err
		}
		return stream.NewDeliverer(s, nick), func() { _ = s.Close() }, nil
	}
	sender, closeSender, err := open(liveSender)
	if err != nil {
		return stream.Message{}, err
	}
	defer closeSender()
	receiver, closeReceiver, err := open(liveReceiver)
	if err != nil {
		return stream.Message{}, err
	}
	defer closeReceiver()

	sent, err := sender.Send(ctx, liveBody)
	if err != nil {
		return stream.Message{}, fmt.Errorf("%s could not send: %w", liveSender, err)
	}
	got, err := receiver.Wait(ctx, "", 20*time.Second, 100*time.Millisecond)
	if err != nil {
		return stream.Message{}, fmt.Errorf("%s could not read: %w", liveReceiver, err)
	}
	for _, m := range got.Messages {
		if stream.NickEqual(m.Author, liveSender) && m.Body == liveBody && m.ID == sent.ID {
			return sent, nil
		}
	}
	return stream.Message{}, fmt.Errorf("%s sent id %s but %s read %d messages and none was it", liveSender, sent.ID, liveReceiver, len(got.Messages))
}

func freeLoopbackAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

// ergoProc is a throwaway ergo this package started.
type ergoProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	log  *limitBuffer
}

func startErgo(bin, conf, dir string) (*ergoProc, error) {
	p := &ergoProc{done: make(chan struct{}), log: &limitBuffer{limit: 4096}}
	p.cmd = exec.Command(bin, "run", "--conf", conf)
	p.cmd.Dir = dir
	p.cmd.Stdout, p.cmd.Stderr = p.log, p.log
	if err := p.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_ = p.cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// waitListening polls until addr accepts a connection, ergo has exited, or the
// startup allowance is spent.
func (p *ergoProc) waitListening(ctx context.Context, addr string) error {
	deadline := time.NewTimer(ergoStartup)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			return fmt.Errorf("it exited: %s", codex.PlainLine(firstLine(strings.TrimSpace(p.log.String()))))
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("nothing listened within %s", ergoStartup)
		case <-tick.C:
			if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
				_ = c.Close()
				return nil
			}
		}
	}
}

// stop ends the server and waits for it, so that the directory it lives in is
// not removed from under a running process: SIGTERM, which ergo takes as a
// request to shut down, and a kill if it has not gone within the grace.
func (p *ergoProc) stop() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(ergoStopGrace):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}
