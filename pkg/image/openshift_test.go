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

func TestExtractOpenShiftBinaryLeavesLibrariesBehind(t *testing.T) {
	root := t.TempDir()
	writeImageFiles(t, root,
		"/usr/bin/openshift-controller-manager",
		"/usr/lib64/libgpgme.so.11",
	)
	target := t.TempDir()

	if _, err := extractOpenShiftBinary(findOpenShiftComponent(t, "openshift-controller-manager"), root, target); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "openshift-controller-manager" {
		t.Fatalf("target has %v, want only the binary; the gpgme libraries are embedded in the binary", entries)
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

func TestSelectOpenShiftComponents(t *testing.T) {
	selected, err := selectOpenShiftComponents("openshift-apiserver", "oauth-apiserver")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0].component != "openshift-apiserver" || selected[1].component != "oauth-apiserver" {
		t.Fatalf("selected = %v, want openshift-apiserver and oauth-apiserver in table order", selected)
	}

	if selected, err = selectOpenShiftComponents(); err != nil {
		t.Fatal(err)
	}
	if len(selected) != len(openshiftComponents) {
		t.Fatalf("selected %d components with no filter, want all %d", len(selected), len(openshiftComponents))
	}

	_, err = selectOpenShiftComponents("openshift-apiserver", "hyperkube")
	if err == nil {
		t.Fatal("expected error for unknown component")
	}
	if !strings.Contains(err.Error(), "hyperkube") {
		t.Fatalf("error = %v, want mention of hyperkube", err)
	}
}

func TestSelectMachineApproverBinary(t *testing.T) {
	selected, err := selectOpenShiftComponents("cluster-machine-approver")
	if err != nil {
		t.Fatal(err)
	}
	// The component image names its binary machine-approver, the name its
	// deployment runs it by.
	if selected[0].path != "/usr/bin/machine-approver" {
		t.Fatalf("cluster-machine-approver path = %q, want /usr/bin/machine-approver", selected[0].path)
	}
}

func TestSelectClusterVersionOperatorBinary(t *testing.T) {
	selected, err := selectOpenShiftComponents("cluster-version-operator")
	if err != nil {
		t.Fatal(err)
	}
	// The release image runs this binary as its own entrypoint, so the
	// component image carries it under its full name.
	if selected[0].path != "/usr/bin/cluster-version-operator" {
		t.Fatalf("cluster-version-operator path = %q, want /usr/bin/cluster-version-operator", selected[0].path)
	}
}

func TestSelectClusterNetworkOperatorBinary(t *testing.T) {
	selected, err := selectOpenShiftComponents("cluster-network-operator")
	if err != nil {
		t.Fatal(err)
	}
	// The release image runs this binary as its own entrypoint, and it
	// renders its workloads from the bindata directory beside it.
	if selected[0].path != "/usr/bin/cluster-network-operator" {
		t.Fatalf("cluster-network-operator path = %q, want /usr/bin/cluster-network-operator", selected[0].path)
	}
	if len(selected[0].dirs) != 1 || selected[0].dirs[0] != "/bindata" {
		t.Fatalf("cluster-network-operator dirs = %v, want /bindata", selected[0].dirs)
	}
}

func TestCopyOpenShiftDirs(t *testing.T) {
	root := t.TempDir()
	writeImageFiles(t, root, "/bindata/network/ovn-kubernetes/000-ns.yaml")
	target := t.TempDir()

	if err := copyOpenShiftDirs(findOpenShiftComponent(t, "cluster-network-operator"), root, target); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(target, "bindata", "network", "ovn-kubernetes", "000-ns.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("bindata file copied empty")
	}

	noDirs := findOpenShiftComponent(t, "openshift-apiserver")
	if err := copyOpenShiftDirs(noDirs, root, target); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "bindata" {
		t.Fatalf("target has %v, want only the copied directory", entries)
	}
}
