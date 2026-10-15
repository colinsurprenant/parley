package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestParseWritableRoots: the writable_roots of the sandbox_workspace_write table
// is found as a table's key, as a dotted key, inside an inline table; literal and
// basic strings decode; and nothing else in a Codex config is mistaken for it.
func TestParseWritableRoots(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		roots []string
		found bool
	}{
		{"a table", "[sandbox_workspace_write]\nwritable_roots = [\"/a\", \"/b\"]\n", []string{"/a", "/b"}, true},
		{"a table with other keys around it", "model = \"gpt-6\"\n\n[sandbox_workspace_write]\nnetwork_access = true\nwritable_roots = [\"/a\"]\nexclude_slash_tmp = false\n", []string{"/a"}, true},
		{"a dotted key at the top", "sandbox_workspace_write.writable_roots = [\"/a\", '/b']\nmodel = \"x\"\n", []string{"/a", "/b"}, true},
		{"a dotted key with spaces and a quoted segment", "sandbox_workspace_write . \"writable_roots\" = [\"/a\"]\n", []string{"/a"}, true},
		{"a quoted table header", "[\"sandbox_workspace_write\"]\nwritable_roots = [\"/a\"]\n", []string{"/a"}, true},
		{"an inline table", "sandbox_workspace_write = { network_access = true, writable_roots = [\"/a\", \"/b\"] }\n", []string{"/a", "/b"}, true},
		{"a dotted key inside another table is that table's", "[mcp_servers.x]\nsandbox_workspace_write.writable_roots = [\"/no\"]\n", nil, false},
		{"a profile's table is ignored", "[profiles.dev.sandbox_workspace_write]\nwritable_roots = [\"/no\"]\n", nil, false},
		{"a profile's dotted key is ignored", "[profiles.dev]\nsandbox_workspace_write.writable_roots = [\"/no\"]\n", nil, false},
		{"a profile and the global key", "[profiles.dev.sandbox_workspace_write]\nwritable_roots = [\"/no\"]\n\n[sandbox_workspace_write]\nwritable_roots = [\"/yes\"]\n", []string{"/yes"}, true},
		{"an array of tables is ignored", "[[sandbox_workspace_write]]\nwritable_roots = [\"/no\"]\n", nil, false},
		{"no sandbox table", "model = \"x\"\n[projects.\"/p\"]\ntrust_level = \"trusted\"\n", nil, false},
		{"the table without the key", "[sandbox_workspace_write]\nnetwork_access = true\n", nil, false},
		{"an empty array", "[sandbox_workspace_write]\nwritable_roots = []\n", nil, true},
		{"an empty file", "", nil, false},
		{"a basic string with escapes", `[sandbox_workspace_write]
writable_roots = ["/a\\b", "/q\"uote", "/tab\there", "/caf\u00e9", "/smile\U0001F600"]
`, []string{`/a\b`, `/q"uote`, "/tab\there", "/café", "/smile😀"}, true},
		{"a literal string keeps its backslashes", "[sandbox_workspace_write]\nwritable_roots = ['/a\\b', 'C:\\x\\y', '/q\"uote']\n", []string{`/a\b`, `C:\x\y`, `/q"uote`}, true},
		{"a multi-line array with comments and a trailing comma", `# the user's config
[sandbox_workspace_write] # the table
writable_roots = [   # opens
    "/one",          # first
    # a whole-line comment
    '/two',
    "/three" ,
]
network_access = true
`, []string{"/one", "/two", "/three"}, true},
		{"a comment holding brackets and quotes", "[sandbox_workspace_write]\nwritable_roots = [\"/a\"] # [\"/no\"] ] ' \"\n", []string{"/a"}, true},
		{"multi-line strings elsewhere that look like the table", `developer_instructions = """
[sandbox_workspace_write]
writable_roots = ["/no"]
"""
notes = '''
[sandbox_workspace_write]
writable_roots = ["/no"]
'''

[sandbox_workspace_write]
writable_roots = ["/yes"]
`, []string{"/yes"}, true},
		{"a multi-line string ending in quotes and a line-ending backslash", "a = \"\"\"x\"\"\"\"\nb = \"\"\"\\\n   y\"\"\"\nc = \"\"\"\"\"\"\n[sandbox_workspace_write]\nwritable_roots = [\"/yes\"]\n", []string{"/yes"}, true},
		{"a multi-line string as a root", "[sandbox_workspace_write]\nwritable_roots = [\"\"\"\n/multi\"\"\", '''/lit''']\n", []string{"/multi", "/lit"}, true},
		{"arrays, inline tables, numbers and dates elsewhere", `a = [1, 2.5, -3, 0x1F, true, [ "n" , ["x"] ], { k = "v", j = [1,2] } ]
b = { c = { d = "}" }, e = ']' }
when = 1979-05-27 07:32:00Z
day = 1979-05-27
[sandbox_workspace_write]
writable_roots = ["/yes"]
`, []string{"/yes"}, true},
		{"CRLF line endings", "[sandbox_workspace_write]\r\nwritable_roots = [\r\n  \"/a\",\r\n  \"/b\"\r\n]\r\n", []string{"/a", "/b"}, true},
		{"a byte order mark", "\xef\xbb\xbf[sandbox_workspace_write]\nwritable_roots = [\"/a\"]\n", []string{"/a"}, true},
		{"no trailing newline", "sandbox_workspace_write.writable_roots=['/a']", []string{"/a"}, true},
		{"a real-shaped config", `model = "gpt-6-astra"
model_reasoning_effort = "high"
approval_policy = "on-request"
sandbox_mode = "workspace-write"

[sandbox_workspace_write]
writable_roots = [
  "/home/someone/.director",
  "/home/someone/Library/Caches/go-build",
]

[projects."/home/someone/dev/src/parley"]
trust_level = "trusted"

[mcp_servers.docs]
command = "npx"
args = ["-y", "docs-mcp", "--root", "/x"]
env = { TOKEN = "t" }

[profiles.fast]
model = "gpt-6-mini"
`, []string{"/home/someone/.director", "/home/someone/Library/Caches/go-build"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			roots, found, err := parseWritableRoots(tc.src)
			if err != nil {
				t.Fatalf("parseWritableRoots: %v\n%s", err, tc.src)
			}
			if found != tc.found || !reflect.DeepEqual(roots, tc.roots) && (len(roots) > 0 || len(tc.roots) > 0) {
				t.Errorf("parseWritableRoots = %q, %v; want %q, %v\n%s", roots, found, tc.roots, tc.found, tc.src)
			}
		})
	}
}

