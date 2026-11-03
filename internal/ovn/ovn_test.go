package ovn

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// newFakeClients returns a fake core client holding the OVN objects the
// network operator renders, and a fake dynamic client serving the
// operator's own configuration object.
func newFakeClients(t *testing.T) (*fake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: namespace},
		Data: map[string]string{
			"ovnkube.conf": "[ovnkubernetesfeature]\nenable-network-segmentation=true\n",
		},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(2)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{NodeSelector: map[string]string{
					"node-role.kubernetes.io/master": "",
					"kubernetes.io/os":               "linux",
				}},
			},
		},
	}
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: daemonSetName, Namespace: namespace},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{PriorityClassName: "system-node-critical"},
			},
		},
	}
	identity := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: identityDaemonSetName, Namespace: identityNamespace},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{NodeSelector: map[string]string{
					"node-role.kubernetes.io/master": "",
					"beta.kubernetes.io/os":          "linux",
				}},
			},
		},
	}
	identityNamespaceObject := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: identityNamespace,
			Annotations: map[string]string{
				"openshift.io/sa.scc.uid-range":           "1000550000/10000",
				"openshift.io/sa.scc.supplemental-groups": "1000550000/10000",
			},
		},
	}
	client := fake.NewSimpleClientset(configMap, deployment, daemonSet, identity, identityNamespaceObject)
	operatorConfig := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": operatorConfigs.GroupVersion().String(),
		"kind":       "Network",
		"metadata":   map[string]any{"name": "cluster"},
	}}
	scheme := runtime.NewScheme()
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{operatorConfigs: "NetworkList"})
	if _, err := dynamicClient.Resource(operatorConfigs).Create(
		context.Background(), operatorConfig, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return client, dynamicClient
}

func ptr(value int32) *int32 { return &value }

// settled reports the render settle delay the tests want: none, so a
// verifying prepare does not wait out a minute per call.
func settled(t *testing.T) {
	t.Helper()
	original := renderSettleDelay
	renderSettleDelay = 0
	t.Cleanup(func() { renderSettleDelay = original })
}

