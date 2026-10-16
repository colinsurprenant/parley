package file

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/stream"
)

func newStream(t *testing.T, nick string) (*Stream, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	s, err := Open(path, nick, WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

// cursorOffset returns the byte offset a cursor points at, checking that it
// carries the fingerprint that makes the offset verifiable.
func cursorOffset(t *testing.T, c stream.Cursor) int64 {
	t.Helper()
	digits, fp, split := strings.Cut(string(c), ":")
	off, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		t.Fatalf("cursor %q does not start with an offset: %v", c, err)
	}
	if off > 0 && (!split || fp == "") {
		t.Fatalf("cursor %q carries no fingerprint", c)
	}
	return off
}

// rawLine is one NDJSON record with a fixed timestamp, so two lines built from
// same-length bodies occupy the same number of bytes.
func rawLine(t *testing.T, author, body string) string {
	t.Helper()
	b, err := json.Marshal(record{Author: author, TS: "2026-09-12T15:04:05Z", Body: body})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b) + "\n"
}

func TestSendThenReadAssignsOffsetIDs(t *testing.T) {
	ctx := context.Background()
	s, path := newStream(t, "codex")
	before := time.Now().Add(-time.Second)

	if _, err := s.Send(ctx, "first line"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs, cursor, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("read %d messages, want 1", len(msgs))
	}
	got := msgs[0]
	if got.ID != "0" {
		t.Fatalf("id %q, want %q: the first line starts at byte 0", got.ID, "0")
	}
	if got.Author != "codex" || got.Body != "first line" {
		t.Fatalf("message %+v, want author codex and body %q", got, "first line")
	}
	if got.TS.Before(before) || got.TS.After(time.Now().Add(time.Second)) {
		t.Fatalf("ts %v is not the append time", got.TS)
	}
	if got.TS.Location() != time.UTC {
		t.Fatalf("ts location %v, want UTC", got.TS.Location())
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if off := cursorOffset(t, cursor); off != fi.Size() {
		t.Fatalf("cursor %q is at %d, want the file size %d", cursor, off, fi.Size())
	}

	// The second message's id is the offset just past the first line.
	firstLineLen := fi.Size()
	if _, err := s.Send(ctx, "second line"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs, cursor2, err := s.Read(ctx, cursor)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "second line" {
		t.Fatalf("read %+v, want only the second line", msgs)
	}
	if msgs[0].ID != strconv.FormatInt(firstLineLen, 10) {
		t.Fatalf("id %q, want %d", msgs[0].ID, firstLineLen)
	}
	if cursor2 == cursor {
		t.Fatalf("cursor did not advance past %q", cursor)
	}

	// Reading again from the advanced cursor yields nothing.
	msgs, cursor3, err := s.Read(ctx, cursor2)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 0 || cursor3 != cursor2 {
		t.Fatalf("read %+v at cursor %q, want nothing and %q", msgs, cursor3, cursor2)
	}
}

func TestReadMultilineBody(t *testing.T) {
	ctx := context.Background()
	s, _ := newStream(t, "codex")
	body := "one\ntwo\nthree"
	if _, err := s.Send(ctx, body); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs, _, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != body {
		t.Fatalf("read %+v, want one message with the multi-line body", msgs)
	}
}

func TestPartialTrailingLineIsNotDelivered(t *testing.T) {
	ctx := context.Background()
	s, path := newStream(t, "codex")
	if _, err := s.Send(ctx, "complete"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	completeEnd := fi.Size()

	// A writer mid-append: a line with no terminating newline.
	partial := `{"author":"codex","ts":"2026-09-12T15:04:05Z","body":"half wri`
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString(partial); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	f.Close()

	msgs, cursor, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "complete" {
		t.Fatalf("read %+v, want only the complete line", msgs)
	}
	if off := cursorOffset(t, cursor); off != completeEnd {
		t.Fatalf("cursor %q is at %d, want %d: it must not pass the partial line", cursor, off, completeEnd)
	}

	// Finishing the line makes it deliverable, exactly once.
	f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString("tten\"}\n"); err != nil {
		t.Fatalf("finish partial: %v", err)
	}
	f.Close()

	msgs, _, err = s.Read(ctx, cursor)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "half written" {
		t.Fatalf("read %+v, want the completed line", msgs)
	}
	if msgs[0].ID != strconv.FormatInt(completeEnd, 10) {
		t.Fatalf("id %q, want %d", msgs[0].ID, completeEnd)
	}
}

func TestSendClosesATornLine(t *testing.T) {
	ctx := context.Background()
	s, path := newStream(t, "codex")

	// A writer that died mid-append, with no newline behind it.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString(`{"author":"codex","ts":"2026-09-12T15:04:05Z","body":"half wri`); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	f.Close()

	if _, err := s.Send(ctx, "whole"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if n := bytes.Count(raw, []byte("\n")); n != 2 {
		t.Fatalf("file holds %d lines, want the torn one closed and this send on its own line:\n%q", n, raw)
	}
	msgs, _, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "whole" {
		t.Fatalf("read %+v, want the torn line skipped and this send delivered intact", msgs)
	}
}

func TestSendTimestampIsTakenUnderTheLock(t *testing.T) {
	ctx := context.Background()
	s, path := newStream(t, "codex")

	blocker, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer blocker.Close()
	if err := syscall.Flock(int(blocker.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("Flock: %v", err)
	}
	const held = 150 * time.Millisecond
	released := make(chan time.Time, 1)
	go func() {
		time.Sleep(held)
		// The release time is taken before the unlock and reported after it, so
		// the test never touches the descriptor once it has stopped waiting.
		at := time.Now()
		_ = syscall.Flock(int(blocker.Fd()), syscall.LOCK_UN)
		released <- at
	}()

	start := time.Now()
	if _, err := s.Send(ctx, "waited its turn"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if elapsed := time.Since(start); elapsed < held {
		t.Fatalf("Send returned after %v, want it to wait for the lock (%v)", elapsed, held)
	}
	release := <-released

	msgs, _, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("read %d messages, want 1", len(msgs))
	}
	if msgs[0].TS.Before(release) {
		t.Fatalf("ts %v is before the lock was released at %v: it was stamped before this writer's turn", msgs[0].TS, release)
	}
}

func TestSendGivesUpTheLockWaitOnCancel(t *testing.T) {
	s, path := newStream(t, "codex")

	blocker, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer blocker.Close()
	if err := syscall.Flock(int(blocker.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("Flock: %v", err)
	}
	defer syscall.Flock(int(blocker.Fd()), syscall.LOCK_UN)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()

	start := time.Now()
	_, err = s.Send(ctx, "never lands")
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled while another writer holds the lock", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Send returned %v after it began, want it to give up shortly after the cancel", elapsed)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("Send appended %d bytes without ever holding the lock", fi.Size())
	}
}

// cancelWhenLocked reports canceled exactly once the append lock on path is
// held. Err is what Send consults, so a context of this shape pins the check
// that has to happen after the lock is won: a cancel that lands while the
// writer ahead finishes is invisible to the check made before the wait.
type cancelWhenLocked struct {
	context.Context
	path string
}

func (c cancelWhenLocked) Err() error {
	f, err := os.OpenFile(c.path, os.O_RDWR, 0o644)
	if err != nil {
		return nil
	}
	defer f.Close()
	// flock is held per open file description, so this conflicts with the lock
	// Send holds even though both are in this process.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return context.Canceled
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return nil
}

func TestSendCanceledAfterWinningTheLockAppendsNothing(t *testing.T) {
	s, path := newStream(t, "codex")
	if _, err := s.Send(context.Background(), "first"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	ctx := cancelWhenLocked{Context: context.Background(), path: path}
	if _, err := s.Send(ctx, "never lands"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled for a cancel that landed under the lock", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the canceled send appended %q", after[len(before):])
	}
	// The lock is released on the way out, so the next writer is not shut out.
	if _, err := s.Send(context.Background(), "after the cancel"); err != nil {
		t.Fatalf("Send after the canceled one: %v", err)
	}
}

func TestSendRefusesAnOverLongLine(t *testing.T) {
	ctx := context.Background()
	s, path := newStream(t, "codex")
	if _, err := s.Send(ctx, "first"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if _, err := s.Send(ctx, strings.Repeat("x", MaxLine)); !errors.Is(err, ErrMessageTooLong) {
		t.Fatalf("err %v, want ErrMessageTooLong", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the refused message was appended:\n%q", after[len(before):])
	}

	// A body that fills the line to just under the limit still round-trips.
	body := nearMaxLineBody(t)
	if _, err := s.Send(ctx, body); err != nil {
		t.Fatalf("Send of a %d-byte body: %v", len(body), err)
	}
	msgs, _, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 2 || msgs[1].Body != body {
		t.Fatalf("read %d messages, want the first line and the %d-byte body intact", len(msgs), len(body))
	}
}

func TestSendReopensAfterRotationUnderTheLock(t *testing.T) {
	ctx := context.Background()
	s, path := newStream(t, "codex")
	if _, err := s.Send(ctx, "before rotation"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	blocker, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer blocker.Close()
	if err := syscall.Flock(int(blocker.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("Flock: %v", err)
	}
	rotated := path + ".1"
	done := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		err := os.Rename(path, rotated)
		if err == nil {
			err = os.WriteFile(path, nil, 0o644)
		}
		_ = syscall.Flock(int(blocker.Fd()), syscall.LOCK_UN)
		done <- err
	}()

	if _, err := s.Send(ctx, "after rotation"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("rotate: %v", err)
	}

	msgs, _, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "after rotation" {
		t.Fatalf("the current file holds %+v, want only the message sent after the rotation", msgs)
	}
	old, err := os.ReadFile(rotated)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if bytes.Contains(old, []byte("after rotation")) {
		t.Fatalf("the send landed in the rotated-away file:\n%q", old)
	}
}

func TestTruncatedFileReportsHistoryGap(t *testing.T) {
	ctx := context.Background()
	s, path := newStream(t, "codex")
	if _, err := s.Send(ctx, "one"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := s.Send(ctx, "two"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, cursor, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if _, err := s.Send(ctx, "after rotation"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	_, restart, err := s.Read(ctx, cursor)
	if !errors.Is(err, stream.ErrHistoryGap) {
		t.Fatalf("err %v, want a history gap", err)
	}
	var gapErr *stream.HistoryGapError
	if !errors.As(err, &gapErr) {
		t.Fatalf("err %v is not a *HistoryGapError", err)
	}
	if gapErr.Cursor != cursor || gapErr.Restart != "0" {
		t.Fatalf("gap %+v, want cursor %q and restart 0", gapErr, cursor)
	}
	if restart != "0" {
		t.Fatalf("returned cursor %q, want 0", restart)
	}

	msgs, _, err := s.Read(ctx, restart)
	if err != nil {
		t.Fatalf("Read after restart: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "after rotation" {
		t.Fatalf("read %+v after the restart, want the post-rotation message", msgs)
	}
}

func TestSameLengthRewriteReportsHistoryGap(t *testing.T) {
	ctx := context.Background()
	s, path := newStream(t, "codex")
	before := rawLine(t, "codex", "one") + rawLine(t, "codex", "two")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	msgs, cursor, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("read %d messages, want 2", len(msgs))
	}

	// Rotated and refilled to exactly the same length: the byte offset alone
	// cannot tell this file from the one the cursor was taken against.
	after := rawLine(t, "codex", "uno") + rawLine(t, "codex", "dos")
	if len(after) != len(before) {
		t.Fatalf("test setup: replacement is %d bytes, want %d", len(after), len(before))
	}
	if err := os.WriteFile(path, []byte(after), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, restart, err := s.Read(ctx, cursor)
	if !errors.Is(err, stream.ErrHistoryGap) {
		t.Fatalf("err %v, want a history gap", err)
	}
	if restart != "0" {
		t.Fatalf("returned cursor %q, want 0", restart)
	}
	msgs, _, err = s.Read(ctx, restart)
	if err != nil {
		t.Fatalf("Read after restart: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Body != "uno" || msgs[1].Body != "dos" {
		t.Fatalf("read %+v after the restart, want the rewritten history", msgs)
	}
}

func TestCursorFromAnotherFileReportsHistoryGap(t *testing.T) {
	ctx := context.Background()
	a, pathA := newStream(t, "codex")
	b, pathB := newStream(t, "codex")
	if err := os.WriteFile(pathA, []byte(rawLine(t, "codex", "one")), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(pathB, []byte(rawLine(t, "codex", "two")), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, cursor, err := a.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if _, restart, err := b.Read(ctx, cursor); !errors.Is(err, stream.ErrHistoryGap) {
		t.Fatalf("err %v (restart %q), want a history gap for a cursor taken against another file", err, restart)
	}
}

func TestCursorPastEndOfFileReportsHistoryGap(t *testing.T) {
	ctx := context.Background()
	s, _ := newStream(t, "codex")
	if _, err := s.Send(ctx, "one"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, _, err := s.Read(ctx, stream.Cursor("99999")); !errors.Is(err, stream.ErrHistoryGap) {
		t.Fatalf("err %v, want a history gap", err)
	}
}

func TestCursorNotAtLineBoundaryReportsHistoryGap(t *testing.T) {
	ctx := context.Background()
	s, _ := newStream(t, "codex")
	if _, err := s.Send(ctx, "one"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Mid-line: the file was rewritten under a cursor that no longer lines up.
	if _, _, err := s.Read(ctx, stream.Cursor("5")); !errors.Is(err, stream.ErrHistoryGap) {
		t.Fatalf("err %v, want a history gap", err)
	}
}

func TestLegacyBareCursorIsOnlyTrustedAtZero(t *testing.T) {
	ctx := context.Background()
	s, _ := newStream(t, "codex")
	if _, err := s.Send(ctx, "one"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, c := range []stream.Cursor{"", "0", "0:"} {
		msgs, _, err := s.Read(ctx, c)
		if err != nil {
			t.Fatalf("Read at %q: %v", c, err)
		}
		if len(msgs) != 1 {
			t.Fatalf("read %d messages at %q, want 1", len(msgs), c)
		}
	}
	// A bare offset anywhere else carries nothing to check it against.
	_, end, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	bare := stream.Cursor(strconv.FormatInt(cursorOffset(t, end), 10))
	if _, _, err := s.Read(ctx, bare); !errors.Is(err, stream.ErrHistoryGap) {
		t.Fatalf("err %v for bare cursor %q, want a history gap", err, bare)
	}
}

func TestMissingFileWithStaleCursorReportsHistoryGap(t *testing.T) {
	ctx := context.Background()
	s, path := newStream(t, "codex")
	if _, err := s.Send(ctx, "one"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, cursor, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, _, err := s.Read(ctx, cursor); !errors.Is(err, stream.ErrHistoryGap) {
		t.Fatalf("err %v, want a history gap", err)
	}
	msgs, restart, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read from the start: %v", err)
	}
	if len(msgs) != 0 || restart != "0" {
		t.Fatalf("read %+v at %q, want nothing at 0", msgs, restart)
	}
}

func TestMalformedLineIsSkipped(t *testing.T) {
	ctx := context.Background()
	s, path := newStream(t, "codex")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString("not json at all\n\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	if _, err := s.Send(ctx, "good"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs, _, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "good" {
		t.Fatalf("read %+v, want the bad line skipped and the good one kept", msgs)
	}
}

func TestOverlongLineIsSkipped(t *testing.T) {
	ctx := context.Background()
	huge := rawLine(t, "codex", strings.Repeat("x", MaxLine+1))

	t.Run("followed by a good line", func(t *testing.T) {
		s, path := newStream(t, "codex")
		if err := os.WriteFile(path, []byte(huge), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := s.Send(ctx, "after the long line"); err != nil {
			t.Fatalf("Send: %v", err)
		}
		msgs, cursor, err := s.Read(ctx, "")
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(msgs) != 1 || msgs[0].Body != "after the long line" {
			t.Fatalf("read %+v, want the over-long line skipped and the next one kept", msgs)
		}
		if msgs[0].ID != strconv.Itoa(len(huge)) {
			t.Fatalf("id %q, want %d: the cursor steps over the whole long line", msgs[0].ID, len(huge))
		}
		msgs, next, err := s.Read(ctx, cursor)
		if err != nil {
			t.Fatalf("Read again: %v", err)
		}
		if len(msgs) != 0 || next != cursor {
			t.Fatalf("read %+v at %q, want nothing and the cursor held at %q", msgs, next, cursor)
		}
	})

	t.Run("at the end of the file", func(t *testing.T) {
		s, path := newStream(t, "codex")
		if err := os.WriteFile(path, []byte(rawLine(t, "codex", "first")+huge), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		msgs, cursor, err := s.Read(ctx, "")
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(msgs) != 1 || msgs[0].Body != "first" {
			t.Fatalf("read %+v, want only the line before the over-long one", msgs)
		}
		// The cursor now sits past a line the reader never buffered: it must
		// still verify against the file rather than report a false gap.
		msgs, next, err := s.Read(ctx, cursor)
		if err != nil {
			t.Fatalf("Read again: %v", err)
		}
		if len(msgs) != 0 || next != cursor {
			t.Fatalf("read %+v at %q, want nothing and the cursor held at %q", msgs, next, cursor)
		}
	})
}

// nearMaxLineBody is a body whose encoded line lands just under MaxLine, so the
// fingerprint window is the whole bound. The margin covers the send's own
// timestamp, which carries up to nine fractional digits where rawLine's carries
// none.
func nearMaxLineBody(t *testing.T) string {
	t.Helper()
	const margin = 16
	return strings.Repeat("y", MaxLine-len(rawLine(t, "codex", ""))-margin)
}

// verifyTimer fills a stream with one line holding body and returns a function
// that times a batch of verifications of the cursor at its end. Every
// verification hashes that line, so the batch is that cost and little else.
func verifyTimer(t *testing.T, body string, rounds int) func() time.Duration {
	t.Helper()
	ctx := context.Background()
	s, _ := newStream(t, "codex")
	if _, err := s.Send(ctx, body); err != nil {
		t.Fatalf("Send of a %d-byte body: %v", len(body), err)
	}
	msgs, cursor, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != body {
		t.Fatalf("read %d messages, want the %d-byte body intact", len(msgs), len(body))
	}
	return func() time.Duration {
		start := time.Now()
		for range rounds {
			msgs, next, err := s.Read(ctx, cursor)
			if err != nil {
				t.Fatalf("Read at %q: %v", cursor, err)
			}
			if len(msgs) != 0 || next != cursor {
				t.Fatalf("read %+v at %q, want nothing and the cursor held", msgs, next)
			}
		}
		return time.Since(start)
	}
}

func TestNearMaxLineCursorVerificationScalesLinearly(t *testing.T) {
	full := nearMaxLineBody(t)
	quarter := full[:len(full)/4]
	const rounds = 20
	timeFull := verifyTimer(t, full, rounds)
	timeQuarter := verifyTimer(t, quarter, rounds)

	// Every verification hashes the line ending at the cursor. Reading that
	// window once makes the cost linear in it; growing the buffer a chunk at a
	// time made it quadratic, and a line this long is where that showed. A
	// wall-clock budget cannot tell the two apart on every machine, since the
	// race detector alone slows the same code several-fold, so the line is timed
	// against one a quarter its length on the same machine: linear cost grows
	// about fourfold (less where fixed per-read work dilutes it) and quadratic
	// cost about sixteenfold. Measured, linear came out at 3.7 to 4.0 (up to 4.6
	// on a machine with every core busy) and quadratic at 14.1 to 15.1, with and
	// without the race detector; 8 is the midpoint of 4 and 16 on a log scale,
	// a factor of two clear of each.
	const maxRatio = 8.0

	// The fastest of several batches is the one least disturbed by whatever else
	// the machine was doing, and the two sizes alternate so a slow stretch lands
	// on both.
	tFull, tQuarter := time.Duration(math.MaxInt64), time.Duration(math.MaxInt64)
	for range 5 {
		tFull = min(tFull, timeFull())
		tQuarter = min(tQuarter, timeQuarter())
	}
	ratio := float64(tFull) / float64(tQuarter)
	t.Logf("%d verifications: %v at %d bytes, %v at %d bytes, ratio %.2f", rounds, tFull, len(full), tQuarter, len(quarter), ratio)
	if ratio > maxRatio {
		t.Fatalf("verifying a %d-byte line took %.1fx as long as a %d-byte one, want under %.0fx: the cost is not linear in the line", len(full), ratio, len(quarter), maxRatio)
	}
}

func BenchmarkFingerprintAtNearMaxLine(b *testing.B) {
	path := filepath.Join(b.TempDir(), "chat.ndjson")
	line, err := json.Marshal(record{Author: "codex", TS: "2026-09-12T15:04:05Z", Body: strings.Repeat("y", MaxLine-128)})
	if err != nil {
		b.Fatalf("marshal: %v", err)
	}
	line = append(line, '\n')
	if err := os.WriteFile(path, line, 0o644); err != nil {
		b.Fatalf("WriteFile: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer f.Close()

	b.SetBytes(int64(len(line)))
	for b.Loop() {
		if _, err := fingerprintAt(f, int64(len(line))); err != nil {
			b.Fatalf("fingerprintAt: %v", err)
		}
	}
}

func TestBadCursorIsAnError(t *testing.T) {
	s, _ := newStream(t, "codex")
	for _, c := range []stream.Cursor{"banana", "banana:cafe", "-1:cafe"} {
		if _, _, err := s.Read(context.Background(), c); err == nil {
			t.Fatalf("cursor %q was accepted", c)
		} else if errors.Is(err, stream.ErrHistoryGap) {
			t.Fatalf("cursor %q reported a gap, want a malformed-cursor error", c)
		}
	}
}

func TestWaitDeliversAnAppendAndTimesOutOnQuiet(t *testing.T) {
	ctx := context.Background()
	s, _ := newStream(t, "codex")

	go func() {
		time.Sleep(30 * time.Millisecond)
		if _, err := s.Send(ctx, "arrived"); err != nil {
			t.Errorf("Send: %v", err)
		}
	}()
	msgs, cursor, err := s.Wait(ctx, "", 5*time.Second, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "arrived" {
		t.Fatalf("waited up %+v, want the appended message", msgs)
	}

	const timeout = 200 * time.Millisecond
	start := time.Now()
	msgs, cursor2, err := s.Wait(ctx, cursor, timeout, 20*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("waited up %+v, want nothing", msgs)
	}
	if cursor2 != cursor {
		t.Fatalf("cursor %q, want it unchanged at %q", cursor2, cursor)
	}
	if elapsed < timeout || elapsed > timeout+2*time.Second {
		t.Fatalf("returned after %v, want close to %v", elapsed, timeout)
	}
}

// countdownCtx reports canceled from its (n+1)th Err call on. Read consults Err
// once on the way in and once per line, so a context of this shape lands the
// cancel between two lines instead of racing a timer against the scan.
type countdownCtx struct {
	context.Context
	left atomic.Int64
	done chan struct{}
	once sync.Once
}

func newCountdownCtx(n int64) *countdownCtx {
	c := &countdownCtx{Context: context.Background(), done: make(chan struct{})}
	c.left.Store(n)
	return c
}

func (c *countdownCtx) Done() <-chan struct{} { return c.done }

func (c *countdownCtx) Err() error {
	if c.left.Add(-1) >= 0 {
		return nil
	}
	c.once.Do(func() { close(c.done) })
	return context.Canceled
}

func TestReadWithACanceledContextReadsNothing(t *testing.T) {
	s, path := newStream(t, "codex")
	var lines strings.Builder
	for i := range 4 {
		lines.WriteString(rawLine(t, "codex", "line "+strconv.Itoa(i)))
	}
	if err := os.WriteFile(path, []byte(lines.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msgs, cursor, err := s.Read(ctx, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	// The check on the way in runs before the first line, so there is no partial
	// progress to report and the cursor stays where the caller had it.
	if len(msgs) != 0 {
		t.Fatalf("read %d messages, want none: the cancel is seen before the first line", len(msgs))
	}
	if cursor != "" {
		t.Fatalf("cursor %q, want it unchanged at %q", cursor, "")
	}
}

func TestReadCanceledMidScanKeepsPartialProgress(t *testing.T) {
	const lineCount = 12
	s, path := newStream(t, "codex")
	var lines strings.Builder
	for i := range lineCount {
		lines.WriteString(rawLine(t, "codex", "line "+strconv.Itoa(i)))
	}
	if err := os.WriteFile(path, []byte(lines.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Enough checks to get past the entry guard and a few lines, far short of
	// the whole file.
	msgs, cursor, err := s.Read(newCountdownCtx(5), "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if len(msgs) == 0 || len(msgs) >= lineCount {
		t.Fatalf("read %d of %d messages, want a partial batch", len(msgs), lineCount)
	}
	// The cursor is just past what came back, so a resume loses nothing and
	// repeats nothing: the two halves make the whole file exactly once.
	rest, _, err := s.Read(context.Background(), cursor)
	if err != nil {
		t.Fatalf("Read from the partial cursor: %v", err)
	}
	if len(msgs)+len(rest) != lineCount {
		t.Fatalf("%d + %d messages across the cancel, want %d", len(msgs), len(rest), lineCount)
	}
	for i, m := range append(append([]stream.Message(nil), msgs...), rest...) {
		if want := "line " + strconv.Itoa(i); m.Body != want {
			t.Fatalf("message %d is %q, want %q", i, m.Body, want)
		}
	}
}

func TestOpenValidatesArguments(t *testing.T) {
	if _, err := Open("", "codex"); err == nil {
		t.Fatal("want an error for an empty path")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "c.ndjson"), ""); err == nil {
		t.Fatal("want an error for an empty nick")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing-dir", "c.ndjson"), "codex"); err == nil {
		t.Fatal("want an error when the parent directory does not exist")
	}
}

// sentIsWhatAReaderSees is the contract Send's return exists for: the message it
// returns is the one a reader gets for that line, ID and TS both, so a seat
// that prints them is printing what another seat's wait will print.
func sentIsWhatAReaderSees(t *testing.T, sent stream.Message, read stream.Message) {
	t.Helper()
	if sent.ID != read.ID {
		t.Fatalf("Send returned id %q, a reader sees %q", sent.ID, read.ID)
	}
	if !sent.TS.Equal(read.TS) {
		t.Fatalf("Send returned ts %v, a reader sees %v", sent.TS, read.TS)
	}
	if sent.Author != read.Author || sent.Body != read.Body {
		t.Fatalf("Send returned %+v, a reader sees %+v", sent, read)
	}
	if sent.Receipt() != "[parley] sent id="+read.ID+" ts="+read.TS.UTC().Format(time.RFC3339) {
		t.Fatalf("the receipt %q does not carry the header's id and ts for %+v", sent.Receipt(), read)
	}
}

func TestSendReturnsTheMessageAReaderSees(t *testing.T) {
	ctx := context.Background()
	s, _ := newStream(t, "codex")
	var sent []stream.Message
	for _, body := range []string{"first", "second\nwith two lines", "third"} {
		m, err := s.Send(ctx, body)
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		sent = append(sent, m)
	}
	msgs, _, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != len(sent) {
		t.Fatalf("read %d messages, sent %d", len(msgs), len(sent))
	}
	if sent[0].ID != "0" {
		t.Fatalf("the first message starts at byte 0, Send returned id %q", sent[0].ID)
	}
	for i := range sent {
		sentIsWhatAReaderSees(t, sent[i], msgs[i])
	}
}

// TestSendAfterATornTailReturnsTheLineNotTheClosingByte: the byte that closes a
// torn line is written ahead of the record, so the record starts one past where
// the write does, and the ID a reader reports is the record's start.
func TestSendAfterATornTailReturnsTheLineNotTheClosingByte(t *testing.T) {
	ctx := context.Background()
	s, path := newStream(t, "codex")
	good := rawLine(t, "someone", "complete")
	torn := `{"author":"someone","ts":"2026-09-12T15:04:05Z","bo`
	if err := os.WriteFile(path, []byte(good+torn), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sent, err := s.Send(ctx, "after the tear")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if want := strconv.Itoa(len(good + torn + "\n")); sent.ID != want {
		t.Fatalf("Send returned id %q, want %s: the offset after the byte that closed the torn line", sent.ID, want)
	}
	msgs, _, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("read %d messages (%+v), want the complete one and the one sent", len(msgs), msgs)
	}
	sentIsWhatAReaderSees(t, sent, msgs[1])
}

// TestSendReturnsTheTrueOffsetUnderAStockWriter: a stock `>>` takes no lock, so
// a line can land between Send's look at the file and its write. Every ID Send
// returns must be where its record really is, whichever way those interleave;
// the window is narrow, so this is a run of many sends against a writer that
// never stops, and a wrong ID shows as a reader finding another body there.
func TestSendReturnsTheTrueOffsetUnderAStockWriter(t *testing.T) {
	ctx := context.Background()
	s, path := newStream(t, "codex")
	stock := rawLine(t, "stock", "appended with >>")
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			done <- err
			return
		}
		defer f.Close()
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if _, err := f.WriteString(stock); err != nil {
				done <- err
				return
			}
		}
	}()
	var sent []stream.Message
	for i := range 200 {
		m, err := s.Send(ctx, "mine "+strconv.Itoa(i))
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		sent = append(sent, m)
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("the stock writer: %v", err)
	}
	msgs, _, err := s.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	byID := make(map[string]stream.Message, len(msgs))
	for _, m := range msgs {
		byID[m.ID] = m
	}
	for _, m := range sent {
		got, ok := byID[m.ID]
		if !ok {
			t.Fatalf("Send returned id %q, and no record starts there", m.ID)
		}
		sentIsWhatAReaderSees(t, m, got)
	}
}
