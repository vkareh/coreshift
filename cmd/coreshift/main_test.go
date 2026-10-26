package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vkareh/coreshift/internal/certgen"
	"github.com/vkareh/coreshift/internal/oauth"
	"github.com/vkareh/coreshift/pkg/image"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestEnsureCertificatesReusesCompleteSet(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir, defaultAPIPort); err != nil {
		t.Fatal(err)
	}
	caBefore := readFile(t, stateDir, certgen.CACertFilename)

	if err := ensureCertificates(stateDir, defaultAPIPort); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(caBefore, readFile(t, stateDir, certgen.CACertFilename)) {
		t.Fatal("complete certificate set was regenerated")
	}
}

func TestEnsureCertificatesRegeneratesSetWithoutLoopbackSAN(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir, defaultAPIPort); err != nil {
		t.Fatal(err)
	}
	writeStaleServiceServingCert(t, stateDir)

	if err := ensureCertificates(stateDir, defaultAPIPort); err != nil {
		t.Fatal(err)
	}
	cert := parseCertificate(t, readFile(t, stateDir, certgen.ServiceServingCertFilename))
	if err := cert.VerifyHostname("127.0.0.1"); err != nil {
		t.Fatalf("regenerated certificate lacks loopback SAN: %v", err)
	}
}

// writeStaleServiceServingCert replaces the service-serving certificate with
// one covering only the service DNS names, as generated before the loopback
// address was added.
func writeStaleServiceServingCert(t *testing.T, stateDir string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "openshift-apiserver"},
		DNSNames:     []string{"openshift-apiserver", "openshift-oauth-apiserver"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(stateDir, certgen.ServiceServingCertFilename), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreAdminContextSwitchesBackFromLoginContext(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir, defaultAPIPort); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, certgen.AdminKubeconfigFilename)

	// Simulate an oc login: a token context is added and made current.
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loginContext := "dev/127-0-0-1:6443"
	config.AuthInfos[loginContext] = &clientcmdapi.AuthInfo{Token: "sha256~example"}
	config.Contexts[loginContext] = &clientcmdapi.Context{Cluster: "coreshift", AuthInfo: loginContext}
	config.CurrentContext = loginContext
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}

	if err := restoreAdminContext(stateDir); err != nil {
		t.Fatal(err)
	}

	config, err = clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.CurrentContext != "admin@coreshift" {
		t.Fatalf("current context = %q, want admin@coreshift", config.CurrentContext)
	}
	if config.Contexts[loginContext] == nil || config.AuthInfos[loginContext] == nil {
		t.Fatal("login context was removed")
	}
}

func TestRestoreAdminContextKeepsCorrectSelectionUntouched(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir, defaultAPIPort); err != nil {
		t.Fatal(err)
	}

	before := readFile(t, stateDir, certgen.AdminKubeconfigFilename)
	if err := restoreAdminContext(stateDir); err != nil {
		t.Fatal(err)
	}
	if after := readFile(t, stateDir, certgen.AdminKubeconfigFilename); !bytes.Equal(before, after) {
		t.Fatal("kubeconfig was rewritten although the selection was already correct")
	}
}

// TestGeneratedFilesFollowAPIPort checks that a non-default port reaches
// every file generated from it: the kubeconfigs, the OAuth discovery
// metadata, the token review kubeconfig, and the kube-apiserver config.
func TestGeneratedFilesFollowAPIPort(t *testing.T) {
	stateDir := t.TempDir()
	const port = 7443
	if err := ensureCertificates(stateDir, port); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		certgen.AdminKubeconfigFilename,
		certgen.ControllerManagerKubeconfigFilename,
		certgen.OAuthServerKubeconfigFilename,
	} {
		config, err := clientcmd.LoadFromFile(filepath.Join(stateDir, name))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if server := config.Clusters["coreshift"].Server; server != apiServerURL(port) {
			t.Fatalf("%s server = %q, want %q", name, server, apiServerURL(port))
		}
	}

	if _, err := writeOAuthMetadata(stateDir, port); err != nil {
		t.Fatal(err)
	}
	if metadata := string(readFile(t, stateDir, "oauth-metadata.json")); !strings.Contains(metadata, apiServerURL(port)) {
		t.Fatalf("oauth metadata issuer does not follow the port:\n%s", metadata)
	}

	if _, err := writeTokenReviewKubeconfig(stateDir, port); err != nil {
		t.Fatal(err)
	}
	if kubeconfig := string(readFile(t, stateDir, "token-review.kubeconfig")); !strings.Contains(kubeconfig, apiServerURL(port)) {
		t.Fatalf("token review kubeconfig does not follow the port:\n%s", kubeconfig)
	}

	if _, err := writeKubeAPIServerConfig(stateDir, filepath.Join(stateDir, "oauth-metadata.json"), port); err != nil {
		t.Fatal(err)
	}
	if config := string(readFile(t, stateDir, "kube-apiserver-config.json")); !strings.Contains(config, fmt.Sprintf("127.0.0.1:%d", port)) {
		t.Fatalf("kube-apiserver config does not follow the port:\n%s", config)
	}
}

// TestKubeAPIServerConfigBindsAllInterfacesInContainer checks that the
// API server's serving address follows where it runs: every interface
// inside a container, so a published port reaches it, and loopback only
// on the host.
func TestKubeAPIServerConfigBindsAllInterfacesInContainer(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv(containerEnv, "1")

	if _, err := writeKubeAPIServerConfig(stateDir, filepath.Join(stateDir, "oauth-metadata.json"), defaultAPIPort); err != nil {
		t.Fatal(err)
	}
	if config := string(readFile(t, stateDir, "kube-apiserver-config.json")); !strings.Contains(config, "0.0.0.0:"+strconv.Itoa(defaultAPIPort)) {
		t.Fatalf("kube-apiserver config does not bind all interfaces:\n%s", config)
	}
}

