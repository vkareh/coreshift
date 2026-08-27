package image

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractBinary(t *testing.T) {
	filesystem := t.TempDir()
	target := t.TempDir()
	source := filepath.Join(filesystem, "usr", "bin", "etcd")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("binary"), 0o751); err != nil {
		t.Fatal(err)
	}

	if err := ExtractBinary(filesystem, "/usr/bin/etcd", target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "etcd"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "binary" {
		t.Fatalf("copied content = %q", got)
	}
	info, err := os.Stat(filepath.Join(target, "etcd"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o751); got != want {
		t.Fatalf("copied mode = %o, want %o", got, want)
	}
}

func TestExtractBinaryExtraLibraries(t *testing.T) {
	filesystem := t.TempDir()
	target := t.TempDir()
	binary := filepath.Join(filesystem, "usr", "bin", "app")
	library := filepath.Join(filesystem, "lib", "libexample.so")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(library), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(library, []byte("library"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ExtractBinary(filesystem, "/usr/bin/app", target, []string{"/lib/libexample.so"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "libexample.so")); err != nil {
		t.Fatal(err)
	}
}

func TestExtractBinaryRejectsEscape(t *testing.T) {
	if err := ExtractBinary(t.TempDir(), "../../etcd", t.TempDir()); err == nil {
		t.Fatal("expected path escape error")
	}
}
