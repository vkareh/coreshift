package networkconfig

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

// newClient returns a fake serving the resources the seed writes, with
// any objects already in place.
func newClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "config.openshift.io", Version: "v1", Resource: "networks"}: "NetworkList",
			{Version: "v1", Resource: "configmaps"}:                             "ConfigMapList",
		},
		objects...,
	)
}

// object returns a parsed manifest, for objects that already exist.
func object(t *testing.T, manifest string) *unstructured.Unstructured {
	t.Helper()
	parsed := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}

// readNetwork returns the cluster network configuration.
func readNetwork(t *testing.T, client *dynamicfake.FakeDynamicClient) *unstructured.Unstructured {
	t.Helper()
	network, err := client.Resource(networks).Get(context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the cluster network: %v", err)
	}
	return network
}

// readProbedMTU returns the probed MTU record.
func readProbedMTU(t *testing.T, client *dynamicfake.FakeDynamicClient) *unstructured.Unstructured {
	t.Helper()
	record, err := client.Resource(configMaps).Namespace(probedMTUNamespace).Get(context.Background(), probedMTUName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the probed MTU record: %v", err)
	}
	return record
}

// checkSpec fails unless the network carries the ranges and network
// type the control plane runs with.
func checkSpec(t *testing.T, network *unstructured.Unstructured) {
	t.Helper()
	if networkType, _, _ := unstructured.NestedString(network.Object, "spec", "networkType"); networkType != "OVNKubernetes" {
		t.Fatalf("network type = %q, want OVNKubernetes", networkType)
	}
	if service, _, _ := unstructured.NestedStringSlice(network.Object, "spec", "serviceNetwork"); len(service) != 1 || service[0] != serviceNetwork {
		t.Fatalf("service network = %v, want [%s]", service, serviceNetwork)
	}
	cluster, _, _ := unstructured.NestedSlice(network.Object, "spec", "clusterNetwork")
	if len(cluster) != 1 {
		t.Fatalf("cluster network = %v, want one entry", cluster)
	}
	entry, _ := cluster[0].(map[string]any)
	cidr, _, _ := unstructured.NestedString(entry, "cidr")
	hostPrefix, _, _ := unstructured.NestedInt64(entry, "hostPrefix")
	if cidr != clusterNetwork || hostPrefix != clusterHostPrefix {
		t.Fatalf("cluster network entry = %v, want %s with host prefix %d", entry, clusterNetwork, clusterHostPrefix)
	}
}

// TestSeedCreatesTheConfiguration checks a fresh boot: both objects the
// operator reads exist afterwards, carrying what the control plane
// runs with.
func TestSeedCreatesTheConfiguration(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := newClient()
	if err := seedWithRetry(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	checkSpec(t, readNetwork(t, client))
	mtu, _, _ := unstructured.NestedString(readProbedMTU(t, client).Object, "data", "mtu")
	if mtu != probedMTU {
		t.Fatalf("probed MTU = %q, want %s", mtu, probedMTU)
	}
}

// TestSeedFillsThePayloadShell checks the shape the version operator
// delivers — a create-only network object with an empty spec — is
// filled in with the ranges rather than replaced.
func TestSeedFillsThePayloadShell(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	shell := object(t, `apiVersion: config.openshift.io/v1
kind: Network
metadata:
  name: cluster
  annotations:
    release.openshift.io/create-only: "true"
spec: {}
`)
	client := newClient(shell)
	if err := seedWithRetry(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	network := readNetwork(t, client)
	checkSpec(t, network)
	if network.GetAnnotations()["release.openshift.io/create-only"] != "true" {
		t.Fatalf("payload annotations = %v, want the shell's kept", network.GetAnnotations())
	}
}

// TestSeedLeavesConfiguredNetworkAlone checks a warm boot: a network
// that already carries its configuration, and the status the operator
// wrote on it, survive the seed untouched.
func TestSeedLeavesConfiguredNetworkAlone(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	configured := object(t, `apiVersion: config.openshift.io/v1
kind: Network
metadata:
  name: cluster
spec:
  networkType: OVNKubernetes
status:
  networkType: OVNKubernetes
`)
	client := newClient(configured)
	if err := seedWithRetry(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	network := readNetwork(t, client)
	if got, _, _ := unstructured.NestedStringSlice(network.Object, "spec", "serviceNetwork"); len(got) != 0 {
		t.Fatalf("configured network's spec was rewritten: %v", network.Object["spec"])
	}
	if _, found, _ := unstructured.NestedString(network.Object, "status", "networkType"); !found {
		t.Fatal("configured network's status was dropped")
	}
}

// TestSeedKeepsTheProbersRecord checks an existing probed MTU record,
// the prober's own output, is never overwritten.
func TestSeedKeepsTheProbersRecord(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	record := object(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: mtu
  namespace: openshift-network-operator
data:
  mtu: "9001"
`)
	client := newClient(record)
	if err := seedWithRetry(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	if mtu, _, _ := unstructured.NestedString(readProbedMTU(t, client).Object, "data", "mtu"); mtu != "9001" {
		t.Fatalf("probed MTU = %q, want the prober's own record kept", mtu)
	}
}

// TestSeedRetriesUntilDelivery checks the seed tolerates the delivery
// window the version operator needs: writes that fail because the
// definitions or namespaces have not landed yet are retried until they
// succeed.
func TestSeedRetriesUntilDelivery(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := newClient()
	var missing atomic.Bool
	missing.Store(true)
	client.PrependReactor("create", "networks", func(action ktesting.Action) (bool, runtime.Object, error) {
		if !missing.Load() {
			return false, nil, nil
		}
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "config.openshift.io", Resource: "networks"}, "cluster")
	})
	delivered := make(chan struct{})
	go func() {
		_ = seedWithRetry(context.Background(), client)
		close(delivered)
	}()
	select {
	case <-delivered:
		t.Fatal("seed finished while the network API was still missing")
	case <-time.After(20 * time.Millisecond):
	}
	missing.Store(false)
	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("seed did not finish after delivery")
	}
	checkSpec(t, readNetwork(t, client))
}

// TestEnsureNetworkRaceWithDelivery checks a create that races the
// version operator's own delivery of the shell — landing second, on an
// object that appeared in between — still ends with the ranges in
// place.
func TestEnsureNetworkRaceWithDelivery(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	shell := object(t, `apiVersion: config.openshift.io/v1
kind: Network
metadata:
  name: cluster
spec: {}
`)
	client := newClient()
	client.PrependReactor("create", "networks", func(action ktesting.Action) (bool, runtime.Object, error) {
		// The version operator delivers its empty shell while the
		// seed's create is in flight; the create lands second.
		if err := client.Tracker().Add(shell); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "config.openshift.io", Resource: "networks"}, "cluster")
	})

	if err := seedWithRetry(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	checkSpec(t, readNetwork(t, client))
}

// TestSeedReportsWriteFailure checks a write failure the delivery
// cannot explain surfaces instead of retrying forever.
func TestSeedReportsWriteFailure(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	client := newClient()
	client.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected failure")
	})

	err := seedWithRetry(ctx, client)
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("error = %v, want the injected failure named", err)
	}
}