// TestKubeAPIServerConfigBindsLoopbackOnHost checks that host mode, with
// the container variable unset, keeps binding loopback only.
func TestKubeAPIServerConfigBindsLoopbackOnHost(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv(containerEnv, "")

	if _, err := writeKubeAPIServerConfig(stateDir, filepath.Join(stateDir, "oauth-metadata.json"), defaultAPIPort); err != nil {
		t.Fatal(err)
	}
	if config := string(readFile(t, stateDir, "kube-apiserver-config.json")); !strings.Contains(config, "127.0.0.1:"+strconv.Itoa(defaultAPIPort)) {
		t.Fatalf("kube-apiserver config does not bind loopback:\n%s", config)
	}
}

// TestAPIStateDirPerPort checks that the default port keeps the original
// state directory while any other port gets its own subdirectory, so runs
// on different ports stay independent.
func TestAPIStateDirPerPort(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)

	def, err := apiStateDir(defaultAPIPort)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "coreshift", "api"); def != want {
		t.Fatalf("default port state dir = %q, want %q", def, want)
	}

	other, err := apiStateDir(7443)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "coreshift", "api", "7443"); other != want {
		t.Fatalf("port 7443 state dir = %q, want %q", other, want)
	}
}

// TestStateRootEmptyVariableStaysBelowHome pins the default location:
// an empty XDG state variable counts as unset by the base-directory
// convention, so the state root stays where existing installs keep it,
// below the home directory.
func TestStateRootEmptyVariableStaysBelowHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")

	got, err := stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "state", "coreshift"); got != want {
		t.Fatalf("state root = %q, want %q", got, want)
	}
}

// writeStateTree fills a state directory with a marker file, the way a
// run does, so cleanup has something to measure and remove.
func writeStateTree(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, releaseMarkerFilename), []byte("reg.example/ocp-release:4.22.8-x86_64"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupRemovesDefaultStateDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir := filepath.Join(base, "coreshift", "api")
	writeStateTree(t, stateDir)

	if err := runCleanup(defaultAPIPort, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("default state directory still exists")
	}
}

func TestCleanupRemovesPortStateDirOnly(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := apiStateDir(7443)
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir)
	writeStateTree(t, filepath.Dir(stateDir))

	if err := runCleanup(7443, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("port state directory still exists")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(stateDir), releaseMarkerFilename)); err != nil {
		t.Fatalf("default state directory was removed too: %v", err)
	}
}

func TestCleanupAllSweepsEveryStateDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	apiRoot := filepath.Join(base, "coreshift", "api")
	writeStateTree(t, apiRoot)
	writeStateTree(t, filepath.Join(apiRoot, "7443"))
	writeStateTree(t, filepath.Join(apiRoot, "8443"))

	if err := runCleanup(defaultAPIPort, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(apiRoot); !os.IsNotExist(err) {
		t.Fatal("state directories still exist after --all")
	}
	if _, err := os.Stat(filepath.Join(base, "coreshift")); !os.IsNotExist(err) {
		t.Fatal("empty state root was left behind")
	}
}

func TestCleanupMissingStateDirIsNotAnError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := runCleanup(defaultAPIPort, false, false); err != nil {
		t.Fatalf("missing state directory must not fail: %v", err)
	}
}

func TestCleanupCache(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cacheBase := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheBase)
	cacheDir := filepath.Join(cacheBase, "coreshift")
	writeStateTree(t, cacheDir)

	if err := runCleanup(defaultAPIPort, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatal("image cache still exists")
	}
}

func TestCleanupRefusesAllWithPort(t *testing.T) {
	cmd := newCleanupCommand()
	cmd.SetArgs([]string{"--all", "--port", "7443"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--port") {
		t.Fatalf("err = %v, want --all and --port together refused", err)
	}
}

// startStandbyProcess starts a process that stays alive until the test
// ends, carrying the given extra command line argument. The two-command
// script keeps the shell itself alive with its original command line, so
// /proc shows the argument for the whole lifetime.
func startStandbyProcess(t *testing.T, extraArg string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 30; true", extraArg)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

func TestCleanupRefusesWhileChildProcessRuns(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir := filepath.Join(base, "coreshift", "api")
	writeStateTree(t, stateDir)

	// A stand-in for a control plane child: its command line carries the
	// state directory the way etcd's --data-dir does.
	startStandbyProcess(t, "--data-dir="+filepath.Join(stateDir, "etcd-data"))

	err := runCleanup(defaultAPIPort, false, false)
	if err == nil || !strings.Contains(err.Error(), "Stop it first") {
		t.Fatalf("err = %v, want a refusal naming the running control plane", err)
	}
	if _, statErr := os.Stat(stateDir); statErr != nil {
		t.Fatalf("refusal removed the state directory anyway: %v", statErr)
	}
}

// TestIsControlPlaneArgs checks the command lines cleanup treats as a
// running control plane: the supervisor by name, and anything carrying a
// state or cache path, as every control plane child does.
func TestIsControlPlaneArgs(t *testing.T) {
	state, cache := "/state", "/cache"
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"./coreshift", "run", "cluster", "--pull-secret", "secret"}, true},
		{[]string{"/usr/local/bin/coreshift", "run", "cluster"}, true},
		{[]string{"./coreshift", "run"}, false},
		{[]string{"./coreshift", "cleanup"}, false},
		{[]string{"./coreshift"}, false},
		{[]string{"etcd", "--data-dir=" + state + "/api/etcd-data"}, true},
		{[]string{"kube-scheduler", "--config=" + state + "/api/kube-scheduler-config.yaml"}, true},
		{[]string{"oc", "login", "--kubeconfig=" + state + "/api/admin.kubeconfig"}, true},
		{[]string{"etcd", "--data-dir=/elsewhere/etcd-data"}, false},
		{[]string{"sh", "-c", "sleep 30; true", "--config=" + cache + "/components/x/y"}, true},
		{[]string{"sh", "-c", "sleep 30; true", "--namespace=default"}, false},
	}
	for _, tc := range cases {
		if got := isControlPlaneArgs(tc.args, state, cache); got != tc.want {
			t.Errorf("isControlPlaneArgs(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestRootWithoutArgumentsPrintsHelp checks that bare coreshift prints
// the command list instead of running anything.
func TestRootWithoutArgumentsPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"run", "create", "delete", "get", "cleanup", "version"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q:\n%s", want, out.String())
		}
	}
}

