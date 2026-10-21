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
