package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/doctor"
	"github.com/colinsurprenant/parley/internal/ergoconf"
)

// The tests here drive the install.sh that the release workflow renders from
// install.sh.in. They render it themselves, against an httptest server that
// serves archives built here (tar on a macOS machine is not the tar of the
// release runner, so no test shells out to build one), in a HOME and a PATH of
// the test's own. The parley in the archives is a stub: a real one would probe
// 127.0.0.1:6667 when doctor ran, where a developer's server may be.

const (
	relErgoVersion = "2.19.1"
	relMarker      = ".parley-release"
)

// relPlatforms are the four archives a release carries, named as parley's are
// and as ergo's are.
var relPlatforms = []struct{ os, arch, ergoOS, ergoArch string }{
	{"darwin", "amd64", "macos", "x86_64"},
	{"darwin", "arm64", "macos", "arm64"},
	{"linux", "amd64", "linux", "x86_64"},
	{"linux", "arm64", "linux", "arm64"},
}

type tarEntry struct {
	name string
	mode int64
	body []byte // nil for a directory
}

// tgz is a gzipped tar of the entries, owned by root as the release's are.
func tgz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, ModTime: time.Unix(0, 0), Format: tar.FormatGNU}
		if e.body == nil {
			h.Typeflag = tar.TypeDir
			h.Name = strings.TrimSuffix(e.name, "/") + "/"
		} else {
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.body != nil {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// stubMain is the parley of the archives: a program that logs each run to
// $STUB_LOG, so a test can tell what ran and what did not, answers version and
// doctor, and connects to nothing. It is built as the real parley is, so the
// installer meets what it meets on a machine: a Go binary of a module named for
// parley with its main package at cmd/parley, at the module version of its tag.
const stubMain = `package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

func main() {
	version := "(unknown)"
	if bi, ok := debug.ReadBuildInfo(); ok {
		version = bi.Main.Version
	}
	if path := os.Getenv("STUB_LOG"); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			first, _, _ := strings.Cut(os.Getenv("PATH"), ":")
			fmt.Fprintf(f, "parley-%s %s PATH=%s\n", version, strings.Join(os.Args[1:], " "), first)
			f.Close()
		}
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Printf("parley %s stub\n", version)
		case "doctor":
			fmt.Println("stub doctor", strings.Join(os.Args[1:], " "))
			code, _ := strconv.Atoi(os.Getenv("STUB_DOCTOR_EXIT"))
			os.Exit(code)
		}
	}
}
`

var stubs struct {
	sync.Mutex
	built    map[string][]byte // version -> the program
	archived map[string][]byte // top directory -> its archive
}

// stubBinary is the stub parley at module version v, built once per test binary:
// in a throwaway git repository tagged v, as the release workflow builds from a
// checkout of a tag, so that the toolchain stamps v as the version.
func stubBinary(t *testing.T, v string) []byte {
	t.Helper()
	stubs.Lock()
	defer stubs.Unlock()
	if b, ok := stubs.built[v]; ok {
		return b
	}
	gomod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	goVersion := regexp.MustCompile(`(?m)^go (\S+)$`).FindSubmatch(gomod)
	if goVersion == nil {
		t.Fatal("go.mod names no go version")
	}
	dir := t.TempDir() // gone with the test that built it; the program is kept
	src := filepath.Join(dir, "cmd", "parley")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(dir, "go.mod"):  "module github.com/colinsurprenant/parley\n\ngo " + string(goVersion[1]) + "\n",
		filepath.Join(src, "main.go"): stubMain,
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GOFLAGS=", "CGO_ENABLED=0",
		"GIT_AUTHOR_NAME=stub", "GIT_AUTHOR_EMAIL=stub@example.com", "GIT_COMMITTER_NAME=stub", "GIT_COMMITTER_EMAIL=stub@example.com")
	run := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir, cmd.Env = dir, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "add", "-A")
	run("git", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "stub "+v)
	run("git", "tag", v)
	bin := filepath.Join(dir, "parley")
	run("go", "build", "-trimpath", "-buildvcs=true", "-o", bin, "./cmd/parley")
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if got := moduleVersion(string(b)); got != v {
		t.Fatalf("the stub was built at module version %q, want %s", got, v)
	}
	if stubs.built == nil {
		stubs.built = map[string][]byte{}
	}
	stubs.built[v] = b
	return b
}

// moduleVersion is the version of parley's module in the build information of
// the Go binary b, or "".
func moduleVersion(b string) string {
	m := regexp.MustCompile(`mod\tgithub\.com/colinsurprenant/parley\t(\S+)`).FindStringSubmatch(b)
	if m == nil {
		return ""
	}
	return m[1]
}

// parleyArchive is the archive of version v for platform p: the stub, the skills
// of this checkout and the marker, under one top directory. It is the same for
// every test that asks, so it is made once.
func parleyArchive(t *testing.T, v string, p struct{ os, arch, ergoOS, ergoArch string }) []byte {
	t.Helper()
	top := "parley-" + v + "-" + p.os + "-" + p.arch
	binary := stubBinary(t, v)
	stubs.Lock()
	defer stubs.Unlock()
	if b, ok := stubs.archived[top]; ok {
		return b
	}
	skills := checkoutSkills(t)
	entries := []tarEntry{
		{name: top, mode: 0o755},
		{name: top + "/parley", mode: 0o755, body: binary},
		{name: top + "/LICENSE", mode: 0o644, body: []byte("license\n")},
		{name: top + "/skills", mode: 0o755},
		{name: top + "/skills/" + relMarker, mode: 0o644, body: []byte("parley " + v + "\n")},
	}
	err := filepath.WalkDir(skills, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(skills, path)
		if err != nil || rel == "." {
			return err
		}
		name := top + "/skills/" + filepath.ToSlash(rel)
		if d.IsDir() {
			entries = append(entries, tarEntry{name: name, mode: 0o755})
			return nil
		}
		body, err := os.ReadFile(path)
		entries = append(entries, tarEntry{name: name, mode: 0o644, body: body})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if stubs.archived == nil {
		stubs.archived = map[string][]byte{}
	}
	stubs.archived[top] = tgz(t, entries)
	return stubs.archived[top]
}

// release is one version's archives, served over HTTP, and the values the
// workflow would render into install.sh.in for it.
type release struct {
	t       *testing.T
	version string
	srv     *httptest.Server

	mu    sync.Mutex
	files map[string][]byte // URL path -> body
	asked []string
	sums  map[string]string // placeholder name -> rendered value
}

func newRelease(t *testing.T, version string) *release {
	t.Helper()
	r := &release{t: t, version: version, files: map[string][]byte{}, sums: map[string]string{}}
	for _, p := range relPlatforms {
		top := "parley-" + version + "-" + p.os + "-" + p.arch
		r.add("/parley/"+top+".tar.gz", "PARLEY_SHA256_"+strings.ToUpper(p.os+"_"+p.arch), parleyArchive(t, version, p))

		etop := "ergo-" + relErgoVersion + "-" + p.ergoOS + "-" + p.ergoArch
		r.add("/ergo/"+etop+".tar.gz", "ERGO_SHA256_"+strings.ToUpper(p.ergoOS+"_"+p.ergoArch), tgz(t, []tarEntry{
			{name: etop, mode: 0o755},
			{name: etop + "/ergo", mode: 0o755, body: []byte("#!/bin/sh\necho ergo-" + relErgoVersion + " " + p.ergoOS + " " + p.ergoArch + "\n")},
		}))
	}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.asked = append(r.asked, strings.TrimPrefix(req.URL.Path, "/"))
		body, ok := r.files[req.URL.Path]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(r.srv.Close)
	r.sums["PARLEY_VERSION"] = version
	r.sums["PARLEY_BASE_URL"] = r.srv.URL + "/parley"
	r.sums["ERGO_VERSION"] = relErgoVersion
	r.sums["ERGO_BASE_URL"] = r.srv.URL + "/ergo"
	return r
}

func (r *release) add(urlPath, placeholder string, body []byte) {
	sum := sha256.Sum256(body)
	r.files[urlPath] = body
	r.sums[placeholder] = hex.EncodeToString(sum[:])
}

// requested is the URL paths asked of the server so far, each without its
// leading slash, in order.
func (r *release) requested() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

func (r *release) archives(prefix string) []string {
	var got []string
	for _, a := range r.requested() {
		if strings.HasPrefix(a, prefix) {
			got = append(got, a)
		}
	}
	return got
}

var placeholderRE = regexp.MustCompile(`@[A-Z0-9_]+@`)

// script renders install.sh.in the way the workflow does, except for the names
// in omit, which stay as the template spells them, and returns the text.
func (r *release) script(omit ...string) string {
	r.t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "install.sh.in"))
	if err != nil {
		r.t.Fatal(err)
	}
	return placeholderRE.ReplaceAllStringFunc(string(raw), func(tok string) string {
		name := strings.Trim(tok, "@")
		for _, o := range omit {
			if o == name {
				return tok
			}
		}
		v, ok := r.sums[name]
		if !ok {
			r.t.Fatalf("install.sh.in has a placeholder the test does not render: %s", tok)
		}
		return v
	})
}