// TestRunWithoutNounPrintsHelp checks that bare run prints the list of
// runnable things and succeeds.
func TestRunWithoutNounPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"run"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cluster") {
		t.Fatalf("run help missing the cluster noun:\n%s", out.String())
	}
}

// TestRunUnknownNounPrintsHelp checks that an unknown noun fails after
// printing the list of runnable things.
func TestRunUnknownNounPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"run", "bogus"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v, want the unknown noun refused", err)
	}
	if !strings.Contains(out.String(), "cluster") {
		t.Fatalf("run help missing the cluster noun:\n%s", out.String())
	}
}

// TestCreateWithoutNounPrintsHelp checks that bare create prints the list
// of creatable things and succeeds.
func TestCreateWithoutNounPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"create"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cluster") {
		t.Fatalf("create help missing the cluster noun:\n%s", out.String())
	}
}

// TestCreateUnknownNounPrintsHelp checks that an unknown noun fails after
// printing the list of creatable things.
func TestCreateUnknownNounPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"create", "bogus"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v, want the unknown noun refused", err)
	}
	if !strings.Contains(out.String(), "cluster") {
		t.Fatalf("create help missing the cluster noun:\n%s", out.String())
	}
}

// TestCreateCommandFlagDefaults checks that the creation flags are
// accepted and come with the documented defaults.
func TestCreateCommandFlagDefaults(t *testing.T) {
	hostArch, err := releaseArch(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	cmd := newCreateClusterCommand()
	for name, want := range map[string]string{
		"name":          "coreshift",
		"image":         "",
		"port":          "6443",
		"fresh":         "false",
		"verbose":       "false",
		"registry":      "quay.io/openshift-release-dev/ocp-release",
		"arch":          hostArch,
		"release-image": "",
		"version":       "",
	} {
		if flag := cmd.Flags().Lookup(name); flag == nil {
			t.Fatalf("flag --%s is not defined", name)
		} else if flag.DefValue != want {
			t.Fatalf("--%s default = %q, want %q", name, flag.DefValue, want)
		}
	}
}

// TestPodmanRunArgs checks the podman command line that starts a cluster
// container: the loopback identity mapping for the API port, the XDG
// environment pointing at the bind-mounted state and cache roots, the
// read-only pull secret, and the release flags and port passed through to
// the run inside.
func TestPodmanRunArgs(t *testing.T) {
	release := releaseOptions{
		image:    "reg.example/ocp-release:4.22.8-x86_64",
		registry: "quay.io/openshift-release-dev/ocp-release",
		arch:     "x86_64",
	}
	got := podmanRunArgs("coreshift-dev", 7443, "/state/root", "/cache/root", "/secret/pull-secret", "localhost/coreshift:v1", release)
	want := []string{
		"run", "--detach",
		"--name", "coreshift-dev",
		"--publish", "127.0.0.1:7443:7443",
		"--env", "XDG_STATE_HOME=/var/lib/coreshift/state",
		"--env", "XDG_CACHE_HOME=/var/lib/coreshift/cache",
		"--volume", "/state/root:/var/lib/coreshift/state/coreshift:z",
		"--volume", "/cache/root:/var/lib/coreshift/cache/coreshift:z",
		"--volume", "/secret/pull-secret:/etc/coreshift/pull-secret:ro,z",
		"localhost/coreshift:v1",
		"--pull-secret=/etc/coreshift/pull-secret",
		"--port=7443",
		"--release-image=reg.example/ocp-release:4.22.8-x86_64",
		"--version=",
		"--registry=quay.io/openshift-release-dev/ocp-release",
		"--arch=x86_64",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("podman args = %q\nwant %q", got, want)
	}
}

// TestRefuseIfPortBusy checks that a port something already listens on is
// refused, naming the port.
func TestRefuseIfPortBusy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	err = refuseIfPortBusy(port)
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Fatalf("err = %v, want a refusal naming port %d", err, port)
	}
}

// TestStateConflictHint checks that a container log showing the state
// directory refusing reuse gains the create-side remedies, and that an
// ordinary failure gains none.
func TestStateConflictHint(t *testing.T) {
	refusal := "Error: state directory /state/api was created by release 4.22.8 and cannot be reused by 4.19.3"
	hint := stateConflictHint(refusal)
	for _, want := range []string{"--fresh", "--port"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint = %q, want it to name %s", hint, want)
		}
	}
	if hint := stateConflictHint("Error: etcd exited: boom"); hint != "" {
		t.Fatalf("hint = %q, want none for an ordinary failure", hint)
	}
}

func TestTailText(t *testing.T) {
	if got := tailText("a\nb\nc\n", 2); got != "b\nc" {
		t.Fatalf("tailText = %q, want the last two lines", got)
	}
	if got := tailText("a\n", 5); got != "a" {
		t.Fatalf("tailText = %q, want all of a short text", got)
	}
}

// TestVersionCommandPrintsVersion checks that the version subcommand
// prints the version the binary was built with.
func TestVersionCommandPrintsVersion(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != version {
		t.Fatalf("version output = %q, want %q", got, version)
	}
}

