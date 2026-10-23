// Package gpgme embeds the gpgme runtime libraries that
// openshift-controller-manager links against beyond glibc, so a run does
// not depend on extracting them from a release payload first. README.md
// records the libraries' provenance and licenses.
package gpgme

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
)

// The libraries openshift-controller-manager links against beyond glibc.
//
//go:embed libgpgme.so.11 libgpg-error.so.0 libassuan.so.0
var libraries embed.FS

// Ensure writes the embedded libraries into dir, reusing files already
// present with matching content, so that dir can serve as a process's
// library search path.
func Ensure(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create library directory: %w", err)
	}
	entries, err := libraries.ReadDir(".")
	if err != nil {
		return fmt.Errorf("list embedded libraries: %w", err)
	}
	for _, entry := range entries {
		content, err := libraries.ReadFile(entry.Name())
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", entry.Name(), err)
		}
		path := filepath.Join(dir, entry.Name())
		if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, content) {
			continue
		}
		if err := os.WriteFile(path, content, 0o755); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}
