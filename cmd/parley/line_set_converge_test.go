package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// cancelAfter is a context that is live for its first n questions (Err) and
// cancelled from the next on, so that a test can stop a run between two chosen
// steps without a race: `line set` asks the context before each write.
type cancelAfter struct {
	context.Context
	left int
}

func (c *cancelAfter) Err() error {
	if c.left > 0 {
		c.left--
		return nil
	}
	return context.Canceled
}

func (b *lineBench) recordDir() string { return filepath.Join(b.base, "lines", "demo") }

// wantRecordTrue fails unless line.json is true of the files: every digest it holds
// for a session is the digest of that session's file as it is (or the file is not
// there), which is what `line drop` proves a file is the line's by.
func (b *lineBench) wantRecordTrue(t *testing.T) {
	t.Helper()
	for _, s := range b.record(t, "demo").Sessions {
		for _, f := range s.Files {
			if _, err := os.Lstat(f.Path); err != nil {
				continue
			}
			if got := fileDigestOf(t, f.Path); got != f.SHA256 {
				t.Errorf("session %s: the record holds %s for %s, which holds %s", s.Nick, f.SHA256, f.Path, got)
			}
		}
	}
}

// twoClaudeSessions is line demo with a Claude session in each of two working
// directories, and the two launch files.
func (b *lineBench) twoClaudeSessions(t *testing.T) (mcp1, mcp2 string) {
	t.Helper()
	lineUp(t, b, "demo")
	co1, co2 := b.gitWorkdir(t, "co-plan"), b.gitWorkdir(t, "co-review")
	b.mustAdd(t, "demo", "plan", "claude", co1)
	b.mustAdd(t, "demo", "review", "claude", co2)
	return filepath.Join(co1, ".mcp.json"), filepath.Join(co2, ".mcp.json")
}

