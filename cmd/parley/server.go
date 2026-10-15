package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/colinsurprenant/parley/internal/doctor"
	"github.com/colinsurprenant/parley/internal/ergoconf"
	"github.com/colinsurprenant/parley/internal/stream/irc"
)

// defaultListen is where the per-machine server listens and where every verb's
// documented stream spec and `parley doctor` look for it.
const defaultListen = "127.0.0.1:6667"

// exitStatus is an exit code that is not one of parley's own three: `parley
// server` ends with the status ergo ended with.
type exitStatus struct {
	code int
	msg  string
}

func (e *exitStatus) Error() string { return e.msg }

// cmdServer runs ergo, the per-machine IRC server every table shares, in the
// foreground. It renders the embedded configuration into the datastore
// directory and runs `ergo run --conf <that file>` there, so one command starts
// the server whatever directory it is run from, and the accounts in the
// datastore carry from one run to the next. There is no daemon: the terminal it
// runs in is the server's lifetime.
func cmdServer(args []string) error {
	fs := flag.NewFlagSet("parley server", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: parley server [--listen 127.0.0.1:6667] [--datastore <dir>] [--ergo-bin ergo]")
		fs.PrintDefaults()
	}
	listen := fs.String("listen", defaultListen, "the loopback IP:port to listen on, plaintext; anything that is not on this machine, or a host name, is refused")
	datastore := fs.String("datastore", "", "the directory holding ergo's account database and the configuration rendered for this run; made 0700 if missing and otherwise left as it is (default $XDG_DATA_HOME/parley-ergo, else ~/.local/share/parley-ergo)")
	ergoBin := fs.String("ergo-bin", doctor.DefaultErgoBin, "the ergo binary to run, resolved on PATH unless it holds a separator")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	if fs.NArg() > 0 {
		return cfgErrf("unexpected argument %q", fs.Arg(0))
	}
	addr, err := irc.LoopbackAddr(*listen)
	if err != nil {
		return cfgErrf("--listen %q: %v", *listen, err)
	}
	if given(fs, "datastore") && *datastore == "" {
		return cfgErrf("--datastore must not be empty")
	}
	// Resolved before anything is written: a missing ergo is the invocation's
	// mistake, and must not leave a datastore directory behind to say so.
	bin, err := doctor.Resolve(*ergoBin)
	if err != nil {
		return cfgErrf("--ergo-bin %q: %v: %s", *ergoBin, err, doctor.ErgoInstallHint)
	}
	dir, err := datastoreDir(*datastore)
	if err != nil {
		return cfgErrf("--datastore: %v", err)
	}
	// MkdirAll creates what is missing at 0700 and changes nothing about a
	// directory that is already there: a datastore made by an earlier setup keeps
	// the mode its owner gave it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the datastore directory: %w", err)
	}
	conf, err := ergoconf.Install(dir, addr)
	if err != nil {
		return err
	}
	// This run's own configuration: no other start can rewrite it under ergo,
	// and it goes when ergo does.
	defer os.Remove(conf)
	return runErgo(bin, conf, dir, addr)
}

// datastoreDir is the absolute datastore directory for the --datastore value.
// When it is empty: $XDG_DATA_HOME/parley-ergo, or ~/.local/share/parley-ergo
// with no $XDG_DATA_HOME. contrib/ergo/README.md always told people to start
// ergo from ~/.local/share/parley-ergo, whatever $XDG_DATA_HOME said, so that
// directory is used while it exists and the XDG one does not: the accounts made
// there carry over.
func datastoreDir(flagValue string) (string, error) {
	if flagValue != "" {
		return filepath.Abs(flagValue)
	}
	home, homeErr := os.UserHomeDir()
	root := os.Getenv("XDG_DATA_HOME")
	if root == "" {
		if homeErr != nil {
			return "", fmt.Errorf("locate the home directory: %w", homeErr)
		}
		return filepath.Join(home, ".local", "share", "parley-ergo"), nil
	}
	dir := filepath.Join(root, "parley-ergo")
	// With no home to look in there is no old datastore to keep, and the XDG
	// path stands on its own.
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) && homeErr == nil {
		legacy := filepath.Join(home, ".local", "share", "parley-ergo")
		if fi, err := os.Stat(legacy); err == nil && fi.IsDir() {
			return legacy, nil
		}
	}
	return filepath.Abs(dir)
}