// TestParseWritableRootsRefusesWhatItCannotFollow: a malformed document is an
// error, never a panic, and carries nothing.
func TestParseWritableRootsRefusesWhatItCannotFollow(t *testing.T) {
	deep := strings.Repeat("[", 5000) + strings.Repeat("]", 5000)
	tests := []struct{ name, src, want string }{
		{"an array with no end", "[sandbox_workspace_write]\nwritable_roots = [\"/a\",\n", "no ]"},
		{"a string that runs into the end of the line", "[sandbox_workspace_write]\nwritable_roots = [\"/a]\n", "newline"},
		{"a string with no end", "a = \"abc", "no closing quote"},
		{"a multi-line string with no end", "a = \"\"\"never ends\n", "no closing"},
		{"a missing comma", "[sandbox_workspace_write]\nwritable_roots = [\"/a\" \"/b\"]\n", "expected , or ]"},
		{"a number among the roots", "[sandbox_workspace_write]\nwritable_roots = [\"/a\", 3]\n", "does not start a string"},
		{"a string, not an array", "[sandbox_workspace_write]\nwritable_roots = \"/a\"\n", "array of strings"},
		{"a number, not an array", "[sandbox_workspace_write]\nwritable_roots = 7\n", "array of strings"},
		{"a nested array", "[sandbox_workspace_write]\nwritable_roots = [[\"/a\"]]\n", "does not start a string"},
		{"defined twice", "[sandbox_workspace_write]\nwritable_roots = [\"/a\"]\nwritable_roots = [\"/b\"]\n", "defined twice"},
		{"defined twice, dotted and table", "sandbox_workspace_write.writable_roots = [\"/a\"]\n[sandbox_workspace_write]\nwritable_roots = [\"/b\"]\n", "defined twice"},
		{"a bad escape", "[sandbox_workspace_write]\nwritable_roots = [\"/a\\qb\"]\n", "not a TOML escape"},
		{"an invalid byte in a value walked past", "model = \"gpt\xff\"\n[sandbox_workspace_write]\nwritable_roots = [\"/a\"]\n", "not valid UTF-8"},
		{"an invalid byte in a comment", "# note \xc3\n[sandbox_workspace_write]\nwritable_roots = [\"/a\"]\n", "not valid UTF-8"},
		{"a unicode escape cut short", "a = \"\\u00", "hex digits"},
		{"a unicode escape that is not hex", "a = \"\\u00zz\"\n", "not a Unicode scalar"},
		{"a surrogate escape", "[sandbox_workspace_write]\nwritable_roots = [\"/a\\uD800\"]\n", "not a Unicode scalar"},
		{"a newline in a string", "[sandbox_workspace_write]\nwritable_roots = [\"/a\n\"]\n", "newline"},
		{"a key with no value", "a =\n", "expected a value"},
		{"a key with no value at the end", "a =", "no value"},
		{"a key with no equals", "a\n", "expected = after"},
		{"garbage", "}{ not toml at all\n", "expected a key"},
		{"binary garbage", "\x00\x01\xff\xfe=\x80\n", "not valid UTF-8"},
		{"a header with no end", "[sandbox_workspace_write\n", "no ]"},
		{"an array header with half a closer", "[[sandbox_workspace_write]\n", "no ]]"},
		{"text after a value", "a = 1 2\n", "after a value"},
		{"an unterminated inline table", "a = { b = 1\n", "no }"},
		{"nesting past the bound", "a = " + deep + "\n", "nested deeper"},
		{"a lone backslash at the end", "a = \"\\", "backslash"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			roots, found, err := parseWritableRoots(tc.src)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseWritableRoots = %q, %v, %v; want an error naming %q", roots, found, err, tc.want)
			}
			if len(roots) != 0 || found {
				t.Errorf("an error came with roots %q, found %v", roots, found)
			}
		})
	}
}

