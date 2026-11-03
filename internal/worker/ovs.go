// The OVS bring-up a real node gets from its openvswitch and
// ovs-configuration services, done by the worker itself: OVS runs as
// supervised daemons, and the gateway bridge the payload's OVN node pod
// expects — br-ex, carrying the node's address on the default-route
// interface as its port — is configured before kubelet starts pods.
package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/vkareh/coreshift/internal/supervisor"
)

// NetworkIsolatedPath is where the host-side create command mounts a
// marker when the worker runs in a network namespace the tool owns: a
// cluster container's namespace in container mode, never the host's
// own. Only then may the OVN node pod's host networking reconfigure
// interfaces, because only there do those interfaces belong to the
// cluster instead of the developer's machine.
const NetworkIsolatedPath = "/etc/coreshift/network-isolated"

// The node's OVS layout, at the paths the payload's network pods mount
// from the node: the OVS daemon's own directories, and the OVN and CNI
// directories the pods share with the node runtime.
const (
	ovsDBPath     = "/etc/openvswitch/conf.db"
	ovsSchemaPath = "/usr/share/openvswitch/vswitch.ovsschema"
	ovsSocket     = "/var/run/openvswitch/db.sock"
	ovsRunDir     = "/var/run/openvswitch/"
	ovsDBServer   = "/usr/sbin/ovsdb-server"
	ovsVswitchd   = "/usr/sbin/ovs-vswitchd"
	ovsVsctl      = "/usr/bin/ovs-vsctl"
	ipTool        = "/usr/sbin/ip"
)

// gatewayBridge is the OVS bridge the payload's OVN node pod names in
// its command line, so the node side must provide it already carrying
// the node's address — the job a real node's ovs-configuration service
// does through NetworkManager.
const gatewayBridge = "br-ex"

// networkDirs are the node directories the payload's network pods
// mount, each at the exact path its hostPath volume names — the image
// carries /var/run and /run as separate trees, so the two spellings
// cannot stand in for each other. Real nodes have them from their OS
// and services; the pods' mounts resolve against the worker
// container's own tree, so the worker creates them before kubelet
// starts the pods that need them.
var networkDirs = []string{
	"/var/run/openvswitch",
	"/var/lib/openvswitch/data",
	"/var/lib/ovn-ic/etc",
	"/var/run/ovn-ic",
	"/run/ovn-kubernetes",
	"/var/run/multus/cni/net.d",
	"/run/netns",
	"/run/systemd/private",
	"/var/log/ovn",
	"/var/lib/cni/networks/ovn-k8s-cni-overlay",
}

// prepareNetworkDirs creates the directories the payload's network
// pods mount from the node, and the syslog socket their audit logging
// writes to, which a container has neither of until it makes them. The
// image's /var is empty — an ostree image fills it at boot, which a
// container never does — so everything below it that the node runtime
// and the pods touch is made here.
func prepareNetworkDirs() error {
	for _, dir := range networkDirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	// The real /var/tmp: the container runtime unpacks images through
	// it, with the sticky bit the standard location carries.
	if err := os.MkdirAll("/var/tmp", 0o1777); err != nil {
		return fmt.Errorf("create /var/tmp: %w", err)
	}
	// Real nodes point /dev/log at their syslog; the pods' ACL audit
	// logging writes there. A socket nothing reads keeps the writes
	// silent instead of failing.
	if _, err := os.Stat("/dev/log"); err != nil {
		if err := os.Symlink("/dev/null", "/dev/log"); err != nil {
			return fmt.Errorf("provide /dev/log: %w", err)
		}
	}
	return shareRootMount()
}

// shareRootMount makes the worker's root mount shared, the adjustment
// kind's node containers make at startup: the network pods mount the
// node's filesystem with host-to-container propagation, which a
// container's private root cannot serve.
func shareRootMount() error {
	if err := runCommand("/usr/bin/mount", "--make-rshared", "/"); err != nil {
		return err
	}
	return nil
}

// prepareOVS readies what the OVS daemons need: the openvswitch kernel
// module, loaded the way a real node's openvswitch service loads it —
// and loaded by the kernel itself when the container cannot, which a
// rootless worker cannot, having no CAP_SYS_MODULE — and the database
// file the database daemon serves.
func prepareOVS() error {
	_ = exec.Command("modprobe", "openvswitch").Run()
	if _, err := os.Stat(ovsDBPath); err == nil {
		return nil
	}
	return runCommand("/usr/bin/ovsdb-tool", "create", ovsDBPath, ovsSchemaPath)
}

