package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/vkareh/coreshift/internal/aggregation"
	"github.com/vkareh/coreshift/internal/certgen"
	"github.com/vkareh/coreshift/internal/crd"
	"github.com/vkareh/coreshift/internal/gpgme"
	"github.com/vkareh/coreshift/internal/mint"
	"github.com/vkareh/coreshift/internal/oauth"
	"github.com/vkareh/coreshift/internal/oauthclient"
	"github.com/vkareh/coreshift/internal/rbac"
	"github.com/vkareh/coreshift/internal/scc"
	"github.com/vkareh/coreshift/internal/supervisor"
	"github.com/vkareh/coreshift/pkg/image"
	clientcmd "k8s.io/client-go/tools/clientcmd"
)

// version is the binary's version, set at build time from git; it tags
// the container images built from the binary.
var version = "unknown"

// defaultReleaseRepository is the repository the release image is
// resolved from unless an exact image is pinned.
const defaultReleaseRepository = "quay.io/openshift-release-dev/ocp-release"

// releaseOptions carries the flags that choose which OpenShift release
// runs: an exact image reference, or a version resolved from a registry.
type releaseOptions struct {
	image    string
	version  string
	registry string
	arch     string
}

// goArchToUname maps Go's runtime architecture names to the uname -m
// names that release image tags use.
var goArchToUname = map[string]string{
	"amd64":   "x86_64",
	"arm64":   "aarch64",
	"ppc64le": "ppc64le",
	"s390x":   "s390x",
}

// releaseArch translates a Go runtime architecture name into the
// uname -m style used by release image tags.
func releaseArch(goArch string) (string, error) {
	arch, ok := goArchToUname[goArch]
	if !ok {
		return "", fmt.Errorf("unsupported architecture %q", goArch)
	}
	return arch, nil
}

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

// defaultAPIPort is the port the API server listens on unless overridden.
const defaultAPIPort = 6443

// containerEnv marks execution inside the minted container image, whose
// Containerfile sets it.
const containerEnv = "CORESHIFT_CONTAINER"

// apiBindAddress is the address the API server listens on: every
// interface inside a container, where a published port must reach it,
// and loopback only on the host.
func apiBindAddress() string {
	if os.Getenv(containerEnv) != "" {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// apiServerURL returns the endpoint clients use to reach the API server
// listening on the given port.
func apiServerURL(port int) string {
	return fmt.Sprintf("https://127.0.0.1:%d", port)
}

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
		Use:   "coreshift",
		Short: "Run a lightweight OpenShift control plane",
	}
	root.AddCommand(newRunCommand())
	root.AddCommand(newCreateCommand())
	root.AddCommand(newCleanupCommand())
	root.AddCommand(newVersionCommand())
	return root
}

// newRunCommand builds the parent of everything runnable. It takes a
// noun naming what to run; today only cluster exists. With no noun, or
// an unknown one, it prints the list and fails.
func newRunCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a CoreShift-managed thing",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Help(); err != nil {
				return err
			}
			if len(args) == 0 {
				return nil
			}
			return fmt.Errorf("unknown thing to run: %s", args[0])
		},
	}
	cmd.AddCommand(newClusterCommand())
	return cmd
}

// newVersionCommand builds the command that prints the version the
// binary was built with. Container images built from the binary are
// tagged with it, so it comes from a variable set at build time.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CoreShift version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version)
		},
	}
}