// TestParseWritableRootsNeverPanics: no prefix of a rich document, and no
// mutation of one, panics the reader; it returns roots or an error.
func TestParseWritableRootsNeverPanics(t *testing.T) {
	doc := "# c\nmodel = \"m\"\nx = \"\"\"a\\\n b\"\"\" \ny = '''l'''\nz = [1, {a = [\"b\"]}]\nd = 1979-05-27 07:32:00Z\n" +
		"[sandbox_workspace_write]\nwritable_roots = [\"/a\\u00e9\", '/b', ]\n[[t]]\nk = 1\n[p.\"q\"]\nr.s = { t = [] }\n"
	for i := 0; i <= len(doc); i++ {
		parseWritableRoots(doc[:i])
		parseWritableRoots(doc[i:])
	}
	for i := 0; i < len(doc); i++ {
		for _, c := range []byte{0, '"', '\'', '[', ']', '{', '}', '\\', '\n', '=', '#', ',', 0xff} {
			parseWritableRoots(doc[:i] + string(c) + doc[i+1:])
		}
	}
}

func FuzzParseWritableRoots(f *testing.F) {
	for _, seed := range []string{
		"", "[sandbox_workspace_write]\nwritable_roots = [\"/a\"]\n", "sandbox_workspace_write.writable_roots=['/a',]",
		"a = \"\"\"x\\\n y\"\"\"\n", "a = { b = [1, {c = 2}] }\n", "[[a]]\n[b.\"c\"]\nd=1979-05-27 07:32:00Z\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		roots, found, err := parseWritableRoots(src)
		if err != nil && (len(roots) != 0 || found) {
			t.Errorf("an error with roots %q, found %v", roots, found)
		}
	})
}

// globalBench is a directory holding the roots a global config names.
type globalBench struct {
	t    *testing.T
	root string
}

func newGlobalBench(t *testing.T) globalBench {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return globalBench{t, root}
}

// dir makes a directory under the bench and returns its physical path.
func (g globalBench) dir(name string) string {
	g.t.Helper()
	d := filepath.Join(g.root, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		g.t.Fatal(err)
	}
	return d
}

// config writes a config.toml under the bench and returns its path.
func (g globalBench) config(content string) string {
	g.t.Helper()
	path := filepath.Join(g.root, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}
	return path
}

