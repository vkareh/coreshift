// Package aggregation registers locally running API servers with the
// kube-apiserver's aggregation layer, so their API groups are served through
// the main API endpoint.
package aggregation

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	aggregatorclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	"k8s.io/utils/ptr"
)

// The backing services live in the default namespace and are addressed on
// port 443, following the names Hypershift's hosted cluster config operator
// registers. kube-apiserver verifies each aggregated server's serving
// certificate against <name>.<namespace>.svc, which the generated
// service-serving certificate covers.
const (
	aggregationNamespace = "default"
	servicePort          = 443
	servicePortName      = "https"
	apiPollInterval      = time.Second
)

// objectRetryInterval spaces out retries when the apiserver rejects early
// writes, e.g. cluster-IP allocation until the allocator repair pass runs.
// A variable for test override.
var objectRetryInterval = 3 * time.Second

// Server describes one aggregated API server reachable on a host address.
type Server struct {
	// Name is the service name the server is reachable under.
	Name string
	// Address is the host address the server listens on. Kubernetes
	// validation rejects loopback and unspecified endpoint addresses, so
	// the servers must listen on a routable host address.
	Address string
	// Port is the port the server listens on.
	Port int
	// Groups lists the API groups the server serves, without the
	// openshift.io suffix (e.g. "project").
	Groups []string
}

// Seed registers the given servers with kube-apiserver's aggregation layer.
// It first waits for the main endpoint to answer requests, then creates a
// selectorless Service and an EndpointSlice routing to localhost for each
// server, and one APIService per served group whose CA bundle is read from
// caBundlePath. The main endpoint must run with aggregator routing enabled
// for the aggregation layer to prefer endpoints over cluster IPs.
func Seed(ctx context.Context, kubeconfigPath string, servers []Server, caBundlePath string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create kube client: %w", err)
	}
	aggregatorClient, err := aggregatorclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create aggregator client: %w", err)
	}
	caBundle, err := os.ReadFile(caBundlePath)
	if err != nil {
		return fmt.Errorf("read CA bundle: %w", err)
	}

	// Aggregated servers rely on kube-apiserver for delegated
	// authentication and authorization from the moment they start, so wait
	// for it to answer before handing control back.
	if err := waitForAPI(ctx, kubeClient); err != nil {
		return err
	}
	return seedWithRetry(ctx, kubeClient, aggregatorClient, servers, caBundle)
}

// WaitReady polls the main API endpoint's health and each aggregated
// server's API availability until all answer or the context expires.
func WaitReady(ctx context.Context, kubeconfigPath string, servers []Server) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create kube client: %w", err)
	}
	aggregatorClient, err := aggregatorclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create aggregator client: %w", err)
	}
	return waitReady(ctx, kubeClient.Discovery().RESTClient(), aggregatorClient, servers)
}

