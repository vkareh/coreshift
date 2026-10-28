package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteCRIConfig checks the CRI-O configuration against what the
// payload's node runtime expects, so the settings cannot drift from the
// verified ones.
func TestWriteCRIConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crio.conf")
	if err := writeCRIConfig(path); err != nil {
		t.Fatal(err)
	}
	config := string(readFile(t, path))
	for _, want := range []string{
		`conmon = "/usr/libexec/crio/conmon"`,
		`cgroup_manager = "cgroupfs"`,
		`default_runtime = "crun"`,
		`plugin_dirs = ["` + cniPluginDir + `"]`,
		`network_dir = "` + cniConfigDir + `"`,
		`pause_image = "` + pauseImage + `"`,
		`pause_command = "/usr/bin/pod"`,
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("CRI-O config missing %q:\n%s", want, config)
		}
	}
}

// TestWriteKubeletConfig checks the kubelet configuration against what
// the payload's kubelet expects, so the settings cannot drift from the
// verified ones.
func TestWriteKubeletConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubelet.conf")
	if err := writeKubeletConfig(path); err != nil {
		t.Fatal(err)
	}
	config := string(readFile(t, path))
	for _, want := range []string{
		"clientCAFile: " + caPath,
		"cgroupDriver: cgroupfs",
		"containerRuntimeEndpoint: unix://" + crioSocket,
		"failSwapOn: false",
		"volumePluginDir: " + kubeletPluginDir,
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("kubelet config missing %q:\n%s", want, config)
		}
	}
}

// TestCopyCNIPlugins checks that the plugins the image ships are copied
// into the directory the CRI-O configuration points at, executably, and
// that anything else in the source directory is left behind.
func TestCopyCNIPlugins(t *testing.T) {
	src := t.TempDir()
	for _, name := range []string{"bridge", "host-local"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(src, "not-a-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "bin")

	if err := copyCNIPlugins(src, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bridge", "host-local"} {
		info, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("plugin %s was not copied: %v", name, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("plugin %s is not executable", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "not-a-plugin")); !os.IsNotExist(err) {
		t.Fatal("a directory was copied as a plugin")
	}
}

func TestCopyCNIPluginsRefusesMissingSource(t *testing.T) {
	err := copyCNIPlugins(filepath.Join(t.TempDir(), "absent"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "read CNI plugins") {
		t.Fatalf("err = %v, want the missing source refused", err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
