// Package pullsecret stores the cluster's global pull secret, the
// Secret a real cluster's operators read registry credentials from. The
// startup seeds it from the run's pull secret file, so commands that
// need the secret after the fact, above all the worker image build,
// read it from the cluster instead of asking for the file again.
package pullsecret

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// The global pull secret's place and shape, matching what a real
// cluster's operators read: a docker-config-json Secret named pull-secret
// in the namespace of the cluster's global configuration, holding the
// credentials file under the type's single standard key.
const (
	secretNamespace = "openshift-config"
	secretName      = "pull-secret"
)

// objectRetryInterval spaces out retries while kube-apiserver warms up or
// rejects early writes. A variable for test override.
var objectRetryInterval = 3 * time.Second

// Seed stores the pull secret as the cluster's global pull secret: the
// namespace it lives in, then the Secret itself. The writes are retried
// and idempotent, so it is safe to call while the API server is still
// coming up. An existing Secret is replaced, so a changed pull secret
// file reaches the cluster on the next run.
func Seed(ctx context.Context, kubeconfigPath string, pullSecret []byte) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create kube client: %w", err)
	}
	return seedWithRetry(ctx, client, pullSecret)
}

// seedWithRetry keeps retrying until the context expires. The apiserver
// can reject the first writes while it warms up.
func seedWithRetry(ctx context.Context, client kubernetes.Interface, pullSecret []byte) error {
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
		lastErr = seed(ctx, client, pullSecret)
		return lastErr == nil, nil
	})
	if pollErr != nil {
		if lastErr == nil {
			return fmt.Errorf("seed the pull secret: %w", ctx.Err())
		}
		return fmt.Errorf("seed the pull secret: %w", lastErr)
	}
	return nil
}

func seed(ctx context.Context, client kubernetes.Interface, pullSecret []byte) error {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: secretNamespace}}
	if _, err := client.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create the %s namespace: %w", secretNamespace, err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: pullSecret},
	}
	_, err := client.CoreV1().Secrets(secretNamespace).Create(ctx, secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = client.CoreV1().Secrets(secretNamespace).Update(ctx, secret, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("store the %s Secret: %w", secretName, err)
	}
	return nil
}

// Fetch returns the pull secret the cluster the admin kubeconfig points
// at stores, or nil when it stores none, the shape a cluster created
// before the Secret existed has.
func Fetch(ctx context.Context, kubeconfigPath string) ([]byte, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("read admin kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create admin client: %w", err)
	}
	return fetch(ctx, client)
}

func fetch(ctx context.Context, client kubernetes.Interface) ([]byte, error) {
	secret, err := client.CoreV1().Secrets(secretNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the cluster's pull secret: %w", err)
	}
	data, ok := secret.Data[corev1.DockerConfigJsonKey]
	if !ok {
		return nil, fmt.Errorf("the cluster's %s Secret carries no %s key", secretName, corev1.DockerConfigJsonKey)
	}
	return data, nil
}
