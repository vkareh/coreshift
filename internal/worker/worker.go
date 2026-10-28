// Package worker joins a control plane from inside a worker container:
// it turns the mounted-in bootstrap material into a node identity, writes
// the node-side configuration the node binaries expect, and runs CRI-O
// and kubelet under the supervisor until the container stops.
package worker

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/vkareh/coreshift/internal/supervisor"
)

// The bootstrap material the host-side create command mounts into the
// worker container, and the files the entrypoint writes that the node
// binaries read. The command takes its configuration from these files,
// never from flags.
const (
	// BootstrapKubeconfigPath is where the worker container reads the
	// join identity from; the host-side create command mounts its minted
	// file here.
	BootstrapKubeconfigPath = "/etc/kubernetes/bootstrap-kubeconfig"
	// NodeNamePath is where the worker container reads the name it joins
	// under; the host-side create command mounts its minted file here.
	NodeNamePath = "/etc/coreshift/node-name"

	nodeKubeconfigPath = "/etc/kubernetes/kubeconfig"
	crioConfigPath     = "/etc/crio/crio.conf"
	kubeletConfigPath  = "/etc/kubernetes/kubelet.conf"
)

// The node binaries, at the paths the payload's rhel-coreos image ships
// them.
const (
	crioPath    = "/usr/bin/crio"
	kubeletPath = "/usr/bin/kubelet"
)

// logDir holds the supervisor's per-child logs inside the container.
const logDir = "/var/log/coreshift"

// The ports kubelet serves on, where the scan for a free pair starts:
// the defaults real nodes use. Variables for test override.
var (
	kubeletServerPort  = 10250
	kubeletHealthzPort = 10248
)

// kubeletPorts picks the ports kubelet serves on. Workers share the host
// network, so two workers' kubelets cannot both take the defaults, and a
// worker's own two ports must differ too; each worker takes the first
// free pair from the defaults, keeping a single worker on the ports a
// real node uses.
func kubeletPorts() (server, healthz int, err error) {
	server, err = freePort("0.0.0.0", kubeletServerPort)
	if err != nil {
		return 0, 0, err
	}
	// The health port scans from its own default, skipping the server
	// port this worker just took: nothing binds the pair until kubelet
	// starts, so the scans cannot see each other any other way.
	healthz, err = freePort("127.0.0.1", kubeletHealthzPort)
	for err == nil && healthz == server {
		healthz, err = freePort("127.0.0.1", healthz+1)
	}
	return server, healthz, err
}

// freePort returns the first port from start a listener on address can
// take, so the kubelet can take it right after.
func freePort(address string, start int) (int, error) {
	const attempts = 100
	for port := start; port < start+attempts; port++ {
		listener, err := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(port)))
		if err == nil {
			listener.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free port for %s from %d up %d", address, start, attempts)
}

// nodeRegisterTimeout bounds the wait for the node's object to appear
// against the control plane.
const nodeRegisterTimeout = 2 * time.Minute

// crioReadyTimeout bounds the wait for CRI-O's socket.
const crioReadyTimeout = time.Minute

// Run joins the control plane and runs the node side until shutdown is
// requested.
func Run(ctx context.Context) error {
	nodeName, err := readNodeName(NodeNamePath)
	if err != nil {
		return err
	}
	if err := join(ctx, nodeName); err != nil {
		return err
	}
	if err := writeCRIConfig(crioConfigPath); err != nil {
		return err
	}
	if err := writeKubeletConfig(kubeletConfigPath); err != nil {
		return err
	}
	if err := os.MkdirAll(cniConfigDir, 0o755); err != nil {
		return fmt.Errorf("create CNI network directory: %w", err)
	}
	if err := copyCNIPlugins(cniPluginSource, cniPluginDir); err != nil {
		return err
	}
	if err := enableCgroupControllers(cgroupRoot); err != nil {
		return err
	}
	serverPort, healthzPort, err := kubeletPorts()
	if err != nil {
		return err
	}

	processes := []supervisor.Process{
		{
			Name: "crio",
			Path: crioPath,
			Args: []string{"--config=" + crioConfigPath},
			// kubelet fails at startup unless the runtime answers. Real
			// nodes order kubelet after CRI-O through systemd; the same
			// ordering happens here by waiting for CRI-O's socket before
			// kubelet starts.
			AfterStart: waitCRIO,
		},
		{
			Name: "kubelet",
			Path: kubeletPath,
			Args: []string{
				"--config=" + kubeletConfigPath,
				"--kubeconfig=" + nodeKubeconfigPath,
				// The payload's kubelet unit names the node this way;
				// the certificate the join requested names it the same.
				"--hostname-override=" + nodeName,
				// The ports this worker's kubelet serves on, taken as a
				// free pair because workers share the host network.
				"--port=" + strconv.Itoa(serverPort),
				"--healthz-port=" + strconv.Itoa(healthzPort),
			},
		},
	}
	sup := &supervisor.Supervisor{
		LogDir: logDir,
		AfterStart: func(ctx context.Context) error {
			return waitNodeRegistered(ctx, nodeName)
		},
	}
	return sup.Run(ctx, processes)
}

// waitCRIO waits until CRI-O's socket exists, so kubelet starts only
// against a runtime that answers.
func waitCRIO(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, crioReadyTimeout)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, apiPollInterval, true, func(ctx context.Context) (bool, error) {
		_, err := os.Stat(crioSocket)
		return err == nil, nil
	})
	if err != nil {
		return fmt.Errorf("wait for CRI-O's socket: %w", ctx.Err())
	}
	return nil
}

// join turns the mounted-in bootstrap material into a node identity: it
// requests a client certificate for the node through the bootstrap
// kubeconfig, waits until the request is approved and issued, and writes
// the node kubeconfig plus the CA the node-side configuration references.
// The approval wait is deliberately unbounded: approval arrives from
// outside the worker, from an operator for now and the machine approver
// once it runs in the control plane.
func join(ctx context.Context, nodeName string) error {
	restConfig, err := clientcmd.BuildConfigFromFlags("", BootstrapKubeconfigPath)
	if err != nil {
		return fmt.Errorf("read bootstrap kubeconfig: %w", err)
	}
	bootstrap, err := clientcmd.LoadFromFile(BootstrapKubeconfigPath)
	if err != nil {
		return fmt.Errorf("read bootstrap kubeconfig: %w", err)
	}
	server, caPEM, err := clusterInfo(bootstrap)
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create bootstrap client: %w", err)
	}
	certPEM, keyPEM, err := requestCertificate(ctx, client, nodeName)
	if err != nil {
		return err
	}
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		return fmt.Errorf("write CA certificate: %w", err)
	}
	return writeNodeKubeconfig(nodeKubeconfigPath, server, nodeName, caPEM, certPEM, keyPEM)
}

// waitNodeRegistered waits until the node's object appears against the
// control plane, so a join that never registers fails the run instead of
// looking healthy.
func waitNodeRegistered(ctx context.Context, nodeName string) error {
	config, err := clientcmd.BuildConfigFromFlags("", nodeKubeconfigPath)
	if err != nil {
		return fmt.Errorf("read node kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create node client: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, nodeRegisterTimeout)
	defer cancel()
	fmt.Println("Waiting for the node to register")
	pollErr := wait.PollUntilContextCancel(ctx, apiPollInterval, true, func(ctx context.Context) (bool, error) {
		_, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return err == nil, nil
	})
	if pollErr != nil {
		return fmt.Errorf("wait for the node to register: %w", ctx.Err())
	}
	fmt.Println("Node registered; it reports NotReady until a CNI exists")
	return nil
}
