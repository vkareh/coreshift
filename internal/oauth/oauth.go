// Package oauth renders the cluster's identity providers into the OAuth
// server's configuration. In real clusters the authentication operator
// watches the OAuth object and rewrites the server's config whenever it
// changes; this package does the equivalent once, before the server starts.
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// The OAuth object is served through the definition the startup seeds and
// is always named "cluster". Its identity providers reference secrets by
// name, and the API documents openshift-config as their namespace, so the
// standard OpenShift instructions for creating them apply.
const (
	oauthName         = "cluster"
	secretNamespace   = "openshift-config"
	htpasswdSecretKey = "htpasswd"
)

var oauthResource = schema.GroupVersionResource{
	Group:    "config.openshift.io",
	Version:  "v1",
	Resource: "oauths",
}

// identityProvider mirrors the fields of an OAuth object identity provider
// that this package reads.
type identityProvider struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	MappingMethod string `json:"mappingMethod"`
	HTPasswd      *struct {
		FileData struct {
			Name string `json:"name"`
		} `json:"fileData"`
	} `json:"htpasswd"`
}

// Provider is one htpasswd identity provider rendered into the OAuth
// server's configuration.
type Provider struct {
	Name          string
	MappingMethod string
	File          string
}

// Render reads the cluster's OAuth object and returns the htpasswd identity
// providers it names, with each referenced secret's file data written into
// stateDir. With no OAuth object, or none with identity providers, there is
// nothing to render. Providers of other types, and providers whose secret
// or secret key is missing, are logged and skipped, following the API's
// documented behavior for them.
func Render(ctx context.Context, kubeconfigPath, stateDir string) ([]Provider, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create kube client: %w", err)
	}
	return render(ctx, dynamicClient, kubeClient, stateDir)
}

func render(ctx context.Context, dynamicClient dynamic.Interface, kubeClient kubernetes.Interface, stateDir string) ([]Provider, error) {
	oauth, err := dynamicClient.Resource(oauthResource).Get(ctx, oauthName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read OAuth object: %w", err)
	}

	var spec struct {
		IdentityProviders []identityProvider `json:"identityProviders"`
	}
	if raw, err := json.Marshal(oauth.Object["spec"]); err != nil {
		return nil, fmt.Errorf("parse OAuth object: %w", err)
	} else if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parse OAuth object: %w", err)
	}

	var providers []Provider
	for i, idp := range spec.IdentityProviders {
		if idp.Type != "HTPasswd" {
			log.Printf("oauth: ignoring identity provider %q: type %q is not supported", idp.Name, idp.Type)
			continue
		}
		if idp.HTPasswd == nil {
			log.Printf("oauth: ignoring identity provider %q: no htpasswd configuration", idp.Name)
			continue
		}
		data, err := htpasswdData(ctx, kubeClient, idp.Name, idp.HTPasswd.FileData.Name)
		if err != nil {
			return nil, err
		}
		if data == nil {
			continue
		}
		// Providers qualify identities by name; the list position is what
		// tells their files apart on disk.
		file := filepath.Join(stateDir, fmt.Sprintf("htpasswd-%d", i))
		if err := os.WriteFile(file, data, 0o600); err != nil {
			return nil, fmt.Errorf("write htpasswd file for provider %q: %w", idp.Name, err)
		}
		mappingMethod := idp.MappingMethod
		if mappingMethod == "" {
			mappingMethod = "claim"
		}
		providers = append(providers, Provider{Name: idp.Name, MappingMethod: mappingMethod, File: file})
	}
	return providers, nil
}

// htpasswdData returns the secret's file data, or nil when the secret or
// its key is missing, in which case the provider is not honored per the
// API's documented behavior and the caller skips it.
func htpasswdData(ctx context.Context, kubeClient kubernetes.Interface, providerName, secretName string) ([]byte, error) {
	secret, err := kubeClient.CoreV1().Secrets(secretNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		log.Printf("oauth: ignoring identity provider %q: secret %q not found in namespace %q", providerName, secretName, secretNamespace)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read secret %s: %w", secretName, err)
	}
	data, ok := secret.Data[htpasswdSecretKey]
	if !ok {
		log.Printf("oauth: ignoring identity provider %q: secret %q has no %q key", providerName, secretName, htpasswdSecretKey)
		return nil, nil
	}
	return data, nil
}
