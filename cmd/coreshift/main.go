package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/vkareh/coreshift/internal/certgen"
	"github.com/vkareh/coreshift/internal/supervisor"
	"github.com/vkareh/coreshift/pkg/image"
)

var version = "dev"

const DefaultReleaseImage = "quay.io/openshift-release-dev/ocp-release:4.22.8-x86_64"

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:     "coreshift",
		Short:   "Run a lightweight OpenShift control plane",
		Version: version,
	}
	root.AddCommand(newAPICommand())
	return root
}

func newAPICommand() *cobra.Command {
	var pullSecretPath string
	cmd := &cobra.Command{
		Use:   "api",
		Short: "Run the OpenShift API control plane",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAPI(cmd.Context(), pullSecretPath)
		},
	}
	cmd.Flags().StringVar(&pullSecretPath, "pull-secret", "", "path to a Red Hat pull secret with credentials for the release image registry (required)")
	cmd.MarkFlagRequired("pull-secret")
	return cmd
}

func runAPI(ctx context.Context, pullSecretPath string) error {
	stateDir, err := apiStateDir()
	if err != nil {
		return err
	}
	if err := ensureCertificates(stateDir); err != nil {
		return err
	}

	pullSecret, err := os.ReadFile(pullSecretPath)
	if err != nil {
		return fmt.Errorf("read pull secret: %w", err)
	}
	client, err := image.NewClient(pullSecret)
	if err != nil {
		return fmt.Errorf("create image client: %w", err)
	}

	releaseFilesystem, err := os.MkdirTemp("", "coreshift-api-release-")
	if err != nil {
		return fmt.Errorf("create release image directory: %w", err)
	}
	defer os.RemoveAll(releaseFilesystem)
	if err := client.PullAndUnpack(ctx, DefaultReleaseImage, releaseFilesystem); err != nil {
		return fmt.Errorf("pull release image: %w", err)
	}

	binaries, err := os.MkdirTemp("", "coreshift-api-bin-")
	if err != nil {
		return fmt.Errorf("create binary directory: %w", err)
	}
	defer os.RemoveAll(binaries)
	etcdPath, err := extractEtcd(ctx, client, releaseFilesystem, binaries)
	if err != nil {
		return err
	}
	kubeBinaries, err := client.ExtractKubeComponents(ctx, releaseFilesystem, binaries)
	if err != nil {
		return fmt.Errorf("extract kube components: %w", err)
	}

	schedulerConfig, err := writeSchedulerConfig(stateDir)
	if err != nil {
		return err
	}

	// Flag sets verified against the 4.22.8 payload: kube-apiserver reaches
	// readiness with exactly the flags below; the controllers stay up with
	// service-account credentials and leader election off.
	processes := []supervisor.Process{
		{
			Name: "etcd",
			Path: etcdPath,
			Args: []string{
				"--data-dir=" + filepath.Join(stateDir, "etcd-data"),
				"--listen-client-urls=http://127.0.0.1:2379",
				"--advertise-client-urls=http://127.0.0.1:2379",
				"--listen-peer-urls=http://127.0.0.1:2380",
			},
		},
		{
			Name: "kube-apiserver",
			Path: kubeBinaries["kube-apiserver"],
			Args: []string{
				"--etcd-servers=http://127.0.0.1:2379",
				"--secure-port=6443",
				"--bind-address=127.0.0.1",
				"--tls-cert-file=" + filepath.Join(stateDir, certgen.APIServerCertFilename),
				"--tls-private-key-file=" + filepath.Join(stateDir, certgen.APIServerKeyFilename),
				"--client-ca-file=" + filepath.Join(stateDir, certgen.CACertFilename),
				"--service-account-issuer=https://kubernetes.default.svc.cluster.local",
				"--service-account-signing-key-file=" + filepath.Join(stateDir, certgen.SAKeyFilename),
				"--service-account-key-file=" + filepath.Join(stateDir, certgen.SAPubFilename),
				// OpenShift's service CIDR.
				"--service-cluster-ip-range=172.30.0.0/16",
				"--authorization-mode=RBAC",
			},
		},
		{
			Name: "kube-controller-manager",
			Path: kubeBinaries["kube-controller-manager"],
			Args: []string{
				"--kubeconfig=" + filepath.Join(stateDir, certgen.ControllerManagerKubeconfigFilename),
				// Bootstrap RBAC grants controller permissions to
				// per-controller service accounts, not to the component's
				// client identity.
				"--use-service-account-credentials=true",
				// Single instance, no lease to elect.
				"--leader-elect=false",
			},
		},
		{
			Name: "kube-scheduler",
			Path: kubeBinaries["kube-scheduler"],
			Args: []string{
				"--config=" + schedulerConfig,
			},
		},
	}

	return (&supervisor.Supervisor{}).Run(ctx, processes)
}