func TestCarryGlobalRootsFromATable(t *testing.T) {
	g := newGlobalBench(t)
	a, b := g.dir("a"), g.dir("b")
	got := carryGlobalRoots(g.config("[sandbox_workspace_write]\nwritable_roots = [\""+a+"\", \""+b+"\"]\n"), nil)
	if got.why != "" || !reflect.DeepEqual(got.carried, []string{a, b}) || len(got.skipped) != 0 {
		t.Errorf("carried %q, skipped %v, why %q; want %q", got.carried, got.skipped, got.why, []string{a, b})
	}
}

func TestCarryGlobalRootsFromADottedKey(t *testing.T) {
	g := newGlobalBench(t)
	a, b := g.dir("a"), g.dir("b")
	got := carryGlobalRoots(g.config("model = \"x\"\nsandbox_workspace_write.writable_roots = ['"+a+"',\n  \""+b+"\"]\n"), nil)
	if got.why != "" || !reflect.DeepEqual(got.carried, []string{a, b}) {
		t.Errorf("carried %q, why %q; want %q", got.carried, got.why, []string{a, b})
	}
}

// TestCarryGlobalRootsDecodesStrings: a basic string's escapes and a literal
// string's backslashes reach the path they name.
func TestCarryGlobalRootsDecodesStrings(t *testing.T) {
	g := newGlobalBench(t)
	odd := g.dir(`we"ird\dir é`)
	plain := g.dir("plain")
	basic := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(odd)
	// The literal form cannot hold a ' but can hold the other two.
	bs := g.dir(`back\slash"q`)
	got := carryGlobalRoots(g.config("[sandbox_workspace_write]\nwritable_roots = [\n  \""+basic+"\",\n  '"+bs+"',\n  \""+strings.TrimSuffix(plain, "plain")+`pl\u0061in`+"\",\n]\n"), nil)
	if got.why != "" || len(got.skipped) != 0 || !reflect.DeepEqual(got.carried, []string{odd, bs, plain}) {
		t.Errorf("carried %q, skipped %v, why %q; want %q", got.carried, got.skipped, got.why, []string{odd, bs, plain})
	}
}