// ovsProcesses returns the OVS daemons as supervised children, in the
// order they depend on each other: the database first, initialized with
// the system identity the OVN node pod's controller registers its
// chassis under, then the switch daemon, whose readiness hook wires the
// gateway bridge OVN configures.
func ovsProcesses() []supervisor.Process {
	return []supervisor.Process{
		{
			Name: "ovsdb-server",
			Path: ovsDBServer,
			Args: []string{
				ovsDBPath,
				"--remote=punix:" + ovsSocket,
				// The pid files are where the payload's OVN pods read
				// the daemons' identities from, the layout the
				// openvswitch service's own units use.
				"--pidfile=" + ovsRunDir + "ovsdb-server.pid",
				"-vconsole:info",
				"--no-chdir",
			},
			AfterStart: initOVS,
		},
		{
			Name: "ovs-vswitchd",
			Path: ovsVswitchd,
			Args: []string{
				"unix:" + ovsSocket,
				"--pidfile=" + ovsRunDir + "ovs-vswitchd.pid",
				"-vconsole:info",
				"--no-chdir",
			},
			AfterStart: configureGateway,
		},
	}
}

// initOVS initializes the OVS database with the system identity a real
// node's openvswitch service records, the identity OVN registers the
// node's chassis under. The command runs before the switch daemon
// exists, so it takes the no-wait form the service's own script uses,
// recording the identity without waiting for the switch to apply it.
func initOVS(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ovsReadyTimeout)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 200*time.Millisecond, true, func(_ context.Context) (bool, error) {
		_, err := os.Stat(ovsSocket)
		return err == nil, nil
	})
	if err != nil {
		return fmt.Errorf("wait for OVS's database socket: %w", ctx.Err())
	}
	return ovsCommand("--no-wait", "--", "init", "--", "set", "Open_vSwitch", ".",
		"external-ids:system-id="+string(uuid.NewUUID()),
		"external-ids:rundir=/var/run/openvswitch")
}

// ovsCommandTimeout bounds every ovs-vsctl call in seconds, so a
// control command against a wedged daemon fails in seconds instead of
// hanging the worker's startup.
const ovsCommandTimeout = "15"

// ovsReadyTimeout bounds the waits for the OVS daemons' sockets.
const ovsReadyTimeout = 25 * time.Second

// ovsCommand runs ovs-vsctl with a timeout, reporting the command with
// its output on failure.
func ovsCommand(args ...string) error {
	return runCommand(ovsVsctl, append([]string{"--timeout=" + ovsCommandTimeout}, args...)...)
}

