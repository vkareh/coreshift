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
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/vkareh/coreshift/internal/aggregation"
	"github.com/vkareh/coreshift/internal/bootcache"
	"github.com/vkareh/coreshift/internal/certgen"
	"github.com/vkareh/coreshift/internal/clusterversion"
	"github.com/vkareh/coreshift/internal/crd"
	"github.com/vkareh/coreshift/internal/dnstls"
	"github.com/vkareh/coreshift/internal/featuregates"
	"github.com/vkareh/coreshift/internal/gate"
	"github.com/vkareh/coreshift/internal/gpgme"
	"github.com/vkareh/coreshift/internal/mint"
	"github.com/vkareh/coreshift/internal/networkconfig"
	"github.com/vkareh/coreshift/internal/oauth"
	"github.com/vkareh/coreshift/internal/oauthclient"
	"github.com/vkareh/coreshift/internal/pullsecret"
	"github.com/vkareh/coreshift/internal/rbac"
	"github.com/vkareh/coreshift/internal/supervisor"
	"github.com/vkareh/coreshift/internal/warnings"
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
	networkOperatorMetricsPort     = 9104
	dnsOperatorMetricsPort         = 60000
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
	machineApproverMetricsPort, networkOperatorMetricsPort,
	dnsOperatorMetricsPort,
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

// oauthEndpointPort is the port clients reach the OAuth server's login
// endpoints on, as the discovery document advertises them: the server's
// own fixed port on the host, and inside a container the port after the
// API port, which the container publishes the server on — clients run
// on the host there, and only the API port's neighborhood is published.
func oauthEndpointPort(port int) int {
	if os.Getenv(containerEnv) != "" {
		return port + 1
	}
	return oauthServerPort
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
// noun naming what to run: the control plane. With no noun, or an
// unknown one, it prints the list and fails.
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
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version)
			return err
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
	cmd.Flags().StringVar(&release.version, "version", "", "OpenShift version to run, X.Y, X.Y.Z, or newest for the registry's absolute newest; defaults to the newest verified release, or the release recorded in the state directory")
	cmd.Flags().StringVar(&release.registry, "registry", defaultReleaseRepository, "repository to resolve the release image from")
	cmd.Flags().StringVar(&release.arch, "arch", defaultArch, "architecture of the release payload, as uname -m reports it")
	if err := cmd.MarkFlagRequired("pull-secret"); err != nil {
		panic(err) // the flag is defined above
	}
	return cmd
}

