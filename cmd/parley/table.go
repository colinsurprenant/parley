package main

import (
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
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream"
)

// A table's files all live under <state base>/tables/<id>/: table.json, the
// record `table new` leaves for `table end`, the sheet copy, and by default the
// worktree. "tables" sits beside the per-nick directories of the same base, which
// is why no participant may be called that.
const (
	tablesDirName     = state.TablesDir
	tableRecordName   = "table.json"
	tableSheetName    = "sheet.md"
	tableWorktreeName = "worktree"
)

// tableRecordMax bounds what readTableRecord reads. The record is the ids, paths
// and nicks of a few seats, a few KiB at the most; a larger file is damage, a
// bad disk's or a stray write's, and is refused as corrupt.
const tableRecordMax = 64 << 10

// tableIDRE is what a table id may be: it is the channel #<id> and a directory
// name, so it is lower case, starts with a letter or a digit, and stays short.
var tableIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func checkTableID(id string) error {
	if !tableIDRE.MatchString(id) {
		return cfgErrf("table id %q must match [a-z0-9][a-z0-9-]{0,31}: it is the channel #<id> and a directory name", id)
	}
	return nil
}

func tableDirOf(base, id string) string { return filepath.Join(base, tablesDirName, id) }

// cmdTable runs the table verbs: new sets a table up, end takes it down.
func cmdTable(args []string) error {
	if len(args) == 0 {
		tableUsage(os.Stderr)
		return cfgErrf("parley table needs a verb: new or end")
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		tableUsage(os.Stdout)
		return nil
	case "new":
		return cmdTableNew(args[1:])
	case "end":
		return cmdTableEnd(args[1:])
	}
	tableUsage(os.Stderr)
	return cfgErrf("unknown table verb %q: the verbs are new and end", args[0])
}

func tableUsage(w io.Writer) {
	fmt.Fprint(w, tableUsageNew+"\n"+tableUsageEnd+"\n")
}

const (
	tableUsageNew = "usage: parley table new <id> --target <repo> --chair <nick> --rounds <n> --seat <nick>=<claude|codex>[:<model>] [--seat ...]\n" +
		"                       [--worktree <dir>] [--server 127.0.0.1:6667] [--state-dir <dir>] [--codex-bin codex]"
	tableUsageEnd = "usage: parley table end <id> [--force] [--state-dir <dir>]"
)

// splitTableID takes the table id off the front of a verb's arguments. The id
// comes first, ahead of the flags, as `parley table new t1 --target ...`; a verb
// given flags first has its flags parsed so that -h answers, and an id after them
// is taken too.
func splitTableID(args []string) (id string, rest []string, hasID bool) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:], true
	}
	return "", args, false
}

// tableRecord is what `table new` keeps of a table so that `table end` can take
// it down: where everything is, and the thread each Codex seat was started as. It
// holds no stream content, only names and paths.
type tableRecord struct {
	ID       string       `json:"id"`
	Stream   string       `json:"stream"`
	Target   string       `json:"target"`
	Worktree string       `json:"worktree"`
	ReplyDir string       `json:"reply_dir"`
	Chair    string       `json:"chair"`
	Rounds   int          `json:"rounds"`
	Cap      int          `json:"cap"`
	Seats    []seatRecord `json:"seats"`
	Created  string       `json:"created"`

	// digest is tableDigest of the bytes the record was read from, set by
	// readTableRecord and never part of the file: it is what the markers table new
	// sealed are compared with.
	digest string
}

// seatRecord is one seat. Thread and ReplyFile are a Codex seat's; a Claude
// seat's reply directory is the channel's own, made when its session starts.
type seatRecord struct {
	Nick      string `json:"nick"`
	Harness   string `json:"harness"`
	Model     string `json:"model,omitempty"`
	Thread    string `json:"thread,omitempty"`
	ReplyFile string `json:"reply_file,omitempty"`
}

// tableDigest is the digest the markers carry: sha256 of the exact bytes of
// table.json, as "sha256:" and lower-case hex.
func tableDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeTableRecord stores the record in dir as table.json, mode 0600, through a
// temp file and a rename: `table end` must find the whole record or none. It
// returns the digest of the bytes it wrote, which the markers are sealed with. A
// record `table end` would refuse to read, for its size, is refused here instead.
func writeTableRecord(dir string, rec tableRecord) (digest string, err error) {
	return writeRecordFile(dir, tableRecordName, "table", rec, tableRecordMax, "table end")
}