func waitReady(ctx context.Context, restClient rest.Interface, aggregatorClient aggregatorclient.Interface, servers []Server) error {
	if err := waitForHealthz(ctx, restClient); err != nil {
		return err
	}
	for _, server := range servers {
		for _, group := range server.Groups {
			if err := waitForGroup(ctx, aggregatorClient, group); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitForHealthz polls the API server's health endpoint until it answers
// or the context expires.
func waitForHealthz(ctx context.Context, client rest.Interface) error {
	err := wait.PollUntilContextCancel(ctx, apiPollInterval, true, func(ctx context.Context) (bool, error) {
		_, err := client.Get().AbsPath("/healthz").DoRaw(ctx)
		return err == nil, nil
	})
	if err != nil {
		return fmt.Errorf("wait for the API server's health endpoint: %w", ctx.Err())
	}
	return nil
}

// waitForGroup polls one aggregated API group's availability, as the
// aggregation layer reports it, until the group is available or the
// context expires.
func waitForGroup(ctx context.Context, client aggregatorclient.Interface, group string) error {
	name := fmt.Sprintf("v1.%s.openshift.io", group)
	err := wait.PollUntilContextCancel(ctx, apiPollInterval, true, func(ctx context.Context) (bool, error) {
		service, err := client.ApiregistrationV1().APIServices().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, condition := range service.Status.Conditions {
			if condition.Type == v1.Available {
				return condition.Status == v1.ConditionTrue, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("wait for the %s API: %w", name, ctx.Err())
	}
	return nil
}

// seedWithRetry keeps retrying registration until the context expires.
// Registration is idempotent, and the apiserver can reject the first writes
// while it warms up (for example cluster-IP allocation until the allocator
// repair pass runs).
func seedWithRetry(ctx context.Context, kubeClient kubernetes.Interface, aggregatorClient aggregatorclient.Interface, servers []Server, caBundle []byte) error {
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
		lastErr = seed(ctx, kubeClient, aggregatorClient, servers, caBundle)
		return lastErr == nil, nil
	})
	if pollErr != nil {
		if lastErr == nil {
			return fmt.Errorf("register aggregated servers: %w", ctx.Err())
		}
		return fmt.Errorf("register aggregated servers: %w", lastErr)
	}
	return nil
}

func seed(ctx context.Context, kubeClient kubernetes.Interface, aggregatorClient aggregatorclient.Interface, servers []Server, caBundle []byte) error {
	for _, server := range servers {
		if err := ensureService(ctx, kubeClient, server); err != nil {
			return err
		}
		if err := ensureEndpointSlice(ctx, kubeClient, server); err != nil {
			return err
		}
		for _, group := range server.Groups {
			if err := ensureAPIService(ctx, aggregatorClient, server, group, caBundle); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitForAPI polls a cheap authenticated request until the main endpoint
// answers or the context expires.
func waitForAPI(ctx context.Context, client kubernetes.Interface) error {
	err := wait.PollUntilContextCancel(ctx, apiPollInterval, true, func(ctx context.Context) (bool, error) {
		_, err := client.CoreV1().Namespaces().Get(ctx, "default", metav1.GetOptions{})
		return err == nil, nil
	})
	if err != nil {
		return fmt.Errorf("wait for kube-apiserver to answer requests: %w", ctx.Err())
	}
	return nil
}

// ensureService creates the selectorless service backing the server's
// APIServices. It is intentionally left untouched when it already exists:
// nothing about it changes between runs.
func ensureService(ctx context.Context, client kubernetes.Interface, server Server) error {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: server.Name, Namespace: aggregationNamespace},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:     servicePortName,
				Port:     servicePort,
				Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	_, err := client.CoreV1().Services(aggregationNamespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("register %s service: %w", server.Name, err)
	}
	return nil
}

// ensureEndpointSlice routes the service to the server's host address. It
// is updated when it already exists because the host address can change
// between runs.
func ensureEndpointSlice(ctx context.Context, client kubernetes.Interface, server Server) error {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      server.Name,
			Namespace: aggregationNamespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: server.Name},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{server.Address},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name:     ptr.To(servicePortName),
			Port:     ptr.To(int32(server.Port)),
			Protocol: ptr.To(corev1.ProtocolTCP),
		}},
	}

	slices := client.DiscoveryV1().EndpointSlices(aggregationNamespace)
	existing, err := slices.Get(ctx, server.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = slices.Create(ctx, slice, metav1.CreateOptions{})
	} else if err == nil {
		slice.ResourceVersion = existing.ResourceVersion
		_, err = slices.Update(ctx, slice, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("register %s endpoints: %w", server.Name, err)
	}
	return nil
}

// ensureAPIService points one API group at the server's service. Unlike the
// service objects it is updated when it already exists, because the CA
// bundle rotates with the generated CA. Priorities follow the values
// Hypershift's hosted cluster config operator registers.
func ensureAPIService(ctx context.Context, client aggregatorclient.Interface, server Server, group string, caBundle []byte) error {
	name := fmt.Sprintf("v1.%s.openshift.io", group)
	apiService := &v1.APIService{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.APIServiceSpec{
			Group:                fmt.Sprintf("%s.openshift.io", group),
			Version:              "v1",
			GroupPriorityMinimum: 9900,
			VersionPriority:      15,
			CABundle:             caBundle,
			Service: &v1.ServiceReference{
				Namespace: aggregationNamespace,
				Name:      server.Name,
				Port:      ptr.To(int32(servicePort)),
			},
		},
	}

	apiServices := client.ApiregistrationV1().APIServices()
	existing, err := apiServices.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = apiServices.Create(ctx, apiService, metav1.CreateOptions{})
	} else if err == nil {
		apiService.ResourceVersion = existing.ResourceVersion
		_, err = apiServices.Update(ctx, apiService, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("register %s: %w", name, err)
	}
	return nil
}
