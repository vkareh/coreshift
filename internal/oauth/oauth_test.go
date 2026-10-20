package oauth

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// captureLogs redirects the package's log output for one test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func oauthObject(t *testing.T, idps ...interface{}) *unstructured.Unstructured {
	t.Helper()
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "config.openshift.io/v1",
		"kind":       "OAuth",
		"metadata":   map[string]interface{}{"name": oauthName},
		"spec":       map[string]interface{}{"identityProviders": idps},
	}}
}

func htpasswdProvider(name, secretName string) map[string]interface{} {
	return map[string]interface{}{
		"name": name,
		"type": "HTPasswd",
		"htpasswd": map[string]interface{}{
			"fileData": map[string]interface{}{"name": secretName},
		},
	}
}

func htpasswdSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: secretNamespace},
		Data:       data,
	}
}

func TestRenderWritesHtpasswdProvider(t *testing.T) {
	oauth := oauthObject(t, htpasswdProvider("developers", "users"))
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), oauth)
	kubeClient := fake.NewSimpleClientset(htpasswdSecret("users", map[string][]byte{
		htpasswdSecretKey: []byte("dev:$2y$05$hash"),
	}))
	stateDir := t.TempDir()

	providers, err := render(context.Background(), dynamicClient, kubeClient, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 {
		t.Fatalf("got %d providers, want 1: %+v", len(providers), providers)
	}
	want := Provider{Name: "developers", MappingMethod: "claim", File: filepath.Join(stateDir, "htpasswd-0")}
	if providers[0] != want {
		t.Fatalf("got provider %+v, want %+v", providers[0], want)
	}
	data, err := os.ReadFile(want.File)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "dev:$2y$05$hash" {
		t.Fatalf("htpasswd file holds %q", data)
	}
}

func TestRenderKeepsConfiguredMappingMethod(t *testing.T) {
	idp := htpasswdProvider("developers", "users")
	idp["mappingMethod"] = "generate"
	oauth := oauthObject(t, idp)
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), oauth)
	kubeClient := fake.NewSimpleClientset(htpasswdSecret("users", map[string][]byte{
		htpasswdSecretKey: []byte("dev:$2y$05$hash"),
	}))

	providers, err := render(context.Background(), dynamicClient, kubeClient, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].MappingMethod != "generate" {
		t.Fatalf("mapping method not carried over: %+v", providers)
	}
}

func TestRenderWithoutOAuthObject(t *testing.T) {
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())

	providers, err := render(context.Background(), dynamicClient, fake.NewSimpleClientset(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 {
		t.Fatalf("got %d providers, want 0: %+v", len(providers), providers)
	}
}

func TestRenderSkipsUnsupportedProviderType(t *testing.T) {
	logs := captureLogs(t)
	oauth := oauthObject(t, map[string]interface{}{"name": "github", "type": "GitHub"})
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), oauth)

	providers, err := render(context.Background(), dynamicClient, fake.NewSimpleClientset(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 {
		t.Fatalf("got %d providers, want 0: %+v", len(providers), providers)
	}
	if !strings.Contains(logs.String(), "github") || !strings.Contains(logs.String(), "not supported") {
		t.Fatalf("missing skip message: %q", logs.String())
	}
}

func TestRenderSkipsProviderWithMissingSecret(t *testing.T) {
	logs := captureLogs(t)
	oauth := oauthObject(t, htpasswdProvider("developers", "absent"))
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), oauth)

	providers, err := render(context.Background(), dynamicClient, fake.NewSimpleClientset(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 {
		t.Fatalf("got %d providers, want 0: %+v", len(providers), providers)
	}
	if !strings.Contains(logs.String(), "absent") || !strings.Contains(logs.String(), "not found") {
		t.Fatalf("missing skip message: %q", logs.String())
	}
}

func TestRenderSkipsProviderWithMissingSecretKey(t *testing.T) {
	logs := captureLogs(t)
	oauth := oauthObject(t, htpasswdProvider("developers", "wrong-key"))
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), oauth)
	kubeClient := fake.NewSimpleClientset(htpasswdSecret("wrong-key", map[string][]byte{
		"other": []byte("dev:$2y$05$hash"),
	}))

	providers, err := render(context.Background(), dynamicClient, kubeClient, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 {
		t.Fatalf("got %d providers, want 0: %+v", len(providers), providers)
	}
	if !strings.Contains(logs.String(), "htpasswd") || !strings.Contains(logs.String(), "no") {
		t.Fatalf("missing skip message: %q", logs.String())
	}
}
