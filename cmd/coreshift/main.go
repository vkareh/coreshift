package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/vkareh/coreshift/internal/aggregation"
	"github.com/vkareh/coreshift/internal/certgen"
	"github.com/vkareh/coreshift/internal/crd"
	"github.com/vkareh/coreshift/internal/gpgme"
	"github.com/vkareh/coreshift/internal/infrastructure"
	"github.com/vkareh/coreshift/internal/join"
	"github.com/vkareh/coreshift/internal/mint"
	"github.com/vkareh/coreshift/internal/oauth"
	"github.com/vkareh/coreshift/internal/oauthclient"
	"github.com/vkareh/coreshift/internal/pullsecret"
	"github.com/vkareh/coreshift/internal/rbac"
	"github.com/vkareh/coreshift/internal/scc"
	"github.com/vkareh/coreshift/internal/supervisor"
	"github.com/vkareh/coreshift/internal/worker"
	"github.com/vkareh/coreshift/pkg/image"
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

// Local ports of the control plane's children, fixed whatever --port the
// API server takes: etcd's client and peer ports, the ports of the
// aggregated API servers kube-apiserver reaches through the aggregation
// layer, and the controllers' health and metrics endpoints. The ports
// appear both in the children's flags and configs and in the endpoints the
// aggregation wiring registers.
const (
	etcdClientPort                 = 2379
	etcdPeerPort                   = 2380
	openShiftAPIServerPort         = 8443
	oauthAPIServerPort             = 8444
	oauthServerPort                = 8445
	openShiftControllerManagerPort = 8446
	clusterPolicyControllerPort    = 10357
	machineApproverMetricsPort     = 9191
)

// The etcd endpoints built from the ports above, so they cannot drift.
var (
	etcdClientURL = "http://127.0.0.1:" + strconv.Itoa(etcdClientPort)
	etcdPeerURL   = "http://127.0.0.1:" + strconv.Itoa(etcdPeerPort)
)

// reservedPorts are the children's ports from above, which the API server
// must not take: inside a container it binds every interface, so the
// collision would surface only as a confusing failure late in the boot.
var reservedPorts = []int{
	etcdClientPort, etcdPeerPort,
	openShiftAPIServerPort, oauthAPIServerPort, oauthServerPort,
	openShiftControllerManagerPort, clusterPolicyControllerPort,
	machineApproverMetricsPort,
}

// refuseReservedPort refuses an API port a control-plane child listens
// on, naming the reserved set so a neighboring port can be picked. It
// runs before any certificate, image, or state work.
func refuseReservedPort(port int) error {
	if !slices.Contains(reservedPorts, port) {
		return nil
	}
	return fmt.Errorf("port %d is reserved for the control plane's own components; the reserved ports are %v; pick a neighboring port", port, reservedPorts)
}

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
	root.AddCommand(newDeleteCommand())
	root.AddCommand(newGetCommand())
	root.AddCommand(newCleanupCommand())
	root.AddCommand(newVersionCommand())
	return root
}

// newRunCommand builds the parent of everything runnable. It takes a
// noun naming what to run: the control plane, or the node side inside a
// worker container. With no noun, or an unknown one, it prints the list
// and fails.
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
	cmd.AddCommand(newWorkerCommand())
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
	var pullSecretPath, name string
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
			return runAPI(cmd.Context(), pullSecretPath, name, port, verbose, release)
		},
	}
	cmd.Flags().StringVar(&pullSecretPath, "pull-secret", "", "path to a Red Hat pull secret with credentials for the release image registry (required)")
	cmd.Flags().StringVar(&name, "name", defaultClusterName, "name of the cluster; its state lives in clusters/<name> under the state root")
	cmd.Flags().IntVar(&port, "port", defaultAPIPort, "port the API server listens on")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "stream child process logs to the terminal")
	cmd.Flags().StringVar(&release.image, "release-image", "", "exact release image reference to run, e.g. quay.io/openshift-release-dev/ocp-release:4.22.8-x86_64")
	cmd.Flags().StringVar(&release.version, "version", "", "OpenShift version to run, X.Y or X.Y.Z; defaults to the newest release, or the release recorded in the state directory")
	cmd.Flags().StringVar(&release.registry, "registry", defaultReleaseRepository, "repository to resolve the release image from")
	cmd.Flags().StringVar(&release.arch, "arch", defaultArch, "architecture of the release payload, as uname -m reports it")
	if err := cmd.MarkFlagRequired("pull-secret"); err != nil {
		panic(err) // the flag is defined above
	}
	return cmd
}

// newWorkerCommand builds the command that runs inside a worker
// container: it joins the control plane whose bootstrap material is
// mounted in and runs the node side until the container stops. Its
// configuration arrives as files, never as flags: the host-side create
// command mounts the bootstrap kubeconfig and the node name in.
func newWorkerCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "worker",
		Short: "Join a control plane and run the node side",
		// Failures carry their own report, and the worker container
		// surfaces this command's errors as the container's log; a usage
		// dump after them would bury it.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return worker.Run(cmd.Context())
		},
	}
}

