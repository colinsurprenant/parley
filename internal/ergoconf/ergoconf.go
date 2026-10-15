// Package ergoconf is the ergo configuration parley runs its IRC channel on,
// embedded in the binary so that one command — `parley server` — starts the
// per-machine server without a checkout beside it.
//
// The file is ircd.yaml, a complete configuration that runs as it stands: a
// listener on 127.0.0.1:6667 and a datastore named by a relative path. Render
// replaces those two values and nothing else, so every other line a reader of
// the file sees is a line the server runs. This package must not import
// internal/stream/irc: that package's tests check the file against the
// settings they run ergo with, and the import would be a cycle.
package ergoconf

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// YAML is the embedded configuration, exactly as written in ircd.yaml.
//
//go:embed ircd.yaml
var YAML string

// The two values Render replaces, spelled as ircd.yaml spells them. Each must
// stand in the file exactly once: a file that has lost one, or grown a second,
// is refused rather than rendered into something that listens or stores
// somewhere other than where it was told.
const (
	listenerValue  = `"127.0.0.1:6667": {}`
	datastoreValue = `path: ergo.db`
)

// DatastoreFile is the account database's name inside a datastore directory,
// the file ircd.yaml names relatively.
const DatastoreFile = "ergo.db"

// ConfigPattern is the name Install gives each rendered configuration inside a
// datastore directory, its * a fresh random part per run.
const ConfigPattern = "ircd-*.yaml"

// Render returns the configuration with its listener set to listen and its
// datastore to the absolute path datastore. A YAML double-quoted scalar is a
// JSON string with a few more escapes allowed, so both values are written as
// JSON strings: a path with a space, a colon or a hash in it stays one value.
func Render(listen, datastore string) ([]byte, error) {
	if listen == "" {
		return nil, errors.New("ergoconf: no listen address")
	}
	if !filepath.IsAbs(datastore) {
		return nil, fmt.Errorf("ergoconf: the datastore path %q is not absolute: ergo resolves a relative one against a working directory this package does not choose", datastore)
	}
	conf := YAML
	for _, r := range []struct{ old, new string }{
		{listenerValue, quote(listen) + ": {}"},
		{datastoreValue, "path: " + quote(datastore)},
	} {
		if n := strings.Count(conf, r.old); n != 1 {
			return nil, fmt.Errorf("ergoconf: the embedded ircd.yaml has %d occurrences of %q, want exactly 1", n, r.old)
		}
		conf = strings.Replace(conf, r.old, r.new, 1)
	}
	return []byte(conf), nil
}

func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	// A string always encodes.
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// Install renders the configuration for the datastore directory dir, which is
// an existing absolute directory, and writes it there in a file of its own
// (ConfigPattern), with the account database at dir/DatastoreFile. It returns
// the configuration's path, which the caller removes once ergo has exited.
// ergo is to be started with dir as its working directory: the configuration
// names its lock file, ircd.lock, relatively, so that is where the file goes,
// and a second ergo started on dir finds the first one's lock and refuses to
// run. Each run gets its own configuration file, so two starts against one
// datastore cannot overwrite each other's before their ergo reads it; a failure
// leaves no half of one.
func Install(dir, listen string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("ergoconf: the datastore directory %q is not absolute", dir)
	}
	conf, err := Render(listen, filepath.Join(dir, DatastoreFile))
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ConfigPattern)
	if err != nil {
		return "", fmt.Errorf("ergoconf: write the configuration: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(conf); err != nil {
		return "", fmt.Errorf("ergoconf: write the configuration: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("ergoconf: write the configuration: %w", err)
	}
	done = true
	return tmp.Name(), nil
}
