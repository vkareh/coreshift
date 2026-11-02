package gate

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// newClient returns a fake serving the resources the gate reads, with
// any objects already in place.
func newClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
			{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
			{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}:  "ClusterRoleBindingList",
		},
		objects...,
	)
}

// definition returns an API definition object as the API serves it.
func definition(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": name},
	}}
}

// captureStdout runs fn with the gate's output captured, returning what
// it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	fn()
	os.Stdout = old
	w.Close()
	return <-done
}

// shortenPolling makes the gate poll in milliseconds for the duration
// of a test.
func shortenPolling(t *testing.T) {
	t.Helper()
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = 2 * time.Second })
}

// shortenTimeout makes the gate time out in milliseconds for the
// duration of a test.
func shortenTimeout(t *testing.T) {
	t.Helper()
	waitTimeout = 10 * time.Millisecond
	t.Cleanup(func() { waitTimeout = 10 * time.Minute })
}

// TestWaitPassesSilentlyWhenObjectsExist checks the gate is invisible
// when what a child needs is already there, whatever applied it.
func TestWaitPassesSilentlyWhenObjectsExist(t *testing.T) {
	shortenPolling(t)

	client := newClient(definition("clusterresourcequotas.quota.openshift.io"))
	var err error
	out := captureStdout(t, func() {
		err = poll(context.Background(), client, "the quota definition", "",
			Definition("clusterresourcequotas.quota.openshift.io"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("gate printed while passing:\n%s", out)
	}
}

// TestWaitPrintsOneLineWhileWaiting checks the wait is visible as one
// progress line and passes once the objects arrive.
func TestWaitPrintsOneLineWhileWaiting(t *testing.T) {
	shortenPolling(t)

	client := newClient()
	object := Definition("oauths.config.openshift.io")
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = client.Resource(object.resource).Create(context.Background(), definition("oauths.config.openshift.io"), metav1.CreateOptions{})
	}()
	var err error
	out := captureStdout(t, func() {
		err = poll(context.Background(), client, "the OAuth definition", "", object)
	})
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 1 || lines[0] != "Waiting for the OAuth definition" {
		t.Fatalf("gate output = %q; want one waiting line", out)
	}
}

// TestWaitTimesOutNamingWhatNeverArrived checks the failure names the
// missing objects and the version operator that delivers them.
func TestWaitTimesOutNamingWhatNeverArrived(t *testing.T) {
	shortenPolling(t)
	shortenTimeout(t)

	var err error
	out := captureStdout(t, func() {
		err = poll(context.Background(), newClient(), "the controller RBAC", "/logs/cluster-version-operator.log",
			ClusterRole("system:openshift:scc:restricted-v3"), ClusterRoleBinding("system:openshift:scc:restricted-v3"))
	})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	for _, want := range []string{
		"the cluster role system:openshift:scc:restricted-v3",
		"the cluster role binding system:openshift:scc:restricted-v3",
		"never arrived",
		"version operator",
		"/logs/cluster-version-operator.log",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err.Error(), want)
		}
	}
	if !strings.Contains(out, "Waiting for the controller RBAC") {
		t.Fatalf("gate output = %q; want the waiting line", out)
	}
}