// writeRecordFile is writeTableRecord for any record: kind names it in a failure,
// max is the most its reader (the command reader, "table end" or "build end")
// reads, and the encoded record is checked against it before anything is written,
// so that setup never leaves a record that its own teardown refuses.
func writeRecordFile(dir, name, kind string, rec any, max int, reader string) (digest string, err error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rec); err != nil {
		return "", fmt.Errorf("encode the %s record: %w", kind, err)
	}
	if buf.Len() > max {
		return "", fmt.Errorf("the %s record would be %d bytes, over the %d bytes %s reads, and it would then refuse to take the %s down: shorten what it holds (the paths, the models)", kind, buf.Len(), max, reader, kind)
	}
	if err := writeFileAtomic(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
		return "", err
	}
	return tableDigest(buf.Bytes()), nil
}

// writeFileAtomic writes data to a temp file beside path and renames it into
// place, so a reader never sees half of it and a failure leaves nothing behind.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create a temp file for %s: %w", path, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return fmt.Errorf("set permissions on %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// noTableErr is the refusal for a table that is not there.
func noTableErr(base, id string) error {
	return cfgErrf("no table %q under %s (looked for %s)", id, base, filepath.Join(tableDirOf(base, id), tableRecordName))
}

// tableDirIsThere is whether anything is at the table's directory: `table end`
// asks it before it takes the id's lock, so that an id that was never a table
// leaves no lock file in the base. The record is read after the lock is taken,
// and that read is the one that counts.
func tableDirIsThere(base, id string) bool {
	_, err := os.Lstat(tableDirOf(base, id))
	return !errors.Is(err, fs.ErrNotExist)
}

// readTableRecord loads and checks the record of table id under base, and keeps
// the digest of the bytes it read once. A table that is not there is the
// invocation's mistake; a record that is there and is not one this binary wrote
// is not trusted, because `table end` deletes the paths it names.
func readTableRecord(base, id string) (tableRecord, error) {
	path := filepath.Join(tableDirOf(base, id), tableRecordName)
	b, err := readRecordFile(path, "table", tableRecordMax)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return tableRecord{}, noTableErr(base, id)
		}
		return tableRecord{}, err
	}
	var rec tableRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return tableRecord{}, fmt.Errorf("table record %s is not valid: %w", path, err)
	}
	rec.digest = tableDigest(b)
	if err := rec.check(id); err != nil {
		return tableRecord{}, fmt.Errorf("table record %s is not valid: %w", path, err)
	}
	return rec, nil
}

// readRecordFile reads a record of kind ("table", "build") through max bytes: a
// regular file, not a link, and not larger than a record this binary wrote. A
// file that is not there is returned as the error OpenRegular gave, for the
// caller to tell from damage.
func readRecordFile(path, kind string, max int) ([]byte, error) {
	f, err := state.OpenRegular(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("read the %s record: %w", kind, err)
	}
	defer f.Close()
	// Read through one byte past the maximum, so that a file too big to be a
	// record is known to be one without being read whole.
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, fmt.Errorf("read the %s record %s: %w", kind, path, err)
	}
	if len(b) > max {
		return nil, fmt.Errorf("%s record %s is not valid: it is larger than %d bytes, and a record this binary wrote is not", kind, path, max)
	}
	return b, nil
}

func (r tableRecord) check(id string) error {
	if r.ID != id {
		return fmt.Errorf("it is table %q, not %q", r.ID, id)
	}
	for _, p := range []struct{ name, path string }{{"target", r.Target}, {"worktree", r.Worktree}, {"reply_dir", r.ReplyDir}} {
		if !filepath.IsAbs(p.path) {
			return fmt.Errorf("%s %q is not an absolute path", p.name, p.path)
		}
	}
	if filepath.Clean(r.Worktree) == filepath.Clean(r.Target) {
		return fmt.Errorf("the worktree is the target itself")
	}
	if !strings.HasPrefix(filepath.Base(r.ReplyDir), replyDirPrefix(id)) {
		return fmt.Errorf("reply_dir %q is not a reply directory of table %q", r.ReplyDir, id)
	}
	if r.Stream == "" {
		return errors.New("no stream")
	}
	if len(r.Seats) == 0 {
		return errors.New("it lists no seats, and a table new made always has one")
	}
	for i, s := range r.Seats {
		if err := stream.ValidNick(s.Nick); err != nil {
			return fmt.Errorf("seat nick %q %v", s.Nick, err)
		}
		for _, prev := range r.Seats[:i] {
			if stream.NickEqual(s.Nick, prev.Nick) {
				return fmt.Errorf("seat nick %q is also seat %q's", s.Nick, prev.Nick)
			}
		}
		if s.Thread != "" && !state.ValidThreadID(s.Thread) {
			return fmt.Errorf("seat %s: thread %q is not a UUID", s.Nick, s.Thread)
		}
	}
	return nil
}

