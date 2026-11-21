// Package testutil holds helpers that tests of several packages share.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Fixture reads a file of the testdata directory, for example
// Fixture(t, "github/pull_request.opened.json").
func Fixture(t testing.TB, name string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "testdata")
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Root returns the path of the testdata directory.
func Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata")
}