// TestClusterCommandResolutionFlagDefaults checks that the release
// resolution flags are accepted and come with the documented defaults.
func TestClusterCommandResolutionFlagDefaults(t *testing.T) {
	hostArch, err := releaseArch(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	cmd := newClusterCommand()
	for name, want := range map[string]string{
		"version":       "",
		"release-image": "",
		"registry":      "quay.io/openshift-release-dev/ocp-release",
		"arch":          hostArch,
	} {
		if flag := cmd.Flags().Lookup(name); flag == nil {
			t.Fatalf("flag --%s is not defined", name)
		} else if flag.DefValue != want {
			t.Fatalf("--%s default = %q, want %q", name, flag.DefValue, want)
		}
	}
}

func TestReleaseArchMapsGoNames(t *testing.T) {
	for _, tc := range []struct{ goArch, want string }{
		{"amd64", "x86_64"},
		{"arm64", "aarch64"},
		{"ppc64le", "ppc64le"},
		{"s390x", "s390x"},
	} {
		arch, err := releaseArch(tc.goArch)
		if err != nil {
			t.Fatalf("%s: %v", tc.goArch, err)
		}
		if arch != tc.want {
			t.Fatalf("releaseArch(%q) = %q, want %q", tc.goArch, arch, tc.want)
		}
	}
}

func TestReleaseArchRejectsUnknownArchitecture(t *testing.T) {
	_, err := releaseArch("plan9")
	if err == nil || !strings.Contains(err.Error(), "plan9") {
		t.Fatalf("err = %v, want unsupported architecture error", err)
	}
}

func TestWriteOAuthServerConfigWithoutProviders(t *testing.T) {
	path, err := writeOAuthServerConfig(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	config := string(readFile(t, filepath.Dir(path), filepath.Base(path)))
	if !strings.Contains(config, "identityProviders: []") {
		t.Fatalf("config without providers is not deny-all only:\n%s", config)
	}
}

func TestWriteOAuthServerConfigRendersProviders(t *testing.T) {
	stateDir := t.TempDir()
	file := filepath.Join(stateDir, "htpasswd-0")
	providers := []oauth.Provider{{Name: "developers", MappingMethod: "claim", File: file}}

	path, err := writeOAuthServerConfig(stateDir, providers)
	if err != nil {
		t.Fatal(err)
	}
	config := string(readFile(t, filepath.Dir(path), filepath.Base(path)))
	want := `  identityProviders:
  - name: developers
    challenge: true
    login: true
    mappingMethod: claim
    provider:
      apiVersion: osin.config.openshift.io/v1
      kind: HTPasswdPasswordIdentityProvider
      file: ` + file + "\n"
	if !strings.Contains(config, want) {
		t.Fatalf("config missing provider stanza, want:\n%s\ngot:\n%s", want, config)
	}
}

func TestEnsureReleaseMarkerTracksFreshDirectory(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureReleaseMarker(stateDir, "reg.example/ocp-release:4.22.8-x86_64"); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, stateDir, releaseMarkerFilename)); got != "reg.example/ocp-release:4.22.8-x86_64" {
		t.Fatalf("marker = %q, want the release image reference", got)
	}
	// The same release reuses the directory unchanged.
	if err := ensureReleaseMarker(stateDir, "reg.example/ocp-release:4.22.8-x86_64"); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureReleaseMarkerRefusesDifferentRelease(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureReleaseMarker(stateDir, "reg.example/ocp-release:4.22.8-x86_64"); err != nil {
		t.Fatal(err)
	}

	err := ensureReleaseMarker(stateDir, "reg.example/ocp-release:4.19.3-x86_64")
	if err == nil {
		t.Fatal("expected a refusal for a different release")
	}
	for _, want := range []string{"4.22.8", "4.19.3", stateDir, "--port"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q:\n%v", want, err)
		}
	}
	if got := string(readFile(t, stateDir, releaseMarkerFilename)); got != "reg.example/ocp-release:4.22.8-x86_64" {
		t.Fatalf("refusal overwrote the marker, now %q", got)
	}
}

// TestEnsureReleaseMarkerUpgradesPatchInPlace checks the relaxed rule's
// accepting side: a newer patch of the same minor stream reuses the
// directory, recording the upgrade, the same move a real cluster makes.
func TestEnsureReleaseMarkerUpgradesPatchInPlace(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureReleaseMarker(stateDir, "reg.example/ocp-release:4.22.8-x86_64"); err != nil {
		t.Fatal(err)
	}

	if err := ensureReleaseMarker(stateDir, "reg.example/ocp-release:4.22.9-x86_64"); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, stateDir, releaseMarkerFilename)); got != "reg.example/ocp-release:4.22.9-x86_64" {
		t.Fatalf("marker = %q, want the upgrading release recorded", got)
	}
}

// TestEnsureReleaseMarkerRefusesPatchDowngrade checks that a same-stream
// older patch still refuses, stating the rule.
func TestEnsureReleaseMarkerRefusesPatchDowngrade(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureReleaseMarker(stateDir, "reg.example/ocp-release:4.22.9-x86_64"); err != nil {
		t.Fatal(err)
	}

	err := ensureReleaseMarker(stateDir, "reg.example/ocp-release:4.22.8-x86_64")
	if err == nil {
		t.Fatal("expected a refusal for a patch downgrade")
	}
	for _, want := range []string{"4.22.9", "4.22.8", "newer patch", stateDir} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q:\n%v", want, err)
		}
	}
	if got := string(readFile(t, stateDir, releaseMarkerFilename)); got != "reg.example/ocp-release:4.22.9-x86_64" {
		t.Fatalf("refusal overwrote the marker, now %q", got)
	}
}