func newClusterCommand() *cobra.Command {
	var pullSecretPath string
	var port int
	var verbose bool
	var release releaseOptions
	defaultArch, err := releaseArch(runtime.GOARCH)
	if err != nil {
		// Every architecture OpenShift releases payloads for is in the
		// mapping; a binary built for anything else cannot run them.
		panic(err)
	}
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Run the OpenShift API control plane",
		// Failures carry their own report, and container mode surfaces
		// this command's errors as the container's log; a usage dump
		// after them would bury it.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAPI(cmd.Context(), pullSecretPath, port, verbose, release)
		},
	}
	cmd.Flags().StringVar(&pullSecretPath, "pull-secret", "", "path to a Red Hat pull secret with credentials for the release image registry (required)")
	cmd.Flags().IntVar(&port, "port", defaultAPIPort, "port the API server listens on")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "stream child process logs to the terminal")
	cmd.Flags().StringVar(&release.image, "release-image", "", "exact release image reference to run, e.g. quay.io/openshift-release-dev/ocp-release:4.22.8-x86_64")
	cmd.Flags().StringVar(&release.version, "version", "", "OpenShift version to run, X.Y or X.Y.Z; defaults to the newest release")
	cmd.Flags().StringVar(&release.registry, "registry", defaultReleaseRepository, "repository to resolve the release image from")
	cmd.Flags().StringVar(&release.arch, "arch", defaultArch, "architecture of the release payload, as uname -m reports it")
	if err := cmd.MarkFlagRequired("pull-secret"); err != nil {
		panic(err) // the flag is defined above
	}
	return cmd
}

func runAPI(ctx context.Context, pullSecretPath string, port int, verbose bool, release releaseOptions) error {
	if verbose {
		// Image cache messages are quiet by default; verbose runs show
		// them again, like the live child logs.
		image.Logger.SetOutput(os.Stderr)
	}
	stateDir, err := apiStateDir(port)
	if err != nil {
		return err
	}
	if err := prepareState(stateDir, port); err != nil {
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

	fmt.Println("Preparing release image")
	releaseRef, err := client.ResolveRelease(ctx, release.registry, release.version, release.arch, release.image)
	if err != nil {
		return fmt.Errorf("resolve release image: %w", err)
	}
	fmt.Printf("  Release image: %s\n", releaseRef)
	if err := ensureReleaseMarker(stateDir, releaseRef); err != nil {
		if err := offerFreshStart(stateDir, port, err); err != nil {
			return err
		}
		// The offer removed the directory: rebuild it the way a first
		// boot does, certificates and release marker included.
		if err := prepareState(stateDir, port); err != nil {
			return err
		}
		if err := ensureReleaseMarker(stateDir, releaseRef); err != nil {
			return err
		}
	}
	releaseRoot, err := client.CachedRelease(ctx, releaseRef)
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
	gpgmeDir, err := ensureGpgmeDir()
	if err != nil {
		return err
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

	oauthMetadataFile, err := writeOAuthMetadata(stateDir, port)
	if err != nil {
		return err
	}

	tokenReviewKubeconfig, err := writeTokenReviewKubeconfig(stateDir, port)
	if err != nil {
		return err
	}

	kubeAPIServerConfig, err := writeKubeAPIServerConfig(stateDir, oauthMetadataFile, port)
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
			"--secure-port=" + strconv.Itoa(port),
			"--bind-address=" + apiBindAddress(),
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
			fmt.Println("Seeding cluster objects")
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
			fmt.Println("Seeding security context constraints")
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
		fmt.Println("Seeding OAuth client")
		return oauthclient.Seed(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename), apiServerURL(port))
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
			// The binary links against the gpgme libraries embedded in
			// this binary and written into the image cache.
			Env: append(os.Environ(), "LD_LIBRARY_PATH="+gpgmeDir, "POD_NAMESPACE=default"),
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

	sup := &supervisor.Supervisor{
		LogDir:  filepath.Join(stateDir, "logs"),
		Verbose: verbose,
		AfterStart: func(ctx context.Context) error {
			fmt.Println("Waiting for the control plane to be ready")
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			kubeconfig := filepath.Join(stateDir, certgen.AdminKubeconfigFilename)
			if err := aggregation.WaitReady(ctx, kubeconfig, aggregatedServers); err != nil {
				return fmt.Errorf("control plane not ready: %w", err)
			}
			printReadySummary(stateDir, port)
			return nil
		},
	}
	return sup.Run(ctx, processes)
}

// ensureGpgmeDir writes the embedded gpgme libraries into the image
// cache and returns their directory, which openshift-controller-manager
// runs with as its library search path.
func ensureGpgmeDir() (string, error) {
	cacheRoot, err := image.CacheRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cacheRoot, "gpgme")
	if err := gpgme.Ensure(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// prepareState writes the certificate set into stateDir and points the
// admin kubeconfig back at the admin context. A fresh start, after the
// old directory was removed, runs it again on the empty directory.
func prepareState(stateDir string, port int) error {
	fmt.Println("Preparing certificates")
	if err := ensureCertificates(stateDir, port); err != nil {
		return err
	}
	return restoreAdminContext(stateDir)
}

// printReadySummary prints what a client needs to use the running control
// plane: the endpoint, the admin kubeconfig, and the login command.
func printReadySummary(stateDir string, port int) {
	endpoint := apiServerURL(port)
	fmt.Println()
	fmt.Println("Control plane ready")
	fmt.Printf("  API endpoint: %s\n", endpoint)
	fmt.Printf("  Kubeconfig:   %s\n", filepath.Join(stateDir, certgen.AdminKubeconfigFilename))
	fmt.Printf("  Log in with:  oc login %s --certificate-authority=%s -u <username> -p <password>\n",
		endpoint, filepath.Join(stateDir, certgen.CACertFilename))
}

// newCreateCommand builds the parent of everything creatable. It takes a
// noun naming what to create; today only cluster exists. With no noun,
// or an unknown one, it prints the list and fails.
func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a CoreShift-managed thing in a podman container",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Help(); err != nil {
				return err
			}
			if len(args) == 0 {
				return nil
			}
			return fmt.Errorf("unknown thing to create: %s", args[0])
		},
	}
	cmd.AddCommand(newCreateClusterCommand())
	return cmd
}

