package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/colinsurprenant/parley/internal/doctor"
)

// TestDocsNameTheOpenCodeVersionLastVerified: wherever the docs say which OpenCode
// parley was last verified against, they say doctor.OpenCodeVerified, so that moving
// the constant after a live check cannot leave a document behind.
func TestDocsNameTheOpenCodeVersionLastVerified(t *testing.T) {
	re := regexp.MustCompile(`verified\s+against\s+OpenCode\s+(\d+\.\d+\.\d+)`)
	for _, f := range []string{"README.md", "docs/line.md", "docs/reference.md"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		found := re.FindAllStringSubmatch(string(raw), -1)
		if len(found) == 0 {
			t.Errorf("%s does not say which OpenCode parley was last verified against", f)
		}
		for _, m := range found {
			if m[1] != doctor.OpenCodeVerified {
				t.Errorf("%s says parley was last verified against OpenCode %s, want %s", f, m[1], doctor.OpenCodeVerified)
			}
		}
	}
}

// installLineDocs are the documents that give the line which installs parley.
var installLineDocs = []string{"docs/table.md"}

// TestDocsGiveTheInstallLineAsDoctorDoes: wherever the docs give the line that
// installs parley, ergo and the skills, it is doctor.InstallLine, the line
// doctor tells a person to run, so the two cannot drift apart.
func TestDocsGiveTheInstallLineAsDoctorDoes(t *testing.T) {
	re := regexp.MustCompile(`curl -fsSL \S*install\.sh \| sh`)
	for _, f := range installLineDocs {
		raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		found := re.FindAllString(string(raw), -1)
		if len(found) == 0 {
			t.Errorf("%s does not give the install line", f)
		}
		for _, line := range found {
			if line != doctor.InstallLine {
				t.Errorf("%s gives the install line as %q, want %q", f, line, doctor.InstallLine)
			}
		}
	}
}
