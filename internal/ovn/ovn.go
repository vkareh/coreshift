// Package ovn places the network operator's OVN workloads within reach
// of the worker a cluster in a container is joined by. The operator
// renders them onto master nodes, which this cluster runs none of, and
// offers no placement configuration; the only placement that runs the
// OVN control plane without waking every payload operator Deployment at
// once is to stop the operator's reconciliation — its own supported
// unmanaged state — and point the one Deployment at the nodes this
// cluster has. The node-side DaemonSet needs no help: it already
// selects every node.
package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/vkareh/coreshift/internal/warnings"
)

// The OVN workloads' namespace and names, as the network operator
// renders them. The node identity DaemonSet lives in its own namespace
// and renders the node certificates' approver, without which the OVN
// pods' certificate managers wait forever on unsigned requests.
const (
	namespace             = "openshift-ovn-kubernetes"
	configMapName         = "ovnkube-config"
	deploymentName        = "ovnkube-control-plane"
	daemonSetName         = "ovnkube-node"
	identityNamespace     = "openshift-network-node-identity"
	identityDaemonSetName = "network-node-identity"
	nodePodLabel          = "app=ovnkube-node"
	planePodApp           = "ovnkube-control-plane"
	clusterCritical       = "system-cluster-critical"
	nodeCritical          = "system-node-critical"
	clusterReplicas       = 1
)

// The selectors that place a workload on every node this cluster has,
// which is the nodes a joined worker is, and the annotations that hold
// a namespace's assigned user range.
var (
	osSelector              = map[string]string{"kubernetes.io/os": "linux"}
	identityOsSelector      = map[string]string{"beta.kubernetes.io/os": "linux"}
	uidRangeAnnotation      = "openshift.io/sa.scc.uid-range"
	supplementaryAnnotation = "openshift.io/sa.scc.supplemental-groups"
)

// The network segmentation setting, which the OVN configuration the
// node pods read gives up: its node side needs a systemd no worker
// container runs.
const (
	segmentationEnabled  = "enable-network-segmentation=true"
	segmentationDisabled = "enable-network-segmentation=false"
)

// nodeNamespaces are the namespaces the network operator renders node
// workloads into.
var nodeNamespaces = []string{
	namespace,
	"openshift-multus",
	"openshift-network-diagnostics",
	identityNamespace,
}

// workerUIDRange is the user range a worker's own user namespace maps:
// a worker is a container, with a small slice of the host's identities
// and no more. Node pods asked to run beyond it, as the security
// allocation of their namespace assigns, cannot even be created.
const workerUIDRange = "1000/10000"

// operatorConfigs is the network operator's own configuration object,
// the one whose management state tells the operator to keep its hands
// off what it rendered. The object's CRD is named for the network
// operators, but it lives in the operator API group with the other
// operator-scoped configuration.
var operatorConfigs = schema.GroupVersionResource{
	Group:    "operator.openshift.io",
	Version:  "v1",
	Resource: "networks",
}

// prepareRetryInterval spaces out the retries while the network
// operator is still delivering the objects Prepare patches. A variable
// for test override.
var prepareRetryInterval = 3 * time.Second

// Prepare makes the OVN workloads placeable on the worker a container
// cluster is about to be joined by. The network operator is put into
// its unmanaged state so it stops re-asserting what it rendered, the
// OVN configuration the node pods read gives up the network
// segmentation feature — whose node side needs a systemd no worker
// container runs — and the one OVN Deployment is pointed at the nodes
// this cluster has, at a replica count one node's shared network can
// carry. Every step is retried until the network operator has rendered,
// and is safe to run again.
func Prepare(ctx context.Context, kubeconfigPath string) error {
	client, dynamicClient, err := clients(kubeconfigPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, prepareTimeout)
	defer cancel()
	fmt.Println("Placing the OVN workloads")
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, prepareRetryInterval, true, func(ctx context.Context) (bool, error) {
		lastErr = prepare(ctx, client, dynamicClient)
		return lastErr == nil, nil
	})
	if pollErr != nil {
		if lastErr == nil {
			return fmt.Errorf("place the OVN workloads: %w", ctx.Err())
		}
		return fmt.Errorf("place the OVN workloads: %w", lastErr)
	}
	return nil
}