// TestEnsureReleaseMarkerExactMatchWithoutVersion checks the fallback for
// references carrying no parseable version: only an exact match reuses
// the directory, as before, rather than guessing at an upgrade.
func TestEnsureReleaseMarkerExactMatchWithoutVersion(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureReleaseMarker(stateDir, "reg.example/ocp-release:nightly"); err != nil {
		t.Fatal(err)
	}
	if err := ensureReleaseMarker(stateDir, "reg.example/ocp-release:nightly"); err != nil {
		t.Fatal(err)
	}

	err := ensureReleaseMarker(stateDir, "reg.example/ocp-release:4.22.9-x86_64")
	if err == nil {
		t.Fatal("expected a refusal without a parseable version to compare")
	}
	if got := string(readFile(t, stateDir, releaseMarkerFilename)); got != "reg.example/ocp-release:nightly" {
		t.Fatalf("refusal overwrote the marker, now %q", got)
	}
}

// writeMarker records a release image reference in a state directory, the
// way a boot does.
func writeMarker(t *testing.T, stateDir, releaseRef string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stateDir, releaseMarkerFilename), []byte(releaseRef), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newTagRegistry serves a minimal v2 registry listing the given tags, so
// release resolution can be exercised without a real one. It returns a
// repository reference pointing at the fake registry.
func newTagRegistry(t *testing.T, tags ...string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/repo/tags/list", func(w http.ResponseWriter, _ *http.Request) {
		page, err := json.Marshal(map[string][]string{"tags": tags})
		if err != nil {
			t.Errorf("marshal tags: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, _ *http.Request) {})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	// 127.0.0.1 would be reached over https; localhost keeps it http.
	return "localhost:" + parsed.Port() + "/repo"
}

// TestBootReleaseReusesRecordedRelease checks the warm re-create: with no
// release requested and a state directory recording one, the recorded
// reference boots again instead of the registry's newest, and the
// registry is still checked for a newer patch of its stream to mention.
func TestBootReleaseReusesRecordedRelease(t *testing.T) {
	stateDir := t.TempDir()
	writeMarker(t, stateDir, "reg.example/ocp-release:4.22.8-x86_64")
	repo := newTagRegistry(t, "4.22.8-x86_64", "4.22.11-x86_64")
	release := releaseOptions{registry: repo, arch: "x86_64"}

	ref, note, err := bootRelease(context.Background(), &image.Client{}, release, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "reg.example/ocp-release:4.22.8-x86_64" {
		t.Fatalf("ref = %q, want the recorded release", ref)
	}
	for _, want := range []string{"4.22.11", "--version 4.22.11"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note = %q, want it to name the newer release and the flag that upgrades:\n%s", note, note)
		}
	}
}

// TestBootReleaseNoteOnlyForNewerPatch checks that the note stays quiet
// when the recorded release is the stream's newest, and when only
// another minor stream is newer, which cannot upgrade in place.
func TestBootReleaseNoteOnlyForNewerPatch(t *testing.T) {
	for name, tags := range map[string][]string{
		"recorded is newest":   {"4.22.8-x86_64", "4.22.11-x86_64"},
		"only another stream":  {"4.22.11-x86_64", "4.23.0-x86_64"},
		"registry unreachable": nil,
	} {
		stateDir := t.TempDir()
		writeMarker(t, stateDir, "reg.example/ocp-release:4.22.11-x86_64")
		var repo string
		if tags != nil {
			repo = newTagRegistry(t, tags...)
		} else {
			repo = "localhost:1/repo" // nothing listens there
		}
		release := releaseOptions{registry: repo, arch: "x86_64"}

		_, note, err := bootRelease(context.Background(), &image.Client{}, release, stateDir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if note != "" {
			t.Fatalf("%s: note = %q, want none", name, note)
		}
	}
}

// TestBootReleaseRequestedReleaseWins checks that an explicitly requested
// version still resolves from the registry, even with a recorded release.
func TestBootReleaseRequestedReleaseWins(t *testing.T) {
	stateDir := t.TempDir()
	writeMarker(t, stateDir, "reg.example/ocp-release:4.22.8-x86_64")
	repo := newTagRegistry(t, "4.22.8-x86_64", "4.22.11-x86_64")
	release := releaseOptions{registry: repo, version: "4.22.11", arch: "x86_64"}

	ref, note, err := bootRelease(context.Background(), &image.Client{}, release, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := repo + ":4.22.11-x86_64"; ref != want {
		t.Fatalf("ref = %q, want %q", ref, want)
	}
	if note != "" {
		t.Fatalf("note = %q, want none for an explicitly requested release", note)
	}
}

func TestEnsureReleaseMarkerRefusesUntrackedEtcdData(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(stateDir, "etcd-data"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := ensureReleaseMarker(stateDir, "reg.example/ocp-release:4.22.8-x86_64")
	if err == nil {
		t.Fatal("expected a refusal for untracked etcd data")
	}
	for _, want := range []string{"before releases were recorded", stateDir, "--port"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q:\n%v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, releaseMarkerFilename)); !os.IsNotExist(err) {
		t.Fatalf("refusal wrote a marker anyway: %v", err)
	}
}

// swapStdin points standard input at f for the duration of the test, so
// offer helpers can be exercised without a real terminal.
func swapStdin(t *testing.T, f *os.File) {
	t.Helper()
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = old
	})
}

func TestParseYesNo(t *testing.T) {
	for _, tc := range []struct {
		answer string
		want   bool
	}{
		{"y\n", true},
		{"Y", true},
		{"yes\n", true},
		{" Yes \t\n", true},
		{"", false},
		{"\n", false},
		{"n\n", false},
		{"no\n", false},
		{"ye\n", false},
		{"yes please\n", false},
	} {
		if got := parseYesNo(tc.answer); got != tc.want {
			t.Errorf("parseYesNo(%q) = %v, want %v", tc.answer, got, tc.want)
		}
	}
}

func TestStartFreshRemovesStateDirOnYes(t *testing.T) {
	stateDir := t.TempDir()
	writeStateTree(t, stateDir)

	if err := startFresh("y\n", stateDir, fmt.Errorf("refusal")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("state directory still exists after an explicit yes")
	}
}

func TestStartFreshKeepsStateDirOtherwise(t *testing.T) {
	for _, answer := range []string{"\n", "n\n", "no\n", "yes please\n"} {
		stateDir := t.TempDir()
		writeStateTree(t, stateDir)
		refusal := fmt.Errorf("refusal")

		if err := startFresh(answer, stateDir, refusal); err != refusal {
			t.Fatalf("%q: err = %v, want the refusal unchanged", answer, err)
		}
		if _, err := os.Stat(stateDir); err != nil {
			t.Fatalf("%q: state directory was removed: %v", answer, err)
		}
	}
}

// TestOfferFreshStartWithoutTerminal checks the path scripts take: a
// pipe on standard input means no question, and the refusal names the
// cleanup command. Prompting here would hang the test on the empty
// pipe, so passing is also proof the question is never asked.
func TestOfferFreshStartWithoutTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.Close()
		w.Close()
	})
	swapStdin(t, r)

	refusal := fmt.Errorf("state directory cannot be reused")
	err = offerFreshStart("/state/api", 7443, refusal)
	if err == nil {
		t.Fatal("expected a refusal without a terminal")
	}
	for _, want := range []string{"state directory cannot be reused", "coreshift cleanup --port 7443"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q:\n%v", want, err)
		}
	}
}

