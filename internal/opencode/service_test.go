package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// serviceFile writes a service.json under a state directory of its own and returns
// its path. mode is the file's.
func serviceFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "service.json")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodService = `{"id":"1b0e1e96-26b8-470c-9aee-f6f58e02b495","version":"2.0.32","url":"http://127.0.0.1:51868","pid":79859,"password":"` + testPassword + `"}`

func TestReadServiceReadsWhatOpenCodeWrites(t *testing.T) {
	s, err := ReadService(serviceFile(t, goodService, 0o600))
	if err != nil {
		t.Fatalf("ReadService = %v", err)
	}
	if s.ID != "1b0e1e96-26b8-470c-9aee-f6f58e02b495" || s.Version != "2.0.32" || s.URL != "http://127.0.0.1:51868" || s.PID != 79859 || s.Password != testPassword {
		t.Fatalf("ReadService = %v (password %q)", s, s.Password)
	}
	// Unknown fields are tolerated, and a service without a password is read
	// (the server will refuse the request, and say so).
	s, err = ReadService(serviceFile(t, `{"url":"http://127.0.0.1:1","pid":2,"future":true}`, 0o600))
	if err != nil || s.Password != "" || s.URL != "http://127.0.0.1:1" {
		t.Fatalf("ReadService of a minimal file = %v, %v", s, err)
	}
	// A file only the user can read, and no more: 0400 is fine.
	if _, err := ReadService(serviceFile(t, goodService, 0o400)); err != nil {
		t.Fatalf("ReadService of a 0400 file = %v", err)
	}
}

func TestReadServiceRefusesWhatOnlyTheUsersOwnOpenCodeWrites(t *testing.T) {
	long := `{"url":"http://127.0.0.1:1","pid":2,"password":"` + strings.Repeat("x", maxServiceFileBytes) + `"}`
	for name, tc := range map[string]struct {
		content string
		mode    os.FileMode
		want    string
	}{
		"group readable":  {goodService, 0o640, "readable by you alone"},
		"world readable":  {goodService, 0o644, "readable by you alone"},
		"group writable":  {goodService, 0o620, "readable by you alone"},
		"world writable":  {goodService, 0o602, "readable by you alone"},
		"too big":         {long, 0o600, "over the"},
		"not json":        {`url=http://127.0.0.1:1`, 0o600, "not the JSON object"},
		"a list":          {`[]`, 0o600, "not the JSON object"},
		"no url":          {`{"pid":2}`, 0o600, "no url and pid"},
		"empty url":       {`{"url":"","pid":2}`, 0o600, "no url and pid"},
		"no pid":          {`{"url":"http://127.0.0.1:1"}`, 0o600, "no url and pid"},
		"a zero pid":      {`{"url":"http://127.0.0.1:1","pid":0}`, 0o600, "no url and pid"},
		"a string pid":    {`{"url":"http://127.0.0.1:1","pid":"7","password":"` + testPassword + `"}`, 0o600, "not the JSON object"},
		"a number url":    {`{"url":5,"pid":7,"password":"` + testPassword + `"}`, 0o600, "not the JSON object"},
		"trailing values": {goodService + ` {}`, 0o600, "not the JSON object"},
	} {
		_, err := ReadService(serviceFile(t, tc.content, tc.mode))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ReadService = %v, want %q", name, err, tc.want)
			continue
		}
		// The refusal never carries the password, whatever was wrong with the file.
		if strings.Contains(err.Error(), testPassword) {
			t.Errorf("%s: the refusal holds the password: %v", name, err)
		}
	}
}

func TestReadServiceRefusesALinkADirectoryAndAFIFO(t *testing.T) {
	good := serviceFile(t, goodService, 0o600)
	link := filepath.Join(t.TempDir(), "service.json")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadService(link); err == nil {
		t.Error("ReadService followed a symbolic link")
	}
	if _, err := ReadService(filepath.Dir(good)); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("ReadService of a directory = %v", err)
	}
	// A FIFO in its place is not waited on for a writer that never comes.
	fifo := filepath.Join(t.TempDir(), "service.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadService(fifo); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("ReadService of a FIFO = %v", err)
	}
}

func TestReadServiceOfAFileThatIsNotThereIsErrNoService(t *testing.T) {
	_, err := ReadService(filepath.Join(t.TempDir(), "opencode", "service.json"))
	if !errors.Is(err, ErrNoService) {
		t.Fatalf("ReadService of a missing file = %v, want ErrNoService", err)
	}
	if !strings.Contains(err.Error(), "service.json") {
		t.Errorf("the error does not say where it looked: %v", err)
	}
}