// newCreateClusterCommand builds the command that runs the control plane
// in a podman container: it mints or takes the container image, starts
// the container, waits for readiness through the published port, and
// leaves the container running in the background.
func newCreateClusterCommand() *cobra.Command {
	var pullSecretPath, name, imagePath string
	var port int
	var fresh bool
	var release releaseOptions
	defaultArch, err := releaseArch(runtime.GOARCH)
	if err != nil {
		panic(err)
	}
	cmd := &cobra.Command{
		Use:          "cluster",
		Short:        "Create the OpenShift API control plane in a podman container",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCreate(cmd.Context(), name, port, fresh, imagePath, pullSecretPath, release)
		},
	}
	cmd.Flags().StringVar(&pullSecretPath, "pull-secret", "", "path to a Red Hat pull secret with credentials for the release image registry (required)")
	cmd.Flags().IntVar(&port, "port", defaultAPIPort, "port the API server is published on")
	cmd.Flags().StringVar(&name, "name", "coreshift", "name of the cluster; its container is called coreshift-<name>")
	cmd.Flags().BoolVar(&fresh, "fresh", false, "remove the state directory before starting")
	cmd.Flags().StringVar(&imagePath, "image", "", "container image holding coreshift itself that the cluster runs in, minted from this binary when not given")
	cmd.Flags().StringVar(&release.image, "release-image", "", "exact OpenShift payload release image to build the control plane from, e.g. quay.io/openshift-release-dev/ocp-release:4.22.8-x86_64")
	cmd.Flags().StringVar(&release.version, "version", "", "OpenShift version to run, X.Y or X.Y.Z; defaults to the newest release")
	cmd.Flags().StringVar(&release.registry, "registry", defaultReleaseRepository, "repository to resolve the release image from")
	cmd.Flags().StringVar(&release.arch, "arch", defaultArch, "architecture of the release payload, as uname -m reports it")
	if err := cmd.MarkFlagRequired("pull-secret"); err != nil {
		panic(err) // the flag is defined above
	}
	return cmd
}

// The container's XDG homes, and where the pull secret is mounted. The
// host's state and cache roots are bind-mounted onto the .../coreshift
// paths the run inside derives from its XDG environment, so
// container-side state lands exactly where the host expects to find the
// kubeconfig and CA.
const (
	containerStateHome  = "/var/lib/coreshift/state"
	containerCacheHome  = "/var/lib/coreshift/cache"
	containerPullSecret = "/etc/coreshift/pull-secret"
)

