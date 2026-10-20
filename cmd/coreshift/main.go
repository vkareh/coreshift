package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/vkareh/coreshift/internal/aggregation"
	"github.com/vkareh/coreshift/internal/certgen"
	"github.com/vkareh/coreshift/internal/crd"
	"github.com/vkareh/coreshift/internal/oauth"
	"github.com/vkareh/coreshift/internal/oauthclient"
	"github.com/vkareh/coreshift/internal/rbac"
	"github.com/vkareh/coreshift/internal/scc"
	"github.com/vkareh/coreshift/internal/supervisor"
	"github.com/vkareh/coreshift/pkg/image"
)

var version = "dev"

const DefaultReleaseImage = "quay.io/openshift-release-dev/ocp-release:4.22.8-x86_64"

// Local ports of the aggregated API servers. kube-apiserver reaches them
// through the aggregation layer; the ports appear both in the server flags
// and in the endpoints the aggregation wiring registers.
const (
	openShiftAPIServerPort         = 8443
	oauthAPIServerPort             = 8444
	oauthServerPort                = 8445
	openShiftControllerManagerPort = 8446
	clusterPolicyControllerPort    = 10357
)

// aggregatedServers lists the aggregated API servers and the API groups
// each one serves, without the openshift.io suffix. kube-apiserver proxies
// these groups to the server named in each entry. The addresses are filled
// in at startup; see runAPI.
var aggregatedServers = []aggregation.Server{
	{
		Name:   "openshift-apiserver",
		Port:   openShiftAPIServerPort,
		Groups: []string{"apps", "authorization", "build", "image", "quota", "route", "security", "template", "project"},
	},
	{
		Name:   "openshift-oauth-apiserver",
		Port:   oauthAPIServerPort,
		Groups: []string{"oauth", "user"},
	},
}

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

	releaseRoot, err := client.CachedRelease(ctx, DefaultReleaseImage)
	if err != nil {
		return fmt.Errorf("prepare release image: %w", err)
	}
	etcdPath, err := client.ExtractEtcd(ctx, releaseRoot)
	if err != nil {
		return err
	}
	kubeBinaries, err := client.ExtractKubeComponents(ctx, releaseRoot)
	if err != nil {
		return fmt.Errorf("extract kube components: %w", err)
	}
	openShiftBinaries, err := client.ExtractOpenShiftComponents(ctx, releaseRoot,
		"openshift-apiserver", "oauth-apiserver", "oauth-server", "openshift-controller-manager", "cluster-policy-controller")
	if err != nil {
		return fmt.Errorf("extract openshift components: %w", err)
	}

	schedulerConfig, err := writeSchedulerConfig(stateDir)
	if err != nil {
		return err
	}

	// Kubernetes validation rejects loopback endpoint addresses, so the
	// aggregated servers must listen on the host's routable address, the
	// same one kube-apiserver advertises for the kubernetes service.
	advertiseAddress, err := defaultRouteAddress()
	if err != nil {
		return err
	}
	for i := range aggregatedServers {
		aggregatedServers[i].Address = advertiseAddress
	}

	openShiftAPIServerConfig, err := writeOpenShiftAPIServerConfig(stateDir, advertiseAddress)
	if err != nil {
		return err
	}

	oauthServerConfig, err := writeOAuthServerConfig(stateDir, nil)
	if err != nil {
		return err
	}

	oauthMetadataFile, err := writeOAuthMetadata(stateDir)
	if err != nil {
		return err
	}

	tokenReviewKubeconfig, err := writeTokenReviewKubeconfig(stateDir)
	if err != nil {
		return err
	}

	kubeAPIServerConfig, err := writeKubeAPIServerConfig(stateDir, oauthMetadataFile)
	if err != nil {
		return err
	}

	openShiftControllerManagerConfig, err := writeOpenShiftControllerManagerConfig(stateDir)
	if err != nil {
		return err
	}
	clusterPolicyControllerConfig, err := writeClusterPolicyControllerConfig(stateDir)
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
				// OpenShift mode stops kube-apiserver from implicitly
				// trusting the system:masters group; the authorizers real
				// clusters run restore that trust explicitly.
				"--authorization-mode=Scope,SystemMasters,RBAC,Node",
				// Delegated authentication for the aggregation layer: the
				// requestheader flags publish how proxied requests carry user
				// identity, and the proxy client flags identify kube-apiserver
				// when forwarding to aggregated servers. Aggregator routing
				// sends proxied traffic to endpoints instead of cluster IPs.
				"--requestheader-client-ca-file=" + filepath.Join(stateDir, certgen.FrontProxyCACertFilename),
				"--requestheader-allowed-names=kube-apiserver-proxy,system:kube-apiserver-proxy,system:openshift-aggregator",
				"--requestheader-username-headers=X-Remote-User",
				"--requestheader-group-headers=X-Remote-Group",
				"--requestheader-extra-headers-prefix=X-Remote-Extra-",
				"--proxy-client-cert-file=" + filepath.Join(stateDir, certgen.FrontProxyClientCertFilename),
				"--proxy-client-key-file=" + filepath.Join(stateDir, certgen.FrontProxyClientKeyFilename),
				"--enable-aggregator-routing=true",
			// The OpenShift config switches the forked kube-apiserver
			// into OpenShift mode, which among other things serves the
			// OAuth discovery document clients like oc need to log in.
			// The flag is hidden and marked deprecated upstream, but it
			// is how every real deployment, including Hypershift's,
			// runs the server.
			"--openshift-config=" + kubeAPIServerConfig,
			// Access tokens issued by the OAuth server are only
			// validated by the aggregated OAuth API server; this webhook
			// is how kube-apiserver asks it to review them. Real
			// clusters carry the same wiring from their operators.
			"--authentication-token-webhook-config-file=" + tokenReviewKubeconfig,
			"--authentication-token-webhook-version=v1",
			},
		AfterStart: func(ctx context.Context) error {
			// The aggregated servers need kube-apiserver up for delegated
			// auth, the aggregation wiring needs it to accept writes, and
			// the controller permissions and the range allocation API
			// must exist before the cluster policy controller starts
			// calling. Bound the wait so a broken kube-apiserver fails
			// the run instead of blocking startup forever.
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			kubeconfig := filepath.Join(stateDir, certgen.AdminKubeconfigFilename)
			if err := aggregation.Seed(ctx, kubeconfig, aggregatedServers, filepath.Join(stateDir, certgen.CACertFilename)); err != nil {
				return err
			}
			if err := rbac.Seed(ctx, kubeconfig); err != nil {
				return err
			}
			return crd.Seed(ctx, kubeconfig)
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
		{
			Name: "openshift-apiserver",
			Path: openShiftBinaries["openshift-apiserver"],
			Args: []string{
				"start",
				"--config=" + openShiftAPIServerConfig,
				"--authentication-kubeconfig=" + filepath.Join(stateDir, certgen.OpenShiftAPIServerKubeconfigFilename),
				"--authorization-kubeconfig=" + filepath.Join(stateDir, certgen.OpenShiftAPIServerKubeconfigFilename),
				// The requestheader flags mirror kube-apiserver's, so
				// proxied requests are authenticated by their front-proxy
				// client certificate and headers.
				"--requestheader-client-ca-file=" + filepath.Join(stateDir, certgen.FrontProxyCACertFilename),
				"--requestheader-allowed-names=kube-apiserver-proxy,system:kube-apiserver-proxy,system:openshift-aggregator",
				"--requestheader-username-headers=X-Remote-User",
				"--requestheader-group-headers=X-Remote-Group",
				"--requestheader-extra-headers-prefix=X-Remote-Extra-",
				"--client-ca-file=" + filepath.Join(stateDir, certgen.CACertFilename),
			},
			// The default security context constraints are stored through
			// the security API this server serves; they are created once it
			// answers, so the controllers that start later, above all the
			// cluster policy controller, see them from the beginning. Bound
			// the wait so a broken server fails the run instead of blocking
			// startup forever.
			AfterStart: func(ctx context.Context) error {
				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				return scc.Seed(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename))
			},
		},
		{
			Name: "openshift-oauth-apiserver",
			Path: openShiftBinaries["oauth-apiserver"],
			Args: []string{
				"start",
				"--secure-port=" + fmt.Sprint(oauthAPIServerPort),
				"--bind-address=" + advertiseAddress,
				"--kubeconfig=" + filepath.Join(stateDir, certgen.OAuthAPIServerKubeconfigFilename),
				"--authentication-kubeconfig=" + filepath.Join(stateDir, certgen.OAuthAPIServerKubeconfigFilename),
				"--authorization-kubeconfig=" + filepath.Join(stateDir, certgen.OAuthAPIServerKubeconfigFilename),
				"--etcd-servers=http://127.0.0.1:2379",
				"--api-audiences=https://kubernetes.default.svc.cluster.local",
				"--tls-cert-file=" + filepath.Join(stateDir, certgen.ServiceServingCertFilename),
				"--tls-private-key-file=" + filepath.Join(stateDir, certgen.ServiceServingKeyFilename),
				"--requestheader-client-ca-file=" + filepath.Join(stateDir, certgen.FrontProxyCACertFilename),
				"--requestheader-allowed-names=kube-apiserver-proxy,system:kube-apiserver-proxy,system:openshift-aggregator",
				"--requestheader-username-headers=X-Remote-User",
				"--requestheader-group-headers=X-Remote-Group",
				"--requestheader-extra-headers-prefix=X-Remote-Extra-",
			"--client-ca-file=" + filepath.Join(stateDir, certgen.CACertFilename),
		},
		// The login client is stored through the OAuth API this server
		// serves; it is created once the server answers, so logins have
		// a client to talk to from the first attempt. Bound the wait so
		// a broken server fails the run instead of blocking startup
		// forever.
		AfterStart: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			return oauthclient.Seed(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename))
		},
	},
	// oauth-server hands login flows to the identity providers in its
		// config; with none configured it serves a deny-all provider, so it
		// starts and answers without cluster-side seed objects. Its API
		// calls, such as issuing tokens through the aggregated OAuth API
		// server, use the generated admin kubeconfig.
		{
			Name: "oauth-server",
			Path: openShiftBinaries["oauth-server"],
			Args: []string{
				"osinserver",
				"--config=" + oauthServerConfig,
			},
			// The server reads identity providers only from its config
			// file; in real clusters the authentication operator renders
			// the OAuth object's providers into it and restarts the
			// server. The same rendering runs here, once the API server
			// answers and before the server starts. Without any providers
			// the config keeps the deny-all-only content written above.
			BeforeStart: func(ctx context.Context) error {
				providers, err := oauth.Render(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename), stateDir)
				if err != nil {
					return err
				}
				_, err = writeOAuthServerConfig(stateDir, providers)
				return err
			},
		},
		// The controller managers run all their controllers against the API
		// server. They elect leaders through a lease in the given namespace;
		// the component namespaces Hypershift passes exist only in its
		// clusters, so they point at the always-present default namespace.
		// POD_NAMESPACE keeps each component's event owner reference in the
		// same namespace.
		{
			Name: "openshift-controller-manager",
			Path: openShiftBinaries["openshift-controller-manager"],
			Args: []string{
				"start",
				"--config=" + openShiftControllerManagerConfig,
				"--kubeconfig=" + filepath.Join(stateDir, certgen.OpenShiftControllerManagerKubeconfigFilename),
				"--namespace=default",
			},
			// The binary links against the gpgme libraries extracted
			// alongside it in its cache directory.
			Env: append(os.Environ(), "LD_LIBRARY_PATH="+filepath.Dir(openShiftBinaries["openshift-controller-manager"]), "POD_NAMESPACE=default"),
		},
		{
			Name: "cluster-policy-controller",
			Path: openShiftBinaries["cluster-policy-controller"],
			Args: []string{
				"start",
				"--config=" + clusterPolicyControllerConfig,
				"--kubeconfig=" + filepath.Join(stateDir, certgen.ClusterPolicyControllerKubeconfigFilename),
				"--namespace=default",
			},
			Env: append(os.Environ(), "POD_NAMESPACE=default"),
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
// A set whose service-serving certificate lacks the loopback address counts
// as incomplete, so it is regenerated rather than reused.
func ensureCertificates(stateDir string) error {
	artifacts := []string{
		certgen.CACertFilename, certgen.CAKeyFilename,
		certgen.APIServerCertFilename, certgen.APIServerKeyFilename,
		certgen.FrontProxyCACertFilename, certgen.FrontProxyCAKeyFilename,
		certgen.FrontProxyClientCertFilename, certgen.FrontProxyClientKeyFilename,
		certgen.ServiceServingCertFilename, certgen.ServiceServingKeyFilename,
		certgen.AdminCertFilename, certgen.AdminKeyFilename,
		certgen.AdminKubeconfigFilename,
		certgen.ControllerManagerKubeconfigFilename,
		certgen.SchedulerKubeconfigFilename,
		certgen.OpenShiftAPIServerKubeconfigFilename,
		certgen.OAuthAPIServerKubeconfigFilename,
		certgen.OAuthServerKubeconfigFilename,
		certgen.OpenShiftControllerManagerKubeconfigFilename,
		certgen.ClusterPolicyControllerKubeconfigFilename,
		certgen.SessionSecretsFilename,
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
	if complete && serviceServingCertHasLoopback(stateDir) {
		return nil
	}
	return certgen.GenerateAll(stateDir, certgen.Options{})
}

// serviceServingCertHasLoopback reports whether the saved service-serving
// certificate covers 127.0.0.1. Sets generated before that address was added
// to the certificate must be regenerated, or clients following the OAuth
// discovery document to the OAuth server fail TLS verification.
func serviceServingCertHasLoopback(stateDir string) bool {
	data, err := os.ReadFile(filepath.Join(stateDir, certgen.ServiceServingCertFilename))
	if err != nil {
		return false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	return cert.VerifyHostname("127.0.0.1") == nil
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

// writeOpenShiftAPIServerConfig writes openshift-apiserver's component
// configuration: serving on the advertise address with the shared
// service-serving certificate, storing to the local etcd, and
// authenticating as the generated admin identity. The empty etcd TLS flag
// overrides clear the in-pod client certificate paths that OpenShift's
// config defaults would otherwise fill in for plain-http etcd.
func writeOpenShiftAPIServerConfig(stateDir, advertiseAddress string) (string, error) {
	path := filepath.Join(stateDir, "openshift-apiserver-config.yaml")
	config := fmt.Sprintf(`apiVersion: openshiftcontrolplane.config.openshift.io/v1
kind: OpenShiftAPIServerConfig
servingInfo:
  bindAddress: %s:%d
  certFile: %s
  keyFile: %s
  clientCA: %s
apiServerArguments:
  etcd-certfile:
  - ""
  etcd-keyfile:
  - ""
  etcd-cafile:
  - ""
storageConfig:
  urls:
  - http://127.0.0.1:2379
kubeClientConfig:
  kubeConfig: %s
`, advertiseAddress, openShiftAPIServerPort,
		filepath.Join(stateDir, certgen.ServiceServingCertFilename),
		filepath.Join(stateDir, certgen.ServiceServingKeyFilename),
		filepath.Join(stateDir, certgen.CACertFilename),
		filepath.Join(stateDir, certgen.OpenShiftAPIServerKubeconfigFilename))
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return "", fmt.Errorf("write openshift-apiserver config: %w", err)
	}
	return path, nil
}

// writeOAuthServerConfig writes oauth-server's component configuration:
// serving on localhost with the shared service-serving certificate, calling
// the API server through the generated admin kubeconfig, and keeping login
// sessions with the generated session secrets. The grant handling and token
// lifetimes follow Hypershift's control-plane operator, which denies grants
// by default because the method must be chosen per OAuth client. The given
// htpasswd providers, if any, are rendered the way the authentication
// operator renders the OAuth object's providers in real clusters.
func writeOAuthServerConfig(stateDir string, providers []oauth.Provider) (string, error) {
	path := filepath.Join(stateDir, "oauth-server-config.yaml")
	identityProviders := "identityProviders: []"
	if len(providers) > 0 {
		stanzas := make([]string, 0, len(providers))
		for _, provider := range providers {
			stanzas = append(stanzas, fmt.Sprintf(`  - name: %s
    challenge: true
    login: true
    mappingMethod: %s
    provider:
      apiVersion: osin.config.openshift.io/v1
      kind: HTPasswdPasswordIdentityProvider
      file: %s
`, provider.Name, provider.MappingMethod, provider.File))
		}
		identityProviders = "identityProviders:\n" + strings.Join(stanzas, "")
	}
	config := fmt.Sprintf(`apiVersion: osin.config.openshift.io/v1
kind: OsinServerConfig
servingInfo:
  bindAddress: 127.0.0.1:%d
  certFile: %s
  keyFile: %s
  clientCA: %s
kubeClientConfig:
  kubeConfig: %s
oauthConfig:
  grantConfig:
    method: deny
    serviceAccountMethod: prompt
  %s
  masterCA: %s
  sessionConfig:
    sessionMaxAgeSeconds: 300
    sessionName: ssn
    sessionSecretsFile: %s
  tokenConfig:
    accessTokenMaxAgeSeconds: 86400
    authorizeTokenMaxAgeSeconds: 300
`, oauthServerPort,
		filepath.Join(stateDir, certgen.ServiceServingCertFilename),
		filepath.Join(stateDir, certgen.ServiceServingKeyFilename),
		filepath.Join(stateDir, certgen.CACertFilename),
		filepath.Join(stateDir, certgen.OAuthServerKubeconfigFilename),
		identityProviders,
		filepath.Join(stateDir, certgen.CACertFilename),
		filepath.Join(stateDir, certgen.SessionSecretsFilename))
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return "", fmt.Errorf("write oauth-server config: %w", err)
	}
	return path, nil
}

// writeOAuthMetadata writes the OAuth authorization server metadata that
// kube-apiserver serves at /.well-known/oauth-authorization-server. The
// issuer is the main API endpoint; the authorization and token endpoints are
// the OAuth server's. The remaining fields match what the installer writes
// in real clusters, so clients see the same document shape as in production.
func writeOAuthMetadata(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "oauth-metadata.json")
	metadata := fmt.Sprintf(`{
  "issuer": "https://127.0.0.1:6443",
  "authorization_endpoint": "https://127.0.0.1:%d/oauth/authorize",
  "token_endpoint": "https://127.0.0.1:%d/oauth/token",
  "scopes_supported": [
    "user:check-access",
    "user:full",
    "user:info",
    "user:list-projects",
    "user:list-scoped"
  ],
  "response_types_supported": ["code", "token"],
  "grant_types_supported": ["authorization_code", "implicit"],
  "code_challenge_methods_supported": ["plain", "S256"]
}
`, oauthServerPort, oauthServerPort)
	if err := os.WriteFile(path, []byte(metadata), 0o600); err != nil {
		return "", fmt.Errorf("write oauth metadata: %w", err)
	}
	return path, nil
}

// writeTokenReviewKubeconfig writes the kubeconfig kube-apiserver uses to
// have OAuth access tokens reviewed: it points at the aggregated OAuth API
// server's token review endpoint and authenticates with the generated
// admin identity, whose permissions cover the review call.
func writeTokenReviewKubeconfig(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "token-review.kubeconfig")
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: coreshift
  cluster:
    certificate-authority: %s
    server: https://127.0.0.1:6443/apis/oauth.openshift.io/v1/tokenreviews
contexts:
- name: admin@coreshift
  context:
    cluster: coreshift
    user: admin
current-context: admin@coreshift
users:
- name: admin
  user:
    client-certificate: %s
    client-key: %s
`, filepath.Join(stateDir, certgen.CACertFilename),
		filepath.Join(stateDir, certgen.AdminCertFilename),
		filepath.Join(stateDir, certgen.AdminKeyFilename))
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		return "", fmt.Errorf("write token review kubeconfig: %w", err)
	}
	return path, nil
}

// writeKubeAPIServerConfig writes the OpenShift kube-apiserver configuration.
// The forked kube-apiserver only serves the OAuth discovery document and the
// rest of its OpenShift behavior when given this file; the operator that
// manages kube-apiserver in real clusters and Hypershift write the same
// shape. The serving address, service CIDR, and service-account key mirror
// the command line, because the config loader defaults them to in-pod paths
// that do not exist here and its values take precedence over the flags.
func writeKubeAPIServerConfig(stateDir, oauthMetadataFile string) (string, error) {
	path := filepath.Join(stateDir, "kube-apiserver-config.json")
	config := fmt.Sprintf(`{
  "apiVersion": "kubecontrolplane.config.openshift.io/v1",
  "kind": "KubeAPIServerConfig",
  "servingInfo": {
    "bindAddress": "127.0.0.1:6443"
  },
  "servicesSubnet": "172.30.0.0/16",
  "serviceAccountPublicKeyFiles": [
    %q
  ],
  "authConfig": {
    "oauthMetadataFile": %q
  }
}
`, filepath.Join(stateDir, certgen.SAPubFilename), oauthMetadataFile)
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return "", fmt.Errorf("write kube-apiserver config: %w", err)
	}
	return path, nil
}

// writeOpenShiftControllerManagerConfig writes openshift-controller-
// manager's component configuration: serving its health endpoint on
// localhost with the shared service-serving certificate. The remaining
// fields follow Hypershift's control-plane operator; unset controller and
// image-template fields fall back to the component's built-in defaults.
func writeOpenShiftControllerManagerConfig(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "openshift-controller-manager-config.yaml")
	config := fmt.Sprintf(`apiVersion: openshiftcontrolplane.config.openshift.io/v1
kind: OpenShiftControllerManagerConfig
dockerPullSecret:
  internalRegistryHostname: image-registry.openshift-image-registry.svc:5000
leaderElection:
  name: openshift-master-controllers
servingInfo:
  bindAddress: 127.0.0.1:%d
  certFile: %s
  keyFile: %s
  clientCA: %s
`, openShiftControllerManagerPort,
		filepath.Join(stateDir, certgen.ServiceServingCertFilename),
		filepath.Join(stateDir, certgen.ServiceServingKeyFilename),
		filepath.Join(stateDir, certgen.CACertFilename))
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return "", fmt.Errorf("write openshift-controller-manager config: %w", err)
	}
	return path, nil
}

// writeClusterPolicyControllerConfig writes cluster-policy-controller's
// component configuration: the same shape openshift-controller-manager
// uses, serving its health endpoint on localhost. Unset fields fall back
// to the component's built-in defaults, which enable every controller.
func writeClusterPolicyControllerConfig(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "cluster-policy-controller-config.yaml")
	config := fmt.Sprintf(`apiVersion: openshiftcontrolplane.config.openshift.io/v1
kind: OpenShiftControllerManagerConfig
servingInfo:
  bindAddress: 127.0.0.1:%d
  certFile: %s
  keyFile: %s
  clientCA: %s
`, clusterPolicyControllerPort,
		filepath.Join(stateDir, certgen.ServiceServingCertFilename),
		filepath.Join(stateDir, certgen.ServiceServingKeyFilename),
		filepath.Join(stateDir, certgen.CACertFilename))
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return "", fmt.Errorf("write cluster-policy-controller config: %w", err)
	}
	return path, nil
}

// defaultRouteAddress returns the local address outbound traffic would
// leave through, without sending anything. The aggregated API servers
// listen on it because Kubernetes rejects loopback endpoint addresses.
func defaultRouteAddress() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", fmt.Errorf("determine host address: %w", err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}
