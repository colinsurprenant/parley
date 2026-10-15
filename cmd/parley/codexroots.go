package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A Codex peer's project config (.codex/config.toml) that names writable_roots
// replaces the user's global list, measured with codex 0.161.0 (docs/build.md,
// "The Codex sandbox"). So when --writable-root names a root, build new carries
// the roots the user's own config lists over as well, and naming a root adds to
// them. This file reads that one key out of $CODEX_HOME/config.toml and carries
// what it finds through the validation --writable-root gets.

// codexConfigMax bounds the read of the user's Codex config. A real one is a few
// kilobytes.
const codexConfigMax = 1 << 20

// tomlMaxDepth bounds how deep arrays and inline tables nest in what the reader
// walks through, so a hostile file cannot run it out of stack.
const tomlMaxDepth = 64

// codexConfigPath is the user's Codex config: $CODEX_HOME/config.toml, or
// ~/.codex/config.toml. An empty $CODEX_HOME is unset, as Codex reads it.
func codexConfigPath() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("neither $CODEX_HOME nor a home directory is set: %v", err)
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// readCodexConfig reads the file at path, a regular file (a link to one is
// followed) of at most codexConfigMax bytes.
func readCodexConfig(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, codexConfigMax+1))
	if err != nil {
		return "", err
	}
	if len(data) > codexConfigMax {
		return "", fmt.Errorf("%s is larger than %d bytes", path, codexConfigMax)
	}
	return string(data), nil
}

// skippedRoot is a global root build new did not carry, and why.
type skippedRoot struct{ root, why string }

// globalRoots is what carrying the user's global Codex roots came to.
type globalRoots struct {
	path string
	// why is, when no root was read from the config, the reason: the file is
	// absent or unreadable, does not parse, has no writable_roots key, or lists
	// none. The Codex peers then have the roots --writable-root named and no
	// others.
	why     string
	carried []string // physical paths, validated, not among the named ones
	skipped []skippedRoot
}

// carryGlobalRoots reads the global roots from the Codex config at path and
// validates each as --writable-root is. A root already among named, or listed
// twice, is carried once.
func carryGlobalRoots(path string, named []string) *globalRoots {
	g := &globalRoots{path: path}
	src, err := readCodexConfig(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			g.why = path + " does not exist"
		} else {
			g.why = "could not read " + path + ": " + err.Error()
		}
		return g
	}
	raw, found, err := parseWritableRoots(src)
	if err != nil {
		g.why = "could not parse " + path + ": " + err.Error()
		return g
	}
	if !found {
		g.why = path + " has no writable_roots key under sandbox_workspace_write"
		return g
	}
	if len(raw) == 0 {
		g.why = path + " lists no writable_roots under sandbox_workspace_write"
		return g
	}
	seen := map[string]bool{}
	for _, n := range named {
		seen[n] = true
	}
	for _, r := range raw {
		phys, err := physicalRoot(r)
		if err != nil {
			g.skipped = append(g.skipped, skippedRoot{r, err.Error()})
			continue
		}
		if !seen[phys] {
			seen[phys] = true
			g.carried = append(g.carried, phys)
		}
	}
	return g
}

// carryGlobal is carryGlobalRoots for the user's Codex config, wherever
// $CODEX_HOME or the home directory puts it.
func carryGlobal(named []string) *globalRoots {
	path, err := codexConfigPath()
	if err != nil {
		return &globalRoots{why: err.Error()}
	}
	return carryGlobalRoots(path, named)
}

// configRoots are the roots the Codex peers' config names: the ones --writable-root
// named, then the user's global ones carried over. Without --writable-root, none:
// the user's global roots apply by themselves.
func (p *buildPlan) configRoots() []string {
	roots := append([]string(nil), p.writableRoots...)
	if p.global != nil {
		roots = append(roots, p.global.carried...)
	}
	return roots
}

