// Package file implements the parley stream over one append-only NDJSON file:
// one JSON object per line, {"author":...,"ts":...,"body":...}. It is the
// development and test transport, and the one a human can drive with stock
// tools — `tail -f` to read, `echo >> file` to write.
//
// The file is the sequencing authority: a message's ID is the byte offset of
// its line, as a decimal string. A cursor is "<offset>:<fingerprint>", the byte
// offset just past the last line delivered together with a hash of that line.
// The offset alone cannot tell a file that grew from one that was rewritten to
// the same length, so the fingerprint is what turns a rotated or rewritten file
// into a reported history gap instead of a silent resume at an offset that now
// means something else.
//
// Rotating or replacing the file while participants are live is not a
// supported operation: flock serializes appends, not a rename by a stock tool,
// so a replacement landing between the inode check and the write can leave one
// message on the detached inode. Readers detect the rotation as a gap; Send
// does not promise a lossless append across it.
package file

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/colinsurprenant/parley/internal/stream"
)

// MaxLine caps what one line may occupy, newline included. A line longer than
// this is not a message any parley writer produces, and buffering it whole
// would let one runaway writer exhaust the reader's memory; it is skipped. It is
// exported because a caller reading a body from a pipe has to bound the read
// before it has a line to refuse.
const MaxLine = 1 << 20

// readBuf is the scan buffer; long lines are accumulated across refills.
const readBuf = 64 << 10

// fingerprintLen is how much of a line's digest goes into the cursor. The
// fingerprint only has to distinguish the line that was there from the line
// that replaced it, so 64 bits is ample and keeps the cursor short.
const fingerprintLen = 16

// startCursor is the start of the file: offset zero has no preceding line, so
// it carries no fingerprint.
const startCursor = stream.Cursor("0")

// errLineTooLong reports a line over MaxLine; it carries no message, only the
// byte count to step over.
var errLineTooLong = errors.New("file stream: line exceeds the maximum size")

// ErrMessageTooLong reports a message whose encoded line would not fit in
// MaxLine. It is refused at the writer rather than appended as a line every
// reader skips.
var ErrMessageTooLong = errors.New("message exceeds the maximum line size")

// lockAttempts bounds the reopen loop in lockedForAppend: one attempt per
// rotation that beat this writer to the lock. A path that keeps changing inode
// under an append is not a stream to keep guessing at.
const lockAttempts = 5

// lockRetryInterval is how long a writer waits between non-blocking attempts at
// the append lock: short enough to be invisible behind a normal append, long
// enough not to spin on a writer that is slow.
const lockRetryInterval = 5 * time.Millisecond

// Stream is an append-only NDJSON chat file.
type Stream struct {
	path         string
	nick         string
	pollInterval time.Duration
}

// Option configures a Stream.
type Option func(*Stream)

// WithPollInterval sets how often Wait re-reads the file to notice new lines.
func WithPollInterval(d time.Duration) Option {
	return func(s *Stream) { s.pollInterval = d }
}

type record struct {
	Author string `json:"author"`
	TS     string `json:"ts"`
	Body   string `json:"body"`
}

// Open prepares the stream at path for the participant nick, creating the file
// if it does not exist so a reader can `tail -f` it before the first message.
func Open(path, nick string, opts ...Option) (*Stream, error) {
	if path == "" {
		return nil, errors.New("file stream: empty path")
	}
	if nick == "" {
		return nil, errors.New("file stream: empty nick")
	}
	s := &Stream{path: path, nick: nick, pollInterval: stream.DefaultPollInterval}
	for _, opt := range opts {
		opt(s)
	}
	f, err := s.open()
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("file stream: close %s: %w", path, err)
	}
	return s, nil
}

// Path returns the file backing this stream.
func (s *Stream) Path() string { return s.path }

