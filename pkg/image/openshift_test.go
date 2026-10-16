package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findOpenShiftComponent returns the extraction table entry for name.
func findOpenShiftComponent(t *testing.T, name string) openshiftComponent {
	t.Helper()
	for _, component := range openshiftComponents {
		if component.component == name {
			return component
		}
	}
	t.Fatalf("no openshiftComponents entry named %q", name)
	return openshiftComponent{}
}

func writeImageFiles(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(p, "/")))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(filepath.Base(p)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtractOpenShiftBinaryCopiesExtraLibraries(t *testing.T) {
	root := t.TempDir()
	writeImageFiles(t, root,
		"/usr/bin/openshift-controller-manager",
		"/usr/lib64/libgpgme.so.11",
		"/usr/lib64/libgpg-error.so.0",
		"/usr/lib64/libassuan.so.0",
	)
	target := t.TempDir()

	path, err := extractOpenShiftBinary(findOpenShiftComponent(t, "openshift-controller-manager"), root, target)
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(target, "openshift-controller-manager"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	for _, name := range []string{
		"openshift-controller-manager",
		"libgpgme.so.11",
		"libgpg-error.so.0",
		"libassuan.so.0",
	} {
		content, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(content) != name {
			t.Fatalf("%s content = %q, want %q", name, content, name)
		}
	}
}

func TestExtractOpenShiftBinaryPlain(t *testing.T) {
	root := t.TempDir()
	writeImageFiles(t, root, "/usr/bin/cluster-policy-controller")
	target := t.TempDir()

	path, err := extractOpenShiftBinary(findOpenShiftComponent(t, "cluster-policy-controller"), root, target)
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(target, "cluster-policy-controller"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("target has %d entries, want only the binary", len(entries))
	}
}

func TestExtractOpenShiftBinaryMissingBinary(t *testing.T) {
	_, err := extractOpenShiftBinary(findOpenShiftComponent(t, "openshift-apiserver"), t.TempDir(), t.TempDir())
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !strings.Contains(err.Error(), "openshift-apiserver") {
		t.Fatalf("error = %v, want mention of openshift-apiserver", err)
	}
}