// TestOfferFreshStartWithoutTerminalOnNullDevice checks that the null
// device counts as no terminal although it is a character device:
// detached containers wire it to standard input, and a refusing run
// inside one must take the scripted path instead of printing a question
// nobody can answer into its log.
func TestOfferFreshStartWithoutTerminalOnNullDevice(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { devNull.Close() })
	swapStdin(t, devNull)

	refusal := fmt.Errorf("state directory cannot be reused")
	err = offerFreshStart("/state/api", 7443, refusal)
	if err == nil {
		t.Fatal("expected a refusal without a terminal")
	}
	for _, want := range []string{"state directory cannot be reused", "coreshift cleanup --port 7443"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q:\n%v", want, err)
		}
	}
}

// noTerminal points standard input at a pipe, so fresh-start offers take
// the scripted path instead of asking a question.
func noTerminal(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	swapStdin(t, r)
	t.Cleanup(func() { w.Close() })
}

// writePullSecret writes a minimal pull secret and returns its path.
func writePullSecret(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pull-secret")
	if err := os.WriteFile(path, []byte(`{"auths":{"quay.io":{"auth":"dXNlcjpwYXNz"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCheckCreateStateAcceptsCompatibleReleases checks the pre-flight's
// accepting side: a same-stream newer patch resolves and upgrades in
// place with no question, an exact image pin compares by the same rule,
// and an empty state directory or an unrequested release is no conflict.
func TestCheckCreateStateAcceptsCompatibleReleases(t *testing.T) {
	repo := newTagRegistry(t, "4.22.8-x86_64", "4.22.9-x86_64")
	secret := writePullSecret(t)
	for name, tc := range map[string]struct {
		marker  string
		release releaseOptions
	}{
		"newer patch of same stream": {
			marker:  "reg.example/ocp-release:4.22.8-x86_64",
			release: releaseOptions{registry: repo, version: "4.22.9", arch: "x86_64"},
		},
		"pinned newer patch of same stream": {
			marker:  "reg.example/ocp-release:4.22.8-x86_64",
			release: releaseOptions{image: "reg.example/ocp-release:4.22.9-x86_64"},
		},
		"pinned recorded release": {
			marker:  "reg.example/ocp-release:nightly",
			release: releaseOptions{image: "reg.example/ocp-release:nightly"},
		},
		"no release requested": {
			marker:  "reg.example/ocp-release:4.22.8-x86_64",
			release: releaseOptions{registry: repo, arch: "x86_64"},
		},
	} {
		stateDir := t.TempDir()
		writeMarker(t, stateDir, tc.marker)
		if err := checkCreateState(context.Background(), stateDir, tc.release, secret); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	fresh := t.TempDir()
	if err := checkCreateState(context.Background(), fresh, releaseOptions{registry: repo, version: "4.22.9", arch: "x86_64"}, secret); err != nil {
		t.Fatalf("empty state directory: %v", err)
	}
}

// TestCheckCreateStateRefusesConflicts checks the pre-flight's refusals,
// which must arrive before any image is minted: a downgrade and a
// cross-stream move against a recorded release, and a pinned image the
// recorded one cannot run.
func TestCheckCreateStateRefusesConflicts(t *testing.T) {
	repo := newTagRegistry(t, "4.19.3-x86_64", "4.22.8-x86_64", "4.22.9-x86_64")
	secret := writePullSecret(t)
	for name, tc := range map[string]struct {
		marker  string
		release releaseOptions
	}{
		"patch downgrade": {
			marker:  "reg.example/ocp-release:4.22.9-x86_64",
			release: releaseOptions{registry: repo, version: "4.22.8", arch: "x86_64"},
		},
		"cross-stream move": {
			marker:  "reg.example/ocp-release:4.22.9-x86_64",
			release: releaseOptions{registry: repo, version: "4.19.3", arch: "x86_64"},
		},
		"pinned image from another stream": {
			marker:  "reg.example/ocp-release:4.22.9-x86_64",
			release: releaseOptions{image: "reg.example/ocp-release:4.19.3-x86_64"},
		},
		"pinned image without a version": {
			marker:  "reg.example/ocp-release:nightly",
			release: releaseOptions{image: "reg.example/ocp-release:4.22.9-x86_64"},
		},
	} {
		noTerminal(t)
		stateDir := t.TempDir()
		writeMarker(t, stateDir, tc.marker)

		err := checkCreateState(context.Background(), stateDir, tc.release, secret)
		if err == nil {
			t.Fatalf("%s: expected a refusal", name)
		}
		for _, want := range []string{"cannot be reused", "newer patch", "--fresh"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: refusal missing %q:\n%v", name, want, err)
			}
		}
		if _, statErr := os.Stat(filepath.Join(stateDir, releaseMarkerFilename)); statErr != nil {
			t.Fatalf("%s: refusal removed the state directory: %v", name, statErr)
		}
	}
}

// TestCheckCreateStateRefusesUntrackedLeftovers checks the plain naming
// of state data that records no release: a deleted cluster's leftovers.
func TestCheckCreateStateRefusesUntrackedLeftovers(t *testing.T) {
	noTerminal(t)
	stateDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(stateDir, "etcd-data"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := checkCreateState(context.Background(), stateDir, releaseOptions{}, writePullSecret(t))
	if err == nil {
		t.Fatal("expected a refusal for untracked state data")
	}
	for _, want := range []string{"left over from a deleted cluster", "--fresh"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q:\n%v", want, err)
		}
	}
}

func readFile(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parseCertificate(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block := pemDecode(t, certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func pemDecode(t *testing.T, data []byte) *pem.Block {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("data is not PEM encoded")
	}
	return block
}

// writeFakePodman writes a podman stand-in into a directory of its own and
// prepends it to PATH. It records every call, one line per call, in the
// file named by PODMAN_LOG; the containers named in PODMAN_CONTAINERS,
// one per line, are the ones that exist and the ones ps lists, and
// inspect answers with the per-container port file from PODMAN_PORTS.
func writeFakePodman(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
echo "$@" >> "$PODMAN_LOG"
if [ "$1" = "container" ]; then
	grep -qx "$3" "$PODMAN_CONTAINERS"
	exit $?
fi
if [ "$1" = "ps" ]; then
	cat "$PODMAN_CONTAINERS"
	exit 0
fi
if [ "$1" = "inspect" ]; then
	eval "container=\${$#}"
	cat "$PODMAN_PORTS/$container"
	exit 0
fi
exit 0
`
	path := filepath.Join(dir, "podman")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("PODMAN_LOG", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("PODMAN_CONTAINERS", filepath.Join(t.TempDir(), "containers"))
	t.Setenv("PODMAN_PORTS", t.TempDir())
}

// setContainers makes the fake podman report exactly these containers as
// existing.
func setContainers(t *testing.T, names ...string) {
	t.Helper()
	if err := os.WriteFile(os.Getenv("PODMAN_CONTAINERS"), []byte(strings.Join(names, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setContainerPort makes the fake podman answer inspect for container
// with port.
func setContainerPort(t *testing.T, container string, port int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("PODMAN_PORTS"), container), []byte(strconv.Itoa(port)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// podmanCalls returns the calls the fake podman recorded.
func podmanCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("PODMAN_LOG"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// captureStdout returns everything printed to standard output while run
// runs.
func captureStdout(t *testing.T, run func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() {
		os.Stdout = old
		r.Close()
	}()
	printed := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		printed <- string(data)
	}()
	run()
	w.Close()
	return <-printed
}

// TestGetWithoutNounPrintsHelp checks that bare get prints the list of
// gettable things and succeeds.
func TestGetWithoutNounPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"get"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"clusters", "kubeconfig"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("get help missing %q:\n%s", want, out.String())
		}
	}
}

// TestGetUnknownNounPrintsHelp checks that an unknown noun fails after
// printing the list of gettable things.
func TestGetUnknownNounPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"get", "bogus"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v, want the unknown noun refused", err)
	}
	if !strings.Contains(out.String(), "clusters") {
		t.Fatalf("get help missing the clusters noun:\n%s", out.String())
	}
}

// TestRunGetClustersEmpty checks that an empty cluster list is reported
// plainly.
func TestRunGetClustersEmpty(t *testing.T) {
	writeFakePodman(t)
	var err error
	out := captureStdout(t, func() { err = runGetClusters() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No clusters") {
		t.Fatalf("output = %q, want the empty list reported", out)
	}
}

// TestRunGetClustersListsClusters checks the row each cluster gets: its
// name, the port its container publishes, the release its state directory
// records, and readiness from a live probe, so a cluster with nothing
// answering on its port shows as not ready and one without a state
// directory yet shows an unknown release.
func TestRunGetClustersListsClusters(t *testing.T) {
	writeFakePodman(t)
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	readyPort, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	setContainers(t, "coreshift-booting", "coreshift-early", "coreshift-ready")
	setContainerPort(t, "coreshift-booting", 17443)
	setContainerPort(t, "coreshift-early", 18443)
	setContainerPort(t, "coreshift-ready", readyPort)
	for _, port := range []int{17443, readyPort} {
		stateDir, err := apiStateDir(port)
		if err != nil {
			t.Fatal(err)
		}
		writeStateTree(t, stateDir)
	}

	out := captureStdout(t, func() { err = runGetClusters() })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("output has %d lines, want a header and three clusters:\n%s", len(lines), out)
	}
	rowFor := func(name string) []string {
		for _, line := range lines {
			if fields := strings.Fields(line); len(fields) > 0 && fields[0] == name {
				return fields
			}
		}
		t.Fatalf("no row for %s in:\n%s", name, out)
		return nil
	}
	// The release reference carries no spaces, so the fields from the
	// third on are the readiness column.
	readiness := func(row []string) string { return strings.Join(row[3:], " ") }
	if got := rowFor("booting"); len(got) < 4 || got[1] != "17443" || got[2] != "reg.example/ocp-release:4.22.8-x86_64" || readiness(got) != "not ready" {
		t.Fatalf("booting row = %q, want its port, release, and not ready", got)
	}
	if got := rowFor("early"); len(got) < 4 || got[1] != "18443" || got[2] != "-" || readiness(got) != "not ready" {
		t.Fatalf("early row = %q, want its port, an unknown release, and not ready", got)
	}
	if got := rowFor("ready"); len(got) != 4 || got[1] != strconv.Itoa(readyPort) || got[3] != "ready" {
		t.Fatalf("ready row = %q, want its port and ready", got)
	}
}

func TestRunGetKubeconfigPrintsPath(t *testing.T) {
	writeFakePodman(t)
	setContainers(t, "coreshift-dev")
	setContainerPort(t, "coreshift-dev", 7443)
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)

	var err error
	out := captureStdout(t, func() { err = runGetKubeconfig("dev") })
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "coreshift", "api", "7443", certgen.AdminKubeconfigFilename)
	if got := strings.TrimSpace(out); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// TestRunGetKubeconfigMissingCluster checks that a missing cluster is
// refused plainly.
func TestRunGetKubeconfigMissingCluster(t *testing.T) {
	writeFakePodman(t)
	err := runGetKubeconfig("nope")
	if err == nil || !strings.Contains(err.Error(), "no cluster named nope") {
		t.Fatalf("err = %v, want a plain refusal", err)
	}
}

// TestGetCommandsRequirePodman checks that the get commands fail naming
// podman when it is missing.
func TestGetCommandsRequirePodman(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for name, run := range map[string]func() error{
		"get clusters":   func() error { return runGetClusters() },
		"get kubeconfig": func() error { return runGetKubeconfig("dev") },
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "podman") {
			t.Fatalf("%s: err = %v, want an error naming podman", name, err)
		}
	}
}

// TestDeleteRequiresPodman checks that the delete command fails naming
// podman when it is missing.
func TestDeleteRequiresPodman(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := runDelete("dev")
	if err == nil || !strings.Contains(err.Error(), "podman") {
		t.Fatalf("err = %v, want an error naming podman", err)
	}
}

// TestGetKubeconfigFlagDefaults checks that the kubeconfig getter comes
// with the documented default.
func TestGetKubeconfigFlagDefaults(t *testing.T) {
	cmd := newGetKubeconfigCommand()
	if flag := cmd.Flags().Lookup("name"); flag == nil {
		t.Fatal("flag --name is not defined")
	} else if flag.DefValue != "coreshift" {
		t.Fatalf("--name default = %q, want coreshift", flag.DefValue)
	}
}

// TestDeleteWithoutNounPrintsHelp checks that bare delete prints the list
// of deletable things and succeeds.
func TestDeleteWithoutNounPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"delete"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cluster") {
		t.Fatalf("delete help missing the cluster noun:\n%s", out.String())
	}
}