func TestCarryGlobalRootsResolvesLinksAndDropsDuplicates(t *testing.T) {
	g := newGlobalBench(t)
	real := g.dir("real")
	named := g.dir("named")
	link := filepath.Join(g.root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got := carryGlobalRoots(g.config("[sandbox_workspace_write]\nwritable_roots = [\""+real+"\", \""+link+"\", \""+real+"/\", \""+named+"\"]\n"), []string{named})
	if got.why != "" || len(got.skipped) != 0 || !reflect.DeepEqual(got.carried, []string{real}) {
		t.Errorf("carried %q, skipped %v, why %q; want %q once: the link and the repeats resolve to it, and %s is named already", got.carried, got.skipped, got.why, real, named)
	}
}

// TestCarryGlobalRootsSkipsWhatFailsValidation: a root that is missing, relative,
// a file, or holds a control character is skipped with its reason, and the rest
// is carried.
func TestCarryGlobalRootsSkipsWhatFailsValidation(t *testing.T) {
	g := newGlobalBench(t)
	ok := g.dir("ok")
	file := filepath.Join(g.root, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(g.root, "missing")
	got := carryGlobalRoots(g.config("[sandbox_workspace_write]\nwritable_roots = [\""+missing+"\", \"rel/dir\", \""+file+"\", \""+ok+"\", \"/x\\u0001y\", \"\"]\n"), nil)
	if got.why != "" || !reflect.DeepEqual(got.carried, []string{ok}) {
		t.Fatalf("carried %q, why %q; want only %q", got.carried, got.why, ok)
	}
	want := []struct{ root, why string }{
		{missing, "no such file or directory"},
		{"rel/dir", "must be an absolute path"},
		{file, "is not a directory"},
		{"/x\x01y", "holds a control character"},
		{"", "must be an absolute path"},
	}
	if len(got.skipped) != len(want) {
		t.Fatalf("skipped %v, want %d", got.skipped, len(want))
	}
	for i, w := range want {
		if s := got.skipped[i]; s.root != w.root || !strings.Contains(s.why, w.why) {
			t.Errorf("skipped[%d] = %+v, want the root %q with a reason holding %q", i, s, w.root, w.why)
		}
	}
}

// TestCarryGlobalRootsCarriesNothingItCannotRead: an absent, unreadable,
// malformed or rootless config carries nothing and says why; none panics.
func TestCarryGlobalRootsCarriesNothingItCannotRead(t *testing.T) {
	g := newGlobalBench(t)
	tests := []struct {
		name string
		path func() string
		why  string
	}{
		{"absent", func() string { return filepath.Join(g.root, "nope.toml") }, "does not exist"},
		{"a directory", func() string { return g.dir("adir") }, "not a regular file"},
		{"malformed", func() string { return g.config("[sandbox_workspace_write]\nwritable_roots = [\"/a\"\n") }, "could not parse"},
		{"binary", func() string { return g.config("\x00\xff\xfe[[[") }, "could not parse"},
		{"without the key", func() string { return g.config("model = \"x\"\n[sandbox_workspace_write]\nnetwork_access = true\n") }, "has no writable_roots key"},
		{"with an empty list", func() string { return g.config("[sandbox_workspace_write]\nwritable_roots = []\n") }, "lists no writable_roots"},
		{"with only profile roots", func() string {
			return g.config("[profiles.p.sandbox_workspace_write]\nwritable_roots = [\"" + g.dir("p") + "\"]\n")
		}, "has no writable_roots key"},
		{"past the size cap", func() string { return g.config("# " + strings.Repeat("x", codexConfigMax) + "\n") }, "larger than"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := carryGlobalRoots(tc.path(), nil)
			if len(got.carried) != 0 || len(got.skipped) != 0 || !strings.Contains(got.why, tc.why) {
				t.Errorf("carried %q, skipped %v, why %q; want nothing carried and why naming %q", got.carried, got.skipped, got.why, tc.why)
			}
		})
	}
}

func TestCodexConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	if got, err := codexConfigPath(); err != nil || got != filepath.Join(home, ".codex", "config.toml") {
		t.Errorf("with no $CODEX_HOME: %q, %v; want ~/.codex/config.toml", got, err)
	}
	other := t.TempDir()
	t.Setenv("CODEX_HOME", other)
	if got, err := codexConfigPath(); err != nil || got != filepath.Join(other, "config.toml") {
		t.Errorf("with $CODEX_HOME: %q, %v; want $CODEX_HOME/config.toml", got, err)
	}
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", "")
	if got, err := codexConfigPath(); err == nil {
		t.Errorf("with neither: %q, nil; want an error", got)
	}
}