// printable is s, or s quoted when it holds a control character, for a line that
// quotes a path or an error the user's environment shaped.
func printable(s string) string {
	if hasControl(s) {
		return strconv.Quote(s)
	}
	return s
}

// globalRootsLog is what build new prints in its setup log about the global roots, each
// a line after "parley build new: ". It says nothing when --writable-root named
// none: the global roots then apply to the peers by themselves.
func (p *buildPlan) globalRootsLog() []string {
	g := p.global
	if g == nil {
		return nil
	}
	if g.why != "" {
		return []string{"warning: naming a root replaced your global Codex writable roots, and build new did not carry them over: " + printable(g.why) +
			"; re-pass any a peer needs with --writable-root (docs/build.md)"}
	}
	var lines []string
	if len(g.carried) > 0 {
		lines = append(lines, "carrying your global Codex writable roots from "+printable(g.path)+" into each Codex peer's config: "+strings.Join(g.carried, ", "))
	}
	for _, s := range g.skipped {
		lines = append(lines, "not carrying global writable root "+printable(s.why))
	}
	if len(g.carried) == 0 && len(g.skipped) == 0 {
		lines = append(lines, "your global Codex writable roots in "+printable(g.path)+" are all among the --writable-root ones")
	}
	return lines
}

// globalRootsRunbook is what the runbook's Codex step says about the global roots
// after the sentence naming the --writable-root ones: the roots carried over and
// the ones that were not, or, when none could be carried, the warning that naming
// a root replaced them and why they were not.
func (p *buildPlan) globalRootsRunbook() []string {
	g := p.global
	if g == nil {
		return nil
	}
	if g.why != "" {
		return []string{
			"Naming a root replaced your global Codex roots: re-pass any it needs. docs/build.md says more.",
			"build new did not carry them over: " + printable(g.why) + ".",
		}
	}
	var lines []string
	if len(g.carried) > 0 {
		lines = append(lines, "It also writes your global Codex roots, carried over from "+printable(g.path)+": "+strings.Join(g.carried, ", ")+".")
	}
	for _, s := range g.skipped {
		lines = append(lines, "Not carried over, global writable root "+printable(s.why)+".")
	}
	if len(g.skipped) > 0 {
		lines = append(lines, "Re-pass any of those it needs with --writable-root. docs/build.md says more.")
	}
	if len(g.carried) == 0 && len(g.skipped) == 0 {
		lines = append(lines, "Your global Codex roots in "+printable(g.path)+" are all among the ones named.")
	}
	return lines
}

// physicalRoot validates r as a writable root and resolves it: absolute, free of
// control characters and of anything that is not UTF-8 (it is written as a TOML
// string), an existing directory, symbolic links evaluated. The error starts with
// r, quoted, so that a caller can put its own label before it.
func physicalRoot(r string) (string, error) {
	switch {
	case !filepath.IsAbs(r):
		return "", fmt.Errorf("%q must be an absolute path", r)
	case hasControl(r):
		return "", fmt.Errorf("%q holds a control character", r)
	case !utf8.ValidString(r):
		return "", fmt.Errorf("%q is not valid UTF-8: it is written into a TOML string", r)
	}
	phys, err := filepath.EvalSymlinks(r)
	if err != nil {
		return "", fmt.Errorf("%q: %v", r, err)
	}
	fi, err := os.Stat(phys)
	switch {
	case err != nil:
		return "", fmt.Errorf("%q: %v", r, err)
	case !fi.IsDir():
		return "", fmt.Errorf("%q is not a directory", r)
	case hasControl(phys) || !utf8.ValidString(phys):
		return "", fmt.Errorf("%q resolves to %q, which holds a control character or is not UTF-8: it is written as a TOML string", r, phys)
	}
	return phys, nil
}

