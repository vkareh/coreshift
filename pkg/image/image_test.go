package image

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func TestUnpackLayerDotSlashEntries(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	entries := []struct {
		name string
		body string
	}{
		{name: "./"},
		{name: "./usr/"},
		{name: "./usr/bin/"},
		{name: "./usr/bin/etcd", body: "binary"},
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o755}
		if e.body != "" {
			hdr.Mode = 0o644
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := unpackLayer(layer, destination); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(destination, "usr", "bin", "etcd"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "binary" {
		t.Fatalf("extracted content = %q, want %q", got, "binary")
	}
}

func TestUnpackLayerRejectsDotSlashEscape(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "./../etc/passwd", Mode: 0o644, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := unpackLayer(layer, t.TempDir()); err == nil {
		t.Fatal("expected path escape error")
	}
}