// runCreate creates a cluster container and waits for it to be ready. It
// refuses a taken container name or a busy port before touching anything,
// mints the container image from the running binary unless --image names
// one, starts the container detached, streams its boot output, and polls
// readiness from the host. On success the container keeps running; on any
// failure it is removed while the state is kept.
func runCreate(ctx context.Context, name string, port int, fresh bool, imagePath, pullSecretPath string, release releaseOptions) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if _, err := exec.LookPath("podman"); err != nil {
		return fmt.Errorf("podman is required to create clusters: %w", err)
	}
	pullSecretPath, err := filepath.Abs(pullSecretPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(pullSecretPath); err != nil {
		return fmt.Errorf("read pull secret: %w", err)
	}
	stateDir, err := apiStateDir(port)
	if err != nil {
		return err
	}
	container := "coreshift-" + name
	if err := refuseIfContainerExists(container); err != nil {
		return err
	}
	if err := refuseIfPortBusy(port); err != nil {
		return err
	}
	if fresh {
		if _, err := os.Stat(stateDir); err == nil {
			if _, err := removeStateDir(stateDir); err != nil {
				return err
			}
		}
	}
	imageRef := imagePath
	if imageRef == "" {
		if imageRef, err = mint.Image(ctx, version); err != nil {
			return err
		}
	}
	stateRoot, err := stateRoot()
	if err != nil {
		return err
	}
	cacheRoot, err := image.CacheRoot()
	if err != nil {
		return err
	}
	for _, dir := range []string{stateRoot, cacheRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	fmt.Printf("Starting container %s\n", container)
	args := podmanRunArgs(container, port, stateRoot, cacheRoot, pullSecretPath, imageRef, release)
	run := exec.CommandContext(ctx, "podman", args...)
	run.Stdout = io.Discard // the detached run only prints the container ID
	run.Stderr = os.Stderr
	if err := run.Run(); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	return streamUntilReady(ctx, container, stateDir, port)
}

// podmanRunArgs builds the podman command line that starts the cluster
// container: detached and named, the API port published as an identity
// mapping on loopback only (bare port publishing would bind every
// interface, and exposing the API to the network must stay an explicit
// act), the state and cache roots bind-mounted, the pull secret mounted
// read-only, and the release flags and port passed through to the run
// inside. The image's entrypoint is `coreshift run cluster`, so the
// arguments after the image reference land on it.
func podmanRunArgs(container string, port int, stateRoot, cacheRoot, pullSecretPath, imageRef string, release releaseOptions) []string {
	return []string{
		"run", "--detach",
		"--name", container,
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"--env", "XDG_STATE_HOME=" + containerStateHome,
		"--env", "XDG_CACHE_HOME=" + containerCacheHome,
		// Shared relabeling, because host-mode runs and further
		// cluster containers keep using the same directories.
		"--volume", stateRoot + ":" + containerStateHome + "/coreshift:z",
		"--volume", cacheRoot + ":" + containerCacheHome + "/coreshift:z",
		"--volume", pullSecretPath + ":" + containerPullSecret + ":ro,z",
		imageRef,
		"--pull-secret=" + containerPullSecret,
		"--port=" + strconv.Itoa(port),
		"--release-image=" + release.image,
		"--version=" + release.version,
		"--registry=" + release.registry,
		"--arch=" + release.arch,
	}
}

// refuseIfContainerExists refuses to create a container whose name is
// taken, so a second create cannot disturb an existing cluster.
func refuseIfContainerExists(container string) error {
	if exec.Command("podman", "container", "exists", container).Run() == nil {
		return fmt.Errorf("container %s already exists; use --name to create another cluster, or remove the container first", container)
	}
	return nil
}

// refuseIfPortBusy refuses to publish the API port when something on the
// host already listens on loopback. The mapping publishes loopback only,
// so that is the only address that can collide.
func refuseIfPortBusy(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("port %d is already in use; use --port to publish the API on another one", port)
	}
	return listener.Close()
}