// TestDeleteUnknownNounPrintsHelp checks that an unknown noun fails after
// printing the list of deletable things.
func TestDeleteUnknownNounPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"delete", "bogus"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v, want the unknown noun refused", err)
	}
	if !strings.Contains(out.String(), "cluster") {
		t.Fatalf("delete help missing the cluster noun:\n%s", out.String())
	}
}

// TestRunDeleteStopsAndRemovesKeepingState checks the delete contract:
// the container is stopped with a timeout generous enough for the control
// plane's teardown and then removed, while the state directory and its
// release marker are left intact.
func TestRunDeleteStopsAndRemovesKeepingState(t *testing.T) {
	writeFakePodman(t)
	setContainers(t, "coreshift-dev")
	setContainerPort(t, "coreshift-dev", 7443)
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := apiStateDir(7443)
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir)

	var delErr error
	out := captureStdout(t, func() { delErr = runDelete("dev") })
	if delErr != nil {
		t.Fatal(delErr)
	}
	calls := podmanCalls(t)
	want := []string{
		"container exists coreshift-dev",
		"inspect --format {{range $port, $bindings := .NetworkSettings.Ports}}{{range $bindings}}{{.HostPort}}{{end}}{{end}} coreshift-dev",
		"stop --timeout 120 coreshift-dev",
		"rm coreshift-dev",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("podman calls = %q\nwant %q", calls, want)
	}
	if !strings.Contains(out, "State kept at "+stateDir) {
		t.Fatalf("output = %q, want the kept state named", out)
	}
	if _, err := os.Stat(filepath.Join(stateDir, releaseMarkerFilename)); err != nil {
		t.Fatal("delete removed the state directory")
	}
}

// TestRunDeleteMissingCluster checks that deleting a missing cluster is
// refused plainly, with nothing stopped or removed.
func TestRunDeleteMissingCluster(t *testing.T) {
	writeFakePodman(t)
	err := runDelete("nope")
	if err == nil || !strings.Contains(err.Error(), "no cluster named nope") {
		t.Fatalf("err = %v, want a plain refusal", err)
	}
	if calls := podmanCalls(t); len(calls) != 1 || calls[0] != "container exists coreshift-nope" {
		t.Fatalf("podman calls = %q, want only the existence check", calls)
	}
}

// TestDeleteClusterFlagDefaults checks that the delete command comes with
// the documented default.
func TestDeleteClusterFlagDefaults(t *testing.T) {
	cmd := newDeleteClusterCommand()
	if flag := cmd.Flags().Lookup("name"); flag == nil {
		t.Fatal("flag --name is not defined")
	} else if flag.DefValue != "coreshift" {
		t.Fatalf("--name default = %q, want coreshift", flag.DefValue)
	}
}