func runAPI(ctx context.Context, pullSecretPath, name string, port int, verbose bool, release releaseOptions) error {
	if err := refuseReservedPort(port); err != nil {
		return err
	}
	if err := refuseInvalidClusterName(name); err != nil {
		return err
	}
	if verbose {
		// Image cache messages are quiet by default; verbose runs show
		// them again, like the live child logs.
		image.Logger.SetOutput(os.Stderr)
	}
	stateDir, err := clusterDir(name)
	if err != nil {
		return err
	}
	if err := prepareState(stateDir, name, port); err != nil {
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
	releaseRef, note, err := bootRelease(ctx, client, release, stateDir)
	if err != nil {
		return fmt.Errorf("resolve release image: %w", err)
	}
	fmt.Printf("  Release image: %s\n", releaseRef)
	if note != "" {
		fmt.Println(note)
	}
	if err := ensureClusterMarker(stateDir, name, releaseRef); err != nil {
		if err := offerFreshStart(stateDir, name, err); err != nil {
			return err
		}
		// The offer removed the directory: rebuild it the way a first
		// boot does, certificates and marker included.
		if err := prepareState(stateDir, name, port); err != nil {
			return err
		}
		if err := ensureClusterMarker(stateDir, name, releaseRef); err != nil {
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
		"openshift-apiserver", "oauth-apiserver", "oauth-server", "openshift-controller-manager", "cluster-policy-controller", "cluster-machine-approver")
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

	tokenReviewKubeconfig, err := writeTokenReviewKubeconfig(stateDir, name, port)
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
				"--listen-client-urls=" + etcdClientURL,
				"--advertise-client-urls=" + etcdClientURL,
				"--listen-peer-urls=" + etcdPeerURL,
			},
		},
		{
			Name: "kube-apiserver",
			Path: kubeBinaries["kube-apiserver"],
			Args: []string{
				"--etcd-servers=" + etcdClientURL,
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
				if err := crd.Seed(ctx, kubeconfig); err != nil {
					return err
				}
				// The managed-node admission's informer retries until the
				// Infrastructure definition lands and then reads the object,
				// so node creation is permitted once both are in, however
				// late against the API server's boot.
				if err := infrastructure.Seed(ctx, kubeconfig); err != nil {
					return err
				}
				// The pull secret is stored as the cluster's global pull
				// secret, the object a real cluster keeps, so commands that
				// need it after the fact read it from the cluster instead
				// of asking for the file again.
				return pullsecret.Seed(ctx, kubeconfig, pullSecret)
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
				// The control plane CA signs the node client certificates
				// the approver approves, the way a real cluster's controller
				// manager issues what its approver approves.
				"--cluster-signing-kubelet-client-cert-file=" + filepath.Join(stateDir, certgen.CACertFilename),
				"--cluster-signing-kubelet-client-key-file=" + filepath.Join(stateDir, certgen.CAKeyFilename),
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
				"--etcd-servers=" + etcdClientURL,
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
		// The machine approver approves the node certificate requests a
		// joining worker's bootstrap identity makes, matching each against
		// the machine standing in for the cloud machine that would back
		// the node. The management and workload clusters are the same
		// here, so both kubeconfig flags point at the one component
		// kubeconfig. Its ClusterOperator status reporting stays off
		// because this control plane runs no ClusterOperator machinery;
		// everything else runs as the payload deploys it.
		{
			Name: "cluster-machine-approver",
			Path: openShiftBinaries["cluster-machine-approver"],
			Args: []string{
				"--management-cluster-kubeconfig=" + filepath.Join(stateDir, certgen.ClusterMachineApproverKubeconfigFilename),
				"--workload-cluster-kubeconfig=" + filepath.Join(stateDir, certgen.ClusterMachineApproverKubeconfigFilename),
				"--disable-status-controller",
				// Single instance, no lease to elect.
				"--leader-elect=false",
			},
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
// kubeconfigs at the port in use, names their entries after the cluster,
// and points the admin kubeconfig's selection back at the admin context.
// A fresh start, after the old directory was removed, runs it again on
// the empty directory.
func prepareState(stateDir, name string, port int) error {
	fmt.Println("Preparing certificates")
	if err := ensureCertificates(stateDir, name, port); err != nil {
		return err
	}
	if err := followAPIPort(stateDir, port); err != nil {
		return err
	}
	if err := followClusterName(stateDir, name); err != nil {
		return err
	}
	return restoreAdminContext(stateDir, name)
}

// kubeconfigFilenames are the kubeconfigs in a cluster's state directory.
// Each embeds the API server's URL as the endpoint its client dials, so
// each must follow the port in use.
var kubeconfigFilenames = []string{
	certgen.AdminKubeconfigFilename,
	certgen.ControllerManagerKubeconfigFilename,
	certgen.SchedulerKubeconfigFilename,
	certgen.OpenShiftAPIServerKubeconfigFilename,
	certgen.OAuthAPIServerKubeconfigFilename,
	certgen.OAuthServerKubeconfigFilename,
	certgen.OpenShiftControllerManagerKubeconfigFilename,
	certgen.ClusterPolicyControllerKubeconfigFilename,
	certgen.ClusterMachineApproverKubeconfigFilename,
}

// followAPIPort points the kubeconfigs at the port in use. The port is
// where this deployment's API server binds, not part of the cluster's
// identity, so a cluster re-created on a different port reuses its state
// warm: certificates encode no port, and the kubeconfigs are the only
// files in state that record it, so they are rewritten whenever they
// disagree with it.
func followAPIPort(stateDir string, port int) error {
	server := apiServerURL(port)
	for _, name := range kubeconfigFilenames {
		path := filepath.Join(stateDir, name)
		config, err := clientcmd.LoadFromFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		changed := false
		for _, cluster := range config.Clusters {
			if cluster.Server != server {
				cluster.Server = server
				changed = true
			}
		}
		if !changed {
			continue
		}
		if err := clientcmd.WriteToFile(*config, path); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

// followClusterName names the kubeconfig entries after the cluster. The
// name is the cluster's identity, and the entries carrying it is what
// lets the kubeconfig files of several clusters merge — the standard way
// to work with several at once — instead of silently colliding on the
// generic names every cluster used to share. A state directory from
// before the entries were named is rewritten here, with no certificate
// regeneration: certificates encode no name.
func followClusterName(stateDir, name string) error {
	for _, filename := range kubeconfigFilenames {
		path := filepath.Join(stateDir, filename)
		config, err := clientcmd.LoadFromFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", filename, err)
		}
		if !renameClusterEntries(config, name) {
			continue
		}
		if err := clientcmd.WriteToFile(*config, path); err != nil {
			return fmt.Errorf("write %s: %w", filename, err)
		}
	}
	return nil
}

// renameClusterEntries renames a kubeconfig's generated entries from the
// generic name to the cluster's own: the cluster entry, and the context
// and user entries derived from it, with every reference between them,
// the current context included, followed along. Contexts an oc login
// added keep their names; only their cluster reference follows. It
// reports whether anything changed.
func renameClusterEntries(config *clientcmdapi.Config, name string) bool {
	changed := renameEntry(config.Clusters, defaultClusterName, name)
	for contextName, context := range config.Contexts {
		user, generated := strings.CutSuffix(contextName, "@"+defaultClusterName)
		if !generated {
			if context.Cluster == defaultClusterName && defaultClusterName != name {
				context.Cluster = name
				changed = true
			}
			continue
		}
		named := user + "@" + name
		if renameEntry(config.AuthInfos, user, named) {
			changed = true
			if context.AuthInfo == user {
				context.AuthInfo = named
			}
		}
		if renameEntry(config.Contexts, contextName, named) {
			if config.CurrentContext == contextName {
				config.CurrentContext = named
			}
			changed = true
		}
		if context.Cluster == defaultClusterName && defaultClusterName != name {
			context.Cluster = name
			changed = true
		}
	}
	return changed
}

// renameEntry moves a map entry from one key to another, reporting
// whether the map changed. Same-key and missing entries change nothing.
func renameEntry[T any](entries map[string]*T, from, to string) bool {
	if from == to {
		return false
	}
	entry, ok := entries[from]
	if !ok {
		return false
	}
	entries[to] = entry
	delete(entries, from)
	return true
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
// noun naming what to create: the control plane, or a worker joining a
// running one. With no noun, or an unknown one, it prints the list and
// fails.
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
	cmd.AddCommand(newCreateWorkerCommand())
	return cmd
}

// newCreateClusterCommand builds the command that runs the control plane
// in a podman container: it mints or takes the container image, starts
// the container, waits for readiness through the published port, and
// leaves the container running in the background.
func newCreateClusterCommand() *cobra.Command {
	var pullSecretPath, name, imagePath string
	var port int
	var fresh, verbose bool
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
			return runCreate(cmd.Context(), name, port, fresh, verbose, imagePath, pullSecretPath, release)
		},
	}
	cmd.Flags().StringVar(&pullSecretPath, "pull-secret", "", "path to a Red Hat pull secret with credentials for the release image registry (required)")
	cmd.Flags().IntVar(&port, "port", defaultAPIPort, "port the API server is published on")
	cmd.Flags().StringVar(&name, "name", "coreshift", "name of the cluster; its container is called coreshift-<name>")
	cmd.Flags().BoolVar(&fresh, "fresh", false, "remove the state directory before starting")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "stream the image build's output to the terminal")
	cmd.Flags().StringVar(&imagePath, "image", "", "container image holding coreshift itself that the cluster runs in, minted from this binary when not given")
	cmd.Flags().StringVar(&release.image, "release-image", "", "exact OpenShift payload release image to build the control plane from, e.g. quay.io/openshift-release-dev/ocp-release:4.22.8-x86_64")
	cmd.Flags().StringVar(&release.version, "version", "", "OpenShift version to run, X.Y or X.Y.Z; defaults to the newest release, or the release recorded in the state directory")
	cmd.Flags().StringVar(&release.registry, "registry", defaultReleaseRepository, "repository to resolve the release image from")
	cmd.Flags().StringVar(&release.arch, "arch", defaultArch, "architecture of the release payload, as uname -m reports it")
	if err := cmd.MarkFlagRequired("pull-secret"); err != nil {
		panic(err) // the flag is defined above
	}
	return cmd
}

// The container's XDG homes, and where the pull secret is mounted. The
// cluster's state directory and the shared image cache are bind-mounted
// onto the .../coreshift paths the run inside derives from its XDG
// environment, so container-side state lands exactly where the host
// expects to find the kubeconfig and CA.
const (
	containerStateHome  = "/var/lib/coreshift/state"
	containerCacheHome  = "/var/lib/coreshift/cache"
	containerPullSecret = "/etc/coreshift/pull-secret"
)

// runCreate creates a cluster container and waits for it to be ready. It
// refuses a reserved or otherwise taken port, a taken container name, an
// unusable cluster name, or a state directory that cannot run the
// requested release before touching anything, mints the container image
// from the running binary unless --image names one, starts the container
// detached, streams its boot output, and polls readiness from the host. On
// success the container keeps running; on any failure it is removed while
// the state is preserved.
func runCreate(ctx context.Context, name string, port int, fresh, verbose bool, imagePath, pullSecretPath string, release releaseOptions) error {
	if err := refuseReservedPort(port); err != nil {
		return err
	}
	if err := refuseInvalidClusterName(name); err != nil {
		return err
	}
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
	stateDir, err := clusterDir(name)
	if err != nil {
		return err
	}
	container := clusterContainerPrefix + name
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
	if err := checkCreateState(ctx, stateDir, release, pullSecretPath); err != nil {
		return err
	}
	imageRef := imagePath
	if imageRef == "" {
		if imageRef, err = mint.Image(ctx, version, verbose); err != nil {
			return err
		}
	}
	cacheRoot, err := image.CacheRoot()
	if err != nil {
		return err
	}
	for _, dir := range []string{stateDir, cacheRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	fmt.Printf("Starting container %s\n", container)
	args := podmanRunArgs(container, name, port, stateDir, cacheRoot, pullSecretPath, imageRef, release)
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
// act), the cluster's state directory and the shared image cache
// bind-mounted, the pull secret mounted read-only, and the name, the
// release flags, and the port passed through to the run inside. The
// image's entrypoint is `coreshift run cluster`, so the arguments after
// the image reference land on it.
func podmanRunArgs(container, name string, port int, stateDir, cacheRoot, pullSecretPath, imageRef string, release releaseOptions) []string {
	return []string{
		"run", "--detach",
		"--name", container,
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"--env", "XDG_STATE_HOME=" + containerStateHome,
		"--env", "XDG_CACHE_HOME=" + containerCacheHome,
		// Shared relabeling, because host-mode runs and further
		// cluster containers keep using the same directories.
		"--volume", stateDir + ":" + filepath.Join(containerStateHome, "coreshift", clustersDirName, name) + ":z",
		"--volume", cacheRoot + ":" + containerCacheHome + "/coreshift:z",
		"--volume", pullSecretPath + ":" + containerPullSecret + ":ro,z",
		imageRef,
		"--pull-secret=" + containerPullSecret,
		"--name=" + name,
		"--port=" + strconv.Itoa(port),
		"--release-image=" + release.image,
		"--version=" + release.version,
		"--registry=" + release.registry,
		"--arch=" + release.arch,
	}
}

// checkCreateState refuses a create whose state directory cannot run the
// release the cluster would boot, before an image is minted or a
// container started. It resolves the requested release the same way the
// run inside will and applies the marker's reuse rule to it, so only a
// cross-stream move or a patch downgrade is a conflict. On a terminal a
// conflict asks the same default-no fresh-start question run cluster
// asks; without one it refuses naming --fresh.
func checkCreateState(ctx context.Context, stateDir string, release releaseOptions, pullSecretPath string) error {
	recorded := stateRelease(stateDir)
	if recorded == "" {
		return nil
	}
	if release.image == "" && release.version == "" {
		// Nothing requested: the run inside reuses the recorded release.
		return nil
	}
	pullSecret, err := os.ReadFile(pullSecretPath)
	if err != nil {
		return fmt.Errorf("read pull secret: %w", err)
	}
	client, err := image.NewClient(pullSecret)
	if err != nil {
		return err
	}
	releaseRef, err := client.ResolveRelease(ctx, release.registry, release.version, release.arch, release.image)
	if err != nil {
		return err
	}
	if releaseReuse(recorded, releaseRef) {
		return nil
	}
	return offerCreateFreshStart(stateDir, stateRefusal(stateDir, recorded, releaseRef,
		"Use --fresh to remove it and start fresh, or a different --name to get a separate one"))
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

// streamUntilReady follows the cluster container's boot output while
// waiting for the control plane's readiness from the host.
func streamUntilReady(ctx context.Context, container, stateDir string, port int) error {
	return streamUntil(ctx, container, "the control plane became ready",
		func(ctx context.Context) error { return waitContainerReady(ctx, stateDir, port) },
		func() {
			printReadySummary(stateDir, port)
			fmt.Printf("Container %s is running in the background\n", container)
		},
		func(err error) error { return containerFailure(container, stateDir, err) },
	)
}

// streamUntil follows a container's boot output while waiting for its
// readiness from the host. The follower is only output: it can end for
// its own reasons, such as a closed pipe, without the container being
// any less alive. Its state is polled instead, and a container that
// stops before becoming ready is a failure; a wait that succeeds runs
// the report and leaves the container running. The waitingFor phrase
// names what was being waited for in a failure, and fail handles the
// failure, including the container's removal.
func streamUntil(ctx context.Context, container, waitingFor string, wait func(context.Context) error, report func(), fail func(error) error) error {
	logs := exec.CommandContext(ctx, "podman", "logs", "--follow", container)
	logs.Stdout = os.Stdout
	logs.Stderr = os.Stderr
	if err := logs.Start(); err != nil {
		return fmt.Errorf("stream container output: %w", err)
	}
	ready := make(chan error, 1)
	go func() { ready <- wait(ctx) }()
	dead := make(chan struct{}, 1)
	go func() {
		for {
			running, err := containerRunning(container)
			if err == nil && !running {
				dead <- struct{}{}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()

	select {
	case err := <-ready:
		stopStreaming(logs)
		if err != nil {
			return fail(err)
		}
		report()
		return nil
	case <-dead:
		stopStreaming(logs)
		return fail(fmt.Errorf("container %s exited before %s", container, waitingFor))
	}
}

// stopStreaming stops following the container's log and waits for the
// follower to end, so later boot output cannot interleave with the ready
// summary or a failure report.
func stopStreaming(logs *exec.Cmd) {
	_ = logs.Process.Kill()
	_ = logs.Wait()
}

// containerRunning reports whether the cluster container is running. A
// failed inspect, such as under a busy podman, is retried by the caller.
func containerRunning(container string) (bool, error) {
	out, err := exec.Command("podman", "inspect", "--format", "{{.State.Running}}", container).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// containerReadyTimeout bounds the whole readiness wait of a created
// cluster. A cold image cache makes the container's first boot pull the
// release payload for a long time before anything answers, so the budget
// is generous; Ctrl-C cancels the wait.
const containerReadyTimeout = 30 * time.Minute

// waitContainerReady waits for the created cluster's readiness from the
// host: first for the kubeconfig in the bind-mounted state directory to
// point at the published port, then for the control plane's readiness
// through it, the same wait host mode runs after boot. A warm re-create
// keeps a kubeconfig from the previous port; the boot rewrites it early,
// and the readiness client must be built from the rewritten URL.
func waitContainerReady(ctx context.Context, stateDir string, port int) error {
	ctx, cancel := context.WithTimeout(ctx, containerReadyTimeout)
	defer cancel()
	kubeconfig := filepath.Join(stateDir, certgen.AdminKubeconfigFilename)
	for !kubeconfigPointsAt(kubeconfig, port) {
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

// kubeconfigPointsAt reports whether the admin kubeconfig's server URL
// names the given port.
func kubeconfigPointsAt(path string, port int) bool {
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return false
	}
	for _, cluster := range config.Clusters {
		if cluster.Server == apiServerURL(port) {
			return true
		}
	}
	return false
}

// containerFailure reports a cluster container that did not become ready:
// the error carries the tail of the container's log and the state
// directory's full logs, a state-directory conflict gains the create-side
// remedies, and the container is removed while the state is preserved.
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

// containerLogTail returns the last lines of the container's log output,
// standard error included: refusals land there.
func containerLogTail(container string) string {
	// Both streams go to one buffer; the command shares a single pipe
	// for identical writers, so the copy is merged and race-free.
	var out bytes.Buffer
	cmd := exec.Command("podman", "logs", container)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if cmd.Run() != nil && out.Len() == 0 {
		return ""
	}
	return tailText(out.String(), containerLogTailLines)
}

// stateConflictHint names the create-side remedies when the container's
// log shows the state directory refusing reuse. The run inside cannot ask
// questions without a terminal, so its own hint names host-mode commands
// that do not apply here.
func stateConflictHint(logTail string) string {
	if strings.Contains(logTail, "cannot be reused by") {
		return "Use --fresh to remove the state directory and start fresh, or a different --name to get a separate one"
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

// clusterContainerPrefix prepends a cluster's name to name its container.
const clusterContainerPrefix = "coreshift-"

// The names and labels of the worker side: containers take the worker
// prefix, carry their cluster as a label, and keep their join material
// below their cluster's state directory.
const (
	workerContainerPrefix = "coreshift-worker-"
	workerClusterLabel    = "coreshift-cluster"
	workersDirName        = "workers"
)

// defaultClusterName is the cluster every command addresses unless a
// flag names another one.
const defaultClusterName = "coreshift"

// dnsLabelPattern is what a cluster's or worker's name may look like: a
// DNS label, the strictest shape the node and machine objects accept. The
// names also name directories, so they cannot carry separators.
var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// refuseInvalidClusterName refuses a cluster name that is not a DNS
// label: the name names the cluster's container and its state directory,
// so it cannot carry separators.
func refuseInvalidClusterName(name string) error {
	if dnsLabelPattern.MatchString(name) && len(name) <= 63 {
		return nil
	}
	return fmt.Errorf("cluster name %q is not a DNS label of at most 63 characters: lowercase letters, digits, and dashes", name)
}

// newCreateWorkerCommand builds the command that joins a worker to a
// running cluster: it mints the worker's container image and join
// material, starts one privileged worker container, and waits for the
// worker's node to register. Without --name the worker is named by
// counting the cluster's workers.
func newCreateWorkerCommand() *cobra.Command {
	var pullSecretPath, name, cluster string
	var verbose bool
	cmd := &cobra.Command{
		Use:          "worker",
		Short:        "Join a worker to a cluster in a podman container",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCreateWorker(cmd.Context(), name, cmd.Flags().Changed("name"), cluster, pullSecretPath, verbose)
		},
	}
	cmd.Flags().StringVar(&pullSecretPath, "pull-secret", "", "path to a Red Hat pull secret, as a fallback for clusters created before the cluster stored one")
	cmd.Flags().StringVar(&name, "name", "", "name of the worker; defaults to worker<number>, one past the cluster's worker count, skipping names in use; its container is called coreshift-worker-<name> and its node carries the name")
	cmd.Flags().StringVar(&cluster, "cluster", defaultClusterName, "name of the cluster the worker joins")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "stream the image build's output to the terminal")
	return cmd
}

// runCreateWorker joins a worker to a running cluster. The name comes
// from --name exactly as typed, or, without the flag, from counting the
// cluster's workers. It refuses a taken worker name, an unknown cluster,
// or a cluster whose API is not answering before touching anything,
// mints the join material and the worker image, starts the container
// detached, streams its boot output, and waits for the worker's node to
// register. On success the container keeps running; on any failure it is
// removed, with the join material, while the cluster's state is preserved.
func runCreateWorker(ctx context.Context, name string, explicit bool, cluster, pullSecretPath string, verbose bool) error {
	// A counted name is worker<number> and always fits; only a typed
	// name can carry a shape the objects cannot.
	if explicit && (!dnsLabelPattern.MatchString(name) || len(name) > 63) {
		return fmt.Errorf("worker name %q is not a DNS label of at most 63 characters: lowercase letters, digits, and dashes", name)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if _, err := exec.LookPath("podman"); err != nil {
		return fmt.Errorf("podman is required to create workers: %w", err)
	}
	if !explicit {
		counted, err := nextWorkerName(cluster)
		if err != nil {
			return err
		}
		name = counted
		fmt.Printf("Worker name: %s\n", name)
	}
	if pullSecretPath != "" {
		abs, err := filepath.Abs(pullSecretPath)
		if err != nil {
			return err
		}
		if _, err := os.Stat(abs); err != nil {
			return fmt.Errorf("read pull secret: %w", err)
		}
		pullSecretPath = abs
	}
	container := workerContainerPrefix + name
	if exec.Command("podman", "container", "exists", container).Run() == nil {
		return fmt.Errorf("worker %s already exists; use --name to create another worker, or delete it first", name)
	}
	port, stateDir, err := workerClusterTarget(cluster)
	if err != nil {
		return err
	}
	if !clusterReady(port) {
		return fmt.Errorf("cluster %s is not running at %s; start it first", cluster, apiServerURL(port))
	}

	materialDir := filepath.Join(stateDir, workersDirName, name)
	if err := join.Mint(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename), materialDir, name); err != nil {
		return err
	}
	if err := launchWorker(ctx, container, cluster, name, stateDir, materialDir, pullSecretPath, verbose); err != nil {
		// The failure path removed the container, when one started; the
		// join material is the worker's own ephemera and goes with it,
		// while the cluster's state stays.
		_ = os.RemoveAll(materialDir)
		return err
	}
	return nil
}

// nextWorkerName picks the name a bare worker creation takes: the number
// is one past the count of the cluster's existing workers — not the
// first free slot — and skips upward past any worker name already in
// use, so repeated bare commands scale out without colliding.
func nextWorkerName(cluster string) (string, error) {
	containers, err := containersNamed(workerContainerPrefix)
	if err != nil {
		return "", err
	}
	count := 0
	for _, container := range containers {
		if label, err := containerLabel(container, workerClusterLabel); err == nil && label == cluster {
			count++
		}
	}
	for n := count + 1; ; n++ {
		name := fmt.Sprintf("worker%d", n)
		if exec.Command("podman", "container", "exists", workerContainerPrefix+name).Run() != nil {
			return name, nil
		}
	}
}

// launchWorker mints the worker image from the release the cluster
// records, so the node binaries in it match the control plane, starts
// the worker container, and waits for its join.
func launchWorker(ctx context.Context, container, cluster, name, stateDir, materialDir, pullSecretPath string, verbose bool) error {
	pullSecret, authfile, cleanup, err := workerPullSecret(ctx, cluster, stateDir, pullSecretPath)
	if err != nil {
		return err
	}
	if cleanup != nil {
		// The cluster's stored secret was written to a throwaway file;
		// the image build is its only reader.
		defer cleanup()
	}
	client, err := image.NewClient(pullSecret)
	if err != nil {
		return fmt.Errorf("create image client: %w", err)
	}
	releaseRef := stateRelease(stateDir)
	if releaseRef == "" {
		return fmt.Errorf("cluster %s records no release to build the worker image from", cluster)
	}
	releaseRoot, err := client.CachedRelease(ctx, releaseRef)
	if err != nil {
		return fmt.Errorf("prepare release image: %w", err)
	}
	base, err := image.ResolveComponentImage(releaseRoot, "rhel-coreos")
	if err != nil {
		return err
	}
	imageRef, err := mint.WorkerImage(ctx, version, base, authfile, verbose)
	if err != nil {
		return err
	}

	fmt.Printf("Starting container %s\n", container)
	if err := runWorkerPodman(ctx, workerRunArgs(container, cluster, materialDir, imageRef)); err != nil {
		return err
	}
	return streamUntilWorkerReady(ctx, container, stateDir, name)
}

// workerPullSecret resolves the pull secret a worker image build pulls
// its base with, as podman's authfile: the flag's file when one was
// given, otherwise the Secret the cluster stores, written to a
// throwaway file whose removal the returned cleanup carries out. A
// cluster created before the Secret existed stores none; the flag is
// the fallback for it.
func workerPullSecret(ctx context.Context, cluster, stateDir, pullSecretPath string) (pullSecret []byte, authfile string, cleanup func(), err error) {
	if pullSecretPath != "" {
		pullSecret, err := os.ReadFile(pullSecretPath)
		if err != nil {
			return nil, "", nil, fmt.Errorf("read pull secret: %w", err)
		}
		return pullSecret, pullSecretPath, nil, nil
	}
	pullSecret, err = pullsecret.Fetch(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename))
	if err != nil {
		return nil, "", nil, err
	}
	if pullSecret == nil {
		return nil, "", nil, fmt.Errorf("cluster %s stores no pull secret; pass --pull-secret for this build, or re-create the cluster to store one", cluster)
	}
	authfile, err = writeAuthFile(pullSecret)
	if err != nil {
		return nil, "", nil, err
	}
	return pullSecret, authfile, func() { os.Remove(authfile) }, nil
}

// writeAuthFile writes the cluster's stored pull secret to a throwaway
// file the image build uses as podman's authfile.
func writeAuthFile(pullSecret []byte) (string, error) {
	file, err := os.CreateTemp("", "coreshift-auth-")
	if err != nil {
		return "", fmt.Errorf("create auth file: %w", err)
	}
	_, err = file.Write(pullSecret)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		os.Remove(file.Name())
		if err == nil {
			err = closeErr
		}
		return "", fmt.Errorf("write auth file: %w", err)
	}
	return file.Name(), nil
}

// workerRunArgs builds the podman command line that starts a worker
// container: detached and named, labeled with the cluster it joins, on
// the host network so it dials the control plane at loopback, and
// privileged, which hands it the writable cgroup hierarchy the node side
// needs. The join material is mounted read-only at the paths the
// entrypoint reads, and the host's kernel modules read-only for kubelet.
// The kubelet state directory stays inside the container: workers are
// ephemeral, recreated rather than restarted.
func workerRunArgs(container, cluster, materialDir, imageRef string) []string {
	return []string{
		"run", "--detach",
		"--name", container,
		"--label", workerClusterLabel + "=" + cluster,
		"--network", "host",
		"--privileged",
		"--volume", filepath.Join(materialDir, join.BootstrapKubeconfigFilename) + ":" + worker.BootstrapKubeconfigPath + ":ro,z",
		"--volume", filepath.Join(materialDir, join.NodeNameFilename) + ":" + worker.NodeNamePath + ":ro,z",
		"--volume", "/lib/modules:/lib/modules:ro",
		imageRef,
	}
}

// rootlessCgroupRemedy names the requirement a rootless launch that
// could not delegate cgroups leaves the user with: the worker is the
// only part of CoreShift that needs rootful podman.
const rootlessCgroupRemedy = "Rootless podman cannot give kubelet the cgroup hierarchy it needs here.\n" +
	"Rootful podman is the worker-only requirement: run the same command with sudo."

// runWorkerPodman starts the worker container, under a delegated cgroup
// scope when rootless: kubelet creates cgroups inside the container,
// which rootless podman only permits from a subtree systemd delegated to
// the user, so the launch asks the user's systemd for one.
func runWorkerPodman(ctx context.Context, args []string) error {
	command := append([]string{"podman"}, args...)
	if os.Geteuid() != 0 {
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return fmt.Errorf("systemd-run is required to delegate cgroups to a rootless worker container: %w\n%s", err, rootlessCgroupRemedy)
		}
		command = append([]string{"systemd-run", "--user", "--scope", "--quiet", "--property=Delegate=yes"}, command...)
	}
	run := exec.CommandContext(ctx, command[0], command[1:]...)
	run.Stdout = io.Discard // the detached run only prints the container ID
	run.Stderr = os.Stderr
	if err := run.Run(); err != nil {
		return fmt.Errorf("start worker container: %w", err)
	}
	return nil
}

// streamUntilWorkerReady follows the worker container's boot output
// while waiting for its node to register.
func streamUntilWorkerReady(ctx context.Context, container, stateDir, name string) error {
	return streamUntil(ctx, container, "the worker joined",
		func(ctx context.Context) error { return waitWorkerNode(ctx, stateDir, name) },
		func() { printWorkerSummary(container, stateDir, name) },
		func(err error) error { return workerFailure(container, stateDir, name, err) },
	)
}

// workerReadyTimeout bounds the whole join wait of a created worker: the
// container must boot CRI-O and kubelet, have its certificate request
// approved, and register its node. Ctrl-C cancels the wait.
const workerReadyTimeout = 10 * time.Minute

// waitWorkerNode waits for the worker's node object to appear against
// the cluster's admin kubeconfig, the moment a join succeeded.
func waitWorkerNode(ctx context.Context, stateDir, name string) error {
	config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(stateDir, certgen.AdminKubeconfigFilename))
	if err != nil {
		return fmt.Errorf("read admin kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create admin client: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, workerReadyTimeout)
	defer cancel()
	fmt.Println("Waiting for the worker to join")
	pollErr := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		return err == nil, nil
	})
	if pollErr != nil {
		return fmt.Errorf("wait for the node to register: %w", ctx.Err())
	}
	return nil
}

// printWorkerSummary prints what a joined worker looks like: its node
// reporting not ready until a CNI exists, and the container running in
// the background.
func printWorkerSummary(container, stateDir, name string) {
	fmt.Println()
	fmt.Println("Worker joined")
	fmt.Printf("  Node:       %s, reporting NotReady until a CNI exists\n", name)
	fmt.Printf("  Kubeconfig: %s\n", filepath.Join(stateDir, certgen.AdminKubeconfigFilename))
	fmt.Printf("Container %s is running in the background\n", container)
}

// workerFailure reports a worker container that did not join: the error
// carries the tail of the container's log, a join that failed on the
// cgroup hierarchy gains the rootful remedy, and the container is
// removed while the cluster's state is preserved.
func workerFailure(container, stateDir, name string, err error) error {
	tail := containerLogTail(container)
	if strings.Contains(tail, worker.CgroupDelegationMark) {
		err = fmt.Errorf("%w\n%s", err, rootlessCgroupRemedy)
	}
	if tail != "" {
		err = fmt.Errorf("%w\n\nrecent log output:\n%s", err, tail)
	}
	if rmErr := exec.Command("podman", "rm", "--force", container).Run(); rmErr != nil {
		err = fmt.Errorf("%w\nremoving the container failed; remove %s by hand", err, container)
	}
	return err
}

// newDeleteCommand builds the parent of everything deletable. It takes a
// noun naming what to delete: a cluster, or a worker joined to one. With
// no noun, or an unknown one, it prints the list and fails.
func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a CoreShift-managed thing",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Help(); err != nil {
				return err
			}
			if len(args) == 0 {
				return nil
			}
			return fmt.Errorf("unknown thing to delete: %s", args[0])
		},
	}
	cmd.AddCommand(newDeleteClusterCommand())
	cmd.AddCommand(newDeleteWorkerCommand())
	return cmd
}

// newDeleteClusterCommand builds the command that deletes a cluster: its
// workers go with it, then its container is stopped, torn down in reverse
// order like a host-mode shutdown, and removed, and its state directory is
// removed too, leaving nothing behind. The preserve-state flag keeps the
// state directory, and the preserve-workers flag leaves the cluster's
// workers running instead.
func newDeleteClusterCommand() *cobra.Command {
	var name string
	var preserveState, preserveWorkers bool
	cmd := &cobra.Command{
		Use:          "cluster",
		Short:        "Delete a cluster, its workers, and its state",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDelete(cmd.Context(), name, preserveState, preserveWorkers)
		},
	}
	cmd.Flags().StringVar(&name, "name", defaultClusterName, "name of the cluster to delete")
	cmd.Flags().BoolVar(&preserveState, "preserve-state", false, "keep the cluster's state directory, so a cluster re-created under the same name boots from it warm")
	cmd.Flags().BoolVar(&preserveWorkers, "preserve-workers", false, "leave the cluster's workers running; implies --preserve-state, since a worker cannot rejoin a cluster re-created without its state")
	return cmd
}

// containerStopSeconds is how long the delete command's stop waits for
// the container to exit after its stop signal. The signal triggers the
// same reverse-order teardown host mode runs; a healthy one takes tens of
// seconds, and the timeout only bounds a wedged container.
const containerStopSeconds = 120

// runDelete deletes a cluster: its workers go with it, then its container
// is stopped and removed, then its state directory is removed too — delete
// is cleanup, leaving nothing behind. The preserve-state flag keeps the
// state directory, and the preserve-workers flag leaves the workers
// running instead of deleting them, for the control-plane restart; it
// implies preserve-state, because a worker cannot rejoin a cluster
// re-created without its state. A cluster whose container is already gone
// but whose state remains reports that, and the state follows the same
// rule.
func runDelete(ctx context.Context, name string, preserveState, preserveWorkers bool) error {
	if preserveWorkers {
		preserveState = true
	}
	if _, err := exec.LookPath("podman"); err != nil {
		return fmt.Errorf("podman is required to delete clusters: %w", err)
	}
	stateDir, err := clusterDir(name)
	if err != nil {
		return err
	}
	container := clusterContainerPrefix + name
	running := exec.Command("podman", "container", "exists", container).Run() == nil
	if !running {
		if _, statErr := os.Stat(stateDir); statErr != nil {
			return unknownCluster(name)
		}
		fmt.Printf("Cluster %s is not running\n", name)
	}
	workers, err := clusterWorkers(name)
	if err != nil {
		return err
	}
	if !preserveWorkers {
		// The workers go before the cluster container, so each one's
		// node is removed while the cluster's API still answers.
		for _, worker := range workers {
			if err := deleteWorker(ctx, worker, name); err != nil {
				return err
			}
		}
	}
	if running {
		fmt.Printf("Stopping container %s\n", container)
		stop := exec.Command("podman", "stop", "--timeout", strconv.Itoa(containerStopSeconds), container)
		stop.Stderr = os.Stderr
		if err := stop.Run(); err != nil {
			return fmt.Errorf("stop container: %w", err)
		}
		if err := exec.Command("podman", "rm", container).Run(); err != nil {
			return fmt.Errorf("remove container: %w", err)
		}
		fmt.Printf("Deleted cluster %s\n", name)
	}
	if preserveWorkers && len(workers) > 0 {
		fmt.Printf("Workers left running: %s\n", strings.Join(workers, ", "))
	}
	if preserveState {
		fmt.Printf("State preserved at %s; coreshift cleanup removes it\n", stateDir)
		return nil
	}
	if _, err := os.Stat(stateDir); os.IsNotExist(err) {
		fmt.Printf("No state at %s\n", stateDir)
		return nil
	}
	if _, err := removeStateDir(stateDir); err != nil {
		return err
	}
	fmt.Printf("State removed at %s\n", stateDir)
	return pruneClustersTree()
}

// clusterWorkers lists the names of a cluster's workers: the worker
// containers carrying its label.
func clusterWorkers(cluster string) ([]string, error) {
	containers, err := containersNamed(workerContainerPrefix)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, container := range containers {
		if label, err := containerLabel(container, workerClusterLabel); err == nil && label == cluster {
			names = append(names, strings.TrimPrefix(container, workerContainerPrefix))
		}
	}
	return names, nil
}

// newDeleteWorkerCommand builds the command that deletes a worker: its
// container is stopped and removed, and the node object and machine
// stand-in its join created go with it, leaving the cluster's node list
// clean.
func newDeleteWorkerCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:          "worker",
		Short:        "Delete a worker, stopping its container and its node",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeleteWorker(cmd.Context(), name)
		},
	}
	cmd.Flags().StringVar(&name, "name", "worker1", "name of the worker to delete")
	return cmd
}