func runAPI(ctx context.Context, pullSecretPath, name string, port int, verbose bool, release releaseOptions) error {
	if err := refuseReservedPort(port); err != nil {
		return err
	}
	if err := refuseInvalidClusterName(name); err != nil {
		return err
	}
	// API server warnings follow the run's output discipline: quiet
	// unless verbose, and quoted in the failure report either way.
	warnings.SetVerbose(verbose)
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
	payloadDir, err := client.CachedPayload(ctx, releaseRef)
	if err != nil {
		return fmt.Errorf("prepare release payload: %w", err)
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
		"openshift-apiserver", "oauth-apiserver", "oauth-server", "openshift-controller-manager", "cluster-policy-controller", "cluster-machine-approver", "cluster-version-operator", "cluster-network-operator", "cluster-dns-operator", "cluster-config-api")
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

	// The version operator wants the node it would schedule payload
	// retrieval on; this control plane runs no nodes, so the host the
	// processes run on stands in, like the bootstrap host does for the
	// installer's bootstrap pod.
	nodeName, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("determine host name: %w", err)
	}

	// gateOn holds a child's start until the objects it needs exist,
	// whoever applied them: the boot cache's instant copies, the seed
	// layer's post-readiness work, the version operator's manifest
	// application once it owns them. A timeout points at the operator,
	// the deliverer.
	gateOn := func(ctx context.Context, waitingFor string, objects ...gate.Object) error {
		return gate.Wait(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename), waitingFor,
			filepath.Join(stateDir, "logs", "cluster-version-operator.log"), objects...)
	}

	// The controller managers' pod security labeling reads the grant that
	// lets every signed-in user use the restricted constraint; the boot
	// cache seeds the payload's copy of the pair, so the gate passes
	// instantly, and the version operator converges the pair to its own
	// versions as it does everything else.
	controllerRBAC := []gate.Object{
		gate.ClusterRole("system:openshift:scc:restricted-v3"),
		gate.ClusterRoleBinding("system:openshift:scc:restricted-v3"),
	}

	// The network operator maps a broad slice of the API before its
	// controllers can start: its watchers resolve the config, operator,
	// and machine-configuration APIs, and its status machinery reads the
	// network ClusterOperator the payload carries. The version operator
	// delivers every one; the binding behind the operator's own
	// service-account identity is in the set for the same reason the
	// approver's is.
	networkOperatorWaits := []gate.Object{
		gate.Definition("networks.config.openshift.io"),
		gate.Definition("networks.operator.openshift.io"),
		gate.Definition("egressrouters.network.operator.openshift.io"),
		gate.Definition("operatorpkis.network.operator.openshift.io"),
		gate.Definition("infrastructures.config.openshift.io"),
		gate.Definition("proxies.config.openshift.io"),
		gate.Definition("ingresscontrollers.operator.openshift.io"),
		gate.Definition("machineconfigs.machineconfiguration.openshift.io"),
		gate.Definition("machineconfigpools.machineconfiguration.openshift.io"),
		gate.ClusterOperator("network"),
		gate.ClusterRoleBinding("cluster-network-operator"),
	}

	// The DNS operator maps the DNS definition its controller watches
	// and runs under the payload's own binding; the version operator
	// delivers both. The feature gate record and the ClusterVersion its
	// feature gate accessor reads are seeded ahead of the version
	// operator, and the cluster operators definition its status
	// controller watches arrived with the network operator's own waits.
	dnsOperatorWaits := []gate.Object{
		gate.Definition("dnses.operator.openshift.io"),
		gate.ClusterRoleBinding("openshift-dns-operator"),
	}

	// The environment the network operator's payload deployment gives
	// it, with every image resolved from the release the run resolved.
	networkOperatorEnvironment, err := networkOperatorEnv(releaseRoot, payloadDir)
	if err != nil {
		return fmt.Errorf("build the network operator environment: %w", err)
	}

	// The environment the DNS operator's payload deployment gives it,
	// with every image resolved from the release the run resolved and
	// the component kubeconfig in the place its client library looks.
	dnsOperatorEnvironment, err := dnsOperatorEnv(releaseRoot, payloadDir, filepath.Join(stateDir, certgen.ClusterDNSOperatorKubeconfigFilename))
	if err != nil {
		return fmt.Errorf("build the DNS operator environment: %w", err)
	}

	// Flag sets verified against the 4.22.8 payload: kube-apiserver reaches
	// readiness with exactly the flags below; the controllers stay up with
	// service-account credentials and leader election off.
	//
	// The children split in two groups around the ready summary. The
	// first group is the API's readiness path — etcd, the API server and
	// its controllers, the version operator ahead of the OpenShift
	// children whose starts gate on what it applies, the aggregated
	// servers, and the controller managers — everything the summary's
	// readiness confirmation waits on. Behind the summary follow the
	// operators that converge in the background the way operators settle
	// after a real cluster's API answers: the machine approver, the
	// network operator, and the DNS operator.
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
			Args: kubeAPIServerArgs(stateDir, etcdClientURL, port, kubeAPIServerConfig, tokenReviewKubeconfig),
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
				// manager issues what its approver approves: the kubelet
				// client signer the join requests, and the client signer
				// the node-side certificate managers, above all the OVN
				// node pod's, request their short-lived certificates
				// with.
				"--cluster-signing-kubelet-client-cert-file=" + filepath.Join(stateDir, certgen.CACertFilename),
				"--cluster-signing-kubelet-client-key-file=" + filepath.Join(stateDir, certgen.CAKeyFilename),
				"--cluster-signing-kube-apiserver-client-cert-file=" + filepath.Join(stateDir, certgen.CACertFilename),
				"--cluster-signing-kube-apiserver-client-key-file=" + filepath.Join(stateDir, certgen.CAKeyFilename),
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
		// The version operator applies the release payload's manifests to
		// the cluster for the control plane's lifetime — the same job the
		// installer's bootstrap gives it on a fresh cluster with no nodes.
		// The flags are that bootstrap pod's: pointed at the release this
		// run resolved, run against the admin kubeconfig the way the
		// bootstrap runs against its loopback one, with update following
		// and the metrics listener off. The payload it applies is the
		// release image's own manifest directories, which the payload
		// cache holds; the operator's supported environment override
		// points it there, standing in for its image's filesystem. It
		// starts ahead of the OpenShift children whose starts gate on the
		// objects it applies.
		{
			Name: "cluster-version-operator",
			Path: openShiftBinaries["cluster-version-operator"],
			Args: []string{
				"start",
				"--release-image=" + releaseRef,
				"--kubeconfig=" + filepath.Join(stateDir, certgen.AdminKubeconfigFilename),
				"--node-name=" + nodeName,
				"--enable-auto-update=false",
				"--listen=",
				"--v=2",
			},
			Env: append(os.Environ(), "PAYLOAD_OVERRIDE="+payloadDir),
			// The operator reads cluster configuration from its first
			// moments and waits for the version object before applying
			// anything, so its prerequisites are seeded here, after every
			// earlier seed has run, right before it starts: the objects
			// it waits for, then the feature gate record its operand
			// maintains in a real cluster — the network operator's
			// feature gate reader requires it, and the version
			// operator's own stop-on-change controller must find it
			// already in place — and then the boot cache, the copies of
			// the payload objects the API path's later gates wait on,
			// seeded from the same payload so those gates pass the
			// moment their children come up instead of waiting out this
			// operator's convergence of them.
			BeforeStart: func(ctx context.Context) error {
				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				fmt.Println("Seeding cluster version operator prerequisites")
				if err := clusterversion.Seed(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename), payloadDir); err != nil {
					return err
				}
				fmt.Println("Seeding feature gate record")
				if err := featuregates.Seed(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename),
					openShiftBinaries["cluster-config-api"], payloadDir); err != nil {
					return err
				}
				fmt.Println("Seeding the API's readiness path")
				return bootcache.Seed(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename), payloadDir)
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
			// The admission chain this server runs gates namespaced writes
			// on informers over the quota and security APIs, so their
			// definitions must exist before it starts. The default security
			// context constraints themselves are applied by the version
			// operator from the release payload; the controllers that read
			// them retry until they arrive.
			BeforeStart: func(ctx context.Context) error {
				return gateOn(ctx, "the quota and security definitions",
					gate.Definition("clusterresourcequotas.quota.openshift.io"),
					gate.Definition("securitycontextconstraints.security.openshift.io"))
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
			// The OAuth definition must exist before this server starts;
			// the boot cache seeds the payload's copy, so the gate passes
			// instantly, and the version operator converges the
			// definition as it does everything else.
			BeforeStart: func(ctx context.Context) error {
				return gateOn(ctx, "the OAuth definition",
					gate.Definition("oauths.config.openshift.io"))
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
			// The controller managers label namespaces with their pod
			// security level from the constraint use grants the cluster
			// carries, so the grant must exist before they start; the
			// boot cache's copy makes that instant.
			BeforeStart: func(ctx context.Context) error {
				return gateOn(ctx, "the controller RBAC", controllerRBAC...)
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
			BeforeStart: func(ctx context.Context) error {
				return gateOn(ctx, "the controller RBAC", controllerRBAC...)
			},
			Env: append(os.Environ(), "POD_NAMESPACE=default"),
		},
	}

	// The operators that settle behind the ready summary: their gates
	// wait for the version operator's delivery the same way, but nothing
	// on the API's readiness path waits on them, so they start once the
	// summary has printed and converge in the background.
	operators := []supervisor.Process{
		// The machine approver approves the node certificate requests a
		// joining node's bootstrap identity makes, matching each against
		// the machine standing in for the cloud machine that would back
		// the node. The management and workload clusters are the same
		// here, so both kubeconfig flags point at the one component
		// kubeconfig, whose certificate carries the payload's own
		// service-account identity so the permissions the payload's
		// binding grants are the approver's. Its ClusterOperator status
		// reporting stays off because this control plane runs no
		// ClusterOperator machinery; everything else runs as the payload
		// deploys it.
		{
			Name: "cluster-machine-approver",
			Path: openShiftBinaries["cluster-machine-approver"],
			Args: []string{
				"--management-cluster-kubeconfig=" + filepath.Join(stateDir, certgen.ClusterMachineApproverKubeconfigFilename),
				"--workload-cluster-kubeconfig=" + filepath.Join(stateDir, certgen.ClusterMachineApproverKubeconfigFilename),
				"--disable-status-controller",
				// Single instance, no lease to elect.
				"--leader-elect=false",
				// The kube-apiserver serves machine.openshift.io/v1 itself
				// (its control plane machine sets), which shadows the
				// machines definition's v1beta1 in discovery; the approver
				// resolves the preferred version and would find no Machine
				// kind there. The payload's own deployment pins the
				// version for exactly this reason.
				"--api-group-version=machine.openshift.io/v1beta1",
			},
			// The version operator delivers the role and binding behind
			// the approver's identity; the approver waits for them like
			// the other children wait for what the payload carries.
			BeforeStart: func(ctx context.Context) error {
				return gateOn(ctx, "the machine approver RBAC",
					gate.ClusterRole("system:openshift:controller:machine-approver"),
					gate.ClusterRoleBinding("system:openshift:controller:machine-approver"))
			},
		},
		// The network operator deploys the cluster's network stack: it
		// reads the network configuration and renders the real
		// OVN-Kubernetes workloads from the bindata beside its binary,
		// running for the control plane's lifetime. The flags and the
		// environment mirror its payload's deployment — the metrics
		// listener, and the release version and images rendered into its
		// workloads — with the kubeconfig following the component
		// pattern under the payload's own service-account identity,
		// whose cluster-admin the payload's binding grants. Its start
		// gates on the API definitions its watchers map, and the network
		// configuration it reconciles is seeded in ahead of it. The
		// workloads it creates select nodes this control plane runs none
		// of, so they sit unscheduled: deployed, honest about it, the
		// same truth the payload's other Deployments tell.
		{
			Name: "cluster-network-operator",
			Path: openShiftBinaries["cluster-network-operator"],
			// The operator reads its bindata relative to its working
			// directory, the way its image runs it from the image root.
			Dir: filepath.Dir(openShiftBinaries["cluster-network-operator"]),
			Args: []string{
				"start",
				"--listen=0.0.0.0:" + strconv.Itoa(networkOperatorMetricsPort),
				"--kubeconfig=" + filepath.Join(stateDir, certgen.ClusterNetworkOperatorKubeconfigFilename),
				// The deployment's pod reads its namespace from its
				// service account; a bare process names it the same way.
				"--namespace=openshift-network-operator",
			},
			Env: networkOperatorEnvironment,
			BeforeStart: func(ctx context.Context) error {
				if err := gateOn(ctx, "the network operator's APIs", networkOperatorWaits...); err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				fmt.Println("Seeding network configuration")
				return networkconfig.Seed(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename), apiServerURL(port), name)
			},
		},
		// The DNS operator deploys the cluster's DNS: it creates its own
		// configuration object on its first reconcile and renders the
		// real CoreDNS DaemonSet, the DNS Service, and the node-resolver
		// DaemonSet that maintains each node's hosts file. The
		// environment mirrors its payload's deployment — the release
		// version and the images rendered into its workloads, the cli
		// image included, which the operator refuses to start without —
		// with the kubeconfig following the component pattern under the
		// payload's own service-account identity, whose permissions the
		// payload's binding grants. The binary reads its kubeconfig the
		// way its client library does, from the environment, so the
		// kubeconfig rides the environment instead of a flag. Unlike the
		// operator Deployments, the DaemonSets it renders select every
		// linux node, so they would serve any node the cluster gains.
		{
			Name: "cluster-dns-operator",
			Path: openShiftBinaries["cluster-dns-operator"],
			Env:  dnsOperatorEnvironment,
			BeforeStart: func(ctx context.Context) error {
				return gateOn(ctx, "the DNS operator's APIs", dnsOperatorWaits...)
			},
			// The CoreDNS pods mount a metrics certificate no signer on
			// this control plane issues; the seed stands in for the
			// signer's output once the operator has created the operand
			// namespace. Bound the wait so a broken operator fails the
			// run instead of blocking startup forever.
			AfterStart: func(ctx context.Context) error {
				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				fmt.Println("Seeding the DNS metrics certificate")
				return dnstls.Seed(ctx, filepath.Join(stateDir, certgen.AdminKubeconfigFilename), stateDir)
			},
		},
	}

	sup := &supervisor.Supervisor{
		LogDir:  filepath.Join(stateDir, "logs"),
		Verbose: verbose,
		// The hook runs between the two groups: it confirms the API
		// path's readiness — the API server and the aggregated servers
		// answering, the controllers up — and prints the summary, and
		// the operators behind it start only once it has.
		AfterStart: func(ctx context.Context) error {
			fmt.Println("Waiting for the control plane to be ready")
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			kubeconfig := filepath.Join(stateDir, certgen.AdminKubeconfigFilename)
			if err := aggregation.WaitReady(ctx, kubeconfig, aggregatedServers); err != nil {
				return fmt.Errorf("control plane not ready: %w", err)
			}
			printReadySummary(stateDir, name, releaseRef, port)
			return nil
		},
	}
	return sup.Run(ctx, processes, operators...)
}

