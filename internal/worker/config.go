package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The node-side paths the configuration below references, fixed inside
// the worker container.
const (
	caPath            = "/etc/kubernetes/ca.crt"
	crioSocket        = "/var/run/crio/crio.sock"
	kubeletPluginDir  = "/var/lib/kubelet/volumeplugins"

	// The OpenShift CNI layout: network configuration the payload puts
	// under /etc/kubernetes/cni/net.d, and the plugin directory real
	// nodes have populated at runtime, with the plugins the image ships
	// under /usr/libexec/cni.
	cniConfigDir    = "/etc/kubernetes/cni/net.d"
	cniPluginDir    = "/var/lib/cni/bin"
	cniPluginSource = "/usr/libexec/cni"
)

// pauseImage is the payload's pod component, the pause image this
// release's nodes run infra containers from.
const pauseImage = "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:e5fc73fc66cd2ff4e781d9af380348be759b6917c3703325d340669c3764e136"

// cgroupRoot is the root of the cgroup hierarchy the container sees.
const cgroupRoot = "/sys/fs/cgroup"

// CgroupDelegationMark appears in the error enabling the cgroup
// controllers returns when the container's cgroup hierarchy was not
// delegated to it; the host-side create command recognizes it to name
// the rootful remedy.
const CgroupDelegationMark = "must be delegated to it"

// enableCgroupControllers makes the controllers kubelet's pod cgroups
// need available below the cgroup hierarchy root the container sees. The
// rules refuse enabling domain controllers on a cgroup that holds
// processes, so when the first attempt is refused, the processes sitting
// in the root are moved into a child cgroup and the attempt repeats. On a
// real node systemd has both done long before kubelet starts.
func enableCgroupControllers(root string) error {
	want := wantedControllers(root)
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte(want), 0o644); err == nil {
		return nil
	}
	if err := moveCgroupProcesses(root, "coreshift"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte(want), 0o644); err != nil {
		return fmt.Errorf("enable the cgroup controllers kubelet needs under %s: %w\n"+
			"the worker container's cgroup hierarchy "+CgroupDelegationMark, root, err)
	}
	return nil
}

// wantedControllers returns the subtree control line enabling the
// controllers kubelet's pod cgroups need, limited to the ones the
// hierarchy makes available.
func wantedControllers(root string) string {
	available, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return ""
	}
	have := strings.Fields(string(available))
	var want []string
	for _, controller := range []string{"cpu", "io", "memory", "pids"} {
		if slices.Contains(have, controller) {
			want = append(want, "+"+controller)
		}
	}
	return strings.Join(want, " ")
}

// moveCgroupProcesses moves the processes sitting in root's cgroup into
// the child cgroup name, which the rules require before domain
// controllers can be enabled on root.
func moveCgroupProcesses(root, name string) error {
	procs, err := os.ReadFile(filepath.Join(root, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("read the cgroup's processes: %w", err)
	}
	inner := filepath.Join(root, name)
	if err := os.MkdirAll(inner, 0o755); err != nil {
		return fmt.Errorf("create the %s cgroup: %w", name, err)
	}
	for _, pid := range strings.Fields(string(procs)) {
		if err := os.WriteFile(filepath.Join(inner, "cgroup.procs"), []byte(pid), 0o644); err != nil {
			return fmt.Errorf("move process %s into the %s cgroup: %w", pid, name, err)
		}
	}
	return nil
}

// writeCRIConfig writes CRI-O's configuration. It follows the payload's
// node runtime where the worker container shares the real node's
// expectations: crun as the default runtime, conmon where the image ships
// it, and the payload's pause image. The departures name the container,
// not a real node: there is no systemd to drive cgroups, no SELinux
// policy for containers to run under, and no permission for the
// nftables host port mapping, so those three run the container's way.
func writeCRIConfig(path string) error {
	config := fmt.Sprintf(`[crio.runtime]
conmon = "/usr/libexec/crio/conmon"
cgroup_manager = "cgroupfs"
default_runtime = "crun"
selinux = false
disable_hostport_mapping = true

[crio.network]
plugin_dirs = ["%s"]
network_dir = "%s"

[crio.image]
pause_image = "%s"
pause_command = "/usr/bin/pod"
`, cniPluginDir, cniConfigDir, pauseImage)
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		return fmt.Errorf("write CRI-O config: %w", err)
	}
	return nil
}

// writeKubeletConfig writes kubelet's component configuration. Like the
// CRI-O config it follows the payload where the container can and
// departs where it cannot: kubelet talks to CRI-O's socket, but drives
// cgroups itself because the container has no systemd, and tolerates the
// host's swap because that is not the node's choice to make. The worker
// container runs in a user namespace, so kubelet runs with its own flag
// for that environment, which skips the node kernel interfaces a user
// namespace denies. Certificate rotation stays off until something
// approves rotation requests.
func writeKubeletConfig(path string) error {
	config := fmt.Sprintf(`apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
authentication:
  anonymous:
    enabled: false
  x509:
    clientCAFile: %s
authorization:
  mode: Webhook
cgroupDriver: cgroupfs
containerRuntimeEndpoint: unix://%s
failSwapOn: false
featureGates:
  KubeletInUserNamespace: true
rotateCertificates: false
volumePluginDir: %s
`, caPath, crioSocket, kubeletPluginDir)
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		return fmt.Errorf("write kubelet config: %w", err)
	}
	return nil
}

// copyCNIPlugins copies the CNI plugins from src into dst.
func copyCNIPlugins(src, dst string) error {
	plugins, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read CNI plugins: %w", err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create CNI plugin directory: %w", err)
	}
	for _, plugin := range plugins {
		if !plugin.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, plugin.Name()))
		if err != nil {
			return fmt.Errorf("read CNI plugin %s: %w", plugin.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, plugin.Name()), data, 0o755); err != nil {
			return fmt.Errorf("copy CNI plugin %s: %w", plugin.Name(), err)
		}
	}
	return nil
}