// runDeleteWorker deletes a worker: the worker the name picks is resolved
// to its cluster through its container's label, then deleted.
func runDeleteWorker(ctx context.Context, name string) error {
	if _, err := exec.LookPath("podman"); err != nil {
		return fmt.Errorf("podman is required to delete workers: %w", err)
	}
	container := workerContainerPrefix + name
	if exec.Command("podman", "container", "exists", container).Run() != nil {
		return fmt.Errorf("no worker named %s", name)
	}
	cluster, err := containerLabel(container, workerClusterLabel)
	if err != nil {
		return err
	}
	if cluster == "" {
		return fmt.Errorf("container %s carries no cluster label; remove it with podman", container)
	}
	return deleteWorker(ctx, name, cluster)
}

// deleteWorker deletes one worker of the named cluster: its container is
// stopped and removed, then the node object and machine stand-in the join
// minted are deleted through the cluster's admin kubeconfig, and the join
// material goes with them. The cluster must be running to remove the pair;
// when it is not, the node stays and the command says so; when the
// cluster's state is gone, the node went with it and there is nothing to
// remove.
func deleteWorker(ctx context.Context, name, cluster string) error {
	container := workerContainerPrefix + name
	fmt.Printf("Stopping container %s\n", container)
	stop := exec.Command("podman", "stop", "--timeout", strconv.Itoa(containerStopSeconds), container)
	stop.Stderr = os.Stderr
	if err := stop.Run(); err != nil {
		return fmt.Errorf("stop container: %w", err)
	}
	if err := exec.Command("podman", "rm", container).Run(); err != nil {
		return fmt.Errorf("remove container: %w", err)
	}
	fmt.Printf("Deleted worker %s\n", name)

	// The container goes first: a running kubelet recreates its node, so
	// the pair is removed only once nothing re-registers it.
	stateDir, err := clusterDir(cluster)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(stateDir); statErr != nil {
		fmt.Printf("Cluster %s has no state left; its node went with it\n", cluster)
		return nil
	}
	if port, _, err := workerClusterTarget(cluster); err == nil && clusterReady(port) {
		if err := join.Remove(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename), name); err != nil {
			return fmt.Errorf("remove the node and machine stand-in: %w", err)
		}
		_ = os.RemoveAll(filepath.Join(stateDir, workersDirName, name))
		fmt.Printf("Node %s removed from cluster %s\n", name, cluster)
		return nil
	}
	fmt.Printf("Cluster %s is not running; its node stays until the cluster's state does\n", cluster)
	return nil
}

