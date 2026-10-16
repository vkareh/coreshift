package aggregation

import (
	"context"
	"errors"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	aggregatorfake "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/fake"
	"k8s.io/utils/ptr"
)

func testServers() []Server {
	return []Server{
		{Name: "openshift-apiserver", Address: "10.1.2.3", Port: 8443, Groups: []string{"project", "quota"}},
		{Name: "openshift-oauth-apiserver", Address: "10.1.2.3", Port: 8444, Groups: []string{"oauth"}},
	}
}

func TestSeedRegistersAllObjects(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	aggregatorClient := aggregatorfake.NewSimpleClientset()
	caBundle := []byte("first-bundle")

	if err := seed(context.Background(), kubeClient, aggregatorClient, testServers(), caBundle); err != nil {
		t.Fatal(err)
	}

	for _, server := range testServers() {
		service, err := kubeClient.CoreV1().Services("default").Get(context.Background(), server.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("service %s: %v", server.Name, err)
		}
		if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 443 || service.Spec.Ports[0].Name != "https" {
			t.Fatalf("service %s has unexpected ports: %+v", server.Name, service.Spec.Ports)
		}
		if len(service.Spec.Selector) != 0 {
			t.Fatalf("service %s must be selectorless, got %v", server.Name, service.Spec.Selector)
		}

		slice, err := kubeClient.DiscoveryV1().EndpointSlices("default").Get(context.Background(), server.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("endpointslice %s: %v", server.Name, err)
		}
		if slice.Labels[discoveryv1.LabelServiceName] != server.Name {
			t.Fatalf("endpointslice %s missing service name label: %+v", server.Name, slice.Labels)
		}
		if len(slice.Endpoints) != 1 || len(slice.Endpoints[0].Addresses) != 1 || slice.Endpoints[0].Addresses[0] != server.Address {
			t.Fatalf("endpointslice %s has unexpected endpoints: %+v", server.Name, slice.Endpoints)
		}
		if !ptr.Equal(slice.Endpoints[0].Conditions.Ready, ptr.To(true)) {
			t.Fatalf("endpointslice %s endpoint not ready: %+v", server.Name, slice.Endpoints[0].Conditions)
		}
		if len(slice.Ports) != 1 || *slice.Ports[0].Port != int32(server.Port) || *slice.Ports[0].Name != "https" {
			t.Fatalf("endpointslice %s has unexpected ports: %+v", server.Name, slice.Ports)
		}

		for _, group := range server.Groups {
			name := "v1." + group + ".openshift.io"
			apiService, err := aggregatorClient.ApiregistrationV1().APIServices().Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("apiservice %s: %v", name, err)
			}
			if apiService.Spec.Group != group+".openshift.io" || apiService.Spec.Version != "v1" {
				t.Fatalf("apiservice %s covers wrong group/version: %s/%s", name, apiService.Spec.Group, apiService.Spec.Version)
			}
			if string(apiService.Spec.CABundle) != "first-bundle" {
				t.Fatalf("apiservice %s has wrong CA bundle: %q", name, apiService.Spec.CABundle)
			}
			if apiService.Spec.GroupPriorityMinimum != 9900 || apiService.Spec.VersionPriority != 15 {
				t.Fatalf("apiservice %s has wrong priorities: %d/%d", name, apiService.Spec.GroupPriorityMinimum, apiService.Spec.VersionPriority)
			}
			ref := apiService.Spec.Service
			if ref == nil || ref.Namespace != "default" || ref.Name != server.Name || *ref.Port != 443 {
				t.Fatalf("apiservice %s points at wrong service: %+v", name, ref)
			}
		}
	}

	// Groups that were never registered must not appear.
	if _, err := aggregatorClient.ApiregistrationV1().APIServices().Get(context.Background(), "v1.image.openshift.io", metav1.GetOptions{}); err == nil {
		t.Fatal("apiservice for unregistered group exists")
	}
}

func TestSeedIsRepeatableAndRotatesCABundle(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	aggregatorClient := aggregatorfake.NewSimpleClientset()

	if err := seed(context.Background(), kubeClient, aggregatorClient, testServers(), []byte("first-bundle")); err != nil {
		t.Fatal(err)
	}
	if err := seed(context.Background(), kubeClient, aggregatorClient, testServers(), []byte("second-bundle")); err != nil {
		t.Fatal(err)
	}

	apiService, err := aggregatorClient.ApiregistrationV1().APIServices().Get(context.Background(), "v1.project.openshift.io", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(apiService.Spec.CABundle) != "second-bundle" {
		t.Fatalf("CA bundle was not rotated: %q", apiService.Spec.CABundle)
	}

	services, err := kubeClient.CoreV1().Services("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(services.Items) != 2 {
		t.Fatalf("expected the same two services after reseeding, got %d", len(services.Items))
	}
}

func TestSeedRefreshesChangedHostAddress(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	aggregatorClient := aggregatorfake.NewSimpleClientset()

	servers := testServers()
	if err := seed(context.Background(), kubeClient, aggregatorClient, servers, nil); err != nil {
		t.Fatal(err)
	}
	for i := range servers {
		servers[i].Address = "10.9.8.7"
	}
	if err := seed(context.Background(), kubeClient, aggregatorClient, servers, nil); err != nil {
		t.Fatal(err)
	}

	slices, err := kubeClient.DiscoveryV1().EndpointSlices("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, slice := range slices.Items {
		if got := slice.Endpoints[0].Addresses[0]; got != "10.9.8.7" {
			t.Fatalf("endpointslice %s was not refreshed, address: %s", slice.Name, got)
		}
	}
}

func TestSeedRetriesRejectedWrites(t *testing.T) {
	objectRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	kubeClient := fake.NewSimpleClientset()
	aggregatorClient := aggregatorfake.NewSimpleClientset()

	// Reject the very first service creation the way a warming apiserver
	// does, e.g. cluster-IP allocation before the repair pass ran.
	var calls int
	kubeClient.PrependReactor("create", "services", func(action ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, apierrors.NewInternalError(errors.New("failed to allocate a serviceIP: range is full"))
		}
		return false, nil, nil
	})

	if err := seedWithRetry(context.Background(), kubeClient, aggregatorClient, testServers(), nil); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("rejected write was not retried, calls: %d", calls)
	}
	service, err := kubeClient.CoreV1().Services("default").Get(context.Background(), "openshift-apiserver", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if service.Name != "openshift-apiserver" {
		t.Fatalf("service was not registered after retry: %+v", service)
	}
}
