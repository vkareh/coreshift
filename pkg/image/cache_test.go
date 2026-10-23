package image

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// testDigest is a syntactically valid sha256 digest for fake references.
const (
	testDigest    = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testDigestRef = "quay.io/example/repo@" + testDigest
)

func TestCachePathKeepsReferenceStructure(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	path, err := cachePath(componentsKind, testDigestRef)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cacheRootForTest(t), componentsKind, "quay.io", "example", "repo@sha256-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestCachePathRejectsInvalidReference(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if _, err := cachePath(componentsKind, "../../etc"); err == nil {
		t.Fatal("expected error for path-escaping reference")
	}
}

func TestExtractCachedUsesExistingDirectory(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	imageRef := testDigestRef
	dir, err := cachePath(componentsKind, imageRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	called := false
	client := &Client{}
	got, err := client.extractCached(context.Background(), componentsKind, imageRef, func(_, _ string) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("extract ran despite cached directory")
	}
	if got != dir {
		t.Fatalf("directory = %q, want %q", got, dir)
	}
}

func TestExtractCachedRejectsInvalidReference(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	client := &Client{}
	_, err := client.extractCached(context.Background(), componentsKind, "../../etc", func(_, _ string) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for path-escaping reference")
	}
}

// TestCacheRootEmptyVariableStaysBelowHome pins the default location:
// an empty XDG cache variable counts as unset by the base-directory
// convention, so the cache root stays where existing installs keep it,
// below the home directory.
func TestCacheRootEmptyVariableStaysBelowHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")

	got, err := CacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".cache", "coreshift"); got != want {
		t.Fatalf("cache root = %q, want %q", got, want)
	}
}

// cacheRootForTest returns the cache root expected for the current
// XDG_CACHE_HOME, mirroring cacheRoot without its error handling.
func cacheRootForTest(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("XDG_CACHE_HOME"), "coreshift")
}
