package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/colinsurprenant/parley/internal/primer"
	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream"
)

// A line is a few long-running sessions, each in a checkout of its own, and one
// human on one channel, under --skill line. What `line new` keeps of it is
// <state base>/lines/<id>/line.json, the record `line add`, `line drop` and `line
// end` read and write; the rest is elsewhere: each session's launch file in its
// checkout (.mcp.json or .codex/config.toml, and a line in the checkout's
// info/exclude), a Codex session's thread record under its own nick's directory
// of the base, its reply directory under $TMPDIR, and the id's lock
// <base>/.ids/<id>.lock. "lines" sits beside the per-nick directories of the
// same base, as "tables" and "builds" do, which is why no participant may be
// called any of the three.
//
// What parley trusts, for the commands in line_new.go, line_add.go and
// line_drop.go (written out in docs/line.md, "What parley trusts"):
//
//   - Trusted: parley's own state (line.json under <state base>/lines/<id>/,
//     made 0700; the thread records, cursors and lock files under the nicks' own
//     directories of the base; the id's lock), and the flags the human gives.
//     A deliberate hand edit of that state is out of scope.
//   - Not parley's state: the user's checkouts. A launch file there is the
//     line's only if its bytes still hash to the digest the record holds, and
//     only that proof lets `line drop` delete it. Nothing in a checkout is
//     overwritten, written through a link or removed on any other ground.
//   - Untrusted: Codex's output and the thread id it announces (checked to be a
//     UUID), git's output, and what a session wrote in its checkout.
const (
	linesDirName    = state.LinesDir
	lineRecordName  = "line.json"
	lineReplyMarker = ".parley-line"

	// lineMCPName and lineCodexConfigName are the launch files, relative to the
	// checkout, and the lines `line add` puts in info/exclude for them.
	lineMCPName         = ".mcp.json"
	lineCodexDirName    = ".codex"
	lineCodexConfigName = ".codex/config.toml"
)

// lineRecordMax bounds what readLineRecord reads: the line's facts and a few
// sessions' paths, a few KiB. A larger file is damage, and `line add` refuses to
// write one that `line drop` and `line end` would then refuse to read.
const lineRecordMax = 64 << 10

func lineDirOf(base, id string) string { return filepath.Join(base, linesDirName, id) }

// lineReplyPrefix is the name of a Codex session's reply directory up to the
// random string os.MkdirTemp adds.
func lineReplyPrefix(id string) string { return "parley-line-" + id + "-" }

func checkLineID(id string) error {
	if !tableIDRE.MatchString(id) {
		return cfgErrf("line id %q must match [a-z0-9][a-z0-9-]{0,31}: it is the channel #<id> and a directory name", id)
	}
	return nil
}

// checkNotALine refuses, for the table or build that is being made (what), an id
// that is live as a line: the id is one namespace for the three, the channel and
// the lock. Called under the id's lock.
func checkNotALine(base, id, what string) error {
	if _, err := os.Lstat(lineDirOf(base, id)); err == nil {
		return cfgErrf("%q is already a line under %s: parley line end %s takes it down, and %s takes another id", id, base, id, what)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("look for %s: %w", lineDirOf(base, id), err)
	}
	return nil
}

// cmdLine runs the line verbs: new sets a line up, add puts a session on it,
// drop takes one off and end takes the line down.
func cmdLine(args []string) error {
	if len(args) == 0 {
		lineUsage(os.Stderr)
		return cfgErrf("parley line needs a verb: new, add, drop or end")
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		lineUsage(os.Stdout)
		return nil
	case "new":
		return cmdLineNew(args[1:])
	case "add":
		return cmdLineAdd(args[1:])
	case "drop":
		return cmdLineDrop(args[1:])
	case "end":
		return cmdLineEnd(args[1:])
	}
	lineUsage(os.Stderr)
	return cfgErrf("unknown line verb %q: the verbs are new, add, drop and end", args[0])
}

func lineUsage(w io.Writer) {
	fmt.Fprint(w, lineUsageNew+"\n"+lineUsageAdd+"\n"+lineUsageDrop+"\n"+lineUsageEnd+"\n")
}

const (
	lineUsageNew  = "usage: parley line new <id> --chair <nick> --cap <n> [--window 10m] [--server 127.0.0.1:6667] [--state-dir <dir>] [--codex-bin codex]"
	lineUsageAdd  = "usage: parley line add <id> --nick <nick> --harness claude|codex [--model <model>] --checkout <absolute dir> [--state-dir <dir>]"
	lineUsageDrop = "usage: parley line drop <id> <nick> [--force] [--state-dir <dir>]"
	lineUsageEnd  = "usage: parley line end <id> [--force] [--state-dir <dir>]"
)