// Send appends one line authored by this participant and returns the message as
// a reader will see it: the ID is the byte offset at which the record starts,
// which is not where the write starts when a torn line made it open with a
// newline, and the TS is the one written into the record. Everything that makes
// the record true of its turn in the stream — the cancel check, the timestamp,
// the state of the file's tail — is decided after the exclusive lock is held,
// since waiting for it can take as long as the writer ahead takes to finish.
func (s *Stream) Send(ctx context.Context, body string) (_ stream.Message, err error) {
	if err := ctx.Err(); err != nil {
		return stream.Message{}, err
	}
	f, err := s.lockedForAppend(ctx)
	if err != nil {
		return stream.Message{}, err
	}
	defer func() {
		uerr := flock(f, syscall.LOCK_UN)
		cerr := f.Close()
		switch {
		case err != nil:
		case uerr != nil:
			err = fmt.Errorf("file stream: unlock %s: %w", s.path, uerr)
		case cerr != nil:
			err = fmt.Errorf("file stream: close %s: %w", s.path, cerr)
		}
	}()
	// The wait for the lock can outlast the caller: a cancel that landed while
	// the writer ahead was finishing must not become an append nobody is waiting
	// for any more. The lock is released by the deferred close.
	if err := ctx.Err(); err != nil {
		return stream.Message{}, err
	}

	// UTC strips the monotonic reading, so the time returned is the instant
	// the record spells and a reader parses back.
	now := time.Now().UTC()
	line, err := json.Marshal(record{
		Author: s.nick,
		TS:     now.Format(time.RFC3339Nano),
		Body:   body,
	})
	if err != nil {
		return stream.Message{}, fmt.Errorf("file stream: encode message: %w", err)
	}
	line = append(line, '\n')
	if len(line) > MaxLine {
		return stream.Message{}, fmt.Errorf("file stream: %w: encoded line is %d bytes, the maximum is %d", ErrMessageTooLong, len(line), MaxLine)
	}

	fi, err := f.Stat()
	if err != nil {
		return stream.Message{}, fmt.Errorf("file stream: stat %s: %w", s.path, err)
	}
	// A torn tail is closed by one byte written ahead of the line, so the line
	// starts one past where the write does, and it is the line's start that a
	// reader reports as the message's ID.
	out, start := line, fi.Size()
	if fi.Size() > 0 {
		var b [1]byte
		if _, err := f.ReadAt(b[:], fi.Size()-1); err != nil {
			return stream.Message{}, fmt.Errorf("file stream: read tail of %s: %w", s.path, err)
		}
		// A writer that died mid-append leaves a line with no newline. Pasting
		// this record onto it would lose both; one byte closes the torn line and
		// costs nothing when the file is intact.
		if b[0] != '\n' {
			out = append([]byte{'\n'}, line...)
			start++
		}
	}
	if _, err := f.Write(out); err != nil {
		return stream.Message{}, fmt.Errorf("file stream: append to %s: %w", s.path, err)
	}
	// The flock is advisory and a stock `>>` takes none, so a line appended
	// between the Stat and the write moves where this one landed. The write is
	// positioned at the end of the file atomically and leaves this descriptor's
	// own offset just past it, which is the line's true end; the Stat's figure
	// stands only if the offset cannot be read.
	if end, err := f.Seek(0, io.SeekCurrent); err == nil {
		start = end - int64(len(line))
	}
	return stream.Message{ID: strconv.FormatInt(start, 10), Author: s.nick, TS: now, Body: body}, nil
}

// lockedForAppend returns the file with its exclusive lock held, reopening if
// the path was rotated while this writer waited for the lock. Every attempt
// goes through lockExclusive, so a canceled context ends the wait wherever it
// is: waiting for the first lock and waiting for the lock on the file a
// rotation replaced it with are the same wait to the caller.
func (s *Stream) lockedForAppend(ctx context.Context) (*os.File, error) {
	for range lockAttempts {
		f, err := s.open()
		if err != nil {
			return nil, err
		}
		if err := s.lockExclusive(ctx, f); err != nil {
			f.Close()
			return nil, err
		}
		current, err := s.isCurrent(f)
		if err != nil {
			release(f)
			return nil, err
		}
		if current {
			return f, nil
		}
		// The path names a different inode than the one we hold: appending here
		// would write into a file nobody reads any more. Drop it and take the
		// lock on the file the path names now.
		release(f)
	}
	return nil, fmt.Errorf("file stream: %s was rotated out from under the append lock %d times running", s.path, lockAttempts)
}

// lockExclusive waits for f's exclusive lock without blocking in the kernel:
// flock(2) takes no deadline, so the only form that can be given up is the
// non-blocking one under a timer. A canceled context returns its own error and
// no lock.
func (s *Stream) lockExclusive(ctx context.Context, f *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("file stream: lock %s: %w", s.path, err)
		}
		if err := sleep(ctx, lockRetryInterval); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isCurrent reports whether f is still the file the path names.
func (s *Stream) isCurrent(f *os.File) (bool, error) {
	fi, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("file stream: stat %s: %w", s.path, err)
	}
	pi, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("file stream: stat %s: %w", s.path, err)
	}
	return os.SameFile(fi, pi), nil
}