// machine is a scratch HOME and PATH. The PATH holds only the tools the installer
// is allowed to need, so no ergo, parley, Go or harness is found unless a test
// puts one there.
type machine struct {
	t     *testing.T
	home  string
	tmp   string
	log   string
	farm  string
	front []string // directories ahead of the tools on PATH
	back  []string // directories behind them
	shell string
	env   []string
}

var installTools = []string{"sh", "uname", "curl", "tar", "gzip", "mktemp", "grep", "cmp", "sha256sum", "shasum",
	"cut", "head", "dirname", "mkdir", "cp", "mv", "rm", "chmod", "readlink", "ln", "sysctl", "od", "tr", "sed"}

func newMachine(t *testing.T) *machine {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &machine{t: t, home: home, tmp: t.TempDir(), log: filepath.Join(t.TempDir(), "stub.log"), shell: "sh"}
	m.farm = m.toolFarm(nil)
	return m
}

// toolFarm is a directory of links to the system's installer tools, less the
// ones named in without.
func (m *machine) toolFarm(without map[string]bool) string {
	m.t.Helper()
	dir := m.t.TempDir()
	for _, name := range installTools {
		if without[name] {
			continue
		}
		real, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if err := os.Symlink(real, filepath.Join(dir, name)); err != nil {
			m.t.Fatal(err)
		}
	}
	return dir
}

func (m *machine) path() string {
	dirs := append(append(append([]string(nil), m.front...), m.farm), m.back...)
	return strings.Join(dirs, string(os.PathListSeparator))
}

type installResult struct {
	stdout, stderr string
	code           int
}

// run runs the script on the machine: a file given as an argument to the shell,
// or, when stdin is not empty, the text piped to it as `curl | sh` does.
func (m *machine) run(script, stdin string, args ...string) installResult {
	m.t.Helper()
	shell, err := exec.LookPath(m.shell)
	if err != nil {
		m.t.Skipf("no %s: %v", m.shell, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	if stdin != "" {
		cmd = exec.CommandContext(ctx, shell, append([]string{"-s", "--"}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
	} else {
		path := filepath.Join(m.t.TempDir(), "install.sh")
		if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
			m.t.Fatal(err)
		}
		cmd = exec.CommandContext(ctx, shell, append([]string{path}, args...)...)
	}
	cmd.Env = append([]string{"HOME=" + m.home, "PATH=" + m.path(), "TMPDIR=" + m.tmp, "STUB_LOG=" + m.log}, m.env...)
	var out, errs strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errs
	res := installResult{}
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			m.t.Fatalf("run the installer: %v", err)
		}
		res.code = exit.ExitCode()
	}
	res.stdout, res.stderr = out.String(), errs.String()
	return res
}

func (m *machine) logged() string {
	b, _ := os.ReadFile(m.log)
	return string(b)
}

func (m *machine) bin(name string) string { return filepath.Join(m.home, ".local", "bin", name) }
func (m *machine) skillsDir() string {
	return filepath.Join(m.home, ".local", "share", "parley", "skills")
}
func (m *machine) read(path string) string { b, _ := os.ReadFile(path); return string(b) }
func (m *machine) write(path, body string, mode os.FileMode) {
	m.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		m.t.Fatal(err)
	}
}

// shim writes an executable script named name in a new directory and returns the
// directory.
func shim(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// unameShim makes the machine report os and arch, whatever it is.
func unameShim(t *testing.T, os_, arch string) string {
	return shim(t, "uname", "case \"$1\" in -s) echo "+os_+" ;; -m) echo "+arch+" ;; esac\n")
}

// snapshot lists everything under root, links by their target, files by size and
// mode: two snapshots are equal when nothing was added, removed or changed.
func snapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			to, _ := os.Readlink(path)
			out = append(out, rel+" -> "+to)
		case info.IsDir():
			out = append(out, rel+"/")
		default:
			out = append(out, rel+" "+info.Mode().String()+" "+strconv.FormatInt(info.Size(), 10))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func wantSame(t *testing.T, what string, before, after []string) {
	t.Helper()
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("%s changed:\nbefore:\n%s\nafter:\n%s", what, strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
}

func wantContains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, got)
		}
	}
}

func wantNot(t *testing.T, what, got string, nots ...string) {
	t.Helper()
	for _, n := range nots {
		if strings.Contains(got, n) {
			t.Errorf("%s holds %q:\n%s", what, n, got)
		}
	}
}

// wantSkills checks the skills of version v are installed and the six links
// resolve into them.
func wantSkills(t *testing.T, m *machine, v string) {
	t.Helper()
	if got := m.read(filepath.Join(m.skillsDir(), relMarker)); got != "parley "+v+"\n" {
		t.Fatalf("the installed skills carry the marker %q, want %s", got, v)
	}
	for _, l := range skillLinks(m.home) {
		resolved, err := filepath.EvalSymlinks(l)
		want := filepath.Join(m.skillsDir(), filepath.Base(l))
		if !isLink(l) || err != nil || resolved != want {
			t.Fatalf("%s resolves to %q (err %v), want a link to %s", l, resolved, err, want)
		}
		if !exists(filepath.Join(l, "SKILL.md")) {
			t.Fatalf("%s holds no SKILL.md", l)
		}
	}
}

// wantInstalled checks a finished install of version v: both binaries, the
// skills and their six links, and no leftover of the run.
func wantInstalled(t *testing.T, m *machine, v string) {
	t.Helper()
	for _, name := range []string{"parley", "ergo"} {
		fi, err := os.Lstat(m.bin(name))
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o755 {
			t.Fatalf("%s is not an executable file (err %v)", m.bin(name), err)
		}
	}
	if got := moduleVersion(m.read(m.bin("parley"))); got != v {
		t.Fatalf("the installed parley is at module version %q, want %s", got, v)
	}
	wantSkills(t, m, v)
	entries, _ := os.ReadDir(filepath.Join(m.home, ".local", "bin"))
	if len(entries) != 2 {
		t.Fatalf("~/.local/bin holds more than parley and ergo: %v", entries)
	}
	entries, _ = os.ReadDir(filepath.Join(m.home, ".local", "share", "parley"))
	if len(entries) != 1 || entries[0].Name() != "skills" {
		t.Fatalf("~/.local/share/parley holds more than skills: %v", entries)
	}
	if entries, _ := os.ReadDir(m.tmp); len(entries) != 0 {
		t.Fatalf("the installer left %v in its temporary directory", entries)
	}
}

