package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewRefusesAPathJSONWouldRewrite: encoding/json replaces each invalid UTF-8
// byte with U+FFFD, so a record, or an .mcp.json, that names such a path would name
// a path other than the one the run used, and the end that reads it would act on
// that one. table new and build new refuse, at plan time, every path they serialize
// unless it is valid UTF-8, naming the flag or the path, with nothing changed. The
// paths need not exist: the refusal comes before the first look at the disk, which
// is also why a name with such a byte can be tried on a filesystem that refuses to
// make one.
func TestNewRefusesAPathJSONWouldRewrite(t *testing.T) {
	bad := func(p string) string { return p + "/bad\xffname" }
	type input struct {
		name string
		// extra is appended to the arguments, so that a repeated single-valued flag
		// overrides the default; env is appended to the environment.
		extra []string
		env   []string
		want  string
	}
	cases := func(b *tableBench) []input {
		return []input{
			{"the target", []string{"--target", bad(b.repo)}, nil, "--target"},
			{"the state directory", []string{"--state-dir", bad(b.base)}, nil, "the state directory"},
			{"the temp directory", nil, []string{"TMPDIR=" + bad(b.tmp)}, "the temp directory ($TMPDIR)"},
			{"codex-bin", []string{"--codex-bin", bad(b.fake)}, nil, "--codex-bin"},
		}
	}
	t.Run("build new", func(t *testing.T) {
		b := newTableBench(t)
		list := append(cases(b),
			input{"a writable root", []string{"--writable-root", bad(b.root)}, nil, "--writable-root"})
		for _, c := range list {
			t.Run(c.name, func(t *testing.T) {
				before := b.buildSnapshot(t)
				res := runParleyEnvIn(t, "", append(b.env(), c.env...), b.buildArgs("b1", nil, c.extra...)...)
				wantUTF8Refusal(t, res, c.want)
				if after := b.buildSnapshot(t); after != before {
					t.Errorf("the run changed the machine.\nbefore:\n%s\nafter:\n%s", before, after)
				}
				if got := baseListing(b.base); got != "(absent)\n" {
					t.Errorf("the run left a trace in the state base:\n%s", got)
				}
			})
		}
	})
	t.Run("table new", func(t *testing.T) {
		b := newTableBench(t)
		list := append(cases(b),
			input{"the worktree", []string{"--worktree", bad(b.root)}, nil, "--worktree"})
		for _, c := range list {
			t.Run(c.name, func(t *testing.T) {
				before := b.snapshot(t)
				res := runParleyEnvIn(t, "", append(b.env(), c.env...), b.newArgs("t1", nil, c.extra...)...)
				wantUTF8Refusal(t, res, c.want)
				b.wantUnchanged(t, before)
				if got := baseListing(b.base); got != "(absent)\n" {
					t.Errorf("the run left a trace in the state base:\n%s", got)
				}
			})
		}
	})
	t.Run("a path that is valid UTF-8 and not ASCII is accepted", func(t *testing.T) {
		b := newTableBench(t)
		odd := filepath.Join(b.root, "é-dir")
		if err := os.Mkdir(odd, 0o755); err != nil {
			t.Fatal(err)
		}
		rec, _ := buildUp(t, b, "b1", nil, "--writable-root", odd)
		if rec.Target != b.repo {
			t.Errorf("the record names %q, want %q", rec.Target, b.repo)
		}
	})
}

// wantUTF8Refusal fails unless res is the exit-2 refusal of a path that is not
// valid UTF-8, naming what.
func wantUTF8Refusal(t *testing.T, res result, what string) {
	t.Helper()
	if res.code != 2 || !strings.Contains(res.stderr, what) || !strings.Contains(res.stderr, "not valid UTF-8") {
		t.Fatalf("exit %d, want 2 refusing %s as not valid UTF-8\nstdout: %s\nstderr: %s", res.code, what, res.stdout, res.stderr)
	}
}