// TestPreparePlacesWorkloads checks the three placements Prepare makes:
// the operator goes unmanaged, the OVN configuration gives up network
// segmentation, and the control-plane Deployment selects the nodes the
// cluster has at one replica.
func TestPreparePlacesWorkloads(t *testing.T) {
	settled(t)
	client, dynamicClient := newFakeClients(t)
	ctx := context.Background()
	if err := prepare(ctx, client, dynamicClient); err != nil {
		t.Fatal(err)
	}
	config, err := dynamicClient.Resource(operatorConfigs).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := unstructured.NestedString(config.Object, "spec", "managementState")
	if err != nil || state != "Unmanaged" {
		t.Fatalf("operator management state = %q, %v; want Unmanaged", state, err)
	}
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cm.Data["ovnkube.conf"], "enable-network-segmentation=true") {
		t.Fatalf("network segmentation still enabled:\n%s", cm.Data["ovnkube.conf"])
	}
	deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if selector := deployment.Spec.Template.Spec.NodeSelector; len(selector) != 1 || selector["kubernetes.io/os"] != "linux" {
		t.Fatalf("deployment node selector = %v; want only the cluster's own nodes", selector)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Fatalf("deployment replicas = %v; want 1", deployment.Spec.Replicas)
	}
	daemonSet, err := client.AppsV1().DaemonSets(namespace).Get(ctx, daemonSetName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if class := daemonSet.Spec.Template.Spec.PriorityClassName; class != clusterCritical {
		t.Fatalf("daemon set priority class = %q; want %q", class, clusterCritical)
	}
	identity, err := client.AppsV1().DaemonSets(identityNamespace).Get(ctx, identityDaemonSetName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if selector := identity.Spec.Template.Spec.NodeSelector; len(selector) != 1 || selector["beta.kubernetes.io/os"] != "linux" {
		t.Fatalf("identity daemon set node selector = %v; want only the cluster's own nodes", selector)
	}
}

// TestPrepareIsSafeToRepeat checks that running Prepare again against
// what it already placed succeeds, so a retried worker creation does not
// wedge the placement.
func TestPrepareIsSafeToRepeat(t *testing.T) {
	settled(t)
	client, dynamicClient := newFakeClients(t)
	ctx := context.Background()
	if err := prepare(ctx, client, dynamicClient); err != nil {
		t.Fatal(err)
	}
	if err := prepare(ctx, client, dynamicClient); err != nil {
		t.Fatal(err)
	}
}

// TestPrepareRepairsAReRender checks that Prepare repairs what the
// network operator re-rendered over its placement — the render pass
// that straddled the switch to unmanaged — instead of passing over the
// overwritten objects.
func TestPrepareRepairsAReRender(t *testing.T) {
	settled(t)
	client, dynamicClient := newFakeClients(t)
	ctx := context.Background()
	if err := prepare(ctx, client, dynamicClient); err != nil {
		t.Fatal(err)
	}
	revert, _ := newFakeClients(t)
	reRendered, err := revert.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AppsV1().Deployments(namespace).Update(ctx, reRendered, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := prepare(ctx, client, dynamicClient); err != nil {
		t.Fatal(err)
	}
	deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if selector := deployment.Spec.Template.Spec.NodeSelector; len(selector) != 1 || selector["kubernetes.io/os"] != "linux" {
		t.Fatalf("deployment node selector = %v; want the placement back", selector)
	}
}

// TestPrepareWaitsForRenderedWorkloads checks that Prepare reports the
// operator's objects missing instead of failing on them, so its retry
// can wait for the operator to render them.
func TestPrepareWaitsForRenderedWorkloads(t *testing.T) {
	settled(t)
	client := fake.NewSimpleClientset()
	scheme := runtime.NewScheme()
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{operatorConfigs: "NetworkList"})
	if err := prepare(context.Background(), client, dynamicClient); err == nil {
		t.Fatal("prepare against an unrendered cluster succeeded")
	}
}

// TestControlPlanePodPrefersTheScheduled checks that the control-plane
// wait measures the pod a node took, not the replaced ReplicaSet's
// Pending leftover beside it.
func TestControlPlanePodPrefersTheScheduled(t *testing.T) {
	pending := runningPod("ovnkube-control-planeold", "ovnkube-control-plane", "", false)
	pending.Status.Phase = corev1.PodPending
	running := runningPod("ovnkube-control-planenew", "ovnkube-control-plane", "worker1", true)
	client := fake.NewSimpleClientset(pending, running)
	pod, err := controlPlanePod(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if pod == nil || pod.Name != "ovnkube-control-planenew" {
		t.Fatalf("control-plane pod = %v; want the scheduled one", pod)
	}
}

// runningPod builds a pod for the waits: a running, ready pod when
// ready is set, and otherwise one waiting on its first container.
func runningPod(name, app, node string, ready bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": app},
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	if !ready {
		pod.Status.Conditions[0].Status = corev1.ConditionFalse
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "ovnkube-controller",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}
	}
	return pod
}

// TestWorkloadsReadyWaitsForBoth checks the wait's measure: both OVN
// workloads must run ready, on the joined worker and in reach.
func TestWorkloadsReadyWaitsForBoth(t *testing.T) {
	node := runningPod("ovnkube-nodeabc", "ovnkube-node", "worker1", true)
	plane := runningPod("ovnkube-control-planexyz", "ovnkube-control-plane", "", true)
	client := fake.NewSimpleClientset(node, plane)
	done, report := workloadsReady(context.Background(), client, "worker1")
	if !done {
		t.Fatalf("workloads not ready: %s", report)
	}
	if !strings.Contains(report, "ovnkube-nodeabc on worker1") || !strings.Contains(report, "ovnkube-control-planexyz") {
		t.Fatalf("workload report = %q", report)
	}
}

// TestWorkloadsReadyNamesWhatIsMissing checks the wait's report when a
// workload is absent or not ready, so a failure names what to look at.
func TestWorkloadsReadyNamesWhatIsMissing(t *testing.T) {
	notReady := runningPod("ovnkube-nodeabc", "ovnkube-node", "worker1", false)
	client := fake.NewSimpleClientset(notReady)
	done, report := workloadsReady(context.Background(), client, "worker1")
	if done {
		t.Fatalf("workloads ready with none running: %s", report)
	}
	for _, want := range []string{"ovnkube-node", "not ready", "CrashLoopBackOff", "no ovnkube-control-plane pod"} {
		if !strings.Contains(report, want) {
			t.Fatalf("workload report %q missing %q", report, want)
		}
	}
}

// TestWaitWorkloadsTimesOut checks that a wait nothing satisfies fails
// naming the context deadline, bounded fast for the test.
func TestWaitWorkloadsTimesOut(t *testing.T) {
	workloadPollInterval = time.Millisecond
	workloadReadyTimeout = 50 * time.Millisecond
	client := fake.NewSimpleClientset()
	err := waitWorkloads(context.Background(), client, "worker1")
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("wait error = %v; want the deadline", err)
	}
}