func (s *Stream) open() (*os.File, error) {
	// O_RDWR, not O_WRONLY: Send reads the last byte of the file to detect a
	// torn line before appending to it.
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("file stream: open %s: %w", s.path, err)
	}
	return f, nil
}

// flock takes or drops f's advisory lock, retrying when a signal interrupts the
// call: the Go runtime preempts goroutines with signals, so flock returns EINTR
// under nothing more exotic than a busy process.
func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func release(f *os.File) {
	_ = flock(f, syscall.LOCK_UN)
	_ = f.Close()
}

// Read returns the complete lines after cursor. A context canceled while it
// scans ends the scan: the messages parsed so far, the cursor just past them,
// and the context's error come back together, so the caller keeps its progress.
func (s *Stream) Read(ctx context.Context, cursor stream.Cursor) ([]stream.Message, stream.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, cursor, err
	}
	off, fp, err := parseCursor(cursor)
	if err != nil {
		return nil, cursor, err
	}

	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if off == 0 {
				return nil, startCursor, nil
			}
			return nil, startCursor, gap(cursor)
		}
		return nil, cursor, fmt.Errorf("file stream: open %s: %w", s.path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, cursor, fmt.Errorf("file stream: stat %s: %w", s.path, err)
	}
	size := fi.Size()
	if off > size {
		return nil, startCursor, gap(cursor)
	}
	if off > 0 {
		ok, err := verify(f, off, fp)
		if err != nil {
			return nil, cursor, err
		}
		if !ok {
			return nil, startCursor, gap(cursor)
		}
	}
	if off == size {
		return nil, held(cursor), nil
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, cursor, fmt.Errorf("file stream: seek %s: %w", s.path, err)
	}

	var (
		msgs     []stream.Message
		lastLine []byte
		readErr  error
		skipped  bool // the last line consumed was over-long and discarded
	)
	pos := off
	r := bufio.NewReaderSize(f, readBuf)
	for {
		// A backlog can be arbitrarily long and a busy file keeps growing under
		// the reader, so the caller's bound is checked per line rather than only
		// on the way in. What was parsed so far is kept and the cursor reported
		// just past it, so the next read resumes without repeating or skipping.
		if err := ctx.Err(); err != nil {
			readErr = err
			break
		}
		line, n, rerr := readLine(ctx, r)
		if rerr != nil {
			if errors.Is(rerr, errLineTooLong) {
				// Step over it: one line no reader can deliver must not wedge
				// the stream for every line behind it.
				pos += n
				lastLine, skipped = nil, true
				continue
			}
			if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
				readErr = rerr
				break
			}
			// Anything not terminated by a newline is a writer mid-append: it
			// is never delivered and the cursor stays before it, so the line is
			// picked up whole on the next read.
			if !errors.Is(rerr, io.EOF) {
				readErr = fmt.Errorf("file stream: read %s: %w", s.path, rerr)
			}
			break
		}
		start := pos
		pos += n
		lastLine, skipped = line, false
		m, ok := decode(line, start)
		if !ok {
			// A hand-written line can be malformed; skipping it (and advancing
			// past it) keeps one bad line from wedging the stream for good.
			continue
		}
		msgs = append(msgs, m)
	}
	if pos == off {
		return msgs, held(cursor), readErr
	}
	end := fingerprint(lastLine)
	if skipped {
		// The discarded line was never buffered, so its fingerprint comes from
		// the file, by the same backward scan verify uses.
		end, err = fingerprintAt(f, pos)
		if err != nil {
			return msgs, held(cursor), errors.Join(readErr, err)
		}
	}
	return msgs, lineCursor(pos, end), readErr
}

// Wait blocks for new lines. The file has no push channel, so the change signal
// is the poll of the file's size inside Read; an IRC adapter will implement
// Wait on server push instead.
func (s *Stream) Wait(ctx context.Context, cursor stream.Cursor, timeout, debounce time.Duration) ([]stream.Message, stream.Cursor, error) {
	return stream.PollWait(ctx, s, cursor, timeout, debounce, s.pollInterval)
}