// buildWithGlobal runs a build new with env added to the bench's, and reads its
// record.
func buildWithGlobal(t *testing.T, b *tableBench, id string, env []string, extra ...string) (buildRecord, result) {
	t.Helper()
	res := runParleyEnvIn(t, "", append(b.env(), env...), b.buildArgs(id, nil, extra...)...)
	if res.code != 0 {
		t.Fatalf("parley build new exited %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	return readRecordForTest(t, b, id), res
}

// codexConfigWith is the config build new writes for a Codex peer that names
// roots.
func codexConfigWith(roots ...string) string {
	return buildCodexConfig(roots)
}

func peerConfig(t *testing.T, rec buildRecord, nick string) string {
	t.Helper()
	return string(mustRead(t, filepath.Join(rec.peer(t, nick).Worktree, ".codex", "config.toml")))
}

// writeGlobalConfig writes the user's Codex config under the bench's home.
func writeGlobalConfig(t *testing.T, b *tableBench, content string) string {
	t.Helper()
	dir := filepath.Join(b.db.home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	writeFileT(t, path, content)
	return path
}

func benchDir(t *testing.T, b *tableBench, name string) string {
	t.Helper()
	d := filepath.Join(b.root, name)
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	phys, err := filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	return phys
}

// TestBuildNewCarriesTheGlobalRootsAlongWithTheNamedOnes: with --writable-root,
// the Codex config holds the named root and the roots of the user's
// ~/.codex/config.toml, and the setup log and the runbook say which.
func TestBuildNewCarriesTheGlobalRootsAlongWithTheNamedOnes(t *testing.T) {
	b := newTableBench(t)
	named, cache, director, gone := benchDir(t, b, "named"), benchDir(t, b, "gocache"), benchDir(t, b, "director"), filepath.Join(b.root, "gone")
	cfgPath := writeGlobalConfig(t, b, "model = \"x\"\n[sandbox_workspace_write]\nwritable_roots = [\n  \""+cache+"\", # the Go build cache\n  \""+gone+"\",\n  '"+director+"',\n  \""+named+"\",\n]\n")

	rec, res := buildWithGlobal(t, b, "b1", nil, "--writable-root", named)
	want := codexConfigWith(named, cache, director)
	for _, p := range rec.Peers {
		if p.Harness != harnessCodex {
			continue
		}
		if got := peerConfig(t, rec, p.Nick); got != want {
			t.Errorf("%s's config\n%s\nwant\n%s", p.Nick, got, want)
		}
	}
	for _, line := range []string{
		"parley build new: carrying your global Codex writable roots from " + cfgPath + " into each Codex peer's config: " + cache + ", " + director + "\n",
		"parley build new: not carrying global writable root \"" + gone + "\": ",
	} {
		if !strings.Contains(res.stderr, line) {
			t.Errorf("the setup log lacks %q:\n%s", line, res.stderr)
		}
	}
	for _, line := range []string{
		"Its sandbox writes its worktree, $TMPDIR, /tmp and " + named + ", as --writable-root asked.",
		"It also writes your global Codex roots, carried over from " + cfgPath + ": " + cache + ", " + director + ".",
		"Not carried over, global writable root \"" + gone + "\": ",
		"Re-pass any of those it needs with --writable-root.",
	} {
		if !strings.Contains(res.stdout, line) {
			t.Errorf("the runbook lacks %q:\n%s", line, res.stdout)
		}
	}
	if strings.Contains(res.stdout, "Naming a root replaced") {
		t.Errorf("the runbook still says the global roots were replaced, though they were carried:\n%s", res.stdout)
	}
}

// TestBuildNewReadsTheGlobalRootsFromCodexHome: $CODEX_HOME names the config, and
// the one in ~/.codex is then not read.
func TestBuildNewReadsTheGlobalRootsFromCodexHome(t *testing.T) {
	b := newTableBench(t)
	named, viaHome, viaCodexHome := benchDir(t, b, "named"), benchDir(t, b, "via-home"), benchDir(t, b, "via-codex-home")
	writeGlobalConfig(t, b, "sandbox_workspace_write.writable_roots = [\""+viaHome+"\"]\n")
	codexHome := benchDir(t, b, "codex-home")
	writeFileT(t, filepath.Join(codexHome, "config.toml"), "sandbox_workspace_write.writable_roots = [\""+viaCodexHome+"\"]\n")

	rec, res := buildWithGlobal(t, b, "b1", []string{"CODEX_HOME=" + codexHome}, "--writable-root", named)
	for _, p := range rec.Peers {
		if p.Harness != harnessCodex {
			continue
		}
		if got, want := peerConfig(t, rec, p.Nick), codexConfigWith(named, viaCodexHome); got != want {
			t.Errorf("%s's config\n%s\nwant\n%s", p.Nick, got, want)
		}
	}
	if !strings.Contains(res.stderr, filepath.Join(codexHome, "config.toml")) || strings.Contains(res.stderr+res.stdout, viaHome) {
		t.Errorf("the log and the runbook should name $CODEX_HOME's config and not ~/.codex's roots:\n%s\n%s", res.stderr, res.stdout)
	}
}

// TestBuildNewKeepsTheReplacedRootsWarningWhenItCannotCarry: an absent, malformed
// or rootless config leaves the Codex config with the named roots only, and the
// warning that naming a root replaced the global ones, with why they were not
// carried.
func TestBuildNewKeepsTheReplacedRootsWarningWhenItCannotCarry(t *testing.T) {
	tests := []struct {
		name   string
		config string // empty: no file
		why    string
	}{
		{"no config", "", "config.toml does not exist"},
		{"a malformed config", "[sandbox_workspace_write]\nwritable_roots = [\"/a\"\n", "could not parse"},
		{"a config with no roots", "model = \"x\"\n", "has no writable_roots key under sandbox_workspace_write"},
		{"a config with an empty list", "[sandbox_workspace_write]\nwritable_roots = []\n", "lists no writable_roots under sandbox_workspace_write"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newTableBench(t)
			named := benchDir(t, b, "named")
			if tc.config != "" {
				writeGlobalConfig(t, b, tc.config)
			}
			rec, res := buildWithGlobal(t, b, "b1", nil, "--writable-root", named)
			for _, p := range rec.Peers {
				if p.Harness != harnessCodex {
					continue
				}
				if got, want := peerConfig(t, rec, p.Nick), codexConfigWith(named); got != want {
					t.Errorf("%s's config\n%s\nwant\n%s", p.Nick, got, want)
				}
			}
			if !strings.Contains(res.stderr, "parley build new: warning: naming a root replaced your global Codex writable roots, and build new did not carry them over: ") ||
				!strings.Contains(res.stderr, tc.why) {
				t.Errorf("the setup log lacks the warning naming %q:\n%s", tc.why, res.stderr)
			}
			for _, want := range []string{"Naming a root replaced your global Codex roots: re-pass any it needs.", "build new did not carry them over: ", tc.why} {
				if !strings.Contains(res.stdout, want) {
					t.Errorf("the runbook lacks %q:\n%s", want, res.stdout)
				}
			}
		})
	}
}

// TestBuildNewWritesNoRootsAndReadsNoConfigWithoutWritableRoot: the user's global
// roots apply by themselves, so build new neither reads their config nor writes
// a writable_roots, and says nothing about them.
func TestBuildNewWritesNoRootsAndReadsNoConfigWithoutWritableRoot(t *testing.T) {
	b := newTableBench(t)
	writeGlobalConfig(t, b, "this is not toml [[[\n")
	rec, res := buildWithGlobal(t, b, "b1", nil)
	for _, p := range rec.Peers {
		if p.Harness != harnessCodex {
			continue
		}
		if got := peerConfig(t, rec, p.Nick); got != codexConfigTOML || strings.Contains(got, "writable_roots") {
			t.Errorf("%s's config without --writable-root:\n%s", p.Nick, got)
		}
	}
	if strings.Contains(res.stderr, "global") || strings.Contains(res.stderr, "could not parse") {
		t.Errorf("build new talks of the global roots though no --writable-root was named:\nstderr: %s", res.stderr)
	}
	if !strings.Contains(res.stdout, "plus any global roots in your ~/.codex/config.toml") {
		t.Errorf("the runbook lost its sentence on the global roots that apply:\n%s", res.stdout)
	}
}

// TestBuildNewWarnsOfACarriedRootThatHoldsTheGitDirectory: a global root that
// holds the target's git directory is written into the peers' config by build
// new, so it draws the warning a named one does.
func TestBuildNewWarnsOfACarriedRootThatHoldsTheGitDirectory(t *testing.T) {
	b := newTableBench(t)
	named := benchDir(t, b, "named")
	writeGlobalConfig(t, b, "sandbox_workspace_write.writable_roots = [\""+filepath.Join(b.repo, ".git")+"\"]\n")
	_, res := buildWithGlobal(t, b, "b1", nil, "--writable-root", named)
	if !strings.Contains(res.stderr, "warning: the global writable root ") || !strings.Contains(res.stderr, "hooks and config") {
		t.Errorf("no warning for a carried root that is the target's git directory:\n%s", res.stderr)
	}
}

// TestBuildNewWithAllGlobalRootsNamedAlready: roots that are all among the named
// ones are carried once, and the runbook does not say they were replaced.
func TestBuildNewWithAllGlobalRootsNamedAlready(t *testing.T) {
	b := newTableBench(t)
	named := benchDir(t, b, "named")
	writeGlobalConfig(t, b, "sandbox_workspace_write.writable_roots = [\""+named+"\"]\n")
	rec, res := buildWithGlobal(t, b, "b1", nil, "--writable-root", named)
	for _, p := range rec.Peers {
		if p.Harness == harnessCodex {
			if got, want := peerConfig(t, rec, p.Nick), codexConfigWith(named); got != want {
				t.Errorf("%s's config\n%s\nwant\n%s", p.Nick, got, want)
			}
		}
	}
	if !strings.Contains(res.stdout, "are all among the ones named.") || strings.Contains(res.stdout, "Naming a root replaced") {
		t.Errorf("the runbook:\n%s", res.stdout)
	}
}

// controlLink makes a symbolic link under the bench whose target holds a newline
// and does not exist, so that evaluating the link fails with an error naming the
// target: the user's environment shaped that text, and a terminal must not take
// it raw.
func (g globalBench) controlLink() string {
	g.t.Helper()
	link := filepath.Join(g.root, "link")
	if err := os.Symlink(filepath.Join(g.root, "gone\nsecond line"), link); err != nil {
		g.t.Fatal(err)
	}
	return link
}

// TestGlobalRootsLogAndRunbookPrintNoRawControlCharacter: the reason a global
// root was not carried quotes EvalSymlinks' error, which names a link's target,
// so the setup log and the runbook print it through printable.
func TestGlobalRootsLogAndRunbookPrintNoRawControlCharacter(t *testing.T) {
	g := newGlobalBench(t)
	got := carryGlobalRoots(g.config("[sandbox_workspace_write]\nwritable_roots = [\""+g.controlLink()+"\"]\n"), nil)
	if len(got.skipped) != 1 || !hasControl(got.skipped[0].why) {
		t.Fatalf("skipped %+v; want one root whose reason holds a control character, or the test proves nothing", got.skipped)
	}
	p := &buildPlan{global: got}
	for name, lines := range map[string][]string{"setup log": p.globalRootsLog(), "runbook": p.globalRootsRunbook()} {
		for _, line := range lines {
			if hasControl(line) {
				t.Errorf("the %s prints a control character raw: %q", name, line)
			}
		}
		if !strings.Contains(strings.Join(lines, "\n"), `second line`) {
			t.Errorf("the %s lost the reason: %q", name, lines)
		}
	}
}

// TestWritableRootErrorPrintsNoRawControlCharacter: a --writable-root that is a
// link to a missing target whose name holds a newline is refused, and the
// refusal on the terminal quotes the text.
func TestWritableRootErrorPrintsNoRawControlCharacter(t *testing.T) {
	g := newGlobalBench(t)
	_, err := physicalRoots([]string{g.controlLink()})
	if err == nil {
		t.Fatal("a link to a missing target was accepted as a writable root")
	}
	if hasControl(err.Error()) {
		t.Errorf("the refusal prints a control character raw: %q", err.Error())
	}
}

// TestWarnWritableRootsQuotesTheGlobalConfigPath: the warning for a carried root
// that holds the target's git directory names the config the root came from,
// and $CODEX_HOME shaped that path.
func TestWarnWritableRootsQuotesTheGlobalConfigPath(t *testing.T) {
	g := newGlobalBench(t)
	gitDir := g.dir(".git")
	p := &buildPlan{commonDir: gitDir, global: &globalRoots{path: g.root + "/home\nx/config.toml", carried: []string{gitDir}}}
	var out bytes.Buffer
	p.warnWritableRoots(&out)
	if out.Len() == 0 {
		t.Fatal("no warning for a carried root that is the target's git directory")
	}
	if n := strings.Count(out.String(), "\n"); n != 1 {
		t.Errorf("the warning is %d lines, want 1 (a raw newline in the path):\n%q", n, out.String())
	}
}

// TestCarryGlobalRootsTellsAnEmptyListFromAMissingKey: a config whose
// writable_roots is an explicit empty list says it lists none, and one without
// the key says it has no key.
func TestCarryGlobalRootsTellsAnEmptyListFromAMissingKey(t *testing.T) {
	g := newGlobalBench(t)
	empty := carryGlobalRoots(g.config("[sandbox_workspace_write]\nwritable_roots = []\n"), nil)
	if !strings.Contains(empty.why, "lists no writable_roots") || strings.Contains(empty.why, "no writable_roots key") {
		t.Errorf("an explicit empty list: %q; want it to say it lists none, and not that it has no key", empty.why)
	}
	missing := carryGlobalRoots(g.config("[sandbox_workspace_write]\nnetwork_access = true\n"), nil)
	if !strings.Contains(missing.why, "has no writable_roots key") || strings.Contains(missing.why, "lists no") {
		t.Errorf("no key: %q; want it to say it has no writable_roots key", missing.why)
	}
}
