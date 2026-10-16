package certgen

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

// TestGenerateAllSmoke is an end-to-end sanity check of the full artifact
// set, independent of the per-artifact unit tests.
func TestGenerateAllSmoke(t *testing.T) {
	const serverURL = "https://coreshift-smoke.test:6443"
	outputDir := t.TempDir()

	if err := GenerateAll(outputDir, Options{APIServerURL: serverURL}); err != nil {
		t.Fatal(err)
	}

	artifacts := []string{
		CACertFilename, CAKeyFilename,
		APIServerCertFilename, APIServerKeyFilename,
		FrontProxyCACertFilename, FrontProxyCAKeyFilename,
		FrontProxyClientCertFilename, FrontProxyClientKeyFilename,
		ServiceServingCertFilename, ServiceServingKeyFilename,
		AdminCertFilename, AdminKeyFilename,
		AdminKubeconfigFilename,
		ControllerManagerKubeconfigFilename,
		SchedulerKubeconfigFilename,
		OpenShiftAPIServerKubeconfigFilename,
		OAuthAPIServerKubeconfigFilename,
		SAKeyFilename, SAPubFilename,
	}
	for _, name := range artifacts {
		info, err := os.Stat(filepath.Join(outputDir, name))
		if err != nil {
			t.Fatalf("expected %s to be written: %v", name, err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", name)
		}
	}

	for _, name := range []string{CACertFilename, APIServerCertFilename, FrontProxyCACertFilename, FrontProxyClientCertFilename, ServiceServingCertFilename, AdminCertFilename} {
		parseCertificate(t, readFile(t, outputDir, name))
	}

	for _, name := range []string{AdminKubeconfigFilename, ControllerManagerKubeconfigFilename, SchedulerKubeconfigFilename, OpenShiftAPIServerKubeconfigFilename, OAuthAPIServerKubeconfigFilename} {
		config, err := clientcmd.Load(readFile(t, outputDir, name))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if err := clientcmd.ConfirmUsable(*config, ""); err != nil {
			t.Fatalf("%s is not a usable kubeconfig: %v", name, err)
		}
		cluster := config.Clusters["coreshift"]
		if cluster == nil || cluster.Server != serverURL {
			t.Fatalf("unexpected cluster config in %s: %#v", name, cluster)
		}
	}
}