// streamUntilReady follows the container's boot output while polling the
// control plane's readiness from the host. Following ends when the
// container stops, so a container that dies before becoming ready is a
// failure; a readiness wait that succeeds prints the ready summary and
// leaves the container running.
func streamUntilReady(ctx context.Context, container, stateDir string, port int) error {
	logs := exec.CommandContext(ctx, "podman", "logs", "--follow", container)
	logs.Stdout = os.Stdout
	logs.Stderr = os.Stderr
	if err := logs.Start(); err != nil {
		return fmt.Errorf("stream container output: %w", err)
	}
	logsDone := make(chan error, 1)
	go func() { logsDone <- logs.Wait() }()
	ready := make(chan error, 1)
	go func() { ready <- waitContainerReady(ctx, stateDir) }()

	select {
	case err := <-ready:
		_ = logs.Process.Kill()
		<-logsDone
		if err != nil {
			return containerFailure(container, stateDir, err)
		}
		printReadySummary(stateDir, port)
		fmt.Printf("Container %s is running in the background\n", container)
		return nil
	case <-logsDone:
		return containerFailure(container, stateDir,
			fmt.Errorf("container %s exited before the control plane became ready", container))
	}
}

// containerReadyTimeout bounds the whole readiness wait of a created
// cluster. A cold image cache makes the container's first boot pull the
// release payload for a long time before anything answers, so the budget
// is generous; Ctrl-C cancels the wait.
const containerReadyTimeout = 30 * time.Minute

// waitContainerReady waits for the created cluster's readiness from the
// host: first for the kubeconfig the container's boot writes into the
// bind-mounted state directory, then for the control plane's readiness
// through the published port, the same wait host mode runs after boot.
func waitContainerReady(ctx context.Context, stateDir string) error {
	ctx, cancel := context.WithTimeout(ctx, containerReadyTimeout)
	defer cancel()
	kubeconfig := filepath.Join(stateDir, certgen.AdminKubeconfigFilename)
	for {
		if _, err := os.Stat(kubeconfig); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err := aggregation.WaitReady(ctx, kubeconfig, aggregatedServers); err != nil {
		return fmt.Errorf("control plane not ready: %w", err)
	}
	return nil
}

// containerFailure reports a cluster container that did not become ready:
// the error carries the tail of the container's log and the state
// directory's full logs, a state-directory conflict gains the create-side
// remedies, and the container is removed while the state is kept.
func containerFailure(container, stateDir string, err error) error {
	tail := containerLogTail(container)
	if hint := stateConflictHint(tail); hint != "" {
		err = fmt.Errorf("%w\n%s", err, hint)
	}
	if tail != "" {
		err = fmt.Errorf("%w\n\nrecent log output:\n%s\n\nfull log: %s", err, tail, filepath.Join(stateDir, "logs"))
	}
	if rmErr := exec.Command("podman", "rm", "--force", container).Run(); rmErr != nil {
		err = fmt.Errorf("%w\nremoving the container failed; remove %s by hand", err, container)
	}
	return err
}

// containerLogTailLines is how many lines of the container's log output a
// failure error includes, matching the supervisor's per-child tail.
const containerLogTailLines = 20

// containerLogTail returns the last lines of the container's log output.
func containerLogTail(container string) string {
	out, err := exec.Command("podman", "logs", container).Output()
	if err != nil {
		return ""
	}
	return tailText(string(out), containerLogTailLines)
}

// stateConflictHint names the create-side remedies when the container's
// log shows the state directory refusing reuse. The run inside cannot ask
// questions without a terminal, so its own hint names host-mode commands
// that do not apply here.
func stateConflictHint(logTail string) string {
	for _, mark := range []string{"cannot be reused by", "before releases were recorded in it"} {
		if strings.Contains(logTail, mark) {
			return "Use --fresh to remove the state directory and start fresh, or a different --port to get a separate one"
		}
	}
	return ""
}

// tailText returns the last lines of text.
func tailText(text string, lines int) string {
	all := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// newCleanupCommand builds the command that removes CoreShift's on-disk
// artifacts: the state directories, and with --cache the shared image
// cache.
func newCleanupCommand() *cobra.Command {
	var port int
	var all bool
	var clearCache bool
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Remove CoreShift state directories and cache",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if all && cmd.Flags().Changed("port") {
				return fmt.Errorf("--all already removes every state directory; drop --port")
			}
			return runCleanup(port, all, clearCache)
		},
	}
	cmd.Flags().IntVar(&port, "port", defaultAPIPort, "port whose state directory is removed")
	cmd.Flags().BoolVar(&all, "all", false, "remove every state directory: the default one and all per-port ones")
	cmd.Flags().BoolVar(&clearCache, "cache", false, "also remove the shared image cache, which the next run rebuilds by re-pulling")
	return cmd
}