// gitEnv is the environment git is run in: the caller's, less the variables that
// would point it at a repository other than the one named with -C.
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_PREFIX":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// gitOutputMax bounds what is kept of git's stdout, and gitStderrMax of its
// stderr. What table new and table end read from git is a few lines; a status
// of a worktree holding a large ignored tree, or a chatty hook, is not kept
// whole. A variable so that a test can make the bound small.
var gitOutputMax = 1 << 20

const gitStderrMax = 64 << 10

// git runs git without a shell and returns what it wrote to stdout. A failure
// carries the line git said on stderr that names the cause, and output past
// gitOutputMax is a failure too.
func git(ctx context.Context, args ...string) (string, error) {
	out, truncated, err := gitCapped(ctx, args...)
	if err == nil && truncated {
		err = fmt.Errorf("git %s: more than %d bytes of output", strings.Join(args, " "), gitOutputMax)
	}
	return out, err
}

// gitCapped is git for a caller that can act on a cut-off stdout: it keeps the
// first gitOutputMax bytes and says whether it dropped any.
func gitCapped(ctx context.Context, args ...string) (out string, truncated bool, err error) {
	return runGit(ctx, "", gitEnv(), args, args)
}

// runGit is the one place git is run: dir is its working directory ("" for
// parley's own), env its environment, args what it is given, and shown the part
// of args a failure names (the flags every call of a kind carries are not worth
// repeating in each message).
func runGit(ctx context.Context, dir string, env, args, shown []string) (out string, truncated bool, err error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	stdout, stderr := &limitBuffer{limit: gitOutputMax}, &limitBuffer{limit: gitStderrMax}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if line := gitFailureLine(stderr.String()); line != "" {
			return stdout.String(), stdout.truncated, fmt.Errorf("git %s: %w: %s", strings.Join(shown, " "), err, line)
		}
		return stdout.String(), stdout.truncated, fmt.Errorf("git %s: %w", strings.Join(shown, " "), err)
	}
	return stdout.String(), stdout.truncated, nil
}

// gitFailureLine is the line of git's stderr that says what went wrong: the
// first "fatal:" or "error:" one, since git may print progress ahead of it
// ("Preparing worktree ..."), and otherwise the first line.
func gitFailureLine(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "fatal:") || strings.HasPrefix(line, "error:") {
			return line
		}
	}
	return firstLine(stderr)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// withoutThreadEnv is the environment a Codex child is run in: the caller's, less
// $CODEX_THREAD_ID, so a table set up from inside a Codex session does not hand
// that session's id to the seats it starts.
func withoutThreadEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, codexThreadEnv+"=") {
			env = append(env, kv)
		}
	}
	return env
}

// limitBuffer keeps the first limit bytes written to it and discards the rest,
// so a runaway child cannot fill memory through a pipe. truncated says whether
// anything was discarded.
type limitBuffer struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	room := max(b.limit-b.buf.Len(), 0)
	b.buf.Write(p[:min(len(p), room)])
	if len(p) > room {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitBuffer) String() string { return b.buf.String() }

// hasControl reports whether s holds a control character.
func hasControl(s string) bool { return strings.IndexFunc(s, unicode.IsControl) >= 0 }

// resolveExisting resolves the symbolic links in path as far as it exists and
// appends the rest as spelled: a path that is yet to be made still has a place
// to be compared by.
func resolveExisting(path string) string {
	path = filepath.Clean(path)
	var rest []string
	for p := path; ; p = filepath.Dir(p) {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved
		}
		if filepath.Dir(p) == p {
			return path
		}
		rest = append(rest, filepath.Base(p))
	}
}

// within reports whether path is dir or somewhere under it, both as resolved.
func within(dir, path string) bool {
	rel, err := filepath.Rel(resolveExisting(dir), resolveExisting(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
