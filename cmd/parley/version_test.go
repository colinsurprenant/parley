package main

import (
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// buildInfo is a hand-built debug.BuildInfo: the line is tested on this, not on
// what the build of the test binary stamped, which holds no revision when the
// source was exported without .git.
func buildInfo(version, goVersion string, settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{GoVersion: goVersion, Main: debug.Module{Version: version}, Settings: settings}
}

func TestVersionLineIsModuleVersionShortRevisionAndGoVersion(t *testing.T) {
	const full = "7eaca55c0ffee0123456789abcdef0123456789a"
	for _, tc := range []struct {
		name string
		bi   *debug.BuildInfo
		want string
	}{
		{"a tagged module, clean", buildInfo("v1.2.3", "go1.25.0", debug.BuildSetting{Key: "vcs.revision", Value: full}, debug.BuildSetting{Key: "vcs.modified", Value: "false"}),
			"parley v1.2.3 rev 7eaca55 go1.25.0"},
		{"a checkout build, dirty", buildInfo("(devel)", "go1.25.1", debug.BuildSetting{Key: "vcs.revision", Value: full}, debug.BuildSetting{Key: "vcs.modified", Value: "true"}),
			"parley (devel) rev 7eaca55-modified go1.25.1"},
		{"no module version", buildInfo("", "go1.25.0", debug.BuildSetting{Key: "vcs.revision", Value: full}),
			"parley (devel) rev 7eaca55 go1.25.0"},
		{"no revision stamped", buildInfo("(devel)", "go1.25.0", debug.BuildSetting{Key: "vcs", Value: "git"}),
			"parley (devel) rev unknown go1.25.0"},
		{"a modified flag without a revision says nothing of it", buildInfo("(devel)", "go1.25.0", debug.BuildSetting{Key: "vcs.modified", Value: "true"}),
			"parley (devel) rev unknown go1.25.0"},
		{"a revision shorter than seven is kept whole", buildInfo("(devel)", "go1.25.0", debug.BuildSetting{Key: "vcs.revision", Value: "abc"}),
			"parley (devel) rev abc go1.25.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionLine(tc.bi); got != tc.want {
				t.Errorf("versionLine = %q, want %q", got, tc.want)
			}
		})
	}
	if got := versionLine(nil); got != "parley (unknown) rev unknown "+runtime.Version() {
		t.Errorf("versionLine(nil) = %q", got)
	}
}

// versionRE is a version line as the real build prints it: the revision is
// whatever the build of the test binary could stamp.
var versionRE = regexp.MustCompile(`^parley \S+ rev (unknown|[0-9a-f]{1,7}(-modified)?) go\S+\n$`)

func TestVersionVerbAndItsAliasesPrintOneLineOnStdout(t *testing.T) {
	var first string
	for _, arg := range []string{"version", "--version", "-version"} {
		res := runParley(t, "", arg)
		if res.code != 0 || res.stderr != "" {
			t.Fatalf("parley %s: exit %d, stderr %q", arg, res.code, res.stderr)
		}
		if !versionRE.MatchString(res.stdout) {
			t.Errorf("parley %s printed %q, want one version line", arg, res.stdout)
		}
		if first == "" {
			first = res.stdout
		} else if res.stdout != first {
			t.Errorf("parley %s printed %q, but parley version printed %q", arg, res.stdout, first)
		}
	}
	if res := runParley(t, "", "version", "extra"); res.code != exitConfig {
		t.Errorf("a stray argument exited %d, want 2", res.code)
	}
}

func TestTopLevelUsageListsVersion(t *testing.T) {
	res := mustRun(t, "", "-h")
	if !strings.Contains(res.stdout, "\n  parley version\n") {
		t.Errorf("the usage lines do not list parley version:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "\n  version  print one line") {
		t.Errorf("the verbs section does not describe version:\n%s", res.stdout)
	}
}

// TestEveryVerbsHelpIsOnStdoutAndExits0: a person who asks a verb for help gets
// its usage where a pager or a grep can read it, and a successful exit; a flag
// error is still a usage error on stderr, exit 2, with nothing on stdout.
func TestEveryVerbsHelpIsOnStdoutAndExits0(t *testing.T) {
	for _, verb := range [][]string{
		{"send"}, {"wait"}, {"channel"}, {"codex"}, {"primer"}, {"server"}, {"doctor"}, {"version"},
		{"table", "new"}, {"table", "end"}, {"build", "new"}, {"build", "end"},
	} {
		name := strings.Join(verb, " ")
		for _, flag := range []string{"-h", "--help"} {
			t.Run(name+" "+flag, func(t *testing.T) {
				res := runParley(t, "", append(append([]string(nil), verb...), flag)...)
				if res.code != 0 {
					t.Errorf("exit %d, want 0\nstderr: %s", res.code, res.stderr)
				}
				if res.stderr != "" {
					t.Errorf("help wrote to stderr: %q", res.stderr)
				}
				if !strings.Contains(res.stdout, "usage: parley "+name) {
					t.Errorf("stdout has no `usage: parley %s`:\n%s", name, res.stdout)
				}
			})
		}
		if name == "version" {
			continue
		}
		t.Run(name+" a flag error", func(t *testing.T) {
			res := runParley(t, "", append(append([]string(nil), verb...), "--no-such-flag")...)
			if res.code != exitConfig {
				t.Errorf("exit %d, want 2", res.code)
			}
			if res.stdout != "" {
				t.Errorf("a flag error wrote to stdout: %q", res.stdout)
			}
			if !strings.Contains(res.stderr, "no-such-flag") || !strings.Contains(res.stderr, "usage: parley "+name) {
				t.Errorf("stderr does not name the flag and show the usage:\n%s", res.stderr)
			}
		})
	}
}
