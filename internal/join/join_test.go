package join

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/vkareh/coreshift/internal/rbac"
)

// newFakeClients returns a fake core client whose token minting answers
// a fixed token, and a fake dynamic client serving the machine resource.
func newFakeClients(t *testing.T) (*fake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "serviceaccounts/token", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "bootstrap-token"}}, nil
	})
	scheme := runtime.NewScheme()
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{machines: "MachineList"})
	return client, dynamicClient
}

// machine reads the machine stand-in the fake dynamic client holds.
func machine(t *testing.T, dynamicClient *dynamicfake.FakeDynamicClient, name string) *unstructured.Unstructured {
	t.Helper()
	object, err := dynamicClient.Resource(machines).Namespace(machineNamespace).Get(
		context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the %s machine stand-in: %v", name, err)
	}
	return object
}

// TestMintWritesJoinMaterial checks what Mint leaves behind: the node
// name, and a bootstrap kubeconfig carrying the cluster's server, CA,
// and the minted token.
func TestMintWritesJoinMaterial(t *testing.T) {
	client, dynamicClient := newFakeClients(t)
	dir := t.TempDir()
	const caPEM = "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"

	if err := mint(context.Background(), "https://127.0.0.1:6443", []byte(caPEM), client, dynamicClient, dir, "worker1"); err != nil {
		t.Fatal(err)
	}

	if got := string(readFile(t, dir, NodeNameFilename)); got != "worker1\n" {
		t.Fatalf("node name = %q, want the worker's name", got)
	}
	config, err := clientcmd.LoadFromFile(dir + "/" + BootstrapKubeconfigFilename)
	if err != nil {
		t.Fatal(err)
	}
	cluster := config.Clusters["coreshift"]
	if cluster.Server != "https://127.0.0.1:6443" {
		t.Fatalf("server = %q, want the cluster's endpoint", cluster.Server)
	}
	if string(cluster.CertificateAuthorityData) != caPEM {
		t.Fatalf("CA = %q, want the cluster's CA embedded", cluster.CertificateAuthorityData)
	}
	if token := config.AuthInfos[rbac.NodeBootstrapperAccount].Token; token != "bootstrap-token" {
		t.Fatalf("token = %q, want the minted token", token)
	}

	// The identity the token presents: the account the seeded role
	// binding grants the node-bootstrap permission to.
	if _, err := client.CoreV1().Namespaces().Get(context.Background(), rbac.NodeBootstrapperNamespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("bootstrap namespace: %v", err)
	}
	if _, err := client.CoreV1().ServiceAccounts(rbac.NodeBootstrapperNamespace).Get(context.Background(), rbac.NodeBootstrapperAccount, metav1.GetOptions{}); err != nil {
		t.Fatalf("bootstrap service account: %v", err)
	}
}

// TestMintShapesMachineStandIn checks the machine stand-in's shape: the
// worker's name as its internal DNS address, which is how the approver
// matches a node certificate request to a machine, and no node ref,
// which would tell the approver the machine already backs a node.
func TestMintShapesMachineStandIn(t *testing.T) {
	client, dynamicClient := newFakeClients(t)

	if err := mint(context.Background(), "https://127.0.0.1:6443", nil, client, dynamicClient, t.TempDir(), "worker1"); err != nil {
		t.Fatal(err)
	}

	object := machine(t, dynamicClient, "worker1")
	addresses, found, err := unstructured.NestedSlice(object.Object, "status", "addresses")
	if err != nil || !found || len(addresses) != 1 {
		t.Fatalf("machine stand-in addresses = %v, %v, want one", addresses, err)
	}
	want := map[string]interface{}{"type": "InternalDNS", "address": "worker1"}
	if fmt.Sprint(addresses[0]) != fmt.Sprint(want) {
		t.Fatalf("machine stand-in address = %v, want %v", addresses[0], want)
	}
	if ref, found, _ := unstructured.NestedMap(object.Object, "status", "nodeRef"); found || ref != nil {
		t.Fatalf("machine stand-in carries a node ref: %v", ref)
	}
}