// newGetCommand builds the parent of everything gettable. It takes a noun
// naming what to get; with no noun, or an unknown one, it prints the list
// and fails.
func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Get CoreShift-managed things",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Help(); err != nil {
				return err
			}
			if len(args) == 0 {
				return nil
			}
			return fmt.Errorf("unknown thing to get: %s", args[0])
		},
	}
	cmd.AddCommand(newGetClustersCommand())
	cmd.AddCommand(newGetKubeconfigCommand())
	cmd.AddCommand(newGetWorkersCommand())
	return cmd
}

// newGetClustersCommand builds the command that lists the clusters with
// their name, published port, recorded release, and readiness.
func newGetClustersCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "clusters",
		Short:        "List clusters",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runGetClusters()
		},
	}
}

// runGetClusters lists the clusters. Readiness is a live probe of each
// cluster's API server, not the container's running state, so a booting
// or wedged cluster shows as not ready.
func runGetClusters() error {
	if _, err := exec.LookPath("podman"); err != nil {
		return fmt.Errorf("podman is required to list clusters: %w", err)
	}
	containers, err := clusterContainers()
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		fmt.Println("No clusters")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tPORT\tRELEASE\tREADY")
	for _, container := range containers {
		name := strings.TrimPrefix(container, clusterContainerPrefix)
		port, err := containerPort(container)
		if err != nil {
			return err
		}
		stateDir, err := clusterDir(name)
		if err != nil {
			return err
		}
		release := stateRelease(stateDir)
		if release == "" {
			release = "-"
		}
		ready := "not ready"
		if clusterReady(port) {
			ready = "ready"
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", name, port, release, ready)
	}
	return w.Flush()
}

// newGetKubeconfigCommand builds the command that prints the path to a
// cluster's admin kubeconfig.
func newGetKubeconfigCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:          "kubeconfig",
		Short:        "Print the path to a cluster's admin kubeconfig",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runGetKubeconfig(name)
		},
	}
	cmd.Flags().StringVar(&name, "name", defaultClusterName, "name of the cluster")
	return cmd
}