// parseInterleaved parses fs over args with the positional arguments allowed
// anywhere among the flags, as `parley line drop demo plan --force` and `parley
// line drop demo --force plan` both write it, and returns the positionals in
// order. The flag package stops at the first one, so it is parsed again after
// each. A help request is flag.ErrHelp, a bad flag any other error, as with
// parseFlags.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := parseFlags(fs, args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// lineRecord is what `line new` keeps of a line so that the other verbs can act
// on it: the line's facts, which every session inherits, and what each session
// made. It holds no stream content, only names, paths and digests.
type lineRecord struct {
	ID       string `json:"id"`
	Stream   string `json:"stream"`
	Server   string `json:"server"`
	StateDir string `json:"state_dir"`
	Chair    string `json:"chair"`
	Cap      int    `json:"cap"`
	// Window is the breaker's window as it is written on a command line: 10m, 90s.
	Window string `json:"window"`
	// CodexBin is the resolved codex when --codex-bin was given to `line new`, and
	// empty when plain codex on the PATH is meant.
	CodexBin string        `json:"codex_bin,omitempty"`
	Created  string        `json:"created"`
	Sessions []lineSession `json:"sessions"`
}

// lineSession is one session of the line. Thread and ReplyDir are a Codex
// session's.
type lineSession struct {
	Nick     string     `json:"nick"`
	Harness  string     `json:"harness"`
	Model    string     `json:"model,omitempty"`
	Checkout string     `json:"checkout"`
	Thread   string     `json:"thread,omitempty"`
	ReplyDir string     `json:"reply_dir,omitempty"`
	Files    []lineFile `json:"files"`
}

// lineFile is a launch file the line wrote in a checkout: its path, the digest
// of its exact bytes, and what else the line made for it. Every session of one
// checkout and harness lists the same file, and the same entry for it; the
// file is the line's until the last of them is dropped.
type lineFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	// ExcludeFile is the file the line appended the file's line to (the checkout's
	// info/exclude), and empty when it added none: there was no git checkout, or the
	// line was already there.
	ExcludeFile string `json:"exclude_file,omitempty"`
	// ExcludeSep is whether the exclude file did not end in a newline, so that the
	// line had to end its last line first: that newline is the line's too, and goes
	// with it when it is the last.
	ExcludeSep bool `json:"exclude_sep,omitempty"`
	// MadeDir is whether the line made the .codex directory the file is in.
	MadeDir bool `json:"made_dir,omitempty"`
}

// windowDuration is the breaker's window; the record check has shown it parses.
func (r lineRecord) windowDuration() time.Duration {
	d, _ := time.ParseDuration(r.Window)
	return d
}

// session finds the session called nick, matching nicks the way the server does.
func (r lineRecord) session(nick string) (int, bool) {
	for i, s := range r.Sessions {
		if stream.NickEqual(s.Nick, nick) {
			return i, true
		}
	}
	return 0, false
}

// sharing is the sessions of the record, but skip (an index, or -1), that use the
// launch file harness has in checkout: the same harness, the same directory.
func (r lineRecord) sharing(harness, checkout string, skip int) []lineSession {
	var out []lineSession
	for i, s := range r.Sessions {
		if i != skip && s.Harness == harness && sameDir(s.Checkout, checkout) {
			out = append(out, s)
		}
	}
	return out
}

// sameDir reports whether two paths name one directory, links resolved as far as
// they exist.
func sameDir(a, b string) bool { return resolveExisting(a) == resolveExisting(b) }

// launchFileOf is the launch file a session of this harness has in checkout, and
// the line `line add` appends to the exclude file for it.
func launchFileOf(harness, checkout string) (path, excludeLine string) {
	if harness == harnessCodex {
		return filepath.Join(checkout, lineCodexDirName, "config.toml"), "/" + lineCodexConfigName
	}
	return filepath.Join(checkout, lineMCPName), "/" + lineMCPName
}

func noLineErr(base, id string) error {
	return cfgErrf("no line %q under %s (looked for %s)", id, base, filepath.Join(lineDirOf(base, id), lineRecordName))
}

// lineDirIsThere is whether anything is at the line's directory: the verbs ask it
// before they take the id's lock, so that an id that was never a line leaves no
// lock file in the base. The record is read after the lock is taken, and that
// read is the one that counts.
func lineDirIsThere(base, id string) bool {
	_, err := os.Lstat(lineDirOf(base, id))
	return !errors.Is(err, fs.ErrNotExist)
}

// readLineRecord loads and checks the record of line id under base, and returns
// the bytes it read too, which a rollback puts back. A line that is not there is
// the invocation's mistake; a record that is there and is not one this binary
// wrote is not trusted, because the verbs delete the paths it names.
func readLineRecord(base, id string) (lineRecord, []byte, error) {
	path := filepath.Join(lineDirOf(base, id), lineRecordName)
	b, err := readRecordFile(path, "line", lineRecordMax)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return lineRecord{}, nil, noLineErr(base, id)
		}
		return lineRecord{}, nil, err
	}
	var rec lineRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return lineRecord{}, nil, fmt.Errorf("line record %s is not valid: %w", path, err)
	}
	if err := rec.check(id, base); err != nil {
		return lineRecord{}, nil, fmt.Errorf("line record %s is not valid: %w", path, err)
	}
	return rec, b, nil
}

