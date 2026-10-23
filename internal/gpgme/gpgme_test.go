package gpgme

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureWritesLibraries(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := libraries.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("embedded %d libraries, want 3", len(entries))
	}
	for _, entry := range entries {
		want, err := libraries.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s content does not match the embedded copy", entry.Name())
		}
	}
}

func TestEnsureReusesMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "libgpgme.so.11")
	stamp := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(stamp) {
		t.Fatal("matching library was rewritten")
	}
}

func TestEnsureReplacesMismatchingFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "libgpgme.so.11")
	if err := os.WriteFile(path, []byte("not a library"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	want, err := libraries.ReadFile("libgpgme.so.11")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("mismatching library was not replaced")
	}
}