// kubeAPIServerArgs assembles the kube-apiserver command line from the
// generated material under the state directory and the etcd client URL
// the etcd process listens on.
func kubeAPIServerArgs(stateDir, etcdClientURL string, port int, openshiftConfigPath, tokenReviewKubeconfig string) []string {
	return []string{
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
		"--openshift-config=" + openshiftConfigPath,
		// Access tokens issued by the OAuth server are only
		// validated by the aggregated OAuth API server; this webhook
		// is how kube-apiserver asks it to review them. Real
		// clusters carry the same wiring from their operators.
		"--authentication-token-webhook-config-file=" + tokenReviewKubeconfig,
		"--authentication-token-webhook-version=v1",
		// Nothing in this cluster resolves a node's bare name, so
		// kubelet connections prefer the node's internal IP, the
		// address every node carries; the hostname-first default
		// leaves logs, exec, and port-forwarding failing on name
		// resolution. Real clusters keep hostname first because
		// their DNS resolves node names.
		"--kubelet-preferred-address-types=InternalIP,Hostname,InternalDNS,ExternalDNS,ExternalIP",
		// The client certificate kubelets authenticate the API
		// server's connections by; without one every
		// kubelet-proxied path — logs, exec, port-forwarding —
		// fails on authentication.
		"--kubelet-client-certificate=" + filepath.Join(stateDir, certgen.KubeletClientCertFilename),
		"--kubelet-client-key=" + filepath.Join(stateDir, certgen.KubeletClientKeyFilename),
	}
}