// runGetKubeconfig prints the path to the named cluster's admin
// kubeconfig, refusing when no state exists for the name.
func runGetKubeconfig(name string) error {
	stateDir, err := clusterDir(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(stateDir, certgen.AdminKubeconfigFilename)); err != nil {
		return unknownCluster(name)
	}
	fmt.Println(filepath.Join(stateDir, certgen.AdminKubeconfigFilename))
	return nil
}

// newGetWorkersCommand builds the command that lists the workers with
// their name, cluster, and readiness.
func newGetWorkersCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "workers",
		Short:        "List workers",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runGetWorkers()
		},
	}
}

// runGetWorkers lists the workers. Readiness is the worker's own node
// condition, read live from the cluster's API, so a worker whose node
// reports not ready — which is every worker until a CNI exists — shows
// as not ready.
func runGetWorkers() error {
	if _, err := exec.LookPath("podman"); err != nil {
		return fmt.Errorf("podman is required to list workers: %w", err)
	}
	containers, err := containersNamed(workerContainerPrefix)
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		fmt.Println("No workers")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tCLUSTER\tREADY")
	for _, container := range containers {
		name := strings.TrimPrefix(container, workerContainerPrefix)
		cluster := "-"
		ready := "not ready"
		if label, err := containerLabel(container, workerClusterLabel); err == nil && label != "" {
			cluster = label
			if _, stateDir, err := workerClusterTarget(label); err == nil && workerNodeReady(stateDir, name) {
				ready = "ready"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", name, cluster, ready)
	}
	return w.Flush()
}

// workerNodeReady reports whether the worker's node object carries a
// ready condition, a live read against the cluster's API. Anything in
// the way — a stopped cluster, a missing node — counts as not ready.
func workerNodeReady(stateDir, name string) bool {
	config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(stateDir, certgen.AdminKubeconfigFilename))
	if err != nil {
		return false
	}
	config.Timeout = readyProbeTimeout
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return false
	}
	node, err := client.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// clusterContainers returns the names of the cluster containers, sorted:
// every container named coreshift-* is one, except the workers, which
// share the prefix and are listed by their own command.
func clusterContainers() ([]string, error) {
	containers, err := containersNamed(clusterContainerPrefix)
	if err != nil {
		return nil, err
	}
	var clusters []string
	for _, name := range containers {
		if !strings.HasPrefix(name, workerContainerPrefix) {
			clusters = append(clusters, name)
		}
	}
	return clusters, nil
}

// containersNamed returns the names of the containers carrying the given
// prefix, sorted.
func containersNamed(prefix string) ([]string, error) {
	out, err := exec.Command("podman", "ps", "-a", "--format", "{{.Names}}").Output()
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var containers []string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(name, prefix) {
			containers = append(containers, name)
		}
	}
	slices.Sort(containers)
	return containers, nil
}

