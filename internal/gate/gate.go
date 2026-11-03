// Package gate holds a supervised child's start until the objects it
// needs exist. Whatever applies them — the seed layer's post-readiness
// work today, the version operator's manifest application once it owns
// them — the gate only polls for the objects themselves, so it passes
// the moment they are there and holds the start until then.
package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// Object is one thing a gated child waits to exist.
type Object struct {
	resource schema.GroupVersionResource
	name     string
	kind     string
}

// Definition returns the wait for one API definition.
func Definition(name string) Object {
	return Object{
		resource: schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
		name:     name,
		kind:     "definition",
	}
}

// ClusterRole returns the wait for one cluster role.
func ClusterRole(name string) Object {
	return Object{
		resource: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
		name:     name,
		kind:     "cluster role",
	}
}

// ClusterRoleBinding returns the wait for one cluster role binding.
func ClusterRoleBinding(name string) Object {
	return Object{
		resource: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
		name:     name,
		kind:     "cluster role binding",
	}
}

// ClusterOperator returns the wait for one cluster operator object.
func ClusterOperator(name string) Object {
	return Object{
		resource: schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "clusteroperators"},
		name:     name,
		kind:     "cluster operator",
	}
}

// waitTimeout bounds the whole wait. The version operator's manifest
// application is what the wait usually sits on; on a cold boot its
// gated objects arrive within about a minute of the API server
// answering, so the budget of ten minutes sits an order of magnitude
// beyond the observed delivery. A variable for test override.
var waitTimeout = 10 * time.Minute

// pollInterval spaces out the existence checks. A variable for test
// override.
var pollInterval = 2 * time.Second

// Wait polls until every object exists, printing one line while it
// waits. It returns as soon as the objects are there, whatever applied
// them; when they never arrive within the timeout, it fails naming them
// and the version operator, the deliverer to look at.
func Wait(ctx context.Context, kubeconfigPath, waitingFor, operatorLog string, objects ...Object) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}
	return poll(ctx, client, waitingFor, operatorLog, objects...)
}

// poll is Wait against an existing client.
func poll(ctx context.Context, client dynamic.Interface, waitingFor, operatorLog string, objects ...Object) error {
	ctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()

	announced := false
	var missing []Object
	pollErr := wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		missing = nil
		for _, object := range objects {
			if _, err := client.Resource(object.resource).Get(ctx, object.name, metav1.GetOptions{}); err != nil {
				missing = append(missing, object)
			}
		}
		if len(missing) == 0 {
			return true, nil
		}
		if !announced {
			announced = true
			fmt.Printf("Waiting for %s\n", waitingFor)
		}
		return false, nil
	})
	if pollErr == nil {
		return nil
	}
	if errors.Is(pollErr, context.Canceled) {
		// Shutdown was requested mid-wait; the supervisor reports that.
		return pollErr
	}
	var absent []string
	for _, object := range missing {
		absent = append(absent, fmt.Sprintf("the %s %s", object.kind, object.name))
	}
	return fmt.Errorf("%s never arrived within %s; the version operator delivers them; full log: %s",
		strings.Join(absent, ", "), waitTimeout, operatorLog)
}
