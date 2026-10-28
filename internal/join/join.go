// Package join mints the material a worker needs to join a control
// plane: the bootstrap identity a real OpenShift node presents (a service
// account bound to the standard node-bootstrap roles, with a token minted
// for it) and the machine stand-in the machine approver matches node
// certificate requests against. The control plane runs the real join
// path, so the material has the shape that path expects, and deleting a
// worker removes the pair again.
package join

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/vkareh/coreshift/internal/rbac"
)

// The files Mint writes, which the create command mounts into the worker
// container read-only at the paths its entrypoint reads.
const (
	BootstrapKubeconfigFilename = "bootstrap-kubeconfig"
	NodeNameFilename            = "node-name"
)

// machineNamespace holds the machine stand-ins; the machine approver
// watches machines in every namespace, and the default namespace keeps
// the stand-ins out of the way of the operator namespaces real clusters
// use.
const machineNamespace = corev1.NamespaceDefault

// machines is the machine resource the stand-ins are, served by the
// machine definition the control plane seeds.
var machines = schema.GroupVersionResource{
	Group:    "machine.openshift.io",
	Version:  "v1beta1",
	Resource: "machines",
}

// Mint creates the join material for one worker named nodeName against
// the cluster the admin kubeconfig at kubeconfigPath points at, writing
// it into dir. Any node and machine a previous worker of the same name
// left behind go first, so a re-created worker joins fresh.
func Mint(ctx context.Context, kubeconfigPath, dir, nodeName string) error {
	config, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("read admin kubeconfig: %w", err)
	}
	server, caPEM, err := clusterServer(config)
	if err != nil {
		return err
	}
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("read admin kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create admin client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create admin client: %w", err)
	}
	return mint(ctx, server, caPEM, client, dynamicClient, dir, nodeName)
}

// mint carries out Mint against given clients, so tests can exercise it
// with fakes.
func mint(ctx context.Context, server string, caPEM []byte, client kubernetes.Interface, dynamicClient dynamic.Interface, dir, nodeName string) error {
	fmt.Println("Minting the bootstrap identity")
	if err := ensureIdentity(ctx, client); err != nil {
		return err
	}
	token, err := mintToken(ctx, client)
	if err != nil {
		return err
	}
	fmt.Println("Minting the machine stand-in")
	if err := forgetWorker(ctx, client, dynamicClient, nodeName); err != nil {
		return err
	}
	if err := ensureMachine(ctx, dynamicClient, nodeName); err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create join material directory: %w", err)
	}
	if err := writeBootstrapKubeconfig(filepath.Join(dir, BootstrapKubeconfigFilename), server, caPEM, token); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, NodeNameFilename), []byte(nodeName+"\n"), 0o644); err != nil {
		return fmt.Errorf("write node name: %w", err)
	}
	return nil
}

// Remove deletes a worker's node object and machine stand-in against the
// cluster the admin kubeconfig points at, so the pair the join minted
// goes when the worker does.
func Remove(ctx context.Context, kubeconfigPath, nodeName string) error {
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("read admin kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create admin client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create admin client: %w", err)
	}
	return forgetWorker(ctx, client, dynamicClient, nodeName)
}

// clusterServer returns the API server address and CA certificate the
// kubeconfig points at. The generated admin kubeconfig embeds both, and
// the bootstrap kubeconfig must carry them embedded too: the paths it
// could reference live on the host, invisible inside the worker
// container.
func clusterServer(config *clientcmdapi.Config) (string, []byte, error) {
	current, ok := config.Contexts[config.CurrentContext]
	if !ok || current == nil {
		return "", nil, fmt.Errorf("admin kubeconfig has no current context")
	}
	cluster := config.Clusters[current.Cluster]
	if cluster == nil {
		return "", nil, fmt.Errorf("admin kubeconfig has no %s cluster", current.Cluster)
	}
	if len(cluster.CertificateAuthorityData) == 0 {
		return "", nil, fmt.Errorf("admin kubeconfig carries no CA certificate")
	}
	return cluster.Server, cluster.CertificateAuthorityData, nil
}