// parseWritableRoots returns the strings of the writable_roots array under
// sandbox_workspace_write in src, a TOML document, and whether it is there at
// all. The key is read as a [sandbox_workspace_write] table's, as a dotted key
// sandbox_workspace_write.writable_roots, and inside an inline table; profiles
// and every other key are walked past, not read. It is not a TOML validator: it
// parses enough to find the key and to know where every other value ends, and a
// document it cannot follow is an error, never a panic.
func parseWritableRoots(src string) (roots []string, found bool, err error) {
	// TOML is UTF-8, so Codex refuses a config that is not, even where the bad
	// byte sits in a value this reader would walk past.
	if !utf8.ValidString(src) {
		return nil, false, errors.New("the file is not valid UTF-8, so it is not TOML")
	}
	r := &tomlReader{s: strings.TrimPrefix(src, "\xef\xbb\xbf")}
	if err := r.document(); err != nil {
		return nil, false, err
	}
	return r.roots, r.found, nil
}

type tomlReader struct {
	s     string
	i     int
	depth int
	roots []string
	found bool
}

func (r *tomlReader) errf(format string, a ...any) error {
	line := 1 + strings.Count(r.s[:min(r.i, len(r.s))], "\n")
	return fmt.Errorf("line %d: %s", line, fmt.Sprintf(format, a...))
}

func (r *tomlReader) eof() bool { return r.i >= len(r.s) }

// peek is the byte at the cursor, or 0 at the end; a caller that must tell a NUL
// byte from the end asks eof.
func (r *tomlReader) peek() byte {
	if r.eof() {
		return 0
	}
	return r.s[r.i]
}

func (r *tomlReader) skipSpace() {
	for !r.eof() && (r.s[r.i] == ' ' || r.s[r.i] == '\t') {
		r.i++
	}
}

func (r *tomlReader) skipComment() {
	if r.peek() == '#' {
		for !r.eof() && r.s[r.i] != '\n' {
			r.i++
		}
	}
}

// skipBlank skips whitespace, newlines and comments.
func (r *tomlReader) skipBlank() {
	for {
		r.skipSpace()
		r.skipComment()
		if !r.eof() && (r.s[r.i] == '\n' || r.s[r.i] == '\r') {
			r.i++
			continue
		}
		return
	}
}

// endOfLine requires the rest of the line to be blank or a comment.
func (r *tomlReader) endOfLine() error {
	r.skipSpace()
	r.skipComment()
	switch {
	case r.eof(), r.s[r.i] == '\n':
		return nil
	case strings.HasPrefix(r.s[r.i:], "\r\n"):
		return nil
	}
	return r.errf("unexpected %q after a value", r.s[r.i])
}

// isRootsKey reports whether path is sandbox_workspace_write.writable_roots.
func isRootsKey(path []string) bool {
	return len(path) == 2 && path[0] == "sandbox_workspace_write" && path[1] == "writable_roots"
}

// document walks the top level: table headers and key/value lines. live is false
// under an array of tables, whose keys are never the one looked for.
func (r *tomlReader) document() error {
	var table []string
	live := true
	for {
		r.skipBlank()
		if r.eof() {
			return nil
		}
		if r.peek() == '[' {
			r.i++
			array := r.peek() == '['
			if array {
				r.i++
			}
			key, err := r.key()
			if err != nil {
				return err
			}
			r.skipSpace()
			closer := "]"
			if array {
				closer = "]]"
			}
			if !strings.HasPrefix(r.s[r.i:], closer) {
				return r.errf("a table header with no %s", closer)
			}
			r.i += len(closer)
			table, live = key, !array
		} else {
			key, err := r.key()
			if err != nil {
				return err
			}
			r.skipSpace()
			if r.peek() != '=' {
				return r.errf("expected = after the key %q", strings.Join(key, "."))
			}
			r.i++
			r.skipSpace()
			if err := r.value(join(table, key), live); err != nil {
				return err
			}
		}
		if err := r.endOfLine(); err != nil {
			return err
		}
	}
}

func join(a, b []string) []string {
	return append(append(make([]string, 0, len(a)+len(b)), a...), b...)
}

