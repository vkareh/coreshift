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

// writeCgroupRoot fills a fake cgroup hierarchy root: the controllers it
// makes available and the processes sitting in it.
func writeCgroupRoot(t *testing.T, controllers string, procs ...string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte(controllers), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if len(procs) > 0 {
		if err := os.WriteFile(filepath.Join(root, "cgroup.procs"), []byte(strings.Join(procs, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestEnableCgroupControllers checks the subtree control line written for
// the controllers kubelet needs, limited to what the hierarchy makes
// available.
func TestEnableCgroupControllers(t *testing.T) {
	root := writeCgroupRoot(t, "cpuset cpu io memory pids hugetlb")
	if err := enableCgroupControllers(root); err != nil {
		t.Fatal(err)
	}
	control := string(readFile(t, filepath.Join(root, "cgroup.subtree_control")))
	if control != "+cpu +io +memory +pids" {
		t.Fatalf("subtree control = %q, want the kubelet controllers", control)
	}

	root = writeCgroupRoot(t, "cpu pids")
	if err := enableCgroupControllers(root); err != nil {
		t.Fatal(err)
	}
	control = string(readFile(t, filepath.Join(root, "cgroup.subtree_control")))
	if control != "+cpu +pids" {
		t.Fatalf("subtree control = %q, want only the available controllers", control)
	}
}

// TestMoveCgroupProcesses checks that the processes sitting in a cgroup
// root are moved into the child cgroup the move targets.
func TestMoveCgroupProcesses(t *testing.T) {
	root := writeCgroupRoot(t, "cpu pids", "1", "2")

	if err := moveCgroupProcesses(root, "coreshift"); err != nil {
		t.Fatal(err)
	}
	moved, err := os.ReadDir(filepath.Join(root, "coreshift"))
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) == 0 {
		t.Fatal("the target cgroup was not created")
	}
	// The fake writes each move over the same file, so the last process
	// moved is the one recorded; both were accepted without error.
	if got := strings.TrimSpace(string(readFile(t, filepath.Join(root, "coreshift", "cgroup.procs")))); got != "2" {
		t.Fatalf("cgroup.procs = %q, want the moved process recorded", got)
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