// cleanupTarget is a directory cleanup removes, with the name for it in
// messages.
type cleanupTarget struct {
	path string
	kind string
}

// runCleanup removes the chosen state directories and, with clearCache,
// the shared image cache. It refuses while a control plane is running,
// because cleaning under a live instance would pull its data out from
// under it. A missing directory is reported, not an error, so the command
// is safe to re-run and to call from scripts.
func runCleanup(port int, all, clearCache bool) error {
	running, err := runningControlPlane()
	if err != nil {
		return err
	}
	if running != "" {
		return fmt.Errorf("a CoreShift control plane is running:\n  %s\n"+
			"Stop it first; cleaning up under a running control plane would pull its data out from under it", running)
	}
	targets, err := cleanupTargets(port, all)
	if err != nil {
		return err
	}
	if clearCache {
		cacheDir, err := image.CacheRoot()
		if err != nil {
			return err
		}
		targets = append(targets, cleanupTarget{path: cacheDir, kind: "image cache"})
	}
	var freed int64
	removed := false
	for _, target := range targets {
		if _, err := os.Stat(target.path); os.IsNotExist(err) {
			fmt.Printf("No %s at %s\n", target.kind, target.path)
			continue
		}
		size, err := removeStateDir(target.path)
		if err != nil {
			return err
		}
		freed += size
		removed = true
	}
	// A full cleanup leaves nothing behind: drop the state directories'
	// parents once they are empty. Removing a non-empty parent fails and
	// is ignored.
	apiRoot, err := apiStateDir(defaultAPIPort)
	if err != nil {
		return err
	}
	_ = os.Remove(apiRoot)
	_ = os.Remove(filepath.Dir(apiRoot))
	if removed {
		fmt.Printf("Freed %s\n", humanSize(freed))
	}
	return nil
}

// cleanupTargets lists the state directories to remove: one port's, or
// with all every per-port directory followed by the default one. The
// default one comes last so its size, measured right before its removal,
// counts only the content the earlier removals left.
func cleanupTargets(port int, all bool) ([]cleanupTarget, error) {
	if !all {
		dir, err := apiStateDir(port)
		if err != nil {
			return nil, err
		}
		return []cleanupTarget{{path: dir, kind: "state directory"}}, nil
	}
	apiRoot, err := apiStateDir(defaultAPIPort)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(apiRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return []cleanupTarget{{path: apiRoot, kind: "state directory"}}, nil
		}
		return nil, err
	}
	var targets []cleanupTarget
	for _, entry := range entries {
		if !entry.IsDir() || !isPortName(entry.Name()) {
			continue
		}
		targets = append(targets, cleanupTarget{path: filepath.Join(apiRoot, entry.Name()), kind: "state directory"})
	}
	return append(targets, cleanupTarget{path: apiRoot, kind: "state directory"}), nil
}

// isPortName reports whether name names a per-port state directory; the
// layout names them after their port, and nothing else there is.
func isPortName(name string) bool {
	_, err := strconv.Atoi(name)
	return err == nil
}

// runningControlPlane returns the command line of a running CoreShift
// control plane process, or an empty string when none is.
func runningControlPlane() (string, error) {
	stateDir, err := stateRoot()
	if err != nil {
		return "", err
	}
	cacheDir, err := image.CacheRoot()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return "", fmt.Errorf("list processes: %w", err)
	}
	self := os.Getpid()
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		args := processArgs(pid)
		if isControlPlaneArgs(args, stateDir, cacheDir) {
			return strings.Join(args, " "), nil
		}
	}
	return "", nil
}

// isControlPlaneArgs reports whether a command line belongs to a running
// control plane: the supervisor by name, or any command line carrying a
// CoreShift state or cache path, which every control plane child's does.
// Other processes mentioning those paths also match, which errs on
// refusing.
func isControlPlaneArgs(args []string, stateDir, cacheDir string) bool {
	if len(args) == 0 {
		return false
	}
	if filepath.Base(args[0]) == "coreshift" && len(args) > 2 && args[1] == "run" && args[2] == "cluster" {
		return true
	}
	for _, arg := range args {
		// Child command lines carry the paths as --flag=value.
		if i := strings.Index(arg, "="); i >= 0 {
			arg = arg[i+1:]
		}
		if withinDir(arg, stateDir) || withinDir(arg, cacheDir) {
			return true
		}
	}
	return false
}