// writeLineRecord stores the record in dir as line.json, mode 0600, through a
// temp file and a rename, and refuses one the reader would refuse for its size.
// It returns the bytes it wrote.
func writeLineRecord(dir string, rec lineRecord) ([]byte, error) {
	if rec.Sessions == nil {
		rec.Sessions = []lineSession{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rec); err != nil {
		return nil, fmt.Errorf("encode the line record: %w", err)
	}
	if buf.Len() > lineRecordMax {
		return nil, fmt.Errorf("the line record would be %d bytes, over the %d bytes the line verbs read, and they would then refuse to take the line down: shorten what it holds (the paths, the models)", buf.Len(), lineRecordMax)
	}
	if err := writeFileAtomic(filepath.Join(dir, lineRecordName), buf.Bytes(), 0o600); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// check holds the record to what a `line new` and the `line add`s after it write.
func (r lineRecord) check(id, base string) error {
	if r.ID != id {
		return fmt.Errorf("it is line %q, not %q", r.ID, id)
	}
	if !filepath.IsAbs(r.StateDir) || filepath.Clean(r.StateDir) != filepath.Clean(base) {
		return fmt.Errorf("it was made under the state directory %q, not %q", r.StateDir, base)
	}
	if r.Stream == "" || !strings.HasPrefix(r.Stream, "irc://"+r.Server+"/") {
		return fmt.Errorf("stream %q is not on server %q", r.Stream, r.Server)
	}
	if err := stream.ValidNick(r.Chair); err != nil {
		return fmt.Errorf("chair %q %v", r.Chair, err)
	}
	if r.Cap < 0 {
		return errors.New("the cap is negative")
	}
	if d, err := time.ParseDuration(r.Window); err != nil || d <= 0 {
		return fmt.Errorf("window %q is not a duration above zero", r.Window)
	}
	if r.CodexBin != "" && !filepath.IsAbs(r.CodexBin) {
		return fmt.Errorf("codex_bin %q is not an absolute path", r.CodexBin)
	}
	for i, s := range r.Sessions {
		if err := s.check(r); err != nil {
			return fmt.Errorf("session %d (%q): %w", i+1, s.Nick, err)
		}
		if stream.NickEqual(s.Nick, r.Chair) {
			return fmt.Errorf("session nick %q is the chair's", s.Nick)
		}
		for _, prev := range r.Sessions[:i] {
			if stream.NickEqual(s.Nick, prev.Nick) {
				return fmt.Errorf("session nick %q is also session %q's", s.Nick, prev.Nick)
			}
		}
	}
	return nil
}

var sha256DigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (s lineSession) check(r lineRecord) error {
	if err := stream.ValidNick(s.Nick); err != nil {
		return fmt.Errorf("nick %v", err)
	}
	switch s.Harness {
	case harnessClaude, harnessCodex:
	default:
		return fmt.Errorf("harness %q is neither claude nor codex", s.Harness)
	}
	if s.Model != "" {
		if err := checkModel("model", s.Model, s.Model); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(s.Checkout) {
		return fmt.Errorf("checkout %q is not an absolute path", s.Checkout)
	}
	if s.Thread != "" && (s.Harness != harnessCodex || !state.ValidThreadID(s.Thread)) {
		return fmt.Errorf("thread %q is not a Codex session's UUID", s.Thread)
	}
	if s.Harness == harnessCodex && s.Thread == "" {
		return errors.New("a Codex session has no thread")
	}
	if s.ReplyDir != "" {
		if s.Harness != harnessCodex || !filepath.IsAbs(s.ReplyDir) || !strings.HasPrefix(filepath.Base(s.ReplyDir), lineReplyPrefix(r.ID)) {
			return fmt.Errorf("reply_dir %q is not a reply directory of line %q", s.ReplyDir, r.ID)
		}
	}
	if len(s.Files) != 1 {
		return fmt.Errorf("it lists %d launch files, and a session of the line has exactly one", len(s.Files))
	}
	f := s.Files[0]
	if want, _ := launchFileOf(s.Harness, s.Checkout); filepath.Clean(f.Path) != want {
		return fmt.Errorf("launch file %q is not %q", f.Path, want)
	}
	if !sha256DigestRE.MatchString(f.SHA256) {
		return fmt.Errorf("digest %q is not sha256: and 64 hex digits", f.SHA256)
	}
	if f.ExcludeFile != "" && !filepath.IsAbs(f.ExcludeFile) {
		return fmt.Errorf("exclude_file %q is not an absolute path", f.ExcludeFile)
	}
	if f.ExcludeSep && f.ExcludeFile == "" {
		return errors.New("exclude_sep without an exclude_file")
	}
	if f.MadeDir && s.Harness != harnessCodex {
		return errors.New("only a Codex session's file is in a directory the line made")
	}
	return nil
}

// lineWindowFlag is the spelling of d the record and the launch files carry.
func lineWindowFlag(d time.Duration) string { return primer.FormatWindow(d) }