// key reads a bare or quoted key, dotted.
func (r *tomlReader) key() ([]string, error) {
	var parts []string
	for {
		r.skipSpace()
		switch c := r.peek(); {
		case r.eof():
			return nil, r.errf("expected a key, found the end of the file")
		case c == '"' || c == '\'':
			seg, err := r.str(false)
			if err != nil {
				return nil, err
			}
			parts = append(parts, seg)
		case isBareKeyByte(c):
			start := r.i
			for !r.eof() && isBareKeyByte(r.s[r.i]) {
				r.i++
			}
			parts = append(parts, r.s[start:r.i])
		default:
			return nil, r.errf("expected a key, found %q", c)
		}
		r.skipSpace()
		if r.peek() != '.' {
			return parts, nil
		}
		r.i++
	}
}

func isBareKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// value reads the value at the cursor. When path is the roots key it must be an
// array of strings, and is kept; any other value is walked past.
func (r *tomlReader) value(path []string, live bool) error {
	if r.depth >= tomlMaxDepth {
		return r.errf("values nested deeper than %d", tomlMaxDepth)
	}
	r.depth++
	defer func() { r.depth-- }()
	target := live && isRootsKey(path)
	if target && r.found {
		return r.errf("writable_roots is defined twice")
	}
	if r.eof() {
		return r.errf("a key with no value")
	}
	switch c := r.peek(); c {
	case '"', '\'':
		if _, err := r.str(true); err != nil {
			return err
		}
	case '[':
		return r.array(target)
	case '{':
		return r.inlineTable(path, live)
	default:
		if err := r.scalar(); err != nil {
			return err
		}
	}
	if target {
		return r.errf("writable_roots must be an array of strings")
	}
	return nil
}

func (r *tomlReader) array(target bool) error {
	r.i++ // [
	var elems []string
	for {
		r.skipBlank()
		if r.eof() {
			return r.errf("an array with no ]")
		}
		if r.s[r.i] == ']' {
			r.i++
			break
		}
		if target {
			if c := r.s[r.i]; c != '"' && c != '\'' {
				return r.errf("writable_roots holds %q, which does not start a string", c)
			}
			s, err := r.str(true)
			if err != nil {
				return err
			}
			elems = append(elems, s)
		} else if err := r.value(nil, false); err != nil {
			return err
		}
		r.skipBlank()
		switch r.peek() {
		case ',':
			r.i++
		case ']':
		default:
			if r.eof() {
				return r.errf("an array with no ]")
			}
			return r.errf("expected , or ] in an array, found %q", r.s[r.i])
		}
	}
	if target {
		r.roots, r.found = elems, true
	}
	return nil
}

func (r *tomlReader) inlineTable(path []string, live bool) error {
	r.i++ // {
	for {
		r.skipBlank()
		if r.peek() == '}' {
			r.i++
			return nil
		}
		key, err := r.key()
		if err != nil {
			return err
		}
		r.skipBlank()
		if r.peek() != '=' {
			return r.errf("expected = after the key %q", strings.Join(key, "."))
		}
		r.i++
		r.skipBlank()
		if err := r.value(join(path, key), live); err != nil {
			return err
		}
		r.skipBlank()
		switch r.peek() {
		case ',':
			r.i++
		case '}':
		default:
			if r.eof() {
				return r.errf("an inline table with no }")
			}
			return r.errf("expected , or } in an inline table, found %q", r.s[r.i])
		}
	}
}

// scalar walks past a number, a boolean or a date-time.
func (r *tomlReader) scalar() error {
	start := r.i
	for !r.eof() && !strings.ContainsRune(" \t\r\n,]}#", rune(r.s[r.i])) {
		r.i++
	}
	if r.i == start {
		return r.errf("expected a value, found %q", r.peek())
	}
	// A date-time may have a space where the T goes.
	if tok := r.s[start:r.i]; len(tok) == 10 && tok[4] == '-' && tok[7] == '-' &&
		r.peek() == ' ' && r.i+1 < len(r.s) && r.s[r.i+1] >= '0' && r.s[r.i+1] <= '9' {
		r.i++
		for !r.eof() && !strings.ContainsRune(" \t\r\n,]}#", rune(r.s[r.i])) {
			r.i++
		}
	}
	return nil
}