// processArgs returns the command line of a process as its arguments.
// Kernel threads and processes that cannot be read give none.
func processArgs(pid int) []string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

// withinDir reports whether path is dir or lies below it.
func withinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// removeStateDir removes a state directory, reporting its size first;
// the cleanup command and the fresh-start offer share it. It returns
// the bytes freed.
func removeStateDir(dir string) (int64, error) {
	size, err := dirSize(dir)
	if err != nil {
		return 0, err
	}
	fmt.Printf("Removing %s (%s)\n", dir, humanSize(size))
	if err := os.RemoveAll(dir); err != nil {
		return 0, fmt.Errorf("remove %s: %w", dir, err)
	}
	return size, nil
}

// dirSize sums the sizes of the regular files under dir. Walk errors are
// skipped, so the sum is best effort; removing the directory does not
// depend on it.
func dirSize(dir string) (int64, error) {
	if _, err := os.Stat(dir); err != nil {
		return 0, err
	}
	var size int64
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	return size, nil
}

// humanSize renders a byte count for people.
func humanSize(size int64) string {
	switch {
	case size >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(size)/(1<<30))
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

// apiStateDir returns the XDG state directory holding the generated
// certificates and etcd data, so they survive across runs. The default
// port keeps the original location; any other port gets its own
// subdirectory, so runs on different ports do not share certificates
// or etcd data.
func apiStateDir(port int) (string, error) {
	root, err := stateRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "api")
	if port != defaultAPIPort {
		dir = filepath.Join(dir, strconv.Itoa(port))
	}
	return dir, nil
}

// stateRoot returns the directory CoreShift keeps its state below:
// $XDG_STATE_HOME/coreshift, defaulting to ~/.local/state/coreshift.
func stateRoot() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("determine home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "coreshift"), nil
}

// releaseMarkerFilename records which release image created the state
// directory's etcd data. etcd data only works with the release that
// wrote it, so the marker is checked before anything starts.
const releaseMarkerFilename = "release-image"

// ensureReleaseMarker refuses to reuse a state directory whose etcd data
// is incompatible with the release this run resolved: one written by
// another release, or one from before markers existed. A fresh directory
// gains a marker naming the resolved release.
func ensureReleaseMarker(stateDir, releaseRef string) error {
	marker := filepath.Join(stateDir, releaseMarkerFilename)
	recorded, err := os.ReadFile(marker)
	if err == nil {
		created := strings.TrimSpace(string(recorded))
		if created == releaseRef {
			return nil
		}
		return fmt.Errorf("state directory %s was created by release %s and cannot be reused by %s\n"+
			"etcd data only works with the release that created it.\n"+
			"Use a different --port to get a separate state directory, or remove %s to start fresh",
			stateDir, created, releaseRef, stateDir)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("read release marker: %w", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "etcd-data")); err == nil {
		return fmt.Errorf("state directory %s has etcd data from before releases were recorded in it\n"+
			"etcd data only works with the release that created it.\n"+
			"Use a different --port to get a separate state directory, or remove %s to start fresh",
			stateDir, stateDir)
	}
	if err := os.WriteFile(marker, []byte(releaseRef), 0o644); err != nil {
		return fmt.Errorf("write release marker: %w", err)
	}
	return nil
}

// offerFreshStart offers to remove a state directory that refused reuse.
// With a terminal on standard input it prints the refusal and asks
// whether to remove the directory and start fresh; an explicit yes
// removes it and returns nil, so the run continues from scratch. Any
// other answer returns the refusal. Without a terminal it does not ask
// and returns the refusal with a hint at the cleanup command.
func offerFreshStart(stateDir string, port int, refusal error) error {
	if !stdinIsTerminal() {
		return fmt.Errorf("%w\nTo remove it without a prompt, run: coreshift cleanup --port %d", refusal, port)
	}
	fmt.Println(refusal.Error())
	fmt.Println()
	fmt.Printf("Remove %s and start fresh? Everything in it will be lost,\n"+
		"including anything created through the API such as a configured\n"+
		"identity provider [y/N]: ", stateDir)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return startFresh(answer, stateDir, refusal)
}