// apiStateDir returns the XDG state directory holding the generated
// certificates and etcd data, so they survive across runs.
func apiStateDir() (string, error) {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "coreshift", "api"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "coreshift", "api"), nil
}

// ensureCertificates generates the certificate and kubeconfig set into
// stateDir unless a complete set is already present. Reuse matters: a fresh
// CA on every run would invalidate kubeconfigs saved outside the state
// directory and leave etcd data trusted by a CA nothing signs for anymore.
func ensureCertificates(stateDir string) error {
	artifacts := []string{
		certgen.CACertFilename, certgen.CAKeyFilename,
		certgen.APIServerCertFilename, certgen.APIServerKeyFilename,
		certgen.AdminCertFilename, certgen.AdminKeyFilename,
		certgen.AdminKubeconfigFilename,
		certgen.ControllerManagerKubeconfigFilename,
		certgen.SchedulerKubeconfigFilename,
		certgen.SAKeyFilename, certgen.SAPubFilename,
	}
	complete := true
	for _, name := range artifacts {
		info, err := os.Stat(filepath.Join(stateDir, name))
		if err != nil || info.Size() == 0 {
			complete = false
			break
		}
	}
	if complete {
		return nil
	}
	return certgen.GenerateAll(stateDir, certgen.Options{})
}

// extractEtcd resolves the etcd image from an unpacked release image,
// pulls it, and extracts the etcd binary into targetDirectory.
func extractEtcd(ctx context.Context, client *image.Client, releaseFilesystem, targetDirectory string) (string, error) {
	etcdImage, err := image.ResolveComponentImage(releaseFilesystem, "etcd")
	if err != nil {
		return "", fmt.Errorf("resolve etcd image: %w", err)
	}

	filesystem, err := os.MkdirTemp("", "coreshift-api-image-")
	if err != nil {
		return "", fmt.Errorf("create image directory: %w", err)
	}
	defer os.RemoveAll(filesystem)
	if err := client.PullAndUnpack(ctx, etcdImage, filesystem); err != nil {
		return "", fmt.Errorf("pull etcd image: %w", err)
	}
	if err := image.ExtractBinary(filesystem, "/usr/bin/etcd", targetDirectory); err != nil {
		return "", fmt.Errorf("extract etcd: %w", err)
	}
	return filepath.Join(targetDirectory, "etcd"), nil
}

// writeSchedulerConfig writes kube-scheduler's component configuration
// pointing at its generated kubeconfig. Plain --kubeconfig is deprecated at
// this payload's Kubernetes version, so the component config is the stable
// way to pass client credentials.
func writeSchedulerConfig(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "kube-scheduler-config.yaml")
	config := fmt.Sprintf(`apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: %s
leaderElection:
  leaderElect: false
`, filepath.Join(stateDir, certgen.SchedulerKubeconfigFilename))
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return "", fmt.Errorf("write kube-scheduler config: %w", err)
	}
	return path, nil
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}
