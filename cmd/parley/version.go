package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
)

// cmdVersion prints one line saying which parley this is, from the build
// information the Go toolchain stamped into the binary: a bug report that
// quotes it names the code that ran.
func cmdVersion(args []string) error {
	fs := flag.NewFlagSet("parley version", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: parley version")
	}
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	if fs.NArg() > 0 {
		return cfgErrf("unexpected argument %q", fs.Arg(0))
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		bi = nil
	}
	if _, err := fmt.Fprintln(os.Stdout, versionLine(bi)); err != nil {
		return fmt.Errorf("write the version: %w", err)
	}
	return nil
}

// revisionShort is how much of the VCS revision a version line keeps.
const revisionShort = 7

// versionLine is the line `parley version` prints: "parley <module version>
// rev <short revision> <go version>". The module version is "(devel)" for a
// build made from a checkout, as the toolchain stamps it; the revision is the
// first 7 characters of the commit the build was made at, with "-modified"
// after it when the work tree had uncommitted changes, and "unknown" for a build
// that carries none (made outside a checkout, or with -buildvcs=false). A nil bi
// is a binary with no build information at all.
func versionLine(bi *debug.BuildInfo) string {
	if bi == nil {
		return "parley (unknown) rev unknown " + runtime.Version()
	}
	version := bi.Main.Version
	if version == "" {
		version = "(devel)"
	}
	var rev, modified string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	switch {
	case rev == "":
		rev = "unknown"
	default:
		rev = shorten(rev, revisionShort)
		if modified == "true" {
			rev += "-modified"
		}
	}
	return fmt.Sprintf("parley %s rev %s %s", version, rev, bi.GoVersion)
}

func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