// runCommand runs a command, reporting its output on failure.
func runCommand(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("run %s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// configureGateway takes the node's default-route interface over for
// OVN, the way a real node's ovs-configuration service does: the
// gateway bridge is created with that interface as its port, and the
// interface's addresses and default route move onto the bridge, so
// traffic keeps flowing while OVN owns the gateway. A bridge a previous
// worker left behind is undone first, because this worker's own OVS
// database starts empty and cannot adopt what it does not know.
func configureGateway(ctx context.Context) error {
	if linkExists(gatewayBridge) {
		if err := undoGateway(); err != nil {
			return err
		}
	}
	iface, gateway, metric, err := defaultRoute()
	if err != nil {
		return err
	}
	// No waiting on the bridge itself: on the first worker the kernel
	// module still has to load, and the wait below is the one that
	// proves the device appeared.
	if err := ovsCommand("--no-wait", "--may-exist", "add-br", gatewayBridge); err != nil {
		return err
	}
	// The bridge's kernel device appears only once the openvswitch
	// module is in: a privileged worker loads it with modprobe, and the
	// kernel loads it on demand where that was refused. Name the module
	// when it never appears, instead of letting later failure hint at
	// it.
	waitCtx, cancel := context.WithTimeout(ctx, ovsReadyTimeout)
	defer cancel()
	err = wait.PollUntilContextCancel(waitCtx, 500*time.Millisecond, true, func(_ context.Context) (bool, error) {
		return linkExists(gatewayBridge), nil
	})
	if err != nil {
		return fmt.Errorf("wait for the %s bridge's device: the openvswitch kernel module could not be loaded", gatewayBridge)
	}
	if err := ovsCommand("--may-exist", "add-port", gatewayBridge, iface); err != nil {
		return err
	}
	// The uplink is recorded the way a real node's network configuration
	// does: the interface's type set to system — what NetworkManager
	// sets on the ports of its bridges — and the bridge's uplink
	// external-id naming it. OVN reads either to find the port the
	// gateway bridge fronts.
	if err := ovsCommand("set", "interface", iface, "type=system"); err != nil {
		return err
	}
	if err := ovsCommand("set", "bridge", gatewayBridge, "external-ids:bridge-uplink="+iface); err != nil {
		return err
	}
	mtu, err := linkMTU(iface)
	if err != nil {
		return err
	}
	if err := ipCommand("link", "set", gatewayBridge, "up"); err != nil {
		return err
	}
	if err := ipCommand("link", "set", "dev", gatewayBridge, "mtu", mtu); err != nil {
		return err
	}
	if err := moveAddresses(iface, gatewayBridge); err != nil {
		return err
	}
	return replaceDefaultRoute(gateway, gatewayBridge, metric)
}

// undoGateway reverses a previous worker's takeover, so the fresh one
// below starts from a plain network. The leftover bridge is not in this
// worker's own OVS database, so it is adopted into it first and then
// deleted through the daemon — the only clean way to remove an OVS
// device — with the addresses and route restored to the uplink the
// moment the bridge lets them go.
func undoGateway() error {
	addresses, _ := interfaceAddresses(gatewayBridge)
	_, gateway, metric, _ := defaultRoute()
	if err := ovsCommand("--may-exist", "add-br", gatewayBridge); err != nil {
		return err
	}
	if err := ovsCommand("--if-exists", "del-br", gatewayBridge); err != nil {
		return err
	}
	uplink, err := uplinkInterface()
	if err != nil {
		return err
	}
	for _, address := range addresses {
		if err := ipCommand("addr", "add", address, "dev", uplink); err != nil {
			return err
		}
	}
	if gateway == "" {
		return nil
	}
	return replaceDefaultRoute(gateway, uplink, metric)
}

// uplinkInterface returns the namespace's virtual ethernet link, the
// interface the gateway bridge takes over: a container's namespace
// carries exactly one, the pair end the container runtime made for it.
func uplinkInterface() (string, error) {
	out, err := exec.Command(ipTool, "-o", "link", "show").Output()
	if err != nil {
		return "", fmt.Errorf("read the namespace's links: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.Contains(fields[1], "@") {
			return strings.Split(fields[1], "@")[0], nil
		}
	}
	return "", fmt.Errorf("the namespace carries no virtual ethernet link to take over")
}

// defaultRoute returns the interface, gateway, and metric of the node's
// default route — the interface OVN takes over, and the route that must
// follow its addresses onto the bridge.
func defaultRoute() (iface, gateway, metric string, err error) {
	out, err := exec.Command(ipTool, "route", "show", "default").Output()
	if err != nil {
		return "", "", "", fmt.Errorf("read the default route: %w", err)
	}
	fields := strings.Fields(string(out))
	for i, field := range fields {
		switch field {
		case "via":
			if i+1 < len(fields) {
				gateway = fields[i+1]
			}
		case "dev":
			if i+1 < len(fields) {
				iface = fields[i+1]
			}
		case "metric":
			if i+1 < len(fields) {
				metric = fields[i+1]
			}
		}
	}
	if iface == "" || gateway == "" {
		return "", "", "", fmt.Errorf("the default route carries no interface or gateway: %s", strings.TrimSpace(string(out)))
	}
	return iface, gateway, metric, nil
}

// replaceDefaultRoute points the default route at dev, keeping the
// gateway and metric it had.
func replaceDefaultRoute(gateway, dev, metric string) error {
	args := []string{"route", "replace", "default", "via", gateway, "dev", dev}
	if metric != "" {
		args = append(args, "metric", metric)
	}
	return ipCommand(args...)
}

// interfaceAddresses returns the global addresses on dev, the ones that
// move with the takeover.
func interfaceAddresses(dev string) ([]string, error) {
	out, err := exec.Command(ipTool, "-o", "addr", "show", "dev", dev, "scope", "global").Output()
	if err != nil {
		return nil, fmt.Errorf("read %s's addresses: %w", dev, err)
	}
	var addresses []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && (fields[2] == "inet" || fields[2] == "inet6") {
			addresses = append(addresses, fields[3])
		}
	}
	return addresses, nil
}

// moveAddresses moves every global address from one device to another.
func moveAddresses(from, to string) error {
	addresses, err := interfaceAddresses(from)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		if err := ipCommand("addr", "del", address, "dev", from); err != nil {
			return err
		}
		if err := ipCommand("addr", "add", address, "dev", to); err != nil {
			return err
		}
	}
	return nil
}

// linkExists reports whether a network device of that name exists.
func linkExists(name string) bool {
	return exec.Command(ipTool, "link", "show", "dev", name).Run() == nil
}

// linkMTU returns a device's MTU, which the bridge inherits so large
// packets do not blackhole behind it.
func linkMTU(name string) (string, error) {
	out, err := exec.Command(ipTool, "-o", "link", "show", "dev", name).Output()
	if err != nil {
		return "", fmt.Errorf("read %s's MTU: %w", name, err)
	}
	fields := strings.Fields(string(out))
	for i, field := range fields {
		if field == "mtu" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("read %s's MTU: the report carries none:\n%s", name, strings.TrimSpace(string(out)))
}

// ipCommand runs ip, reporting the command with its output on failure.
func ipCommand(args ...string) error {
	return runCommand(ipTool, args...)
}