// prepareTimeout bounds the wait for the network operator to render the
// objects Prepare patches and settle after them.
const prepareTimeout = 5 * time.Minute

// renderSettleDelay is how long prepare waits, after the objects verify
// as placed, before believing them: the operator's render pass can
// straddle the switch to unmanaged, and its writes keep landing for as
// long as that pass runs. A variable for test override.
var renderSettleDelay = time.Minute

func prepare(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface) error {
	// The operator goes unmanaged first, so it cannot revert the patches
	// that follow between them.
	unmanaged := []byte(`{"spec":{"managementState":"Unmanaged"}}`)
	if _, err := dynamicClient.Resource(operatorConfigs).Patch(ctx, "cluster", types.MergePatchType, unmanaged, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("make the network operator unmanaged: %w", err)
	}
	if _, err := place(ctx, client); err != nil {
		return err
	}
	// Whether that pass placed the workloads or found them placed, a
	// render pass that straddled the unmanaged switch may still be
	// writing. Let it finish, then confirm it overwrote nothing.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(renderSettleDelay):
	}
	overwritten, err := place(ctx, client)
	if err != nil {
		return err
	}
	if overwritten {
		return fmt.Errorf("the network operator re-rendered the workloads; placing them again")
	}
	return nil
}

// place puts the OVN workloads within reach of the nodes this cluster
// has, reporting whether it changed anything. Every step checks the
// object's current state first, so a pass over already-placed objects
// changes nothing — the signal prepare waits for — and a pass over
// objects the operator re-rendered repairs them.
func place(ctx context.Context, client kubernetes.Interface) (bool, error) {
	changed := false
	config, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return changed, fmt.Errorf("read the %s ConfigMap: %w", configMapName, err)
	}
	conf, ok := config.Data["ovnkube.conf"]
	if !ok {
		return changed, fmt.Errorf("the %s ConfigMap carries no ovnkube.conf", configMapName)
	}
	if strings.Contains(conf, segmentationEnabled) {
		config.Data["ovnkube.conf"] = strings.Replace(conf, segmentationEnabled, segmentationDisabled, 1)
		if _, err := client.CoreV1().ConfigMaps(namespace).Update(ctx, config, metav1.UpdateOptions{}); err != nil {
			return changed, fmt.Errorf("disable network segmentation in the %s ConfigMap: %w", configMapName, err)
		}
		changed = true
	}
	deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return changed, fmt.Errorf("read the %s Deployment: %w", deploymentName, err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != clusterReplicas ||
		!reflect.DeepEqual(deployment.Spec.Template.Spec.NodeSelector, osSelector) {
		placement := []byte(fmt.Sprintf(`[
			{"op":"replace","path":"/spec/template/spec/nodeSelector","value":%s},
			{"op":"replace","path":"/spec/replicas","value":%d}
		]`, selectorJSON(osSelector), clusterReplicas))
		if _, err := client.AppsV1().Deployments(namespace).Patch(ctx, deploymentName, types.JSONPatchType, placement, metav1.PatchOptions{}); err != nil {
			return changed, fmt.Errorf("place the %s Deployment on the cluster's nodes: %w", deploymentName, err)
		}
		changed = true
	}
	// The node DaemonSets' own critical priority asks for an OOM score
	// only the initial user namespace can grant, and a worker is a
	// container: the node workloads run at the control plane's critical
	// priority instead, which the node's kubelet scores by memory like
	// any burstable pod.
	retuned, err := retuneNodeCritical(ctx, client)
	if err != nil {
		return changed, err
	}
	changed = changed || retuned
	// The node identity DaemonSet carries the approver that signs off
	// the OVN pods' certificate requests; it selects master nodes like
	// every other control plane workload, so it gets the same placement.
	// Its pods also run as users its namespace's security allocation
	// assigns, which the worker's user namespace cannot map, so the
	// allocation is re-annotated into the range it can.
	identity, err := client.AppsV1().DaemonSets(identityNamespace).Get(ctx, identityDaemonSetName, metav1.GetOptions{})
	if err != nil {
		return changed, fmt.Errorf("read the %s DaemonSet: %w", identityDaemonSetName, err)
	}
	if !reflect.DeepEqual(identity.Spec.Template.Spec.NodeSelector, identityOsSelector) {
		placement := []byte(fmt.Sprintf(`[{"op":"replace","path":"/spec/template/spec/nodeSelector","value":%s}]`, selectorJSON(identityOsSelector)))
		if _, err := client.AppsV1().DaemonSets(identityNamespace).Patch(ctx, identityDaemonSetName, types.JSONPatchType, placement, metav1.PatchOptions{}); err != nil {
			return changed, fmt.Errorf("place the %s DaemonSet on the cluster's nodes: %w", identityDaemonSetName, err)
		}
		changed = true
	}
	target, err := client.CoreV1().Namespaces().Get(ctx, identityNamespace, metav1.GetOptions{})
	if err != nil {
		return changed, fmt.Errorf("read the %s namespace: %w", identityNamespace, err)
	}
	if target.Annotations[uidRangeAnnotation] != workerUIDRange || target.Annotations[supplementaryAnnotation] != workerUIDRange {
		uidRange := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q,%q:%q}}}`,
			uidRangeAnnotation, workerUIDRange, supplementaryAnnotation, workerUIDRange)
		if _, err := client.CoreV1().Namespaces().Patch(ctx, identityNamespace, types.MergePatchType, []byte(uidRange), metav1.PatchOptions{}); err != nil {
			return changed, fmt.Errorf("re-annotate the %s namespace's user range: %w", identityNamespace, err)
		}
		changed = true
	}
	return changed, nil
}

// selectorJSON renders a node selector as the JSON a patch value needs.
func selectorJSON(selector map[string]string) string {
	encoded, err := json.Marshal(selector)
	if err != nil {
		// A map of string keys to string values cannot fail to encode.
		panic(err)
	}
	return string(encoded)
}

// retuneNodeCritical moves every node workload the network operator
// rendered from its own critical priority to the control plane's, the
// one a worker's user namespace can live with, reporting whether any
// workload needed the move.
func retuneNodeCritical(ctx context.Context, client kubernetes.Interface) (bool, error) {
	changed := false
	priority := fmt.Sprintf(`[{"op":"replace","path":"/spec/template/spec/priorityClassName","value":"%s"}]`, clusterCritical)
	for _, ns := range nodeNamespaces {
		daemonSets, err := client.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return changed, fmt.Errorf("list the %s daemon sets: %w", ns, err)
		}
		for _, daemonSet := range daemonSets.Items {
			if daemonSet.Spec.Template.Spec.PriorityClassName != nodeCritical {
				continue
			}
			if _, err := client.AppsV1().DaemonSets(ns).Patch(ctx, daemonSet.Name, types.JSONPatchType, []byte(priority), metav1.PatchOptions{}); err != nil {
				return changed, fmt.Errorf("retune the %s DaemonSet's priority: %w", daemonSet.Name, err)
			}
			changed = true
		}
	}
	return changed, nil
}

// workloadReadyTimeout bounds the wait for the OVN workloads to run on
// the joined worker: the node pod's containers must come up, and the
// images they run must be pulled into the worker's runtime first. A
// variable for test override.
var workloadReadyTimeout = 15 * time.Minute

// workloadPollInterval spaces out the polls watching the OVN workloads.
// A variable for test override.
var workloadPollInterval = 5 * time.Second

// WaitWorkloads waits until the OVN workloads run: the node pod on the
// joined worker, containers up and ready, and the control-plane pod the
// placement put within reach. Both are the real workloads the network
// operator rendered, so their readiness is the honest measure of the
// network stack coming up.
func WaitWorkloads(ctx context.Context, kubeconfigPath, nodeName string) error {
	client, _, err := clients(kubeconfigPath)
	if err != nil {
		return err
	}
	return waitWorkloads(ctx, client, nodeName)
}

func waitWorkloads(ctx context.Context, client kubernetes.Interface, nodeName string) error {
	ctx, cancel := context.WithTimeout(ctx, workloadReadyTimeout)
	defer cancel()
	fmt.Println("Waiting for the OVN workloads to run")
	var report string
	pollErr := wait.PollUntilContextCancel(ctx, workloadPollInterval, true, func(ctx context.Context) (bool, error) {
		done, current := workloadsReady(ctx, client, nodeName)
		report = current
		return done, nil
	})
	if pollErr != nil {
		return fmt.Errorf("wait for the OVN workloads: %w\n%s", ctx.Err(), report)
	}
	fmt.Println("OVN workloads running:", report)
	return nil
}

// workloadsReady reports whether both OVN workloads run, with a report
// naming what runs or what is missing.
func workloadsReady(ctx context.Context, client kubernetes.Interface, nodeName string) (bool, string) {
	nodeOK, nodeReport := nodeWorkload(ctx, client, nodeName)
	planeOK, planeReport := planeWorkload(ctx, client)
	if nodeOK && planeOK {
		return true, nodeReport + ", " + planeReport
	}
	return false, nodeReport + "; " + planeReport
}

// nodeWorkload reports the OVN node pod on nodeName: its name when it
// runs ready, or what is missing.
func nodeWorkload(ctx context.Context, client kubernetes.Interface, nodeName string) (bool, string) {
	pod, err := podOnNode(ctx, client, nodeName)
	if err != nil {
		return false, err.Error()
	}
	if pod == nil {
		return false, "no ovnkube-node pod on " + nodeName
	}
	if !podReady(pod) {
		return false, "the ovnkube-node pod is not ready: " + podReport(pod)
	}
	return true, pod.Name + " on " + nodeName
}

// planeWorkload reports the OVN control-plane pod: its name when it
// runs ready, or what is missing.
func planeWorkload(ctx context.Context, client kubernetes.Interface) (bool, string) {
	pod, err := controlPlanePod(ctx, client)
	if err != nil {
		return false, err.Error()
	}
	if pod == nil {
		return false, "no ovnkube-control-plane pod"
	}
	if !podReady(pod) {
		return false, "the ovnkube-control-plane pod is not ready: " + podReport(pod)
	}
	return true, pod.Name
}

// podOnNode returns the OVN node pod scheduled on nodeName.
func podOnNode(ctx context.Context, client kubernetes.Interface, nodeName string) (*corev1.Pod, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: nodePodLabel})
	if err != nil {
		return nil, fmt.Errorf("list the OVN node pods: %w", err)
	}
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName == nodeName {
			return &pods.Items[i], nil
		}
	}
	return nil, nil
}

// controlPlanePod returns the OVN control-plane pod the placement runs,
// preferring one a node took: the placement's rollout can leave the
// replaced ReplicaSet's pod Pending beside the running one.
func controlPlanePod(ctx context.Context, client kubernetes.Interface) (*corev1.Pod, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + planePodApp})
	if err != nil {
		return nil, fmt.Errorf("list the OVN control-plane pods: %w", err)
	}
	var pending *corev1.Pod
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName != "" {
			return &pods.Items[i], nil
		}
		pending = &pods.Items[i]
	}
	return pending, nil
}

// podReady reports whether a pod runs with its containers ready.
func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podReport names a pod's phase and the state of its first container
// not running, the honest one-line answer to what it waits for.
func podReport(pod *corev1.Pod) string {
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Running == nil && status.State.Waiting != nil {
			return string(pod.Status.Phase) + ", " + status.Name + ": " + status.State.Waiting.Reason
		}
	}
	return string(pod.Status.Phase)
}

// clients builds the API clients the kubeconfig points at.
func clients(kubeconfigPath string) (kubernetes.Interface, dynamic.Interface, error) {
	config, err := warnings.Config(kubeconfigPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read admin kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("create admin client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("create admin client: %w", err)
	}
	return client, dynamicClient, nil
}