// clusterTarget resolves a cluster running in a container to its port and
// state directory, refusing when no cluster with that name exists. The
// state directory comes from the name directly; only the port is asked of
// the container, which is where this deployment's API server binds.
func clusterTarget(name string) (int, string, error) {
	container := clusterContainerPrefix + name
	if exec.Command("podman", "container", "exists", container).Run() != nil {
		return 0, "", unknownCluster(name)
	}
	port, err := containerPort(container)
	if err != nil {
		return 0, "", err
	}
	dir, err := clusterDir(name)
	if err != nil {
		return 0, "", err
	}
	return port, dir, nil
}

// workerClusterTarget resolves the cluster a worker joins to its port and
// state directory: a cluster in a container through its published port,
// and a host-mode cluster, which has no container to ask, through the
// port its admin kubeconfig records — boot rewrites the kubeconfig to the
// port in use, so it names where a running cluster answers.
func workerClusterTarget(cluster string) (int, string, error) {
	port, stateDir, err := clusterTarget(cluster)
	if err == nil {
		return port, stateDir, nil
	}
	dir, dirErr := clusterDir(cluster)
	if dirErr != nil {
		return 0, "", dirErr
	}
	if port := kubeconfigServerPort(dir); port != 0 {
		return port, dir, nil
	}
	return 0, "", err
}

