package image

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeHyperkubeBinaries(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(root, "usr", "bin", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtractKubeBinaries(t *testing.T) {
	root := t.TempDir()
	writeHyperkubeBinaries(t, root, "hyperkube", "kube-apiserver", "kube-controller-manager", "kube-scheduler")
	target := t.TempDir()

	binaries, err := extractKubeBinaries(root, target)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"kube-apiserver":          filepath.Join(target, "kube-apiserver"),
		"kube-controller-manager": filepath.Join(target, "kube-controller-manager"),
		"kube-scheduler":          filepath.Join(target, "kube-scheduler"),
	}
	if !reflect.DeepEqual(binaries, want) {
		t.Fatalf("binaries = %v, want %v", binaries, want)
	}
	for component, path := range want {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", component, err)
		}
		if string(content) != component {
			t.Fatalf("%s content = %q, want %q", component, content, component)
		}
	}
}

func TestExtractKubeBinariesMissingBinary(t *testing.T) {
	root := t.TempDir()
	writeHyperkubeBinaries(t, root, "kube-apiserver")

	_, err := extractKubeBinaries(root, t.TempDir())
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !strings.Contains(err.Error(), "kube-controller-manager") {
		t.Fatalf("error = %v, want mention of kube-controller-manager", err)
	}
}