func shells(t *testing.T) []string {
	list := []string{"sh"}
	if _, err := exec.LookPath("dash"); err == nil {
		list = append(list, "dash")
	}
	return list
}

// TestReleaseInstallFromAPipe: the advertised form, the script piped to the
// shell, on a machine with no ergo, parley or Go and ~/.local/bin off PATH. It
// ends with parley, ergo and the skills in place, says where PATH needs a line,
// and writes nothing to stderr.
func TestReleaseInstallFromAPipe(t *testing.T) {
	for _, shell := range shells(t) {
		t.Run(shell, func(t *testing.T) {
			r := newRelease(t, "v1.2.3")
			m := newMachine(t)
			m.shell = shell
			res := m.run("", r.script())
			if res.code != 0 || res.stderr != "" {
				t.Fatalf("the install exited %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
			}
			wantInstalled(t, m, "v1.2.3")
			wantContains(t, "stdout", res.stdout,
				"ergo: installed "+relErgoVersion, "parley: installed v1.2.3", "skills: installed v1.2.3",
				"linked: "+filepath.Join(m.home, ".claude", "skills", "parley-build"),
				"parley doctor was not run", "parley is not on your PATH", `export PATH="$HOME/.local/bin:$PATH"`)
			if m.logged() != "" {
				t.Errorf("a parley ran during the install: %q", m.logged())
			}
		})
	}
}

// TestReleaseInstallHashesWithShasumToo: the machine has one of sha256sum and
// shasum, never both necessarily, and the install works on either.
func TestReleaseInstallHashesWithShasumToo(t *testing.T) {
	r := newRelease(t, "v1.2.3")
	m := newMachine(t)
	if _, err := exec.LookPath("shasum"); err != nil {
		t.Skip("no shasum")
	}
	m.farm = m.toolFarm(map[string]bool{"sha256sum": true})
	if res := m.run(r.script(), ""); res.code != 0 {
		t.Fatalf("the install exited %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	wantInstalled(t, m, "v1.2.3")
}

// TestReleaseRerunLeavesAloneWhatIsThereAndUpgrades: a rerun succeeds and says
// what it left alone; a newer release replaces parley and the skills, and nothing
// of the old skills stays.
func TestReleaseRerunLeavesAloneWhatIsThereAndUpgrades(t *testing.T) {
	r := newRelease(t, "v1.2.3")
	m := newMachine(t)
	if res := m.run(r.script(), ""); res.code != 0 {
		t.Fatalf("the first install exited %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	first, err := os.Stat(m.bin("parley"))
	if err != nil {
		t.Fatal(err)
	}
	res := m.run(r.script(), "")
	if res.code != 0 || res.stderr != "" {
		t.Fatalf("the rerun exited %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	wantInstalled(t, m, "v1.2.3")
	if again, err := os.Stat(m.bin("parley")); err != nil || !os.SameFile(first, again) {
		t.Errorf("the rerun replaced a parley that was already the release (err %v)", err)
	}
	wantContains(t, "the rerun", res.stdout,
		"ergo: left alone", m.bin("ergo")+" is already a file",
		"parley: "+m.bin("parley")+" is already v1.2.3; left alone",
		"skills: "+m.skillsDir()+" is already at v1.2.3; left alone")
	if n := strings.Count(res.stdout, "already linked:"); n != 6 {
		t.Errorf("the rerun reports %d links already in place, want 6:\n%s", n, res.stdout)
	}
	wantNot(t, "the rerun", res.stdout, "Sessions already running")
	if got := r.archives("ergo/"); len(got) != 1 {
		t.Errorf("the rerun fetched ergo again: %v", got)
	}
	installed := first

	// A file the old skills carried and the new ones do not.
	m.write(filepath.Join(m.skillsDir(), "old-only"), "x", 0o644)
	newer := newRelease(t, "v1.3.0")
	res = m.run(newer.script(), "")
	if res.code != 0 || res.stderr != "" {
		t.Fatalf("the upgrade exited %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	wantInstalled(t, m, "v1.3.0")
	// parley changes by rename, never by a write in place: the file at the path is
	// a new one, so a session that is running the old one is running a file that
	// nothing has written to.
	if upgraded, err := os.Stat(m.bin("parley")); err != nil || os.SameFile(installed, upgraded) {
		t.Errorf("the upgrade wrote parley in place, not by rename (err %v)", err)
	}
	if exists(filepath.Join(m.skillsDir(), "old-only")) {
		t.Error("a file of the old skills is still there after the upgrade")
	}
	wantContains(t, "the upgrade", res.stdout,
		"parley: v1.2.3 -> v1.3.0 at "+m.bin("parley"),
		"Sessions already running keep the old parley until they are restarted, but their replies (parley send) use the new one at once.",
		"launch files name another parley path keeps that path", "parley line drop", "parley line add",
		"skills: installed v1.3.0")
	// The parley that was replaced was told apart by its text and never run.
	if strings.Contains(m.logged(), "parley-v1.2.3") {
		t.Errorf("the old parley was run:\n%s", m.logged())
	}
}

// TestReleaseTellsARealParleyBinaryByItsModulePath: the string the installer looks
// for is what a Go build of this module carries, and the version it prints for a
// binary it replaces is the one `go version -m` reads out of it. The binary is
// built here and never run.
func TestReleaseTellsARealParleyBinaryByItsModulePath(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH")
	}
	built := filepath.Join(t.TempDir(), "parley")
	build := exec.Command(goBin, "build", "-trimpath", "-o", built, "./cmd/parley")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	info, err := exec.Command(goBin, "version", "-m", built).Output()
	if err != nil {
		t.Fatal(err)
	}
	var have string
	for _, line := range strings.Split(string(info), "\n") {
		if f := strings.Fields(line); len(f) >= 3 && f[0] == "mod" && f[1] == "github.com/colinsurprenant/parley" {
			have = f[2]
		}
	}
	if have == "" {
		t.Fatalf("no module line in\n%s", info)
	}

	r := newRelease(t, "v1.2.3")
	m := newMachine(t)
	raw, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	m.write(m.bin("parley"), string(raw), 0o755)
	res := m.run(r.script(), "")
	if res.code != 0 {
		t.Fatalf("the install exited %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	wantContains(t, "stdout", res.stdout, "parley: "+have+" -> v1.2.3")
	wantInstalled(t, m, "v1.2.3")
}

// TestReleaseStopsBeforeInstallingAnything: each way the download or the script
// can be wrong ends the run with nothing written to the machine, and says why.
func TestReleaseStopsBeforeInstallingAnything(t *testing.T) {
	flip := func(b []byte) []byte {
		c := append([]byte(nil), b...)
		c[len(c)/2] ^= 0xff
		return c
	}
	for _, tc := range []struct {
		name string
		do   func(r *release, m *machine) installResult
		want string
	}{
		{"a corrupted parley archive", func(r *release, m *machine) installResult {
			for k, v := range r.files {
				if strings.HasPrefix(k, "/parley/") {
					r.files[k] = flip(v)
				}
			}
			return m.run(r.script(), "")
		}, "does not match its checksum"},
		{"a corrupted ergo archive after a good parley one", func(r *release, m *machine) installResult {
			for k, v := range r.files {
				if strings.HasPrefix(k, "/ergo/") {
					r.files[k] = flip(v)
				}
			}
			return m.run(r.script(), "")
		}, "does not match its checksum"},
		{"a wrong parley checksum", func(r *release, m *machine) installResult {
			for k := range r.sums {
				if strings.HasPrefix(k, "PARLEY_SHA256_") {
					r.sums[k] = strings.Repeat("0", 64)
				}
			}
			return m.run(r.script(), "")
		}, "does not match its checksum"},
		{"a wrong ergo checksum", func(r *release, m *machine) installResult {
			for k := range r.sums {
				if strings.HasPrefix(k, "ERGO_SHA256_") {
					r.sums[k] = strings.Repeat("f", 64)
				}
			}
			return m.run(r.script(), "")
		}, "does not match its checksum"},
		{"a checksum that is not a sha256", func(r *release, m *machine) installResult {
			r.sums["ERGO_SHA256_LINUX_ARM64"] = "abc"
			return m.run(r.script(), "")
		}, "not a sha256"},
		{"an archive that is not there", func(r *release, m *machine) installResult {
			for k := range r.files {
				if strings.HasPrefix(k, "/ergo/") {
					delete(r.files, k)
				}
			}
			return m.run(r.script(), "")
		}, "could not download"},
		{"an unsupported operating system", func(r *release, m *machine) installResult {
			m.front = []string{unameShim(m.t, "FreeBSD", "amd64")}
			return m.run(r.script(), "")
		}, "unsupported platform FreeBSD/amd64"},
		{"an unsupported architecture", func(r *release, m *machine) installResult {
			m.front = []string{unameShim(m.t, "Linux", "riscv64")}
			return m.run(r.script(), "")
		}, "unsupported platform Linux/riscv64"},
		{"a placeholder left in the script", func(r *release, m *machine) installResult {
			return m.run(r.script("ERGO_SHA256_LINUX_ARM64"), "")
		}, "still holds a placeholder"},
		{"the template itself", func(r *release, m *machine) installResult {
			raw, err := os.ReadFile(filepath.Join("..", "..", "install.sh.in"))
			if err != nil {
				m.t.Fatal(err)
			}
			return m.run(string(raw), "")
		}, "still holds a placeholder"},
		{"an argument", func(r *release, m *machine) installResult {
			return m.run(r.script(), "", "--prefix=/usr")
		}, "takes no arguments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, "v1.2.3")
			m := newMachine(t)
			before := snapshot(t, m.home)
			res := tc.do(r, m)
			if res.code != 1 || !strings.Contains(res.stderr, tc.want) {
				t.Fatalf("exit %d, want 1 and stderr with %q\nstdout:\n%s\nstderr:\n%s", res.code, tc.want, res.stdout, res.stderr)
			}
			wantSame(t, "the home directory", before, snapshot(t, m.home))
			if entries, _ := os.ReadDir(m.tmp); len(entries) != 0 {
				t.Errorf("the installer left %v in its temporary directory", entries)
			}
		})
	}
}

// TestReleaseAsksNothingBeforeItHasRefused: the refusals that come before the
// first download make no request at all.
func TestReleaseAsksNothingBeforeItHasRefused(t *testing.T) {
	r := newRelease(t, "v1.2.3")
	m := newMachine(t)
	m.run(r.script("PARLEY_VERSION"), "")
	m.front = []string{unameShim(t, "Plan9", "arm64")}
	m.run(r.script(), "")
	m.front = nil
	m.write(m.bin("parley"), "#!/bin/sh\necho mine\n", 0o755)
	m.run(r.script(), "")
	if got := r.requested(); len(got) != 0 {
		t.Fatalf("the server was asked for %v before a refusal", got)
	}
}

// TestReleaseTruncatedScriptInstallsNothing: a script cut anywhere inside main()
// or its helpers does not parse, so nothing of it runs, and one cut between two
// definitions defines them and calls nothing. The last line is the call, and
// cut inside its name it is no command; cut after it, it is the whole script.
func TestReleaseTruncatedScriptInstallsNothing(t *testing.T) {
	r := newRelease(t, "v1.2.3")
	full := r.script()
	lastLine := strings.LastIndex(strings.TrimSuffix(full, "\n"), "\n") + 1
	if strings.TrimSpace(full[lastLine:]) != `main "$@"` {
		t.Fatalf("the last line of install.sh.in is %q, want the call to main", full[lastLine:])
	}
	var cuts []int
	for i, c := range full[:lastLine+3] {
		if c == '\n' || i%97 == 0 {
			cuts = append(cuts, i)
		}
	}
	cuts = append(cuts, lastLine, lastLine+1, lastLine+2, lastLine+3)
	m := newMachine(t)
	before := snapshot(t, m.home)
	for _, n := range cuts {
		m.run("", full[:n])
		wantSame(t, "the home directory after a cut at byte "+strconv.Itoa(n), before, snapshot(t, m.home))
		if got := r.requested(); len(got) != 0 {
			t.Fatalf("a script cut at byte %d of %d reached the server: %v\n%s", n, len(full), got, full[max(0, n-80):n])
		}
	}
	// Whole, it is the install.
	if res := m.run("", full); res.code != 0 {
		t.Fatalf("the whole script exited %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	wantInstalled(t, m, "v1.2.3")
}

// TestReleaseChoosesTheArchivesOfThePlatform: uname's answer, and under Rosetta
// sysctl's, picks the parley archive and the ergo archive; a machine whose sysctl
// does not know the key is an Intel Mac, not an error.
func TestReleaseChoosesTheArchivesOfThePlatform(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		os, arch, sysctl     string
		parleyName, ergoName string
	}{
		{"linux amd64", "Linux", "x86_64", "", "parley-v1.2.3-linux-amd64", "ergo-2.19.1-linux-x86_64"},
		{"linux arm64", "Linux", "aarch64", "", "parley-v1.2.3-linux-arm64", "ergo-2.19.1-linux-arm64"},
		{"a Mac on arm64", "Darwin", "arm64", "echo 1", "parley-v1.2.3-darwin-arm64", "ergo-2.19.1-macos-arm64"},
		{"a shell under Rosetta", "Darwin", "x86_64", "echo 1", "parley-v1.2.3-darwin-arm64", "ergo-2.19.1-macos-arm64"},
		{"an Intel Mac", "Darwin", "x86_64", "echo \"sysctl: unknown oid 'hw.optional.arm64'\" >&2; exit 1", "parley-v1.2.3-darwin-amd64", "ergo-2.19.1-macos-x86_64"},
		{"a Mac with no sysctl at all", "Darwin", "x86_64", "", "parley-v1.2.3-darwin-amd64", "ergo-2.19.1-macos-x86_64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, "v1.2.3")
			m := newMachine(t)
			m.front = []string{unameShim(t, tc.os, tc.arch)}
			if tc.sysctl != "" {
				m.front = append(m.front, shim(t, "sysctl", tc.sysctl+"\n"))
			} else {
				// A farm without sysctl, so the call finds nothing.
				m.farm = m.toolFarm(map[string]bool{"sysctl": true})
			}
			res := m.run(r.script(), "")
			if res.code != 0 {
				t.Fatalf("the install exited %d\n%s%s", res.code, res.stdout, res.stderr)
			}
			got := strings.Join(r.requested(), " ")
			want := "parley/" + tc.parleyName + ".tar.gz ergo/" + tc.ergoName + ".tar.gz"
			if got != want {
				t.Fatalf("the server was asked for %q, want %q", got, want)
			}
		})
	}
}

// TestReleaseRefusesWhatIsNotItsOwnAtAnInstallPath: a symlink or a file that is
// not parley's at an install path ends the run before anything is written, and
// what was there is as it was. A foreign file is told by its text, so a script
// that would announce itself if run is not run.
func TestReleaseRefusesWhatIsNotItsOwnAtAnInstallPath(t *testing.T) {
	mine := "#!/bin/sh\necho mine >> \"$STUB_LOG\"\n"
	for _, tc := range []struct {
		name  string
		setup func(m *machine)
		want  string
	}{
		{"a symlink at parley", func(m *machine) {
			m.write(filepath.Join(m.home, "elsewhere", "parley"), "#!/bin/sh\n", 0o755)
			mustSymlink(t, filepath.Join(m.home, "elsewhere", "parley"), m.bin("parley"))
		}, "parley is a symlink"},
		{"a dangling symlink at parley", func(m *machine) {
			mustSymlink(t, filepath.Join(m.home, "gone"), m.bin("parley"))
		}, "parley is a symlink"},
		{"a foreign file at parley", func(m *machine) { m.write(m.bin("parley"), mine, 0o755) }, "is not a parley binary"},
		{"a launcher that runs parley by its package path", func(m *machine) {
			m.write(m.bin("parley"), "#!/bin/sh\necho launcher >> \"$STUB_LOG\"\nexec go run github.com/colinsurprenant/parley/cmd/parley \"$@\"\n", 0o755)
		}, "is not a parley binary"},
		{"a binary that is not Go", func(m *machine) { m.write(m.bin("parley"), systemFile(t, "ls"), 0o755) }, "is not a parley binary"},
		{"a Go binary that is not parley", func(m *machine) { m.write(m.bin("parley"), systemFile(t, "go"), 0o755) }, "is not a parley binary"},
		{"a directory at parley", func(m *machine) {
			if err := os.MkdirAll(m.bin("parley"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "is not a regular file"},
		{"a symlink at ergo", func(m *machine) {
			m.write(filepath.Join(m.home, "elsewhere", "ergo"), "#!/bin/sh\n", 0o755)
			mustSymlink(t, filepath.Join(m.home, "elsewhere", "ergo"), m.bin("ergo"))
			// Not on PATH, so it is the install path and not an ergo found.
		}, "ergo is a symlink"},
		{"a dangling symlink at ergo", func(m *machine) { mustSymlink(t, filepath.Join(m.home, "gone"), m.bin("ergo")) }, "ergo is a symlink"},
		{"a directory at ergo", func(m *machine) {
			if err := os.MkdirAll(m.bin("ergo"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "ergo is not a regular file"},
		{"a skills directory that is not ours", func(m *machine) { m.write(filepath.Join(m.skillsDir(), "mine"), "x", 0o644) }, "is not a skills directory this script made"},
		{"a skills directory whose marker is not ours", func(m *machine) { m.write(filepath.Join(m.skillsDir(), relMarker), "hello\n", 0o644) }, "is not a skills directory this script made"},
		{"a skills symlink", func(m *machine) {
			if err := os.MkdirAll(filepath.Join(m.home, "elsewhere"), 0o755); err != nil {
				t.Fatal(err)
			}
			m.write(filepath.Join(m.home, "elsewhere", relMarker), "parley v0\n", 0o644)
			mustSymlink(t, filepath.Join(m.home, "elsewhere"), m.skillsDir())
		}, "is not a skills directory this script made"},
		{"a skills.new that is not ours", func(m *machine) { m.write(filepath.Join(m.skillsDir()+".new", "mine"), "x", 0o644) }, "skills.new is not a skills directory this script made"},
		{"a skills.old that is not ours", func(m *machine) { m.write(filepath.Join(m.skillsDir()+".old", "mine"), "x", 0o644) }, "skills.old is not a skills directory this script made"},
		{"a bin directory that is a file", func(m *machine) { m.write(filepath.Join(m.home, ".local", "bin"), "x", 0o644) }, "cannot make or write in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, "v1.2.3")
			m := newMachine(t)
			tc.setup(m)
			before := snapshot(t, m.home)
			res := m.run(r.script(), "")
			if res.code != 1 || !strings.Contains(res.stderr, tc.want) {
				t.Fatalf("exit %d, want 1 and stderr with %q\nstdout:\n%s\nstderr:\n%s", res.code, tc.want, res.stdout, res.stderr)
			}
			wantSame(t, "the home directory", before, snapshot(t, m.home))
			if got := r.requested(); len(got) != 0 {
				t.Errorf("the server was asked for %v before the refusal", got)
			}
			if m.logged() != "" {
				t.Errorf("something ran: %q", m.logged())
			}
		})
	}
}

// systemFile is the bytes of the program name finds on PATH.
func systemFile(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("no %s on PATH", name)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestReleaseTellsParleyByAllThreeMarksOfItsBinary: a file at the parley path is
// replaced only if it begins with the magic number of an executable, carries the
// marker the Go linker puts before build information, and has the build line
// naming parley's main package up to the end of the line. Files made of those
// parts show which one each is needed for.
func TestReleaseTellsParleyByAllThreeMarksOfItsBinary(t *testing.T) {
	const (
		buildinf = "\xff Go buildinf:"
		pathLine = "path\tgithub.com/colinsurprenant/parley/cmd/parley\n"
	)
	file := func(magic, mark, line string) string {
		return magic + strings.Repeat("\x00", 60) + mark + strings.Repeat("\x00", 20) + line + "mod\tgithub.com/colinsurprenant/parley\tv0.5.0\t\n"
	}
	for _, magic := range []string{"\x7fELF", "\xcf\xfa\xed\xfe", "\xce\xfa\xed\xfe", "\xca\xfe\xba\xbe", "\xbe\xba\xfe\xca"} {
		t.Run(fmt.Sprintf("replaced, with magic % x", magic), func(t *testing.T) {
			r := newRelease(t, "v1.2.3")
			m := newMachine(t)
			m.write(m.bin("parley"), file(magic, buildinf, pathLine), 0o755)
			res := m.run(r.script(), "")
			if res.code != 0 {
				t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
			}
			wantContains(t, "stdout", res.stdout, "parley: v0.5.0 -> v1.2.3")
		})
	}
	for _, tc := range []struct{ name, body string }{
		{"no magic number: a text file", file("#!/bin/sh\n", buildinf, pathLine)},
		{"another magic number", file("MZ\x90\x00", buildinf, pathLine)},
		{"no build information marker", file("\x7fELF", "", pathLine)},
		{"another build information marker", file("\x7fELF", "\xff Go buildinf?", pathLine)},
		{"no build line", file("\x7fELF", buildinf, "")},
		{"a build line of another package", file("\x7fELF", buildinf, "path\tgithub.com/colinsurprenant/parley/cmd/other\n")},
		{"a build line that goes on", file("\x7fELF", buildinf, "path\tgithub.com/colinsurprenant/parley/cmd/parley-x\n")},
		{"a build line with a space for its tab", file("\x7fELF", buildinf, "path github.com/colinsurprenant/parley/cmd/parley\n")},
		{"a package path with another character for a dot", file("\x7fELF", buildinf, "path\tgithubXcom/colinsurprenant/parley/cmd/parley\n")},
	} {
		t.Run("refused, with "+tc.name, func(t *testing.T) {
			r := newRelease(t, "v1.2.3")
			m := newMachine(t)
			m.write(m.bin("parley"), tc.body, 0o755)
			before := snapshot(t, m.home)
			res := m.run(r.script(), "")
			if res.code != 1 || !strings.Contains(res.stderr, "is not a parley binary") {
				t.Fatalf("exit %d, want 1 and a refusal\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
			}
			wantSame(t, "the home directory", before, snapshot(t, m.home))
			if got := r.requested(); len(got) != 0 {
				t.Errorf("the server was asked for %v before the refusal", got)
			}
		})
	}
}

// TestReleaseLeavesAnErgoAlone: an ergo on PATH, or a file at ~/.local/bin/ergo
// that PATH does not reach, is left as it is and said so, and ergo is not
// downloaded.
func TestReleaseLeavesAnErgoAlone(t *testing.T) {
	t.Run("on PATH", func(t *testing.T) {
		r := newRelease(t, "v1.2.3")
		m := newMachine(t)
		elsewhere := shim(t, "ergo", "echo theirs\n")
		m.back = []string{elsewhere}
		res := m.run(r.script(), "")
		if res.code != 0 {
			t.Fatalf("the install exited %d\n%s%s", res.code, res.stdout, res.stderr)
		}
		wantContains(t, "stdout", res.stdout, "ergo: left alone, ergo is already installed at "+filepath.Join(elsewhere, "ergo"),
			"ergo on your PATH: "+filepath.Join(elsewhere, "ergo"))
		if exists(m.bin("ergo")) {
			t.Error("an ergo was installed beside the one on PATH")
		}
		if got := r.archives("ergo/"); len(got) != 0 {
			t.Errorf("ergo was downloaded: %v", got)
		}
		wantContains(t, "parley", m.read(m.bin("parley")), "v1.2.3")
	})
	t.Run("a regular file at the install path", func(t *testing.T) {
		r := newRelease(t, "v1.2.3")
		m := newMachine(t)
		m.write(m.bin("ergo"), "#!/bin/sh\necho mine\n", 0o644)
		res := m.run(r.script(), "")
		if res.code != 0 {
			t.Fatalf("the install exited %d\n%s%s", res.code, res.stdout, res.stderr)
		}
		wantContains(t, "stdout", res.stdout, "ergo: left alone, "+m.bin("ergo")+" is already a file")
		if got := m.read(m.bin("ergo")); got != "#!/bin/sh\necho mine\n" {
			t.Errorf("the ergo file was rewritten: %q", got)
		}
		if fi, _ := os.Lstat(m.bin("ergo")); fi.Mode().Perm() != 0o644 {
			t.Errorf("the ergo file's mode changed to %v", fi.Mode().Perm())
		}
		if got := r.archives("ergo/"); len(got) != 0 {
			t.Errorf("ergo was downloaded: %v", got)
		}
	})
	t.Run("a symlink that PATH reaches", func(t *testing.T) {
		r := newRelease(t, "v1.2.3")
		m := newMachine(t)
		target := filepath.Join(shim(t, "ergo", "echo theirs\n"), "ergo")
		mustSymlink(t, target, m.bin("ergo"))
		m.front = []string{filepath.Dir(m.bin("ergo"))}
		res := m.run(r.script(), "")
		if res.code != 0 {
			t.Fatalf("the install exited %d\n%s%s", res.code, res.stdout, res.stderr)
		}
		if to, err := os.Readlink(m.bin("ergo")); err != nil || to != target {
			t.Errorf("the ergo link is now %q (err %v), want it left at %q", to, err, target)
		}
	})
}

// TestReleaseKeepsGoingWhenTheSkillsCannotBeLinked: a link that a checkout owns,
// or one that dangles, makes skills/install.sh refuse. The run prints what it
// said and how to clear it, still installs parley and ergo and runs doctor, and
// exits 1, since the skills are not in place.
func TestReleaseKeepsGoingWhenTheSkillsCannotBeLinked(t *testing.T) {
	for _, tc := range []struct {
		name string
		link string // the path that is in the way, relative to HOME
		to   func(m *machine) string
	}{
		{"a link a checkout owns", ".claude/skills/parley-build", func(*machine) string { return filepath.Join(checkoutSkills(t), "parley-build") }},
		{"a dangling link", ".agents/skills/parley-line", func(m *machine) string { return filepath.Join(m.home, "gone", "parley-line") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, "v1.2.3")
			m := newMachine(t)
			m.back = []string{shim(t, "claude", "echo claude 2.1.0\n")}
			inTheWay := filepath.Join(m.home, filepath.FromSlash(tc.link))
			to := tc.to(m)
			mustSymlink(t, to, inTheWay)
			res := m.run(r.script(), "")
			if res.code != 1 {
				t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
			}
			wantContains(t, "stderr", res.stderr, inTheWay+" exists and is not a link to", "skills/install.sh --uninstall", "dangling link has to be removed by hand")
			wantContains(t, "stdout", res.stdout, "parley: installed v1.2.3", "ergo: installed", "skills: installed v1.2.3", "parley doctor --harness claude")
			for _, name := range []string{"parley", "ergo"} {
				if !exists(m.bin(name)) {
					t.Errorf("%s was not installed", name)
				}
			}
			// skills/install.sh refuses before it links any: the link in the way is as
			// it was, and none of the other five was made.
			if got, err := os.Readlink(inTheWay); err != nil || got != to {
				t.Errorf("the link in the way is now %q (err %v), want it left at %q", got, err, to)
			}
			for _, l := range skillLinks(m.home) {
				if l != inTheWay && isLink(l) {
					t.Errorf("%s was linked after a refusal", l)
				}
			}
		})
	}
}

// TestReleaseNamesWhichBinaryWinsOnPATH: the person's own PATH says which parley
// runs, and the report says so when it is not the installed one.
func TestReleaseNamesWhichBinaryWinsOnPATH(t *testing.T) {
	t.Run("another parley ahead", func(t *testing.T) {
		r := newRelease(t, "v1.2.3")
		m := newMachine(t)
		other := shim(t, "parley", "echo other\n")
		m.front = []string{other, filepath.Dir(m.bin("parley"))}
		res := m.run(r.script(), "")
		if res.code != 0 {
			t.Fatalf("the install exited %d\n%s%s", res.code, res.stdout, res.stderr)
		}
		wantContains(t, "stdout", res.stdout,
			"parley on your PATH is "+filepath.Join(other, "parley")+", which wins over the one installed here, "+m.bin("parley"),
			`export PATH="$HOME/.local/bin:$PATH"`)
	})
	t.Run("the installed one first", func(t *testing.T) {
		r := newRelease(t, "v1.2.3")
		m := newMachine(t)
		other := shim(t, "parley", "echo other\n")
		m.front = []string{filepath.Dir(m.bin("parley")) + "/", other}
		res := m.run(r.script(), "")
		if res.code != 0 {
			t.Fatalf("the install exited %d\n%s%s", res.code, res.stdout, res.stderr)
		}
		wantContains(t, "stdout", res.stdout, "parley on your PATH: ", "ergo on your PATH: ")
		wantNot(t, "stdout", res.stdout, "wins over", "export PATH", "not on your PATH")
	})
	t.Run("not on PATH", func(t *testing.T) {
		r := newRelease(t, "v1.2.3")
		m := newMachine(t)
		res := m.run(r.script(), "")
		wantContains(t, "stdout", res.stdout, "parley is not on your PATH", "ergo is not on your PATH", `export PATH="$HOME/.local/bin:$PATH"`)
	})
}

// TestReleaseClearsTheLeftoversOfAnEarlierRun: a skills.new or skills.old that
// carries the marker is what a run that was stopped left, whatever else it holds,
// and the next run clears it.
func TestReleaseClearsTheLeftoversOfAnEarlierRun(t *testing.T) {
	for _, name := range []string{"skills.new", "skills.old"} {
		for _, tc := range []struct {
			what  string
			files []string
		}{
			{"only the marker", nil},
			{"the marker and some files", []string{"half", "parley-build/SKILL.md"}},
		} {
			t.Run(name+" holding "+tc.what, func(t *testing.T) {
				r := newRelease(t, "v1.2.3")
				m := newMachine(t)
				dir := filepath.Join(filepath.Dir(m.skillsDir()), name)
				m.write(filepath.Join(dir, relMarker), "parley v0.1.0\n", 0o644)
				for _, f := range tc.files {
					m.write(filepath.Join(dir, f), "x", 0o644)
				}
				res := m.run(r.script(), "")
				if res.code != 0 || res.stderr != "" {
					t.Fatalf("the install exited %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
				}
				wantInstalled(t, m, "v1.2.3")
				if exists(filepath.Join(m.skillsDir(), "half")) {
					t.Error("a leftover file reached the installed skills")
				}
			})
		}
	}
}

// TestReleaseAnInterruptedSkillsSwapNeverLeavesAPathTheNextRunRefuses: an
// interrupt, as Ctrl-C is, in the middle of copying the new skills or of deleting
// the old ones kills the cp or the rm and ends the run. The names the next run's
// preflight reads are only ever whole directories, so the rerun installs, over
// the old skills that were never touched, or over the new ones that were.
func TestReleaseAnInterruptedSkillsSwapNeverLeavesAPathTheNextRunRefuses(t *testing.T) {
	realOf := func(name string) string {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("no %s", name)
		}
		return p
	}
	// The shims kill the installer's shell with an interrupt from inside the one
	// command, after doing part of its work, as a terminal's Ctrl-C does.
	cp := "case \" $* \" in\n*\" -R \"*)\n\tfor dest in \"$@\"; do :; done\n\tmkdir -p \"$dest\"\n\t: > \"$dest/partial\"\n\tkill -INT \"$PPID\"\n\texit 130\n\t;;\nesac\nexec " + realOf("cp") + " \"$@\"\n"
	rm := "case \"$*\" in\n*/.discard.*)\n\tfor dest in \"$@\"; do :; done\n\t" + realOf("rm") + " -f -- \"$dest\"/*/" + relMarker + "\n\tkill -INT \"$PPID\"\n\texit 130\n\t;;\nesac\nexec " + realOf("rm") + " \"$@\"\n"
	for _, tc := range []struct {
		name, tool, body string
		installed        bool // an install of v1.2.3 is there first
	}{
		{"copying, on a first install", "cp", cp, false},
		{"copying, over an installed release", "cp", cp, true},
		{"deleting the old skills, over an installed release", "rm", rm, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := newRelease(t, "v1.2.3")
			next := newRelease(t, "v1.3.0")
			m := newMachine(t)
			if tc.installed {
				if res := m.run(old.script(), ""); res.code != 0 {
					t.Fatalf("the first install exited %d\n%s%s", res.code, res.stdout, res.stderr)
				}
			}
			m.front = []string{shim(t, tc.tool, tc.body)}
			res := m.run(next.script(), "")
			if res.code == 0 {
				t.Fatalf("the interrupted run exited 0\n%s", res.stdout)
			}
			if tc.installed && tc.tool == "cp" {
				wantSkills(t, m, "v1.2.3")
			}
			for _, name := range []string{"skills", "skills.new", "skills.old"} {
				dir := filepath.Join(filepath.Dir(m.skillsDir()), name)
				if exists(dir) && !exists(filepath.Join(dir, relMarker)) {
					t.Fatalf("%s is left without the marker, which the next run refuses\n%s", dir, strings.Join(snapshot(t, filepath.Dir(m.skillsDir())), "\n"))
				}
			}
			m.front = nil
			res = m.run(next.script(), "")
			if res.code != 0 {
				t.Fatalf("the rerun exited %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
			}
			wantSkills(t, m, "v1.3.0")
		})
	}
}

// TestReleaseARunStoppedMidSwapIsRecoveredByRunningAgain: a run stopped between
// moving the old skills aside and moving the new ones in leaves only skills.old
// and skills.new; the next run installs.
func TestReleaseARunStoppedMidSwapIsRecoveredByRunningAgain(t *testing.T) {
	r := newRelease(t, "v1.2.3")
	m := newMachine(t)
	m.write(filepath.Join(m.skillsDir()+".old", relMarker), "parley v0.1.0\n", 0o644)
	m.write(filepath.Join(m.skillsDir()+".new", relMarker), "parley v1.2.3\n", 0o644)
	if res := m.run(r.script(), ""); res.code != 0 {
		t.Fatalf("the install exited %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	wantInstalled(t, m, "v1.2.3")
}

// TestReleaseRunsDoctorForTheHarnessesItFinds: doctor runs from ~/.local/bin with
// --harness for each of claude and codex found, and for opencode only when it
// reports a 2; its failure does not fail the install.
func TestReleaseRunsDoctorForTheHarnessesItFinds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		harnesses map[string]string
		doctor    string
		want      string
		not       []string
		ran       bool // whether doctor was run
	}{
		{"claude and codex", map[string]string{"claude": "echo 2.1.0", "codex": "echo codex-cli 0.161.0"}, "0", "parley doctor --harness claude --harness codex", []string{"--harness opencode"}, true},
		{"opencode 2", map[string]string{"opencode": "echo opencode v2.0.32"}, "0", "parley doctor --harness opencode", []string{"--harness claude"}, true},
		{"opencode 1", map[string]string{"opencode": "echo 1.18.30", "claude": "echo 2.1.0"}, "0", "parley doctor --harness claude", []string{"--harness opencode"}, true},
		{"opencode of no version", map[string]string{"opencode": "exit 3"}, "0", "parley doctor was not run", []string{"--harness"}, false},
		{"a doctor that fails", map[string]string{"codex": "echo 0.161.0"}, "1", "parley doctor exited 1; the install itself is complete", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, "v1.2.3")
			m := newMachine(t)
			for name, body := range tc.harnesses {
				m.back = append(m.back, shim(t, name, body+"\n"))
			}
			m.env = []string{"STUB_DOCTOR_EXIT=" + tc.doctor}
			res := m.run(r.script(), "")
			if res.code != 0 {
				t.Fatalf("the install exited %d\n%s%s", res.code, res.stdout, res.stderr)
			}
			wantContains(t, "stdout", res.stdout, tc.want)
			wantNot(t, "stdout", res.stdout, tc.not...)
			if tc.ran {
				// The installed parley ran, with ~/.local/bin first on PATH.
				wantContains(t, "the stub log", m.logged(), "parley-v1.2.3 doctor", "PATH="+filepath.Dir(m.bin("parley")))
			} else if m.logged() != "" {
				t.Errorf("doctor ran with no harness found:\n%s", m.logged())
			}
		})
	}
}

// TestErgoPinsAgree keeps the one ergo parley is verified against in step across
// the places that name it: the workflow that renders it into install.sh, the CI
// job that installs it for the tests, the configuration it was written for and
// the version doctor checks, and the checksums of the two platforms CI covers.
// The placeholders install.sh.in holds are the ones the workflow renders.
func TestErgoPinsAgree(t *testing.T) {
	read := func(parts ...string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	release := read(".github", "workflows", "release.yml")
	ci := read(".github", "workflows", "ci.yml")
	script := read("install.sh.in")

	version := regexp.MustCompile(`(?m)^\s+ERGO_VERSION: (\S+)$`).FindStringSubmatch(release)
	if version == nil {
		t.Fatal("release.yml sets no ERGO_VERSION")
	}
	sums := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s+ERGO_SHA256_([A-Z0-9_]+): ([0-9a-f]{64})$`).FindAllStringSubmatch(release, -1) {
		os_, arch, _ := strings.Cut(strings.ToLower(m[1]), "_")
		sums[os_+"-"+arch] = m[2]
	}
	if len(sums) != 4 {
		t.Fatalf("release.yml pins %d ergo checksums, want 4: %v", len(sums), sums)
	}
	for _, p := range relPlatforms {
		if sums[p.ergoOS+"-"+p.ergoArch] == "" {
			t.Errorf("release.yml pins no checksum for %s-%s", p.ergoOS, p.ergoArch)
		}
	}

	if want := doctor.ErgoVerified; version[1] != want {
		t.Errorf("release.yml installs ergo %s, doctor verifies %s", version[1], want)
	}
	for _, want := range []string{"Install ergo v" + version[1], "ergo-" + version[1] + "-${{ matrix.ergo }}",
		"releases/download/v" + version[1] + "/", "grep -x ergo-" + version[1]} {
		if !strings.Contains(ci, want) {
			t.Errorf("ci.yml does not say %q: it installs another ergo than release.yml pins", want)
		}
	}
	if !strings.Contains(ergoconf.YAML, "ergo v"+version[1]+" configuration") {
		t.Errorf("internal/ergoconf/ircd.yaml is not an ergo v%s configuration", version[1])
	}
	matrix := regexp.MustCompile(`(?m)^\s+ergo: (\S+)\n\s+ergo_sha256: ([0-9a-f]{64})$`).FindAllStringSubmatch(ci, -1)
	if len(matrix) == 0 {
		t.Fatal("ci.yml's matrix names no ergo checksum")
	}
	for _, m := range matrix {
		if sums[m[1]] != m[2] {
			t.Errorf("ci.yml pins %s for ergo %s, release.yml pins %q", m[2], m[1], sums[m[1]])
		}
	}

	// The placeholders the template holds are the ones the workflow renders, and
	// the names the script reads its archives by are the ones it renders.
	tokens := func(s string) []string {
		seen := map[string]bool{}
		var out []string
		for _, tok := range placeholderRE.FindAllString(s, -1) {
			if !seen[tok] {
				seen[tok] = true
				out = append(out, tok)
			}
		}
		sort.Strings(out)
		return out
	}
	if got, want := strings.Join(tokens(release), " "), strings.Join(tokens(script), " "); got != want {
		t.Errorf("release.yml renders %s, install.sh.in holds %s", got, want)
	}
	if !strings.Contains(release, relMarker) || !strings.Contains(script, "marker="+relMarker) {
		t.Errorf("release.yml and install.sh.in do not both name the marker file %s", relMarker)
	}
	gomod := regexp.MustCompile(`(?m)^module (\S+)$`).FindStringSubmatch(read("go.mod"))
	if gomod == nil || !strings.Contains(script, "parley_module="+gomod[1]+"\n") || !strings.Contains(script, "parley_package=$parley_module/cmd/parley\n") {
		t.Errorf("install.sh.in does not tell parley's binary by the module %v and cmd/parley", gomod)
	}
}

// TestReleaseWorkflowAndInstallScriptAgreeOnTheArchives keeps what the workflow
// builds and uploads in step with what install.sh.in asks for: an archive's name
// and its top directory, the platforms, the checksum each platform's archive is
// rendered into, and the files a release carries. Each is a string the two files
// both spell, so a rename in one is a failure here and not a 404 or a checksum
// mismatch for the first person to run the line.
func TestReleaseWorkflowAndInstallScriptAgreeOnTheArchives(t *testing.T) {
	read := func(parts ...string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	workflow := read(".github", "workflows", "release.yml")
	script := read("install.sh.in")
	one := func(src, what, pattern string) []string {
		t.Helper()
		m := regexp.MustCompile(pattern).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("no %s (%s)", what, pattern)
		}
		return m
	}
	has := func(src, what string, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(src, w) {
				t.Errorf("%s lacks %q", what, w)
			}
		}
	}

	// The name, and the top directory the name is.
	built := one(workflow, "archive name in release.yml", `(?m)^\s+name=(\S+)$`)[1]
	asked := one(script, "archive name in install.sh.in", `(?m)^\s+parley_name=(\S+)$`)[1]
	if got := strings.ReplaceAll(built, "$TAG", "$parley_version"); got != asked {
		t.Errorf("release.yml names the archive %s, install.sh.in asks for %s", built, asked)
	}
	has(workflow, "release.yml",
		`stage=$RUNNER_TEMP/stage/$name`,
		`cp "$RUNNER_TEMP/bin/$os-$arch/parley" "$stage/parley"`,
		`cp -R skills "$stage/skills"`,
		`"$stage/skills/`+relMarker+`"`,
		`tar --owner=0 --group=0 -czf "$RUNNER_TEMP/dist/$name.tar.gz" -C "$RUNNER_TEMP/stage" "$name"`)
	has(script, "install.sh.in",
		`fetch "$parley_base/$parley_name.tar.gz"`,
		`parley_new=$tmp/parley/$parley_name/parley`,
		`skills_new=$tmp/parley/$parley_name/skills`,
		"marker="+relMarker)

	// The platforms, and the checksum rendered for each.
	targets := strings.Fields(one(workflow, "TARGETS", `(?m)^\s+TARGETS: (.+)$`)[1])
	cases := regexp.MustCompile(`(?m)^\t(\w+)-(\w+)\) parley_sha=\$parley_sha_(\w+) ;;$`).FindAllStringSubmatch(script, -1)
	if len(cases) != len(targets) {
		t.Fatalf("release.yml builds %v, install.sh.in chooses among %d archives", targets, len(cases))
	}
	wired := map[string]bool{}
	for _, c := range cases {
		wired[c[1]+"/"+c[2]] = true
		if c[3] != c[1]+"_"+c[2] {
			t.Errorf("install.sh.in reads the %s-%s checksum from parley_sha_%s", c[1], c[2], c[3])
		}
	}
	for _, target := range targets {
		if !wired[target] {
			t.Errorf("release.yml builds %s, install.sh.in has no archive for it", target)
		}
		osName, arch, _ := strings.Cut(target, "/")
		up := strings.ToUpper(osName + "_" + arch)
		low := osName + "_" + arch
		has(workflow, "release.yml",
			"sum_"+low+"=$(sum "+osName+" "+arch+")",
			`-e "s|@PARLEY_SHA256_`+up+`@|$sum_`+low+`|g"`)
		has(script, "install.sh.in", "parley_sha_"+low+"='@PARLEY_SHA256_"+up+"@'")
	}

	// The ergo archives: the platform labels CI and the workflow pin are the ones
	// the script maps to a name and a checksum.
	has(script, "install.sh.in", "ergo_name=ergo-$ergo_version-$ergo_os-$ergo_arch", `fetch "$ergo_base/$ergo_name.tar.gz"`)
	for _, p := range relPlatforms {
		label := p.ergoOS + "-" + p.ergoArch
		low := p.ergoOS + "_" + p.ergoArch
		has(script, "install.sh.in", "\t"+label+") ergo_sha=$ergo_sha_"+low+" ;;")
	}

	// The files a release carries: one archive per target and install.sh, which is
	// the name the install line downloads.
	has(workflow, "release.yml",
		`"$dist"/parley-"$TAG"-*.tar.gz "$dist/install.sh"`,
		`-ne `+strconv.Itoa(len(targets)+1)+` ]`,
		`install.sh.in > "$dist/install.sh"`)
	if !strings.HasSuffix(doctor.InstallLine, "/releases/latest/download/install.sh | sh") {
		t.Errorf("the install line %q does not download the asset release.yml uploads, install.sh", doctor.InstallLine)
	}
}
