package worker

import (
	"net"
	"strconv"
	"testing"
)

// swapKubeletPortStarts moves the port scans onto an ephemeral base for
// the test's duration, so the tests never depend on what runs on the
// machine, and returns the base.
func swapKubeletPortStarts(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	oldServer, oldHealthz := kubeletServerPort, kubeletHealthzPort
	// The same spacing the real defaults have: the health port two
	// below the server port.
	kubeletServerPort, kubeletHealthzPort = base, base-2
	t.Cleanup(func() { kubeletServerPort, kubeletHealthzPort = oldServer, oldHealthz })
	return base
}

// holdPorts binds one listener per given address and port for the test's
// duration, the way a running worker's kubelet holds its ports.
func holdPorts(t *testing.T, ports ...string) {
	t.Helper()
	for _, address := range ports {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("hold %s: %v", address, err)
		}
		t.Cleanup(func() { listener.Close() })
	}
}

// TestKubeletPortsTakeDefaults checks that a lone worker's kubelet keeps
// the ports a real node uses.
func TestKubeletPortsTakeDefaults(t *testing.T) {
	base := swapKubeletPortStarts(t)

	server, healthz, err := kubeletPorts()
	if err != nil {
		t.Fatal(err)
	}
	if server != base || healthz != base-2 {
		t.Fatalf("ports = %d, %d, want the scan starts %d, %d", server, healthz, base, base-2)
	}
}

// TestKubeletPortsAvoidTakenAndEachOther checks the shared-network rule:
// with two workers' kubelets holding the first ports, a third takes the
// next free pair, and its two ports differ from each other, which the
// scans cannot see on their own because nothing binds the pair until
// kubelet starts.
func TestKubeletPortsAvoidTakenAndEachOther(t *testing.T) {
	base := swapKubeletPortStarts(t)
	holdPorts(t,
		"127.0.0.1:"+strconv.Itoa(base-2),
		"127.0.0.1:"+strconv.Itoa(base-1),
		"0.0.0.0:"+strconv.Itoa(base),
		"0.0.0.0:"+strconv.Itoa(base+1),
	)

	server, healthz, err := kubeletPorts()
	if err != nil {
		t.Fatal(err)
	}
	if server != base+2 {
		t.Fatalf("server port = %d, want %d", server, base+2)
	}
	// The health scan from its start reaches the server port this worker
	// just took and must skip past it rather than land on it.
	if healthz != base+3 {
		t.Fatalf("health port = %d, want %d, past the taken pair and the server port", healthz, base+3)
	}
}