func TestAServiceNeverPrintsItsPassword(t *testing.T) {
	s, err := ReadService(serviceFile(t, goodService, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{
		fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%s", s),
		fmt.Sprintf("%v", &s), fmt.Sprintf("%#v", &s), ServiceWhere(s),
	} {
		if strings.Contains(out, testPassword) {
			t.Errorf("the password is in %q", out)
		}
	}
	if out := fmt.Sprintf("%+v", s); !strings.Contains(out, "redacted") {
		t.Errorf("%q does not say the password is held back", out)
	}
}

func TestServiceClientHoldsToLoopback(t *testing.T) {
	for _, u := range []string{"http://example.com:8080", "http://localhost:8080", "http://192.168.1.5:8080", "https://127.0.0.1:8080", "http://0.0.0.0:8080", "http://127.0.0.1:8080/api", "http://127.0.0.1"} {
		_, err := Service{URL: u, PID: 1, Password: testPassword}.Client()
		if err == nil {
			t.Errorf("Service.Client accepted %s", u)
		} else if strings.Contains(err.Error(), testPassword) {
			t.Errorf("the refusal of %s holds the password: %v", u, err)
		}
	}
	c, err := Service{URL: "http://127.0.0.1:51868", PID: 1, Password: testPassword}.Client()
	if err != nil || c == nil {
		t.Fatalf("Service.Client of a loopback service = %v", err)
	}
}

func TestServicePath(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
		err  string
	}{
		"xdg":              {map[string]string{"XDG_STATE_HOME": "/s", "HOME": "/h"}, "/s/opencode/service.json", ""},
		"home":             {map[string]string{"HOME": "/h"}, "/h/.local/state/opencode/service.json", ""},
		"an empty xdg":     {map[string]string{"XDG_STATE_HOME": "", "HOME": "/h"}, "/h/.local/state/opencode/service.json", ""},
		"a relative xdg":   {map[string]string{"XDG_STATE_HOME": "state", "HOME": "/h"}, "", "not an absolute path"},
		"nothing":          {map[string]string{}, "", "not known where"},
		"a relative home":  {map[string]string{"HOME": "h"}, "", "not known where"},
		"xdg without home": {map[string]string{"XDG_STATE_HOME": "/s"}, "/s/opencode/service.json", ""},
	} {
		got, err := ServicePath(env(tc.env))
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: ServicePath = %q, %v, want an error with %q", name, got, err, tc.err)
		case tc.err == "" && (err != nil || got != tc.want):
			t.Errorf("%s: ServicePath = %q, %v, want %q", name, got, err, tc.want)
		}
	}
}

func TestAURLIsPrintedWithoutItsUserinfo(t *testing.T) {
	for raw, want := range map[string]string{
		"http://user:secret@127.0.0.1:1":      "http://127.0.0.1:1",
		"http://user@127.0.0.1:1/path":        "http://127.0.0.1:1/path",
		"http://a@b:secret@127.0.0.1:1":       "http://127.0.0.1:1",
		"http://u:p ss@host:1":                "http://host:1",
		"http://127.0.0.1:1":                  "http://127.0.0.1:1",
		"http://127.0.0.1:1/x@y":              "http://127.0.0.1:1/x@y",
		"not a url with secret@somewhere":     "not a url with secret@somewhere",
		"HTTP://USER:SECRET@127.0.0.1:1?q=@z": "HTTP://127.0.0.1:1?q=@z",
	} {
		if got := RedactURL(raw); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestNoErrorAboutAURLQuotesItsUserinfo: a registration file can name any URL, and
// the refusal of it says what was wrong with it. Whatever is wrong, the credentials
// in it are not repeated, in the client's error or in the service's own printing.
func TestNoErrorAboutAURLQuotesItsUserinfo(t *testing.T) {
	for _, raw := range []string{
		"http://user:hunter2@127.0.0.1:1",
		"http://user:hunter2@example.com:1",
		"https://user:hunter2@127.0.0.1:1",
		"http://user:hunter2@127.0.0.1",
		"http://user:hunter2@127.0.0.1:99999",
		"http://user:hunter2@127.0.0.1:1/path",
		"http://user:hun ter2@127.0.0.1:1",
		"http://user:hunter2@[::1:1",
	} {
		_, err := NewHTTPClient(raw, Auth{})
		if err == nil {
			t.Errorf("NewHTTPClient accepted %q", raw)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the refusal of %q quotes the credentials: %v", raw, err)
		}
		if _, err := (Service{URL: raw, PID: 1}).Client(); err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("Service.Client of %q = %v", raw, err)
		}
		if out := fmt.Sprintf("%v %+v %#v %s", Service{URL: raw, PID: 1}, Service{URL: raw}, Service{URL: raw}, ServiceWhere(Service{URL: raw, PID: 1})); strings.Contains(out, "hunter2") {
			t.Errorf("the service prints the credentials of %q: %s", raw, out)
		}
	}
	// And through Find, with a file on disk.
	path := serviceFile(t, `{"url":"http://user:hunter2@127.0.0.1:1","pid":2}`, 0o600)
	_, _, _, err := Discovery{Path: path}.Find(context.Background())
	if !errors.Is(err, ErrServiceAddress) || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("Find = %v", err)
	}
}