// str reads a string at the cursor: basic or literal, and when multi is true
// their multi-line forms.
func (r *tomlReader) str(multi bool) (string, error) {
	q := r.s[r.i]
	if multi && strings.HasPrefix(r.s[r.i:], strings.Repeat(string(q), 3)) {
		return r.multiline(q)
	}
	r.i++
	var b strings.Builder
	for {
		if r.eof() {
			return "", r.errf("a string with no closing quote")
		}
		switch c := r.s[r.i]; {
		case c == q:
			r.i++
			return b.String(), nil
		case c == '\n' || c == '\r':
			return "", r.errf("a string with a newline in it")
		case c == '\\' && q == '"':
			if err := r.escape(&b); err != nil {
				return "", err
			}
		default:
			b.WriteByte(c)
			r.i++
		}
	}
}

// multiline reads a triple-quoted string, basic or literal, the cursor at its
// opening delimiter.
func (r *tomlReader) multiline(q byte) (string, error) {
	delim := strings.Repeat(string(q), 3)
	r.i += 3
	// A newline right after the opening delimiter is not part of the string.
	switch {
	case strings.HasPrefix(r.s[r.i:], "\r\n"):
		r.i += 2
	case r.peek() == '\n':
		r.i++
	}
	var b strings.Builder
	for {
		if r.eof() {
			return "", r.errf("a multi-line string with no closing %s", delim)
		}
		if strings.HasPrefix(r.s[r.i:], delim) {
			// Up to two more quotes may sit just before the closing three: they
			// are the string's.
			n := 3
			for n < 5 && r.i+n < len(r.s) && r.s[r.i+n] == q {
				n++
			}
			b.WriteString(strings.Repeat(string(q), n-3))
			r.i += n
			return b.String(), nil
		}
		c := r.s[r.i]
		if c == '\\' && q == '"' {
			// A backslash that ends a line trims the whitespace and newlines after it.
			j := r.i + 1
			for j < len(r.s) && (r.s[j] == ' ' || r.s[j] == '\t') {
				j++
			}
			if j < len(r.s) && (r.s[j] == '\n' || r.s[j] == '\r') {
				for j < len(r.s) && strings.ContainsRune(" \t\r\n", rune(r.s[j])) {
					j++
				}
				r.i = j
				continue
			}
			if err := r.escape(&b); err != nil {
				return "", err
			}
			continue
		}
		b.WriteByte(c)
		r.i++
	}
}

// escape decodes the escape sequence at the cursor, a backslash, into b.
func (r *tomlReader) escape(b *strings.Builder) error {
	r.i++
	if r.eof() {
		return r.errf("a backslash at the end of the file")
	}
	c := r.s[r.i]
	r.i++
	switch c {
	case 'b':
		b.WriteByte('\b')
	case 't':
		b.WriteByte('\t')
	case 'n':
		b.WriteByte('\n')
	case 'f':
		b.WriteByte('\f')
	case 'r':
		b.WriteByte('\r')
	case '"':
		b.WriteByte('"')
	case '\\':
		b.WriteByte('\\')
	case 'u', 'U':
		n := 4
		if c == 'U' {
			n = 8
		}
		if r.i+n > len(r.s) {
			return r.errf(`\%c needs %d hex digits`, c, n)
		}
		v, err := strconv.ParseUint(r.s[r.i:r.i+n], 16, 32)
		if err != nil || !utf8.ValidRune(rune(v)) {
			return r.errf(`\%c%s is not a Unicode scalar value`, c, r.s[r.i:r.i+n])
		}
		r.i += n
		b.WriteRune(rune(v))
	default:
		return r.errf(`\%c is not a TOML escape`, c)
	}
	return nil
}