// kubeconfigServerPort returns the port the admin kubeconfig's server URL
// names, or zero when there is none to read.
func kubeconfigServerPort(stateDir string) int {
	config, err := clientcmd.LoadFromFile(filepath.Join(stateDir, certgen.AdminKubeconfigFilename))
	if err != nil {
		return 0
	}
	for _, cluster := range config.Clusters {
		server, err := url.Parse(cluster.Server)
		if err != nil {
			continue
		}
		if port, err := strconv.Atoi(server.Port()); err == nil {
			return port
		}
	}
	return 0
}

// containerLabel returns the value of a container's label, or an empty
// string when the container carries none.
func containerLabel(container, label string) (string, error) {
	out, err := exec.Command("podman", "inspect", "--format",
		fmt.Sprintf(`{{index .Config.Labels %q}}`, label), container).Output()
	if err != nil {
		return "", fmt.Errorf("inspect container %s: %w", container, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// containerPort returns the port a cluster container publishes its API
// on. The create command publishes it as an identity mapping, so the host
// port is the port the cluster runs on.
func containerPort(container string) (int, error) {
	out, err := exec.Command("podman", "inspect", "--format",
		"{{range $port, $bindings := .NetworkSettings.Ports}}{{range $bindings}}{{.HostPort}}{{end}}{{end}}",
		container).Output()
	if err != nil {
		return 0, fmt.Errorf("inspect container %s: %w", container, err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("read %s's published port: %w", container, err)
	}
	return port, nil
}

// bootRelease returns the release image reference this run boots, with a
// note to print under it. An explicitly requested release resolves as
// before; with none requested and a state directory recording a release,
// the recorded one boots again — a warm re-create, not a surprise
// upgrade — and the registry is still checked for a newer patch of its
// stream to mention in the note.
func bootRelease(ctx context.Context, client *image.Client, release releaseOptions, stateDir string) (string, string, error) {
	recorded := stateRelease(stateDir)
	if release.image == "" && release.version == "" && recorded != "" {
		return recorded, newerReleaseNote(ctx, client, release, recorded), nil
	}
	ref, err := client.ResolveRelease(ctx, release.registry, release.version, release.arch, release.image)
	return ref, "", err
}

// newerReleaseNote returns a one-line note naming the newest patch of the
// recorded release's stream when the registry holds a newer one, saying
// the state directory can be upgraded in place and with which flag. The
// check is best effort: without an answer from the registry the warm
// re-create proceeds without a note.
func newerReleaseNote(ctx context.Context, client *image.Client, release releaseOptions, recorded string) string {
	version, ok := image.ParseRefVersion(recorded)
	if !ok {
		return ""
	}
	tags, err := client.ListTags(ctx, release.registry)
	if err != nil {
		return ""
	}
	stream := fmt.Sprintf("%d.%d", version.Major, version.Minor)
	newest, err := image.SelectRelease(image.ReleaseVersions(tags, release.arch), release.arch, stream)
	if err != nil || newest.Version.Compare(version) <= 0 {
		return ""
	}
	return fmt.Sprintf("  A newer release, %s, exists; this state can be upgraded in place with --version %s",
		newest.Version, newest.Version)
}

// readyProbeTimeout bounds the readiness probe of a cluster's API server.
const readyProbeTimeout = 3 * time.Second

// clusterReady reports whether a cluster's API server answers its health
// endpoint. The probe is live with a short timeout, so a booting or
// wedged control plane shows as not ready whatever its container's
// running state says. TLS is not verified: reaching a healthy API server
// is the question, not whose certificate it serves.
func clusterReady(port int) bool {
	client := &http.Client{
		Timeout:   readyProbeTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := client.Get(apiServerURL(port) + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// newCleanupCommand builds the command that removes CoreShift's on-disk
// artifacts: cluster state directories, and with --cache the shared image
// cache.
func newCleanupCommand() *cobra.Command {
	var name string
	var all bool
	var clearCache bool
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Remove CoreShift cluster state and cache",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if all && cmd.Flags().Changed("name") {
				return fmt.Errorf("--all already removes every cluster's state; drop --name")
			}
			return runCleanup(name, all, clearCache)
		},
	}
	cmd.Flags().StringVar(&name, "name", defaultClusterName, "name of the cluster whose state directory is removed")
	cmd.Flags().BoolVar(&all, "all", false, "remove every cluster's state: the whole clusters tree")
	cmd.Flags().BoolVar(&clearCache, "cache", false, "also remove the shared image cache, which the next run rebuilds by re-pulling")
	return cmd
}

// cleanupTarget is a directory cleanup removes, with the name for it in
// messages.
type cleanupTarget struct {
	path string
	kind string
}

// runCleanup removes the chosen cluster state and, with clearCache, the
// shared image cache. It owns the clusters tree and nothing else under
// the state root, so unrelated directories kept there survive. It refuses
// while a control plane is running, because cleaning under a live
// instance would pull its data out from under it. A missing directory is
// reported, not an error, so the command is safe to re-run and to call
// from scripts.
func runCleanup(name string, all, clearCache bool) error {
	running, err := runningControlPlane()
	if err != nil {
		return err
	}
	if running != "" {
		return fmt.Errorf("a CoreShift control plane is running:\n  %s\n"+
			"Stop it first; cleaning up under a running control plane would pull its data out from under it", running)
	}
	targets, err := cleanupTargets(name, all)
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
	// A cleanup leaves no empty shells behind: the clusters tree and the
	// state root go too once they are empty.
	if err := pruneClustersTree(); err != nil {
		return err
	}
	if removed {
		fmt.Printf("Freed %s\n", humanSize(freed))
	}
	return nil
}

// pruneClustersTree removes the clusters tree and the state root once they
// are empty, so a state removal leaves no empty shells behind. Removing a
// non-empty directory fails and is ignored, so kept siblings survive.
func pruneClustersTree() error {
	tree, err := clustersTree()
	if err != nil {
		return err
	}
	_ = os.Remove(tree)
	_ = os.Remove(filepath.Dir(tree))
	return nil
}

// cleanupTargets lists the state to remove: the named cluster's
// directory, or with all every cluster directory followed by the clusters
// tree itself. The tree comes last so its size, measured right before its
// removal, counts only the content the earlier removals left.
func cleanupTargets(name string, all bool) ([]cleanupTarget, error) {
	if !all {
		dir, err := clusterDir(name)
		if err != nil {
			return nil, err
		}
		return []cleanupTarget{{path: dir, kind: "state directory"}}, nil
	}
	tree, err := clustersTree()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(tree)
	if err != nil {
		if os.IsNotExist(err) {
			return []cleanupTarget{{path: tree, kind: "clusters tree"}}, nil
		}
		return nil, err
	}
	var targets []cleanupTarget
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		targets = append(targets, cleanupTarget{path: filepath.Join(tree, entry.Name()), kind: "state directory"})
	}
	return append(targets, cleanupTarget{path: tree, kind: "clusters tree"}), nil
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

// clustersDirName is the directory under the state root holding every
// cluster's state: one directory per cluster, named after it. The name is
// the cluster's identity in state; the port its API server binds this
// deployment appears nowhere in it.
const clustersDirName = "clusters"

// clusterDir returns the named cluster's state directory.
func clusterDir(name string) (string, error) {
	root, err := stateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, clustersDirName, name), nil
}

// clustersTree returns the directory holding every cluster's state.
func clustersTree() (string, error) {
	root, err := stateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, clustersDirName), nil
}

// existingClusters lists the names of the clusters that exist: the
// directories under the clusters tree.
func existingClusters() ([]string, error) {
	tree, err := clustersTree()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(tree)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// unknownCluster is the error for a name no cluster exists under, naming
// the clusters that do.
func unknownCluster(name string) error {
	clusters, err := existingClusters()
	if err != nil || len(clusters) == 0 {
		return fmt.Errorf("no cluster named %s", name)
	}
	return fmt.Errorf("no cluster named %s; clusters that exist: %s", name, strings.Join(clusters, ", "))
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

// clusterMarkerFilename records a cluster state directory's name and the
// release image that created its etcd data, so the directory is
// self-describing. etcd data only moves within the release's minor
// stream, so the release is checked before anything starts: a newer patch
// of the stream upgrades the directory in place, anything else refuses.
const clusterMarkerFilename = "cluster"

// clusterMarker is the content of a cluster state directory's marker.
type clusterMarker struct {
	Name    string `json:"name"`
	Release string `json:"release"`
}

// readClusterMarker returns the marker a state directory records, or the
// zero marker when it records none.
func readClusterMarker(stateDir string) (clusterMarker, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, clusterMarkerFilename))
	if os.IsNotExist(err) {
		return clusterMarker{}, nil
	}
	if err != nil {
		return clusterMarker{}, fmt.Errorf("read cluster marker: %w", err)
	}
	var marker clusterMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return clusterMarker{}, fmt.Errorf("parse cluster marker: %w", err)
	}
	return marker, nil
}

// writeClusterMarker records a cluster's name and release in its state
// directory.
func writeClusterMarker(stateDir string, marker clusterMarker) error {
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode cluster marker: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, clusterMarkerFilename), data, 0o644); err != nil {
		return fmt.Errorf("write cluster marker: %w", err)
	}
	return nil
}