// readLine returns the next complete line, including its newline. A line over
// MaxLine is discarded and reported as errLineTooLong with the bytes it
// occupies, so the caller can step the cursor past it. Any other error — a
// canceled context included, since one line may take as many refills as MaxLine
// allows — means no complete line is there: the count is meaningless and the
// caller must not advance.
func readLine(ctx context.Context, r *bufio.Reader) ([]byte, int64, error) {
	var (
		line    []byte
		n       int64
		tooLong bool
	)
	for {
		if err := ctx.Err(); err != nil {
			return nil, n, err
		}
		frag, err := r.ReadSlice('\n')
		n += int64(len(frag))
		switch {
		case err == nil:
			if tooLong || n > MaxLine {
				return nil, n, errLineTooLong
			}
			return append(line, frag...), n, nil
		case errors.Is(err, bufio.ErrBufferFull):
			if n > MaxLine {
				tooLong, line = true, nil
				continue
			}
			line = append(line, frag...)
		default:
			return nil, n, err
		}
	}
}

func decode(line []byte, start int64) (stream.Message, bool) {
	trimmed := bytes.TrimRight(line, "\r\n")
	if len(bytes.TrimSpace(trimmed)) == 0 {
		return stream.Message{}, false
	}
	var rec record
	if err := json.Unmarshal(trimmed, &rec); err != nil {
		return stream.Message{}, false
	}
	m := stream.Message{
		ID:     strconv.FormatInt(start, 10),
		Author: rec.Author,
		Body:   rec.Body,
	}
	// A timestamp written by hand may not parse; the body still gets delivered,
	// with the zero time marking the missing stamp.
	if ts, err := time.Parse(time.RFC3339Nano, rec.TS); err == nil {
		m.TS = ts
	}
	return m, true
}

// verify reports whether the cursor still points where it did: the byte before
// it must end a line, and that line must still hash to the cursor's
// fingerprint. A truncation, a rotation, or a rewrite to the same length fails
// one of the two, and the caller turns that into a history gap.
func verify(f *os.File, off int64, fp string) (bool, error) {
	if fp == "" {
		// A bare offset past the start of the file predates fingerprinted
		// cursors, or lost its fingerprint: it cannot be checked, so it is
		// treated as a gap rather than trusted.
		return false, nil
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], off-1); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, fmt.Errorf("file stream: read at %d: %w", off-1, err)
	}
	if b[0] != '\n' {
		return false, nil
	}
	got, err := fingerprintAt(f, off)
	if err != nil {
		return false, err
	}
	return got == fp, nil
}

// fingerprintAt hashes the line ending at off, found by looking back for the
// newline that closes the line before it. The window is bounded by MaxLine:
// past that the line is one the reader skips whole, and hashing the bounded
// window is what the reader does for it too, so the two agree.
func fingerprintAt(f *os.File, off int64) (string, error) {
	if off <= 0 {
		return "", nil
	}
	// The window is read once rather than grown a chunk at a time: a line close
	// to MaxLine would otherwise be recopied at every step.
	start := off - min(off, MaxLine)
	window := make([]byte, off-start)
	if _, err := f.ReadAt(window, start); err != nil {
		return "", fmt.Errorf("file stream: read at %d: %w", start, err)
	}
	// window ends with the newline of the line being hashed; the newline before
	// it closes the previous line and marks where this one starts. With none in
	// the window the line runs past MaxLine, and the bounded tail is what the
	// reader hashes for it too, so the two agree.
	if i := bytes.LastIndexByte(window[:len(window)-1], '\n'); i >= 0 {
		return fingerprint(window[i+1:]), nil
	}
	return fingerprint(window), nil
}

func fingerprint(line []byte) string {
	if len(line) == 0 {
		return ""
	}
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:])[:fingerprintLen]
}

func gap(c stream.Cursor) error {
	return &stream.HistoryGapError{Cursor: c, Restart: startCursor}
}

// held is the cursor to report when nothing was consumed: the caller's own,
// with the empty cursor spelled as the start of the file.
func held(c stream.Cursor) stream.Cursor {
	if c == "" {
		return startCursor
	}
	return c
}

func lineCursor(off int64, fp string) stream.Cursor {
	return stream.Cursor(strconv.FormatInt(off, 10) + ":" + fp)
}

// parseCursor splits a cursor into its byte offset and the fingerprint of the
// line ending there. A bare decimal offset is a legacy cursor: it is accepted
// as 0, the start of the file, and anywhere else it has no fingerprint to check
// and Read reports a gap.
func parseCursor(c stream.Cursor) (int64, string, error) {
	if c == "" {
		return 0, "", nil
	}
	digits, fp, _ := strings.Cut(string(c), ":")
	off, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("file stream: bad cursor %q: %w", string(c), err)
	}
	if off < 0 {
		return 0, "", fmt.Errorf("file stream: bad cursor %q: negative offset", string(c))
	}
	return off, fp, nil
}