// networkOperatorImages lists the image variables the network
// operator's payload deployment sets, each with the release component
// its image comes from, so the workloads the operator renders carry
// the payload's real images. The table is verified against the 4.22
// payload's deployment; the misspelled route override variable is the
// deployment's own, kept verbatim.
var networkOperatorImages = []struct{ env, component string }{
	{"KUBE_PROXY_IMAGE", "kube-proxy"},
	{"KUBE_RBAC_PROXY_IMAGE", "kube-rbac-proxy"},
	{"MULTUS_IMAGE", "multus-cni"},
	{"MULTUS_ADMISSION_CONTROLLER_IMAGE", "multus-admission-controller"},
	{"CNI_PLUGINS_IMAGE", "container-networking-plugins"},
	{"BOND_CNI_PLUGIN_IMAGE", "network-interface-bond-cni"},
	{"WHEREABOUTS_CNI_IMAGE", "multus-whereabouts-ipam-cni"},
	{"ROUTE_OVERRRIDE_CNI_IMAGE", "multus-route-override-cni"},
	{"MULTUS_NETWORKPOLICY_IMAGE", "multus-networkpolicy"},
	{"OVN_IMAGE", "ovn-kubernetes"},
	{"EGRESS_ROUTER_CNI_IMAGE", "egress-router-cni"},
	{"NETWORK_METRICS_DAEMON_IMAGE", "network-metrics-daemon"},
	{"NETWORK_CHECK_SOURCE_IMAGE", "cluster-network-operator"},
	{"NETWORK_CHECK_TARGET_IMAGE", "cluster-network-operator"},
	{"NETWORK_OPERATOR_IMAGE", "cluster-network-operator"},
	{"CLOUD_NETWORK_CONFIG_CONTROLLER_IMAGE", "cloud-network-config-controller"},
	{"CLI_IMAGE", "cli"},
	{"FRR_K8S_IMAGE", "metallb-frr"},
	{"NETWORKING_CONSOLE_PLUGIN_IMAGE", "networking-console-plugin"},
}