// stateRelease returns the release a state directory's marker records, or
// an empty string when it records none.
func stateRelease(stateDir string) string {
	marker, err := readClusterMarker(stateDir)
	if err != nil {
		return ""
	}
	return marker.Release
}

// ensureClusterMarker refuses to reuse a state directory whose etcd data
// cannot run the release this run resolved, and records the cluster's
// name and the release on a fresh directory. A newer patch of the same
// minor stream reuses the directory as an in-place upgrade, the same move
// a real OpenShift cluster makes on a patch update; anything else
// refuses.
func ensureClusterMarker(stateDir, name, releaseRef string) error {
	marker, err := readClusterMarker(stateDir)
	if err != nil {
		return err
	}
	if marker.Release != "" && !releaseReuse(marker.Release, releaseRef) {
		return stateRefusal(stateDir, marker.Release, releaseRef,
			fmt.Sprintf("Use a different --name to get a separate state directory, or remove %s to start fresh", stateDir))
	}
	if marker.Name == name && marker.Release == releaseRef {
		return nil
	}
	return writeClusterMarker(stateDir, clusterMarker{Name: name, Release: releaseRef})
}

// releaseReuse reports whether a state directory recorded as created by
// recorded can boot releaseRef: the same reference, or a newer patch of
// the same minor stream, which upgrades the directory in place like a
// real cluster's patch update. References carrying no parseable version
// fall back to exact match rather than guessing.
func releaseReuse(recorded, releaseRef string) bool {
	if recorded == releaseRef {
		return true
	}
	have, haveVersion := image.ParseRefVersion(recorded)
	want, wantVersion := image.ParseRefVersion(releaseRef)
	return haveVersion && wantVersion &&
		want.Major == have.Major && want.Minor == have.Minor &&
		want.Compare(have) >= 0
}

// stateRefusal is the error for a state directory that cannot run a
// release, stating the reuse rule and the mode's remedy.
func stateRefusal(stateDir, recorded, releaseRef, remedy string) error {
	return fmt.Errorf("state directory %s was created by release %s and cannot be reused by %s\n"+
		"Only a newer patch of the same minor stream can upgrade it in place.\n%s",
		stateDir, recorded, releaseRef, remedy)
}

// offerFreshStart offers to remove a state directory that refused reuse.
// With a terminal on standard input it prints the refusal and asks
// whether to remove the directory and start fresh; an explicit yes
// removes it and returns nil, so the run continues from scratch. Any
// other answer returns the refusal. Without a terminal it does not ask
// and returns the refusal with a hint at the cleanup command.
func offerFreshStart(stateDir, name string, refusal error) error {
	return askFreshStart(stateDir, refusal,
		fmt.Sprintf("To remove it without a prompt, run: coreshift cleanup --name %s", name))
}

// offerCreateFreshStart offers the same for a create, whose scripted
// remedy is its own --fresh flag rather than the cleanup command.
func offerCreateFreshStart(stateDir string, refusal error) error {
	return askFreshStart(stateDir, refusal, "To remove it without a prompt, run create cluster with --fresh")
}

// askFreshStart prints a refusal and asks whether to remove the state
// directory and start fresh. With a terminal an explicit yes removes the
// directory and returns nil; any other answer returns the refusal.
// Without a terminal no question is asked, and the refusal gains hint.
func askFreshStart(stateDir string, refusal error, hint string) error {
	if !stdinIsTerminal() {
		return fmt.Errorf("%w\n%s", refusal, hint)
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
// terminal. Pipes and character devices such as the null device that
// detached containers wire to standard input answer no, so scripts and
// containers never see a prompt.
func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// ensureCertificates generates the certificate and kubeconfig set into
// stateDir unless a complete set is already present. Reuse matters: a fresh
// CA on every run would invalidate kubeconfigs saved outside the state
// directory and leave etcd data trusted by a CA nothing signs for anymore.
// A set whose service-serving certificate lacks the loopback address counts
// as incomplete, so it is regenerated rather than reused.
func ensureCertificates(stateDir, name string, port int) error {
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
		certgen.ClusterMachineApproverKubeconfigFilename,
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
	return certgen.GenerateAll(stateDir, certgen.Options{APIServerURL: apiServerURL(port), ClusterName: name})
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

// restoreAdminContext points the admin kubeconfig's current context back
// at the certificate-based admin context, which carries the cluster's
// name like every generated entry. Logging in with oc adds token
// contexts to the file and makes one of them current; a startup would
// then try the token before the OAuth API that validates it exists, and
// stall until the seeding timeouts fail the run. Login contexts are
// kept; only the selection is corrected.
func restoreAdminContext(stateDir, name string) error {
	path := filepath.Join(stateDir, certgen.AdminKubeconfigFilename)
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return fmt.Errorf("read admin kubeconfig: %w", err)
	}
	contextName := "admin@" + name
	if config.CurrentContext == contextName {
		return nil
	}
	if _, ok := config.Contexts[contextName]; !ok {
		return fmt.Errorf("admin kubeconfig has no %s context", contextName)
	}
	config.CurrentContext = contextName
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
  - %s
kubeClientConfig:
  kubeConfig: %s
`, advertiseAddress, openShiftAPIServerPort,
		filepath.Join(stateDir, certgen.ServiceServingCertFilename),
		filepath.Join(stateDir, certgen.ServiceServingKeyFilename),
		filepath.Join(stateDir, certgen.CACertFilename),
		etcdClientURL,
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
// admin identity, whose permissions cover the review call. The entries
// carry the cluster's name, like every generated kubeconfig's.
func writeTokenReviewKubeconfig(stateDir, name string, port int) (string, error) {
	path := filepath.Join(stateDir, "token-review.kubeconfig")
	admin := "admin@" + name
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %s
  cluster:
    certificate-authority: %s
    server: %s/apis/oauth.openshift.io/v1/tokenreviews
contexts:
- name: %s
  context:
    cluster: %s
    user: %s
current-context: %s
users:
- name: %s
  user:
    client-certificate: %s
    client-key: %s
`, name,
		filepath.Join(stateDir, certgen.CACertFilename),
		apiServerURL(port),
		admin, name, admin, admin, admin,
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
