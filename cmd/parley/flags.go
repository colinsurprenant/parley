package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
)

// parseFlags is fs.Parse with the output of -h and --help on stdout, where a
// person asking for help reads it and a pipe to a pager catches it, and the
// output of a flag error on stderr, where it always was. The flag package writes
// both to one writer, so the writer is a buffer until the outcome is known. A
// help request still returns flag.ErrHelp, for the caller to end the verb with
// exit 0; any other error is returned for the caller to make a configuration
// error of.
func parseFlags(fs *flag.FlagSet, args []string) error {
	var out bytes.Buffer
	fs.SetOutput(&out)
	err := fs.Parse(args)
	dst := os.Stderr
	if errors.Is(err, flag.ErrHelp) {
		dst = os.Stdout
	}
	if out.Len() > 0 {
		_, _ = dst.Write(out.Bytes())
	}
	return err
}