// TestMintReplacesLeftovers checks the fresh-join rule: a node and
// machine a previous worker of the same name left behind are removed
// before the new pair is minted, so the approver sees a machine created
// now and a node that does not exist yet.
func TestMintReplacesLeftovers(t *testing.T) {
	client, dynamicClient := newFakeClients(t)
	leftoverNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker1"}}
	if _, err := client.CoreV1().Nodes().Create(context.Background(), leftoverNode, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	leftoverMachine := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": machines.GroupVersion().String(),
		"kind":       "Machine",
		"metadata":   map[string]interface{}{"name": "worker1", "namespace": machineNamespace},
	}}
	if _, err := dynamicClient.Resource(machines).Namespace(machineNamespace).Create(
		context.Background(), leftoverMachine, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := mint(context.Background(), "https://127.0.0.1:6443", nil, client, dynamicClient, t.TempDir(), "worker1"); err != nil {
		t.Fatal(err)
	}

	if _, err := client.CoreV1().Nodes().Get(context.Background(), "worker1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("leftover node survived: %v", err)
	}
	// The stand-in minted now is the one present, a fresh object rather
	// than the stale one.
	object := machine(t, dynamicClient, "worker1")
	if _, found, _ := unstructured.NestedSlice(object.Object, "status", "addresses"); !found {
		t.Fatal("machine stand-in carries no address; the stale one was kept")
	}
}

// TestRemoveDeletesPair checks the delete contract: the worker's node
// object and machine stand-in both go, and a missing pair is not an
// error.
func TestRemoveDeletesPair(t *testing.T) {
	client, dynamicClient := newFakeClients(t)
	if err := mint(context.Background(), "https://127.0.0.1:6443", nil, client, dynamicClient, t.TempDir(), "worker1"); err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker1"}}
	if _, err := client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := forgetWorker(context.Background(), client, dynamicClient, "worker1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Nodes().Get(context.Background(), "worker1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("node survived: %v", err)
	}
	if _, err := dynamicClient.Resource(machines).Namespace(machineNamespace).Get(
		context.Background(), "worker1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("machine stand-in survived: %v", err)
	}
	if err := forgetWorker(context.Background(), client, dynamicClient, "worker1"); err != nil {
		t.Fatalf("removing a missing pair is an error: %v", err)
	}
}

// TestClusterServer checks the endpoints and CA the bootstrap kubeconfig
// inherits from the admin kubeconfig.
func TestClusterServer(t *testing.T) {
	config := clientcmdapi.NewConfig()
	config.Clusters["coreshift"] = &clientcmdapi.Cluster{
		Server:                   "https://127.0.0.1:7443",
		CertificateAuthorityData: []byte("ca"),
	}
	config.Contexts["admin@coreshift"] = &clientcmdapi.Context{Cluster: "coreshift", AuthInfo: "admin"}
	config.CurrentContext = "admin@coreshift"

	server, caPEM, err := clusterServer(config)
	if err != nil {
		t.Fatal(err)
	}
	if server != "https://127.0.0.1:7443" || string(caPEM) != "ca" {
		t.Fatalf("server = %q, CA = %q, want the cluster's endpoint and CA", server, caPEM)
	}
}

// TestClusterServerRefusesMissingCA checks that a kubeconfig without an
// embedded CA is refused: the bootstrap kubeconfig must carry the CA
// with it into the container.
func TestClusterServerRefusesMissingCA(t *testing.T) {
	config := clientcmdapi.NewConfig()
	config.Clusters["coreshift"] = &clientcmdapi.Cluster{Server: "https://127.0.0.1:7443"}
	config.Contexts["admin@coreshift"] = &clientcmdapi.Context{Cluster: "coreshift", AuthInfo: "admin"}
	config.CurrentContext = "admin@coreshift"

	if _, _, err := clusterServer(config); err == nil || err.Error() != "admin kubeconfig carries no CA certificate" {
		t.Fatalf("err = %v, want the missing CA refused", err)
	}
}

func readFile(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