// The two texts ergo writes when a second `parley server` meets the first, as
// ergo 2.19.1 writes them (cmd/parley/testdata holds the lines they were taken
// from). The first is its refusal of a datastore whose lock file another ergo
// holds, the second its refusal of a listener another process already has.
const (
	ergoFlockText = "Couldn't acquire flock (is another Ergo running?)"
	ergoBindText  = "bind: address already in use"
)

// ergoWatch passes what ergo writes through unchanged and notes whether either
// of the two texts above went by, whichever of its two streams carried it.
type ergoWatch struct {
	mu          sync.Mutex
	flock, bind bool
}

// tee returns a writer that copies to w and watches what it copies. Each stream
// gets its own, so a text split across two writes is still found.
func (e *ergoWatch) tee(w io.Writer) *teeWatch { return &teeWatch{e: e, w: w} }

type teeWatch struct {
	e       *ergoWatch
	w       io.Writer
	tail    string // the end of what has gone by, one byte short of the longest text
	midLine bool   // what went by did not end in a newline
}

func (t *teeWatch) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 {
		t.midLine = p[n-1] != '\n'
	}
	seen := t.tail + string(p[:n])
	t.e.mu.Lock()
	t.e.flock = t.e.flock || strings.Contains(seen, ergoFlockText)
	t.e.bind = t.e.bind || strings.Contains(seen, ergoBindText)
	t.e.mu.Unlock()
	if keep := len(ergoFlockText) - 1; len(seen) > keep {
		seen = seen[len(seen)-keep:]
	}
	t.tail = seen
	return n, err
}

// hints is the line to add after ergo's own output, for each of the two
// failures that were seen.
func (e *ergoWatch) hints(dir, addr string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var lines []string
	if e.flock {
		lines = append(lines, fmt.Sprintf("parley server: another ergo is using this datastore (%s); `pgrep -l ergo` lists the ergo processes running", dir))
	}
	if e.bind {
		lines = append(lines, fmt.Sprintf("parley server: %s: something already listens there, possibly another parley server; `pgrep -l ergo` lists the ergo processes running", addr))
	}
	return lines
}

// runErgo runs ergo in the foreground and returns when it has exited. Its
// output passes straight through, and when it exits non-zero having refused to
// start for one of the two reasons above, one line per reason follows it. An
// interrupt or a terminate signal is passed on to it, and a hangup, which ends a
// terminal's session and which ergo would take for a rehash, is passed on as a
// terminate: parley stops the server it started when its terminal goes. The
// status is ergo's own: nil after it was stopped by the signal that was first
// forwarded, an exit code otherwise. The signals are caught before it starts, so
// none is lost in between, and the wait is for ergo to be gone — a stopped
// server leaves no process behind.
func runErgo(bin, conf, dir, addr string) error {
	cmd := exec.Command(bin, "run", "--conf", conf)
	cmd.Dir = dir
	var watch ergoWatch
	errTee := watch.tee(os.Stderr)
	cmd.Stdout, cmd.Stderr = watch.tee(os.Stdout), errTee
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	fmt.Fprintf(os.Stderr, "parley server: running %s on %s, datastore %s\n", bin, addr, dir)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ergo: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var forwarded os.Signal
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				sig = syscall.SIGTERM
			}
			// The first signal is the one ergo is stopping for; a later one is
			// passed on but does not change what its death means.
			if forwarded == nil {
				forwarded = sig
			}
			_ = cmd.Process.Signal(sig)
		case err := <-done:
			status := ergoStatus(err, forwarded)
			var exit *exitStatus
			if errors.As(status, &exit) {
				lines := watch.hints(dir, addr)
				if len(lines) > 0 && errTee.midLine {
					fmt.Fprintln(os.Stderr)
				}
				for _, line := range lines {
					fmt.Fprintln(os.Stderr, line)
				}
			}
			return status
		}
	}
}

// ergoStatus turns how ergo ended into parley's own end: nothing for a clean
// exit and for a stop by the signal parley forwarded first, ergo's code for any
// other exit, and the shell's 128+signal for a death by a signal that was not
// that one.
func ergoStatus(err error, forwarded os.Signal) error {
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return fmt.Errorf("wait for ergo: %w", err)
	}
	if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		if forwarded != nil && ws.Signal() == forwarded {
			return nil
		}
		return &exitStatus{code: 128 + int(ws.Signal()), msg: fmt.Sprintf("ergo was killed by signal %d (%v)", int(ws.Signal()), ws.Signal())}
	}
	return &exitStatus{code: exit.ExitCode(), msg: fmt.Sprintf("ergo exited with status %d", exit.ExitCode())}
}