// networkOperatorTunings are the OVN-Kubernetes timers the payload's
// deployment passes the operator as literals.
var networkOperatorTunings = []string{
	"OVN_NB_RAFT_ELECTION_TIMER=10",
	"OVN_SB_RAFT_ELECTION_TIMER=16",
	"OVN_NORTHD_PROBE_INTERVAL=10000",
	"OVN_CONTROLLER_INACTIVITY_PROBE=180000",
	"OVN_NB_INACTIVITY_PROBE=60000",
}

// networkOperatorEnv builds the environment the network operator's
// payload deployment gives it: the release's version, from the
// payload's own record so a digest-pinned release still reports what
// it is, the images resolved from the release, and the deployment's
// OVN timers.
func networkOperatorEnv(releaseRoot, payloadDir string) ([]string, error) {
	version, err := image.PayloadVersion(payloadDir)
	if err != nil {
		return nil, err
	}
	env := append(os.Environ(), "RELEASE_VERSION="+version)
	for _, entry := range networkOperatorImages {
		pullspec, err := image.ResolveComponentImage(releaseRoot, entry.component)
		if err != nil {
			return nil, fmt.Errorf("resolve the %s image: %w", entry.component, err)
		}
		env = append(env, entry.env+"="+pullspec)
	}
	return append(env, networkOperatorTunings...), nil
}

// dnsOperatorImages lists the image variables the DNS operator's
// payload deployment sets, each with the release component its image
// comes from, so the workloads the operator renders carry the payload's
// real images. The table is verified against the 4.22 payload's
// deployment.
var dnsOperatorImages = []struct{ env, component string }{
	{"IMAGE", "coredns"},
	{"OPENSHIFT_CLI_IMAGE", "cli"},
	{"KUBE_RBAC_PROXY_IMAGE", "kube-rbac-proxy"},
}