// ensureIdentity creates the node bootstrap service account and its
// namespace, the pair the seeded role binding grants the node-bootstrap
// permission to. Both already exist once any worker joined.
func ensureIdentity(ctx context.Context, client kubernetes.Interface) error {
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: rbac.NodeBootstrapperNamespace},
	}
	if _, err := client.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create the %s namespace: %w", rbac.NodeBootstrapperNamespace, err)
	}
	account := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rbac.NodeBootstrapperAccount,
			Namespace: rbac.NodeBootstrapperNamespace,
		},
	}
	if _, err := client.CoreV1().ServiceAccounts(rbac.NodeBootstrapperNamespace).Create(ctx, account, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create the %s service account: %w", rbac.NodeBootstrapperAccount, err)
	}
	return nil
}

// mintToken mints a token for the node bootstrap service account, the
// credential a real node's bootstrap kubeconfig carries.
func mintToken(ctx context.Context, client kubernetes.Interface) (string, error) {
	request, err := client.CoreV1().ServiceAccounts(rbac.NodeBootstrapperNamespace).CreateToken(ctx,
		rbac.NodeBootstrapperAccount, &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("mint the %s token: %w", rbac.NodeBootstrapperAccount, err)
	}
	return request.Status.Token, nil
}

// ensureMachine creates the machine stand-in for the worker: a machine
// carrying the worker's name as its internal DNS address, which is how
// the machine approver matches a node certificate request to a machine.
// A fresh stand-in matters, because the approver only trusts machines
// created shortly before the request they back.
func ensureMachine(ctx context.Context, client dynamic.Interface, nodeName string) error {
	machine := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": machines.GroupVersion().String(),
		"kind":       "Machine",
		"metadata": map[string]interface{}{
			"name":      nodeName,
			"namespace": machineNamespace,
		},
	}}
	created, err := client.Resource(machines).Namespace(machineNamespace).Create(ctx, machine, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create the %s machine stand-in: %w", nodeName, err)
	}
	// The address the approver matches against lives in the status, a
	// subresource, so it is set with the subresource's update.
	if err := unstructured.SetNestedSlice(created.Object, []interface{}{
		map[string]interface{}{"type": "InternalDNS", "address": nodeName},
	}, "status", "addresses"); err != nil {
		return fmt.Errorf("set the %s machine stand-in's address: %w", nodeName, err)
	}
	if _, err := client.Resource(machines).Namespace(machineNamespace).UpdateStatus(ctx, created, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("set the %s machine stand-in's address: %w", nodeName, err)
	}
	return nil
}

// forgetWorker deletes the node object and machine stand-in a worker of
// this name may have left behind. A missing pair is not an error.
func forgetWorker(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface, nodeName string) error {
	if err := client.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the %s node: %w", nodeName, err)
	}
	if err := dynamicClient.Resource(machines).Namespace(machineNamespace).Delete(ctx, nodeName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the %s machine stand-in: %w", nodeName, err)
	}
	return nil
}

// writeBootstrapKubeconfig assembles the bootstrap kubeconfig handed to
// the worker container: the same API server and CA as the admin
// kubeconfig, with the bootstrap token in place of the admin certificate.
// The CA is embedded, so the one file carries the whole identity.
func writeBootstrapKubeconfig(path, server string, caPEM []byte, token string) error {
	config := clientcmdapi.NewConfig()
	config.Clusters["coreshift"] = &clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthorityData: caPEM,
	}
	config.AuthInfos[rbac.NodeBootstrapperAccount] = &clientcmdapi.AuthInfo{Token: token}
	config.Contexts[rbac.NodeBootstrapperAccount+"@coreshift"] = &clientcmdapi.Context{
		Cluster:  "coreshift",
		AuthInfo: rbac.NodeBootstrapperAccount,
	}
	config.CurrentContext = rbac.NodeBootstrapperAccount + "@coreshift"
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		return fmt.Errorf("write bootstrap kubeconfig: %w", err)
	}
	return nil
}