// startFresh carries out the answer to the fresh-start question: only
// an explicit yes removes the state directory; anything else, including
// an empty answer, keeps the refusal.
func startFresh(answer, stateDir string, refusal error) error {
	if !parseYesNo(answer) {
		return refusal
	}
	if _, err := removeStateDir(stateDir); err != nil {
		return err
	}
	return nil
}

// parseYesNo parses the answer to a yes-or-no question that defaults to
// no: only an explicit yes counts as one.
func parseYesNo(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}

// stdinIsTerminal reports whether standard input is an interactive
// terminal. Pipes and redirected files answer no, so scripts never see
// a prompt.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// ensureCertificates generates the certificate and kubeconfig set into
// stateDir unless a complete set is already present. Reuse matters: a fresh
// CA on every run would invalidate kubeconfigs saved outside the state
// directory and leave etcd data trusted by a CA nothing signs for anymore.
// A set whose service-serving certificate lacks the loopback address counts
// as incomplete, so it is regenerated rather than reused.
func ensureCertificates(stateDir string, port int) error {
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
	return certgen.GenerateAll(stateDir, certgen.Options{APIServerURL: apiServerURL(port)})
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

// adminContextName is the certificate-based context the generated admin
// kubeconfig selects.
const adminContextName = "admin@coreshift"

// restoreAdminContext points the admin kubeconfig's current context back at
// the certificate-based admin context. Logging in with oc adds token
// contexts to the file and makes one of them current; a startup would then
// try the token before the OAuth API that validates it exists, and stall
// until the seeding timeouts fail the run. Login contexts are kept; only
// the selection is corrected.
func restoreAdminContext(stateDir string) error {
	path := filepath.Join(stateDir, certgen.AdminKubeconfigFilename)
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return fmt.Errorf("read admin kubeconfig: %w", err)
	}
	if config.CurrentContext == adminContextName {
		return nil
	}
	if _, ok := config.Contexts[adminContextName]; !ok {
		return fmt.Errorf("admin kubeconfig has no %s context", adminContextName)
	}
	config.CurrentContext = adminContextName
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		return fmt.Errorf("write admin kubeconfig: %w", err)
	}
	return nil
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
func writeOAuthMetadata(stateDir string, port int) (string, error) {
	path := filepath.Join(stateDir, "oauth-metadata.json")
	metadata := fmt.Sprintf(`{
  "issuer": %q,
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
`, apiServerURL(port), oauthServerPort, oauthServerPort)
	if err := os.WriteFile(path, []byte(metadata), 0o600); err != nil {
		return "", fmt.Errorf("write oauth metadata: %w", err)
	}
	return path, nil
}

// writeTokenReviewKubeconfig writes the kubeconfig kube-apiserver uses to
// have OAuth access tokens reviewed: it points at the aggregated OAuth API
// server's token review endpoint and authenticates with the generated
// admin identity, whose permissions cover the review call.
func writeTokenReviewKubeconfig(stateDir string, port int) (string, error) {
	path := filepath.Join(stateDir, "token-review.kubeconfig")
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: coreshift
  cluster:
    certificate-authority: %s
    server: %s/apis/oauth.openshift.io/v1/tokenreviews
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
		apiServerURL(port),
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
func writeKubeAPIServerConfig(stateDir, oauthMetadataFile string, port int) (string, error) {
	path := filepath.Join(stateDir, "kube-apiserver-config.json")
	config := fmt.Sprintf(`{
  "apiVersion": "kubecontrolplane.config.openshift.io/v1",
  "kind": "KubeAPIServerConfig",
  "servingInfo": {
    "bindAddress": "%s:%d"
  },
  "servicesSubnet": "172.30.0.0/16",
  "serviceAccountPublicKeyFiles": [
    %q
  ],
  "authConfig": {
    "oauthMetadataFile": %q
  }
}
`, apiBindAddress(), port, filepath.Join(stateDir, certgen.SAPubFilename), oauthMetadataFile)
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