// dnsOperatorEnv builds the environment the DNS operator's payload
// deployment gives it: the release's version, from the payload's own
// record so a digest-pinned release still reports what it is, and the
// images resolved from the release. The operator takes its kubeconfig
// from the environment the way its client library does, so the
// component kubeconfig rides along.
func dnsOperatorEnv(releaseRoot, payloadDir, kubeconfigPath string) ([]string, error) {
	version, err := image.PayloadVersion(payloadDir)
	if err != nil {
		return nil, err
	}
	env := append(os.Environ(),
		"RELEASE_VERSION="+version,
		"KUBECONFIG="+kubeconfigPath,
	)
	for _, entry := range dnsOperatorImages {
		pullspec, err := image.ResolveComponentImage(releaseRoot, entry.component)
		if err != nil {
			return nil, fmt.Errorf("resolve the %s image: %w", entry.component, err)
		}
		env = append(env, entry.env+"="+pullspec)
	}
	return env, nil
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
	certgen.ClusterNetworkOperatorKubeconfigFilename,
	certgen.ClusterDNSOperatorKubeconfigFilename,
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

// printReadySummary prints what a client needs to know about the ready
// control plane: the cluster's name, the release it runs, the API
// endpoint, the admin kubeconfig, and the context it selects.
func printReadySummary(stateDir, name, release string, port int) {
	fmt.Println()
	fmt.Println("Control plane ready")
	fmt.Printf("  Name:         %s\n", name)
	fmt.Printf("  Release:      %s\n", release)
	fmt.Printf("  API endpoint: %s\n", apiServerURL(port))
	fmt.Printf("  Kubeconfig:   %s\n", filepath.Join(stateDir, certgen.AdminKubeconfigFilename))
	fmt.Printf("  Context:      admin@%s\n", name)
}

// podmanPlain builds a podman call against the store of whoever is
// running: root's under sudo, the invoking user's rootless store
// otherwise.
func podmanPlain(args ...string) *exec.Cmd {
	return exec.Command("podman", args...)
}

// newCreateCommand builds the parent of everything creatable. It takes a
// noun naming what to create: the control plane. With no noun, or an
// unknown one, it prints the list and fails.
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
	cmd.Flags().StringVar(&release.version, "version", "", "OpenShift version to run, X.Y, X.Y.Z, or newest for the registry's absolute newest; defaults to the newest verified release, or the release recorded in the state directory")
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
// refuses a reserved or otherwise taken port — the API's or the OAuth
// login endpoint's, which follows it — a taken container name, an
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
	// API server warnings follow the command's output discipline: quiet
	// unless verbose, and quoted in the failure report either way.
	warnings.SetVerbose(verbose)
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
	if err := refuseOAuthPort(port); err != nil {
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
	if err := ensureClusterNetwork(); err != nil {
		return err
	}
	args := podmanRunArgs(container, name, port, stateDir, cacheRoot, pullSecretPath, imageRef, release)
	run := exec.CommandContext(ctx, "podman", args...)
	run.Stdout = io.Discard // the detached run only prints the container ID
	run.Stderr = os.Stderr
	if err := run.Run(); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	return streamUntilReady(ctx, container, stateDir, name, port)
}

// clusterNetworkName is the podman network cluster containers run on:
// the project's own bridge network, created by the create command and
// removed by the cache cleanup, so the containers the tool manages are
// easy to tell apart from anything else in the store.
const clusterNetworkName = "coreshift"

// ensureClusterNetwork creates the network cluster containers run on
// when it does not exist yet.
func ensureClusterNetwork() error {
	if exec.Command("podman", "network", "exists", clusterNetworkName).Run() == nil {
		return nil
	}
	create := exec.Command("podman", "network", "create", clusterNetworkName)
	create.Stderr = os.Stderr
	if err := create.Run(); err != nil {
		return fmt.Errorf("create the %s network: %w", clusterNetworkName, err)
	}
	return nil
}

// removeClusterNetwork removes the network cluster containers run on,
// when nothing uses it anymore: like the minted images it is derived
// state, rebuilt the next time a cluster is created.
func removeClusterNetwork(ctx context.Context) {
	if exec.Command("podman", "network", "exists", clusterNetworkName).Run() != nil {
		return
	}
	if err := exec.CommandContext(ctx, "podman", "network", "rm", clusterNetworkName).Run(); err == nil {
		fmt.Println("Removed network", clusterNetworkName)
	}
}

// podmanRunArgs builds the podman command line that starts the cluster
// container: detached and named, the API port published as an identity
// mapping on loopback only (bare port publishing would bind every
// interface, and exposing the API to the network must stay an explicit
// act), the OAuth login endpoint published the same way on the port after
// it, so `oc login` from the host can follow the discovery document to
// the OAuth server, the cluster's state directory and the shared image
// cache bind-mounted, the pull secret mounted read-only, and the name, the
// release flags, and the port passed through to the run inside. The
// container runs on the project's own podman network. The image's
// entrypoint is `coreshift run cluster`, so the arguments after the image
// reference land on it.
func podmanRunArgs(container, name string, port int, stateDir, cacheRoot, pullSecretPath, imageRef string, release releaseOptions) []string {
	return []string{
		"run", "--detach",
		"--name", container,
		"--network", clusterNetworkName,
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", port+1, oauthServerPort),
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

// refuseOAuthPort refuses an API port whose following port, where a
// created cluster publishes the OAuth login endpoint, cannot be used: a
// reserved control-plane port, or one something already listens on.
func refuseOAuthPort(port int) error {
	oauthPort := port + 1
	err := refuseReservedPort(oauthPort)
	if err == nil {
		err = refuseIfPortBusy(oauthPort)
	}
	if err == nil {
		return nil
	}
	return fmt.Errorf("the OAuth login endpoint publishes on port %d, the port after the API port: %w", oauthPort, err)
}

// streamUntilReady follows the cluster container's boot output while
// waiting for the control plane's readiness from the host. The release
// in the summary is the one the run inside resolved, read from the
// marker it wrote into the shared state directory.
func streamUntilReady(ctx context.Context, container, stateDir, name string, port int) error {
	return streamUntil(ctx, container, "the control plane became ready",
		func(ctx context.Context) error { return waitContainerReady(ctx, stateDir, port) },
		func() {
			printReadySummary(stateDir, name, stateRelease(stateDir), port)
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
			running, err := containerRunning(podmanPlain, container)
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

// containerRunning reports whether the container is running, read
// through podman: the builder names the store that can see the
// container. A failed inspect, such as under a busy podman, is
// transient; the boot wait retries it, and the listings read it as not
// running.
func containerRunning(podman func(...string) *exec.Cmd, container string) (bool, error) {
	out, err := podman("inspect", "--format", "{{.State.Running}}", container).Output()
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

// defaultClusterName is the cluster every command addresses unless a
// flag names another one.
const defaultClusterName = "coreshift"

// dnsLabelPattern is what a cluster's name may look like: a DNS label,
// the strictest shape the cluster's objects accept. The name also names
// a directory, so it cannot carry separators.
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

// newDeleteCommand builds the parent of everything deletable. It takes a
// noun naming what to delete: a cluster. With no noun, or an unknown
// one, it prints the list and fails.
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
	return cmd
}

// newDeleteClusterCommand builds the command that deletes a cluster: its
// container is stopped, torn down in reverse order like a host-mode
// shutdown, and removed, and its state directory is removed too, leaving
// nothing behind. The preserve-state flag keeps the state directory.
func newDeleteClusterCommand() *cobra.Command {
	var name string
	var preserveState bool
	cmd := &cobra.Command{
		Use:          "cluster",
		Short:        "Delete a cluster and its state",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDelete(cmd.Context(), name, preserveState)
		},
	}
	cmd.Flags().StringVar(&name, "name", defaultClusterName, "name of the cluster to delete")
	cmd.Flags().BoolVar(&preserveState, "preserve-state", false, "keep the cluster's state directory, so a cluster re-created under the same name boots from it warm")
	return cmd
}

// containerStopSeconds is how long the delete command's stop waits for
// the container to exit after its stop signal. The signal triggers the
// same reverse-order teardown host mode runs; a healthy one takes tens of
// seconds, and the timeout only bounds a wedged container.
const containerStopSeconds = 120

// runDelete deletes a cluster: its container is stopped and removed,
// then its state directory is removed too — delete is cleanup, leaving
// nothing behind. The preserve-state flag keeps the state directory. A
// cluster whose container is already gone but whose state remains
// reports that, and the state follows the same rule.
func runDelete(ctx context.Context, name string, preserveState bool) error {
	if _, err := exec.LookPath("podman"); err != nil {
		return fmt.Errorf("podman is required to delete clusters: %w", err)
	}
	stateDir, err := clusterDir(name)
	if err != nil {
		return err
	}
	container := clusterContainerPrefix + name
	running := podmanPlain("container", "exists", container).Run() == nil
	if !running {
		if _, statErr := os.Stat(stateDir); statErr != nil {
			return unknownCluster(name)
		}
		fmt.Printf("Cluster %s is not running\n", name)
	}
	if running {
		fmt.Printf("Stopping container %s\n", container)
		stop := podmanPlain("stop", "--timeout", strconv.Itoa(containerStopSeconds), container)
		stop.Stderr = os.Stderr
		if err := stop.Run(); err != nil {
			return fmt.Errorf("stop container: %w", err)
		}
		if err := podmanPlain("rm", container).Run(); err != nil {
			return fmt.Errorf("remove container: %w", err)
		}
		fmt.Printf("Deleted cluster %s\n", name)
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

// runGetClusters lists the clusters. Readiness requires the container
// running and its API server answering a live probe: a stopped
// container is not ready without probing, because the port it
// published can since have been taken by another live cluster, and a
// running but booting or wedged one fails the probe.
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
	if _, err := fmt.Fprintln(w, "NAME\tPORT\tRELEASE\tREADY"); err != nil {
		return err
	}
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
		if running, err := containerRunning(podmanPlain, container); err == nil && running && clusterReady(port) {
			ready = "ready"
		}
		if _, err := fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", name, port, release, ready); err != nil {
			return err
		}
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

// clusterContainers returns the names of the cluster containers, sorted:
// every container named coreshift-* is one.
func clusterContainers() ([]string, error) {
	return containersNamed(podmanPlain, clusterContainerPrefix)
}

// containersNamed returns the names of the containers carrying the given
// prefix, sorted, read through podman: the builder names the store that
// can see them.
func containersNamed(podman func(...string) *exec.Cmd, prefix string) ([]string, error) {
	out, err := podman("ps", "-a", "--format", "{{.Names}}").Output()
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

// containerPort returns the port a cluster container publishes its API
// on. The create command publishes the API as an identity mapping, so
// among the container's mappings — the OAuth login endpoint's is the
// only other one — the API's is the one whose host side matches its
// container side, and the host port is the port the cluster runs on.
func containerPort(container string) (int, error) {
	out, err := podmanPlain("inspect", "--format",
		"{{range $port, $bindings := .NetworkSettings.Ports}}{{range $bindings}}{{.HostPort}} {{$port}}{{println}}{{end}}{{end}}",
		container).Output()
	if err != nil {
		return 0, fmt.Errorf("inspect container %s: %w", container, err)
	}
	for _, mapping := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(mapping)
		if len(fields) != 2 || fields[0] != strings.TrimSuffix(fields[1], "/tcp") {
			continue
		}
		port, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("read %s's published port: the API's identity mapping is missing from:\n%s", container, strings.TrimSpace(string(out)))
}

// bootRelease returns the release image reference this run boots, with a
// note to print under it. An explicitly requested release resolves as
// before, with a warning when it is beyond the newest verified line;
// with none requested and a state directory recording a release, the
// recorded one boots again — a warm re-create, not a surprise upgrade —
// and the registry is still checked for a newer patch of its stream to
// mention in the note.
//
// The returned reference is the run's one release value. The state
// marker records it, the children's binaries and the operator's payload
// are extracted from it, and the operator is told it, so the release
// the marker names, the processes run, and the manifests applied can
// never differ; a patch upgrade moves all three together.
func bootRelease(ctx context.Context, client *image.Client, release releaseOptions, stateDir string) (string, string, error) {
	recorded := stateRelease(stateDir)
	if release.image == "" && release.version == "" && recorded != "" {
		return recorded, newerReleaseNote(ctx, client, release, recorded), nil
	}
	ref, err := client.ResolveRelease(ctx, release.registry, release.version, release.arch, release.image)
	if err != nil {
		return "", "", err
	}
	return ref, image.BeyondLineWarning(ref), nil
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
// wedged control plane shows as not ready. A port alone does not
// identify its cluster — a stopped container's can be held by another
// live one — so callers confirm the container is running before trusting
// a yes. TLS is not verified: reaching a healthy API server is the
// question, not whose certificate it serves.
func clusterReady(port int) bool {
	client := &http.Client{
		Timeout:   readyProbeTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := client.Get(apiServerURL(port) + "/healthz")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// newCleanupCommand builds the command that removes CoreShift's on-disk
// artifacts: cluster state directories, and with --cache everything
// derived and rebuildable — the shared image cache and the minted
// container images.
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
			return runCleanup(cmd.Context(), name, all, clearCache)
		},
	}
	cmd.Flags().StringVar(&name, "name", defaultClusterName, "name of the cluster whose state directory is removed")
	cmd.Flags().BoolVar(&all, "all", false, "remove every cluster's state: the whole clusters tree")
	cmd.Flags().BoolVar(&clearCache, "cache", false, "also remove everything derived and rebuildable: the shared image cache and the minted container images, which the next run rebuilds")
	return cmd
}

// cleanupTarget is a directory cleanup removes, with the name for it in
// messages.
type cleanupTarget struct {
	path string
	kind string
}

// runCleanup removes the chosen cluster state and, with clearCache,
// everything derived and rebuildable: the minted container images and the
// shared image cache. The images go first, so an image a container still
// holds fails the command before any state is removed. It owns the
// clusters tree and nothing else under the state root, so unrelated
// directories kept there survive. It refuses while a control plane is
// running, because cleaning under a live instance would pull its data out
// from under it; a preserved-state cluster simply re-mints its image on
// re-create. A missing directory is reported, not an error, so the
// command is safe to re-run and to call from scripts.
func runCleanup(ctx context.Context, name string, all, clearCache bool) error {
	running, err := runningControlPlane()
	if err != nil {
		return err
	}
	if running != "" {
		return fmt.Errorf("a CoreShift control plane is running:\n  %s\n"+
			"Stop it first; cleaning up under a running control plane would pull its data out from under it", running)
	}
	var freed int64
	removed := false
	if clearCache {
		imageFreed, err := mint.Remove(ctx)
		if err != nil {
			return err
		}
		freed += imageFreed
		removed = removed || imageFreed > 0
		removeClusterNetwork(ctx)
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

// procRoot is where the running-control-plane scan reads process command
// lines from. It is a variable so tests can give the scan a world of their
// own, holding only the stand-ins they plant; the shipped command always
// reads the real one.
var procRoot = "/proc"

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
	entries, err := os.ReadDir(procRoot)
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
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
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
// as incomplete, so it is regenerated rather than reused; so is a set whose
// API server certificate lacks the in-cluster service address.
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
	if complete && serviceServingCertHasLoopback(stateDir) && apiServerCertHasServiceAddress(stateDir) {
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

// apiServerCertHasServiceAddress reports whether the saved API server
// certificate covers the in-cluster service address. Sets generated
// before the address was added must be regenerated, or in-cluster
// clients — pods following the in-cluster configuration, above all the
// CoreDNS the DNS operator deploys — fail TLS verification against the
// API they dial by that address.
func apiServerCertHasServiceAddress(stateDir string) bool {
	data, err := os.ReadFile(filepath.Join(stateDir, certgen.APIServerCertFilename))
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
	return cert.VerifyHostname("172.30.0.1") == nil
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
  bindAddress: %s:%d
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
`, apiBindAddress(), oauthServerPort,
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
// issuer is the main API endpoint; the authorization and token endpoints
// are the OAuth server's, on the port clients reach it. The remaining
// fields match what the installer writes in real clusters, so clients see
// the same document shape as in production.
func writeOAuthMetadata(stateDir string, port int) (string, error) {
	path := filepath.Join(stateDir, "oauth-metadata.json")
	oauthPort := oauthEndpointPort(port)
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
`, apiServerURL(port), oauthPort, oauthPort)
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
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

func main() {
	root := newRootCommand()
	// The error is printed here, not by the command library, so the
	// warnings the API server sent can be quoted in its tail.
	root.SilenceErrors = true
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", warnings.Attach(err))
		os.Exit(1)
	}
}