// TestLineSetRewritesAFileRestoredAfterItWentMissing: a file that went missing
// during a run keeps the digest the record has for it, so that the original bytes put
// back pass the proof. The numbers the record holds are then the ones asked for, and
// the file still says the old ones: a rerun judges the file by what it says, and
// writes it.
func TestLineSetRewritesAFileRestoredAfterItWentMissing(t *testing.T) {
	b := newLineBench(t)
	mcp1, mcp2 := b.twoClaudeSessions(t)
	orig := mustRead(t, mcp2)
	if err := os.Remove(mcp2); err != nil {
		t.Fatal(err)
	}
	res := mustLine(t, b, b.lset("demo", "--cap", "30")...)
	if !strings.Contains(res.stdout, mcp2+" is not there (session review)") {
		t.Fatalf("stdout:\n%s", res.stdout)
	}
	b.wantMCP(t, mcp1, "demo", "30", "10m")
	if rec := b.record(t, "demo"); rec.Cap != 30 {
		t.Fatalf("record cap %d, want 30", rec.Cap)
	}

	// The cap-8 file comes back: the very bytes the record still has a digest of.
	if err := os.WriteFile(mcp2, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	res = mustLine(t, b, b.lset("demo", "--cap", "30")...)
	t.Logf("stdout:\n%s", res.stdout)
	b.wantMCP(t, mcp2, "demo", "30", "10m")
	if strings.Contains(res.stdout, "nothing was changed") {
		t.Errorf("a run that wrote a file says nothing was changed:\n%s", res.stdout)
	}
	for _, want := range []string{
		"Rewrote " + mcp2 + " (session review).",
		"Left " + mcp1 + " as it is (session plan): it already has cap 30 and window 10m.",
		"review (Claude Code): restart it, resuming its conversation:",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	b.wantRecordTrue(t)
	// And now it is a no-op.
	res = mustLine(t, b, b.lset("demo", "--cap", "30")...)
	if !strings.Contains(res.stdout, "nothing was changed") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	mustLine(t, b, b.ldrop("demo", "plan")...)
	mustLine(t, b, b.ldrop("demo", "review")...)
}

// padRecord makes line.json exactly lineRecordMax bytes, the most the verbs read,
// by lengthening a field no check looks at.
func (b *lineBench) padRecord(t *testing.T) {
	t.Helper()
	rec := b.record(t, "demo")
	data, err := writeLineRecord(b.recordDir(), rec)
	if err != nil {
		t.Fatal(err)
	}
	rec.Created += strings.Repeat("x", lineRecordMax-len(data))
	if data, err = writeLineRecord(b.recordDir(), rec); err != nil || len(data) != lineRecordMax {
		t.Fatalf("the padded record is %d bytes (%v), want %d", len(data), err, lineRecordMax)
	}
}

// TestLineSetRefusesBeforeAnyWriteWhenTheRecordWouldGrowPastItsLimit: the size of
// the records the run will write is checked while it is planned. A record that is at
// the limit and that the new numbers would lengthen is a refusal with every file and
// the record as they were, and not a failure after the files were replaced, which
// would leave stale digests that every rerun fails on the same way; numbers that do
// not lengthen it go through.
func TestLineSetRefusesBeforeAnyWriteWhenTheRecordWouldGrowPastItsLimit(t *testing.T) {
	b := newLineBench(t)
	mcp1, mcp2 := b.twoClaudeSessions(t)
	b.padRecord(t)
	before := b.lsnap(t, filepath.Dir(mcp1), filepath.Dir(mcp2))

	res := b.run(t, b.lset("demo", "--cap", "30")...)
	if res.code != 1 || res.stdout != "" || !strings.Contains(res.stderr, "the line record would be 65537 bytes, over the 65536 bytes") || !strings.Contains(res.stderr, "nothing was changed") {
		t.Fatalf("exit %d, want a refusal for the size\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	b.wantLineUnchanged(t, before, filepath.Dir(mcp1), filepath.Dir(mcp2))
	wantNoTempFiles(t, filepath.Dir(mcp1), filepath.Dir(mcp2), b.recordDir())

	// A cap of the same length does not grow it, and goes through.
	mustLine(t, b, b.lset("demo", "--cap", "9")...)
	b.wantMCP(t, mcp1, "demo", "9", "10m")
	b.wantMCP(t, mcp2, "demo", "9", "10m")
	b.wantRecordTrue(t)
}

// TestLineSetReprovesAHeldFileBeforeRecordingItsDigest: a file found to hold the bytes
// the run would write is taken as the line's, and its digest recorded, only if it
// still is when the record is written; one changed in between is not recorded.
func TestLineSetReprovesAHeldFileBeforeRecordingItsDigest(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")
	rec := b.record(t, "demo")
	// What a `line set --cap 30` killed after it replaced the file leaves: the file
	// holds what the run writes, and the record the digest of what it replaced.
	want := rec
	want.Cap = 30
	data, err := lineMCPJSON(binPath, want, b.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mcp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := planLineSet(rec, b.base, binPath, 30, "10m")
	if err != nil {
		t.Fatal(err)
	}
	// It is changed after it was judged, and before its digest is recorded.
	edit(t, mcp, "\n")
	edited := mustRead(t, mcp)
	recBefore := string(mustRead(t, filepath.Join(b.recordDir(), "line.json")))

	err = plan.apply(context.Background(), b.recordDir())
	if err == nil {
		t.Fatalf("apply recorded the digest of a file that is not what it was found to be")
	}
	if !strings.Contains(err.Error(), "will not record the digest of "+mcp+": ") {
		t.Errorf("apply: %v", err)
	}
	if got := mustRead(t, mcp); string(got) != string(edited) {
		t.Errorf("the edited file was written over:\n%s", got)
	}
	if got := string(mustRead(t, filepath.Join(b.recordDir(), "line.json"))); got != recBefore {
		t.Errorf("the record changed:\n%s", got)
	}
	wantNoTempFiles(t, co, b.recordDir())
}

// TestLineSetAdoptsAFileHoldingWhatItWritesForTheSameNumbersOnly: the state a run
// killed between a file's rename and its record leaves is gone on from by the same
// numbers from the same binary, which recognize the file by its exact bytes and
// record its digest; other numbers are refused for that file, and the refusal says
// how it was left, and echoes none of the file.
func TestLineSetAdoptsAFileHoldingWhatItWritesForTheSameNumbersOnly(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")
	rec := b.record(t, "demo")
	want := rec
	want.Cap = 30
	data, err := lineMCPJSON(binPath, want, b.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mcp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	before := b.lsnap(t, co)

	res := b.run(t, b.lset("demo", "--cap", "40")...)
	if res.code != 1 || res.stdout != "" || !strings.Contains(res.stderr, "will not rewrite "+mcp+": ") ||
		!strings.Contains(res.stderr, "cut short between replacing this file and recording its digest") || !strings.Contains(res.stderr, "with the same numbers and from the same binary") {
		t.Fatalf("exit %d, want a refusal saying how the file was left\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	// Nothing of the file's bytes is in it: the file is not shown to be the line's, so
	// its command, flags and numbers are not echoed.
	for _, bad := range []string{string(data), "--cap", "--window", "--chair", "mcpServers", "PARLEY_NICK"} {
		if strings.Contains(res.stderr, bad) || strings.Contains(res.stdout, bad) {
			t.Errorf("the refusal holds %q, which came from the file:\n%s", bad, res.stderr)
		}
	}
	b.wantLineUnchanged(t, before, co)

	res = mustLine(t, b, b.lset("demo", "--cap", "30")...)
	if !strings.Contains(res.stdout, mcp+" already holds what this run writes (session plan): its digest is recorded.") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	b.wantMCP(t, mcp, "demo", "30", "10m")
	b.wantRecordTrue(t)
	if rec := b.record(t, "demo"); rec.Cap != 30 {
		t.Errorf("record cap %d, want 30", rec.Cap)
	}
	mustLine(t, b, b.ldrop("demo", "plan")...)
}

// TestLineSetSaysWhatIsGoneWhenTheNumbersAreTheSame: naming the numbers the line has
// is not an early return that hides a launch file that is gone: it is named, with the
// way back for the sessions that use it, and nothing is written.
func TestLineSetSaysWhatIsGoneWhenTheNumbersAreTheSame(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co, "--model", "opus")
	b.mustAdd(t, "demo", "review", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")
	if err := os.Remove(mcp); err != nil {
		t.Fatal(err)
	}
	before := b.lsnap(t, co)

	res := mustLine(t, b, b.lset("demo", "--cap", "8", "--window", "10m")...)
	t.Logf("stdout:\n%s", res.stdout)
	if strings.Contains(res.stdout, "nothing was changed") {
		t.Errorf("a run that finds a launch file gone says nothing was changed:\n%s", res.stdout)
	}
	if strings.Contains(res.stdout, "Updated") || strings.Contains(res.stdout, "--resume") {
		t.Errorf("a run that writes nothing says it updated the record, or tells a session with no file to resume:\n%s", res.stdout)
	}
	wantInOrder(t, res.stdout, []string{
		"Line demo already has cap 8 and window 10m; nothing was written.",
		mcp + " is not there (sessions plan, review), so nothing was written for it.",
	})
	for _, nick := range []string{"plan", "review"} {
		if want := nick + " (Claude Code): " + mcp + " is not there, so restarting it loads no parley server."; !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	drop := lineWith(t, res.stdout, "line drop demo plan")
	add := lineWith(t, res.stdout, "line add demo --nick plan")
	if want := "writes the file with the line's numbers"; !strings.Contains(res.stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
	}
	if strings.Contains(res.stdout, "primer still states the old numbers") {
		t.Errorf("no number changed, so no primer went stale:\n%s", res.stdout)
	}
	b.wantLineUnchanged(t, before, co)

	// The way back runs as printed.
	for _, line := range []string{drop, add} {
		if r := b.runBinaryIn(t, "sh", "-c", line); r.code != 0 {
			t.Fatalf("%s exited %d\n%s%s", line, r.code, r.stdout, r.stderr)
		}
	}
	b.wantMCP(t, mcp, "demo", "8", "10m")
}

// TestLineSetNamesAnotherBinaryWhenTheNumbersAreTheSame: a file that runs another
// parley binary and has the numbers asked for is left, and the note that says whose it
// is is printed all the same, not hidden by an early return.
func TestLineSetNamesAnotherBinaryWhenTheNumbersAreTheSame(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	dev := filepath.Join(b.root, "parley-dev")
	if err := os.WriteFile(dev, mustRead(t, binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := b.runBinaryIn(t, dev, b.ladd("demo", "plan", "claude", co)...); res.code != 0 {
		t.Fatalf("line add by the other binary exited %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	mcp := filepath.Join(co, ".mcp.json")
	cmd, _ := readMCP(t, mcp)
	if cmd == binPath {
		t.Fatalf("the file runs %q, want the other binary", cmd)
	}
	before := b.lsnap(t, co)

	res := mustLine(t, b, b.lset("demo", "--cap", "8")...)
	note := "Note: the parley command in " + mcp + " is " + cmd + ", not " + binPath + ", the binary running line set; the file already has the numbers asked for, so this run leaves it as it is."
	if !strings.Contains(res.stdout, note) || !strings.Contains(res.stdout, "nothing was changed") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	b.wantLineUnchanged(t, before, co)
}

// TestLineSetChecksTheContextBeforeEveryWrite: a run stopped before its first file is
// written has written nothing, a run with no file to write still does not commit the
// numbers once it is cancelled, and an interrupt that comes while the last file is
// replaced is seen before the numbers are written; each says what was done.
func TestLineSetChecksTheContextBeforeEveryWrite(t *testing.T) {
	t.Run("nothing to rewrite", func(t *testing.T) {
		b := newLineBench(t)
		lineUp(t, b, "demo")
		co := b.gitWorkdir(t, "co")
		b.mustAdd(t, "demo", "plan", "claude", co)
		if err := os.Remove(filepath.Join(co, ".mcp.json")); err != nil {
			t.Fatal(err)
		}
		before := b.lsnap(t, co)
		plan, err := planLineSet(b.record(t, "demo"), b.base, binPath, 30, "10m")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err = plan.apply(ctx, b.recordDir())
		if err == nil || !strings.Contains(err.Error(), "interrupted") || !strings.Contains(err.Error(), "line.json was not changed and holds cap 8, window 10m") {
			t.Fatalf("apply under a cancelled context, with no file to write: %v", err)
		}
		b.wantLineUnchanged(t, before, co)
	})

	t.Run("while the last file is replaced", func(t *testing.T) {
		b := newLineBench(t)
		lineUp(t, b, "demo")
		co := b.gitWorkdir(t, "co")
		b.mustAdd(t, "demo", "plan", "claude", co)
		mcp := filepath.Join(co, ".mcp.json")
		plan, err := planLineSet(b.record(t, "demo"), b.base, binPath, 30, "10m")
		if err != nil {
			t.Fatal(err)
		}
		// Live when the file is about to be written; cancelled by the time the numbers are.
		err = plan.apply(&cancelAfter{Context: context.Background(), left: 1}, b.recordDir())
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("apply cancelled during its last file: %v", err)
		}
		for _, want := range []string{"rewritten: " + mcp + ";", "not rewritten: none;", "line.json holds the new digest of " + mcp + ", and cap 8, window 10m, which are written last"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the report lacks %q:\n%v", want, err)
			}
		}
		// The record is true: the file's digest, the old numbers.
		b.wantMCP(t, mcp, "demo", "30", "10m")
		if rec := b.record(t, "demo"); rec.Cap != 8 {
			t.Errorf("the numbers were written, cap %d", rec.Cap)
		}
		b.wantRecordTrue(t)
		mustLine(t, b, b.ldrop("demo", "plan")...)
	})
}

// TestLineSetIsConsistentWhereverItIsInterrupted: a run over two files asks its context
// three times, before each file and before the numbers. Stopped at any of them, the
// record is true of the files, and a run with other numbers goes on from there and ends
// with every file and the record agreeing.
func TestLineSetIsConsistentWhereverItIsInterrupted(t *testing.T) {
	const questions = 3
	for cut := 0; cut <= questions; cut++ {
		t.Run(fmt.Sprintf("cancelled after %d questions", cut), func(t *testing.T) {
			b := newLineBench(t)
			mcp1, mcp2 := b.twoClaudeSessions(t)
			plan, err := planLineSet(b.record(t, "demo"), b.base, binPath, 30, "10m")
			if err != nil {
				t.Fatal(err)
			}
			err = plan.apply(&cancelAfter{Context: context.Background(), left: cut}, b.recordDir())
			b.wantRecordTrue(t)
			rec := b.record(t, "demo")
			if cut == questions {
				// Not cut: every file and the record have the numbers.
				if err != nil {
					t.Fatalf("apply: %v", err)
				}
				b.wantMCP(t, mcp1, "demo", "30", "10m")
				b.wantMCP(t, mcp2, "demo", "30", "10m")
				if rec.Cap != 30 {
					t.Errorf("record cap %d, want 30", rec.Cap)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "interrupted") {
				t.Fatalf("apply cancelled after %d questions: %v", cut, err)
			}
			if rec.Cap != 8 {
				t.Errorf("record cap %d, want the old numbers until the last step", rec.Cap)
			}
			// Other numbers go on from wherever it was cut.
			mustLine(t, b, b.lset("demo", "--cap", "40", "--window", "5m")...)
			b.wantMCP(t, mcp1, "demo", "40", "5m")
			b.wantMCP(t, mcp2, "demo", "40", "5m")
			if rec := b.record(t, "demo"); rec.Cap != 40 || rec.Window != "5m" {
				t.Errorf("record cap %d window %s, want 40 and 5m", rec.Cap, rec.Window)
			}
			b.wantRecordTrue(t)
			mustLine(t, b, b.ldrop("demo", "plan")...)
			mustLine(t, b, b.ldrop("demo", "review")...)
		})
	}
}

// TestLineSetEveryRunConverges: a run is cut after the first of two files (the second
// cannot be written), and a run with other numbers completes from there, with every
// file and the record in agreement; so does a run with the numbers the cut one had, and
// one with the record's own.
func TestLineSetEveryRunConverges(t *testing.T) {
	b := newLineBench(t)
	mcp1, mcp2, _ := b.partWay(t, "")
	b.wantMCP(t, mcp1, "demo", "30", "10m")
	b.wantMCP(t, mcp2, "demo", "8", "10m")

	for _, step := range []struct {
		capN   int
		window string
	}{{40, "5m"}, {30, "10m"}, {8, "10m"}} {
		res := mustLine(t, b, b.lset("demo", "--cap", strconv.Itoa(step.capN), "--window", step.window)...)
		if strings.Contains(res.stdout, "nothing was changed") {
			t.Errorf("--cap %d: a run that wrote files says nothing was changed:\n%s", step.capN, res.stdout)
		}
		cap := strconv.Itoa(step.capN)
		b.wantMCP(t, mcp1, "demo", cap, step.window)
		b.wantMCP(t, mcp2, "demo", cap, step.window)
		if rec := b.record(t, "demo"); rec.Cap != step.capN || rec.Window != step.window {
			t.Errorf("--cap %d --window %s: record cap %d window %s", step.capN, step.window, rec.Cap, rec.Window)
		}
		b.wantRecordTrue(t)
		wantNoTempFiles(t, filepath.Dir(mcp1), filepath.Dir(mcp2), b.recordDir())
	}
	mustLine(t, b, b.ldrop("demo", "plan")...)
	mustLine(t, b, b.ldrop("demo", "review")...)
}

// TestLineSetRecordsEachFileAsItIsWritten: after the first of two files the record
// holds that file's new digest and the old numbers, and `line drop` recognizes every
// file: the first by its new digest, the second by the one it always had.
func TestLineSetRecordsEachFileAsItIsWritten(t *testing.T) {
	b := newLineBench(t)
	mcp1, mcp2, _ := b.partWay(t, "")
	rec := b.record(t, "demo")
	if rec.Cap != 8 || rec.Window != "10m" {
		t.Errorf("record cap %d window %s, want the old numbers, 8 and 10m", rec.Cap, rec.Window)
	}
	b.wantRecordTrue(t)
	if got := sessionByNick(t, rec, "plan").Files[0].SHA256; got != fileDigestOf(t, mcp1) {
		t.Errorf("plan's digest %s, want the first file's, %s", got, fileDigestOf(t, mcp1))
	}
	mustLine(t, b, b.ldrop("demo", "plan")...)
	mustLine(t, b, b.ldrop("demo", "review")...)
	for _, f := range []string{mcp1, mcp2} {
		if _, err := os.Lstat(f); err == nil {
			t.Errorf("%s is still there: drop did not recognize it", f)
		}
	}
}

// TestLineSetRecordWriteFailingAfterARenameIsFinishedByTheSameNumbers: the record cannot
// be written once the first file has been replaced (the record's directory is not
// writable, as a full disk would also do). The report says the record was not changed
// and does not hold the file's digest, and names the line that finishes it; other
// numbers get the documented refusal, with nothing changed; the same numbers
// recognize the file by its bytes, record its digest and finish, and tell its session to
// restart.
func TestLineSetRecordWriteFailingAfterARenameIsFinishedByTheSameNumbers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a directory's mode does not stop root writing in it")
	}
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")
	plan, err := planLineSet(b.record(t, "demo"), b.base, binPath, 30, "10m")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(b.recordDir(), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(b.recordDir(), 0o700) })
	err = plan.apply(context.Background(), b.recordDir())
	if err := os.Chmod(b.recordDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("apply succeeded with a record that could not be written")
	}
	finish := b.setLine(binPath, 30, "10m")
	for _, want := range []string{
		"rewritten: " + mcp + ";", "not rewritten: none;",
		"line.json was not changed and holds cap 8, window 10m; it does not hold the digest of " + mcp,
		mcp + " holds the bytes this run writes: once the cause is cleared, finish with " + finish + ", the same numbers from the same binary",
		"other numbers are refused for that file until then",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure lacks %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "holds the new digest of none") {
		t.Errorf("the failure says the record holds the digest of none:\n%v", err)
	}
	b.wantMCP(t, mcp, "demo", "30", "10m")
	if rec := b.record(t, "demo"); rec.Cap != 8 || sessionByNick(t, rec, "plan").Files[0].SHA256 == fileDigestOf(t, mcp) {
		t.Fatalf("the record changed: %+v", rec)
	}
	wantNoTempFiles(t, co, b.recordDir())

	// Other numbers: refused, the way a file left by a kill is.
	before := b.lsnap(t, co)
	res := b.run(t, b.lset("demo", "--cap", "40")...)
	if res.code != 1 || res.stdout != "" || !strings.Contains(res.stderr, "will not rewrite "+mcp+": ") ||
		!strings.Contains(res.stderr, "cut short between replacing this file and recording its digest") {
		t.Fatalf("exit %d, want the documented refusal\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	b.wantLineUnchanged(t, before, co)

	// The same numbers adopt the file and finish.
	res = b.runBinaryIn(t, "sh", "-c", finish)
	if res.code != 0 {
		t.Fatalf("the printed command exited %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	for _, want := range []string{
		mcp + " already holds what this run writes (session plan): its digest is recorded.",
		"plan (Claude Code): restart it, resuming its conversation:",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	if rec := b.record(t, "demo"); rec.Cap != 30 {
		t.Errorf("record cap %d, want 30", rec.Cap)
	}
	b.wantRecordTrue(t)
	mustLine(t, b, b.ldrop("demo", "plan")...)
}

// TestLineSetSameNumbersThatAdoptsAHeldFileTellsItsSessionsToRestart: a run that names
// the record's own numbers and finds a file holding the bytes it would write (the
// earlier run was killed after replacing it, and a front may have started from what it
// held before) records the digest and tells that file's sessions to restart, and no
// other.
func TestLineSetSameNumbersThatAdoptsAHeldFileTellsItsSessionsToRestart(t *testing.T) {
	b := newLineBench(t)
	mcp1, mcp2 := b.twoClaudeSessions(t)
	rec := b.record(t, "demo")
	// The record has the digest of a file that said cap 30, as an earlier cut-short run
	// leaves it, while the file holds the bytes for the record's own numbers.
	other := rec
	other.Cap = 30
	data, err := lineMCPJSON(binPath, other, b.base)
	if err != nil {
		t.Fatal(err)
	}
	rec.Sessions[0].Files[0].SHA256 = tableDigest(data)
	if _, err := writeLineRecord(b.recordDir(), rec); err != nil {
		t.Fatal(err)
	}

	res := mustLine(t, b, b.lset("demo", "--cap", "8")...)
	t.Logf("stdout:\n%s", res.stdout)
	for _, want := range []string{
		mcp1 + " already holds what this run writes (session plan): its digest is recorded.",
		"Left " + mcp2 + " as it is (session review): it already has cap 8 and window 10m.",
		"plan (Claude Code): restart it, resuming its conversation:",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	for _, bad := range []string{"review (Claude Code)", "no session needs a restart", "still states the old numbers"} {
		if strings.Contains(res.stdout, bad) {
			t.Errorf("stdout says %q:\n%s", bad, res.stdout)
		}
	}
	if got := sessionByNick(t, b.record(t, "demo"), "plan").Files[0].SHA256; got != fileDigestOf(t, mcp1) {
		t.Errorf("plan's digest %s, want the file's, %s", got, fileDigestOf(t, mcp1))
	}
	b.wantRecordTrue(t)
}
