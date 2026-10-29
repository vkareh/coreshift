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

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vkareh/coreshift/internal/certgen"
	"github.com/vkareh/coreshift/internal/oauth"
	"github.com/vkareh/coreshift/pkg/image"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestEnsureCertificatesReusesCompleteSet(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir, defaultClusterName, defaultAPIPort); err != nil {
		t.Fatal(err)
	}
	caBefore := readFile(t, stateDir, certgen.CACertFilename)

	if err := ensureCertificates(stateDir, defaultClusterName, defaultAPIPort); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(caBefore, readFile(t, stateDir, certgen.CACertFilename)) {
		t.Fatal("complete certificate set was regenerated")
	}
}

func TestEnsureCertificatesRegeneratesSetWithoutLoopbackSAN(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir, defaultClusterName, defaultAPIPort); err != nil {
		t.Fatal(err)
	}
	writeStaleServiceServingCert(t, stateDir)

	if err := ensureCertificates(stateDir, defaultClusterName, defaultAPIPort); err != nil {
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

// TestFollowAPIPort checks the port mobility contract: a warm boot on a
// different port rewrites every kubeconfig's server URL to the port in
// use while leaving the certificates, which encode no port, untouched.
func TestFollowAPIPort(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir, defaultClusterName, 6443); err != nil {
		t.Fatal(err)
	}
	caBefore := readFile(t, stateDir, certgen.CACertFilename)

	if err := followAPIPort(stateDir, 7443); err != nil {
		t.Fatal(err)
	}
	for _, name := range kubeconfigFilenames {
		config, err := clientcmd.LoadFromFile(filepath.Join(stateDir, name))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if server := config.Clusters["coreshift"].Server; server != apiServerURL(7443) {
			t.Fatalf("%s server = %q, want %q", name, server, apiServerURL(7443))
		}
	}
	if err := ensureCertificates(stateDir, defaultClusterName, 7443); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(caBefore, readFile(t, stateDir, certgen.CACertFilename)) {
		t.Fatal("the port change regenerated the certificates")
	}
}

// TestFollowAPIPortLeavesMatchingKubeconfigs checks that a warm boot on
// the same port rewrites nothing.
func TestFollowAPIPortLeavesMatchingKubeconfigs(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir, defaultClusterName, 6443); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, stateDir, certgen.AdminKubeconfigFilename)

	if err := followAPIPort(stateDir, 6443); err != nil {
		t.Fatal(err)
	}
	if after := readFile(t, stateDir, certgen.AdminKubeconfigFilename); !bytes.Equal(before, after) {
		t.Fatal("kubeconfig was rewritten although the port already matched")
	}
}

// rewindNames puts a kubeconfig's entries back on the generic names the
// generator wrote before they were named after the cluster: the user
// entry plain, everything else named coreshift. It is the shape a state
// directory from before carries.
func rewindNames(t *testing.T, path string) {
	t.Helper()
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for contextName, context := range config.Contexts {
		user, ok := strings.CutSuffix(contextName, "@"+defaultClusterName)
		if !ok {
			continue
		}
		if entry, ok := config.AuthInfos[contextName]; ok {
			config.AuthInfos[user] = entry
			delete(config.AuthInfos, contextName)
		}
		context.AuthInfo = user
	}
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}
}

// TestFollowClusterName checks the naming contract: a warm boot of a
// state directory whose kubeconfigs carry the generic names rewrites
// every entry to the cluster's name, the references between them
// included, while leaving the certificates, which encode no name,
// untouched.
func TestFollowClusterName(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir, defaultClusterName, 6443); err != nil {
		t.Fatal(err)
	}
	for _, name := range kubeconfigFilenames {
		rewindNames(t, filepath.Join(stateDir, name))
	}
	caBefore := readFile(t, stateDir, certgen.CACertFilename)

	// A login context an oc login left behind: it keeps its name, and
	// only its cluster reference follows the rename.
	loginContext := "dev/127-0-0-1:6443"
	adminPath := filepath.Join(stateDir, certgen.AdminKubeconfigFilename)
	config, err := clientcmd.LoadFromFile(adminPath)
	if err != nil {
		t.Fatal(err)
	}
	config.AuthInfos[loginContext] = &clientcmdapi.AuthInfo{Token: "sha256~example"}
	config.Contexts[loginContext] = &clientcmdapi.Context{Cluster: defaultClusterName, AuthInfo: loginContext}
	config.CurrentContext = loginContext
	if err := clientcmd.WriteToFile(*config, adminPath); err != nil {
		t.Fatal(err)
	}

	if err := followClusterName(stateDir, "alpha"); err != nil {
		t.Fatal(err)
	}
	for _, name := range kubeconfigFilenames {
		config, err := clientcmd.LoadFromFile(filepath.Join(stateDir, name))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if config.Clusters["alpha"] == nil || config.Clusters[defaultClusterName] != nil {
			t.Fatalf("%s: cluster entry not named for the cluster", name)
		}
		if err := clientcmd.ConfirmUsable(*config, ""); err != nil {
			t.Fatalf("%s is not usable after the rename: %v", name, err)
		}
	}

	config, err = clientcmd.LoadFromFile(adminPath)
	if err != nil {
		t.Fatal(err)
	}
	context := config.Contexts["admin@alpha"]
	if context == nil || context.Cluster != "alpha" || context.AuthInfo != "admin@alpha" {
		t.Fatalf("admin context = %#v, want it named for the cluster and pointing at the named user", context)
	}
	if config.AuthInfos["admin@alpha"] == nil || config.AuthInfos["admin"] != nil {
		t.Fatal("admin user entry not named for the cluster")
	}
	if login := config.Contexts[loginContext]; login == nil || login.Cluster != "alpha" || login.AuthInfo != loginContext {
		t.Fatalf("login context = %#v, want it kept with its cluster reference followed", login)
	}
	if config.AuthInfos[loginContext] == nil {
		t.Fatal("login user entry was removed")
	}
	if err := restoreAdminContext(stateDir, "alpha"); err != nil {
		t.Fatal(err)
	}
	config, err = clientcmd.LoadFromFile(adminPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.CurrentContext != "admin@alpha" {
		t.Fatalf("current context = %q, want the named admin context", config.CurrentContext)
	}
	if !bytes.Equal(caBefore, readFile(t, stateDir, certgen.CACertFilename)) {
		t.Fatal("the name change regenerated the certificates")
	}
}

// TestFollowClusterNameLeavesNamedKubeconfigs checks that a warm boot of
// a state directory whose kubeconfigs already carry the cluster's name
// rewrites nothing.
func TestFollowClusterNameLeavesNamedKubeconfigs(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir, "alpha", 6443); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, stateDir, certgen.AdminKubeconfigFilename)

	if err := followClusterName(stateDir, "alpha"); err != nil {
		t.Fatal(err)
	}
	if after := readFile(t, stateDir, certgen.AdminKubeconfigFilename); !bytes.Equal(before, after) {
		t.Fatal("kubeconfig was rewritten although the entries already carried the cluster's name")
	}
}

// TestRefuseInvalidClusterName checks that a cluster name naming a
// directory under the state root must be a DNS label, and that the
// default name passes.
func TestRefuseInvalidClusterName(t *testing.T) {
	for _, name := range []string{"../escape", "a/b", "Bad Name", ""} {
		if err := refuseInvalidClusterName(name); err == nil {
			t.Fatalf("name %q: expected a refusal", name)
		}
	}
	if err := refuseInvalidClusterName(defaultClusterName); err != nil {
		t.Fatalf("default name refused: %v", err)
	}
}

func TestRestoreAdminContextSwitchesBackFromLoginContext(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir, "dev", defaultAPIPort); err != nil {
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
	config.Contexts[loginContext] = &clientcmdapi.Context{Cluster: "dev", AuthInfo: loginContext}
	config.CurrentContext = loginContext
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}

	if err := restoreAdminContext(stateDir, "dev"); err != nil {
		t.Fatal(err)
	}

	config, err = clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.CurrentContext != "admin@dev" {
		t.Fatalf("current context = %q, want admin@dev", config.CurrentContext)
	}
	if config.Contexts[loginContext] == nil || config.AuthInfos[loginContext] == nil {
		t.Fatal("login context was removed")
	}
}

func TestRestoreAdminContextKeepsCorrectSelectionUntouched(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir, "dev", defaultAPIPort); err != nil {
		t.Fatal(err)
	}

	before := readFile(t, stateDir, certgen.AdminKubeconfigFilename)
	if err := restoreAdminContext(stateDir, "dev"); err != nil {
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
	if err := ensureCertificates(stateDir, "dev", port); err != nil {
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
		if server := config.Clusters["dev"].Server; server != apiServerURL(port) {
			t.Fatalf("%s server = %q, want %q", name, server, apiServerURL(port))
		}
	}

	if _, err := writeOAuthMetadata(stateDir, port); err != nil {
		t.Fatal(err)
	}
	if metadata := string(readFile(t, stateDir, "oauth-metadata.json")); !strings.Contains(metadata, apiServerURL(port)) {
		t.Fatalf("oauth metadata issuer does not follow the port:\n%s", metadata)
	}

	if _, err := writeTokenReviewKubeconfig(stateDir, "dev", port); err != nil {
		t.Fatal(err)
	}
	kubeconfig := string(readFile(t, stateDir, "token-review.kubeconfig"))
	if !strings.Contains(kubeconfig, apiServerURL(port)) {
		t.Fatalf("token review kubeconfig does not follow the port:\n%s", kubeconfig)
	}
	if !strings.Contains(kubeconfig, "admin@dev") {
		t.Fatalf("token review kubeconfig does not carry the cluster's name:\n%s", kubeconfig)
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

// TestClusterDirByName checks that a cluster's state directory comes from
// its name, so every command resolves the same directory for it whatever
// port its API server binds.
func TestClusterDirByName(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)

	dir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "coreshift", "clusters", "dev"); dir != want {
		t.Fatalf("cluster state dir = %q, want %q", dir, want)
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

// writeStateTree fills a cluster's state directory with a marker file,
// the way a boot does, so cleanup has something to measure and remove.
func writeStateTree(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, dir, name, "reg.example/ocp-release:4.22.8-x86_64")
}

func TestCleanupRemovesDefaultStateDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := clusterDir(defaultClusterName)
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, defaultClusterName)

	if err := runCleanup(defaultClusterName, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("cluster state directory still exists")
	}
}

func TestCleanupRemovesNamedClusterOnly(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	removed, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := clusterDir("other")
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, removed, "dev")
	writeStateTree(t, kept, "other")

	if err := runCleanup("dev", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(removed); !os.IsNotExist(err) {
		t.Fatal("named cluster's state directory still exists")
	}
	if _, err := os.Stat(filepath.Join(kept, clusterMarkerFilename)); err != nil {
		t.Fatalf("another cluster's state directory was removed too: %v", err)
	}
}

func TestCleanupAllSweepsClustersTree(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	tree, err := clustersTree()
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, filepath.Join(tree, "alpha"), "alpha")
	writeStateTree(t, filepath.Join(tree, "beta"), "beta")
	// An unrelated directory kept under the state root by choice: a full
	// cleanup owns the clusters tree and nothing else.
	sibling := filepath.Join(base, "coreshift", "keepme")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := runCleanup(defaultClusterName, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatal("clusters tree still exists after --all")
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("unrelated directory under the state root was removed: %v", err)
	}
}

func TestCleanupMissingStateDirIsNotAnError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := runCleanup(defaultClusterName, false, false); err != nil {
		t.Fatalf("missing state directory must not fail: %v", err)
	}
}

func TestCleanupCache(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cacheBase := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheBase)
	cacheDir := filepath.Join(cacheBase, "coreshift")
	writeStateTree(t, cacheDir, defaultClusterName)

	if err := runCleanup(defaultClusterName, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatal("image cache still exists")
	}
}

func TestCleanupRefusesAllWithName(t *testing.T) {
	cmd := newCleanupCommand()
	cmd.SetArgs([]string{"--all", "--name", "dev"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("err = %v, want --all and --name together refused", err)
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
	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, "dev")

	// A stand-in for a control plane child: its command line carries the
	// state directory the way etcd's --data-dir does.
	startStandbyProcess(t, "--data-dir="+filepath.Join(stateDir, "etcd-data"))

	err = runCleanup("dev", false, false)
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
		{[]string{"etcd", "--data-dir=" + state + "/clusters/dev/etcd-data"}, true},
		{[]string{"kube-scheduler", "--config=" + state + "/clusters/dev/kube-scheduler-config.yaml"}, true},
		{[]string{"oc", "login", "--kubeconfig=" + state + "/clusters/dev/admin.kubeconfig"}, true},
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
	for _, want := range []string{"cluster", "worker"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("run help missing the %s noun:\n%s", want, out.String())
		}
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
// environment pointing at the bind-mounted state and cache paths, the
// read-only pull secret, and the name, release flags, and port passed
// through to the run inside.
func TestPodmanRunArgs(t *testing.T) {
	release := releaseOptions{
		image:    "reg.example/ocp-release:4.22.8-x86_64",
		registry: "quay.io/openshift-release-dev/ocp-release",
		arch:     "x86_64",
	}
	got := podmanRunArgs("coreshift-dev", "dev", 7443, "/state/root/clusters/dev", "/cache/root", "/secret/pull-secret", "localhost/coreshift:v1", release)
	want := []string{
		"run", "--detach",
		"--name", "coreshift-dev",
		"--publish", "127.0.0.1:7443:7443",
		"--env", "XDG_STATE_HOME=/var/lib/coreshift/state",
		"--env", "XDG_CACHE_HOME=/var/lib/coreshift/cache",
		"--volume", "/state/root/clusters/dev:/var/lib/coreshift/state/coreshift/clusters/dev:z",
		"--volume", "/cache/root:/var/lib/coreshift/cache/coreshift:z",
		"--volume", "/secret/pull-secret:/etc/coreshift/pull-secret:ro,z",
		"localhost/coreshift:v1",
		"--pull-secret=/etc/coreshift/pull-secret",
		"--name=dev",
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

// TestRefuseReservedPort checks that every port a control-plane child
// listens on is refused with the full reserved set named, and that any
// other port, the default one included, passes.
func TestRefuseReservedPort(t *testing.T) {
	for _, port := range reservedPorts {
		err := refuseReservedPort(port)
		if err == nil {
			t.Fatalf("port %d: expected a refusal", port)
		}
		for _, reserved := range reservedPorts {
			if !strings.Contains(err.Error(), strconv.Itoa(reserved)) {
				t.Fatalf("port %d: refusal does not name reserved port %d:\n%v", port, reserved, err)
			}
		}
	}
	for _, port := range []int{defaultAPIPort, 8447} {
		if err := refuseReservedPort(port); err != nil {
			t.Fatalf("port %d: unexpected refusal: %v", port, err)
		}
	}
}

// TestRunClusterRefusesReservedPort checks that run cluster refuses a
// reserved port before anything else: a pull secret path that does not
// exist still yields the port refusal, not a read error.
func TestRunClusterRefusesReservedPort(t *testing.T) {
	root := newRootCommand()
	root.SetArgs([]string{"run", "cluster", "--pull-secret", "/nonexistent", "--port", strconv.Itoa(oauthServerPort)})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("err = %v, want a reserved-port refusal", err)
	}
}

// TestCreateClusterRefusesReservedPort checks that create cluster refuses
// a reserved port before anything else: with no podman on PATH and no
// pull secret to read, the port refusal is still the one that arrives.
func TestCreateClusterRefusesReservedPort(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := newRootCommand()
	root.SetArgs([]string{"create", "cluster", "--pull-secret", "/nonexistent", "--port", strconv.Itoa(etcdClientPort)})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("err = %v, want a reserved-port refusal", err)
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
	refusal := "Error: state directory /state/clusters/dev was created by release 4.22.8 and cannot be reused by 4.19.3"
	hint := stateConflictHint(refusal)
	for _, want := range []string{"--fresh", "--name"} {
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
		"name":          "coreshift",
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

// TestWriteOpenShiftAPIServerConfigContents checks that the config serves
// on the advertise address with the shared service-serving certificate
// and stores to the local etcd, so the constants cannot drift from what
// the config carries.
func TestWriteOpenShiftAPIServerConfigContents(t *testing.T) {
	stateDir := t.TempDir()
	path, err := writeOpenShiftAPIServerConfig(stateDir, "10.1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	config := string(readFile(t, stateDir, filepath.Base(path)))
	for _, want := range []string{
		fmt.Sprintf("10.1.2.3:%d", openShiftAPIServerPort),
		filepath.Join(stateDir, certgen.ServiceServingCertFilename),
		"- " + etcdClientURL,
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("config missing %q:\n%s", want, config)
		}
	}
}

func TestEnsureClusterMarkerTracksFreshDirectory(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureClusterMarker(stateDir, "dev", "reg.example/ocp-release:4.22.8-x86_64"); err != nil {
		t.Fatal(err)
	}
	marker, err := readClusterMarker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if marker != (clusterMarker{Name: "dev", Release: "reg.example/ocp-release:4.22.8-x86_64"}) {
		t.Fatalf("marker = %+v, want the cluster's name and the release image reference", marker)
	}
	// The same name and release reuse the directory unchanged.
	if err := ensureClusterMarker(stateDir, "dev", "reg.example/ocp-release:4.22.8-x86_64"); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureClusterMarkerRefusesDifferentRelease(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureClusterMarker(stateDir, "dev", "reg.example/ocp-release:4.22.8-x86_64"); err != nil {
		t.Fatal(err)
	}

	err := ensureClusterMarker(stateDir, "dev", "reg.example/ocp-release:4.19.3-x86_64")
	if err == nil {
		t.Fatal("expected a refusal for a different release")
	}
	for _, want := range []string{"4.22.8", "4.19.3", stateDir, "--name"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q:\n%v", want, err)
		}
	}
	marker, err := readClusterMarker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if marker.Release != "reg.example/ocp-release:4.22.8-x86_64" {
		t.Fatalf("refusal overwrote the marker, now %+v", marker)
	}
}

// TestEnsureClusterMarkerUpgradesPatchInPlace checks the relaxed rule's
// accepting side: a newer patch of the same minor stream reuses the
// directory, recording the upgrade, the same move a real cluster makes.
func TestEnsureClusterMarkerUpgradesPatchInPlace(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureClusterMarker(stateDir, "dev", "reg.example/ocp-release:4.22.8-x86_64"); err != nil {
		t.Fatal(err)
	}

	if err := ensureClusterMarker(stateDir, "dev", "reg.example/ocp-release:4.22.9-x86_64"); err != nil {
		t.Fatal(err)
	}
	marker, err := readClusterMarker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if marker != (clusterMarker{Name: "dev", Release: "reg.example/ocp-release:4.22.9-x86_64"}) {
		t.Fatalf("marker = %+v, want the upgrading release recorded", marker)
	}
}

// TestEnsureClusterMarkerRefusesPatchDowngrade checks that a same-stream
// older patch still refuses, stating the rule.
func TestEnsureClusterMarkerRefusesPatchDowngrade(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureClusterMarker(stateDir, "dev", "reg.example/ocp-release:4.22.9-x86_64"); err != nil {
		t.Fatal(err)
	}

	err := ensureClusterMarker(stateDir, "dev", "reg.example/ocp-release:4.22.8-x86_64")
	if err == nil {
		t.Fatal("expected a refusal for a patch downgrade")
	}
	for _, want := range []string{"4.22.9", "4.22.8", "newer patch", stateDir} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q:\n%v", want, err)
		}
	}
	marker, err := readClusterMarker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if marker.Release != "reg.example/ocp-release:4.22.9-x86_64" {
		t.Fatalf("refusal overwrote the marker, now %+v", marker)
	}
}

// TestEnsureClusterMarkerExactMatchWithoutVersion checks the fallback for
// references carrying no parseable version: only an exact match reuses
// the directory, as before, rather than guessing at an upgrade.
func TestEnsureClusterMarkerExactMatchWithoutVersion(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureClusterMarker(stateDir, "dev", "reg.example/ocp-release:nightly"); err != nil {
		t.Fatal(err)
	}
	if err := ensureClusterMarker(stateDir, "dev", "reg.example/ocp-release:nightly"); err != nil {
		t.Fatal(err)
	}

	err := ensureClusterMarker(stateDir, "dev", "reg.example/ocp-release:4.22.9-x86_64")
	if err == nil {
		t.Fatal("expected a refusal without a parseable version to compare")
	}
	marker, err := readClusterMarker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if marker.Release != "reg.example/ocp-release:nightly" {
		t.Fatalf("refusal overwrote the marker, now %+v", marker)
	}
}

// writeMarker records a cluster's name and release image reference in a
// state directory, the way a boot does.
func writeMarker(t *testing.T, stateDir, name, releaseRef string) {
	t.Helper()
	if err := writeClusterMarker(stateDir, clusterMarker{Name: name, Release: releaseRef}); err != nil {
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
	writeMarker(t, stateDir, "dev", "reg.example/ocp-release:4.22.8-x86_64")
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
		writeMarker(t, stateDir, "dev", "reg.example/ocp-release:4.22.11-x86_64")
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
	writeMarker(t, stateDir, "dev", "reg.example/ocp-release:4.22.8-x86_64")
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
	writeStateTree(t, stateDir, "dev")

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
		writeStateTree(t, stateDir, "dev")
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
	err = offerFreshStart("/state/clusters/dev", "dev", refusal)
	if err == nil {
		t.Fatal("expected a refusal without a terminal")
	}
	for _, want := range []string{"state directory cannot be reused", "coreshift cleanup --name dev"} {
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
	err = offerFreshStart("/state/clusters/dev", "dev", refusal)
	if err == nil {
		t.Fatal("expected a refusal without a terminal")
	}
	for _, want := range []string{"state directory cannot be reused", "coreshift cleanup --name dev"} {
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
		writeMarker(t, stateDir, "dev", tc.marker)
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
		writeMarker(t, stateDir, "dev", tc.marker)

		err := checkCreateState(context.Background(), stateDir, tc.release, secret)
		if err == nil {
			t.Fatalf("%s: expected a refusal", name)
		}
		for _, want := range []string{"cannot be reused", "newer patch", "--fresh"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: refusal missing %q:\n%v", name, want, err)
			}
		}
		if _, statErr := os.Stat(filepath.Join(stateDir, clusterMarkerFilename)); statErr != nil {
			t.Fatalf("%s: refusal removed the state directory: %v", name, statErr)
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
// one per line, are the ones that exist and the ones ps lists; inspect
// answers with the per-container port file from PODMAN_PORTS, and with
// the per-container label file from PODMAN_LABELS when asked for labels;
// logs answers with the per-container file from PODMAN_LOGS.
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
	case "$3" in
	*Labels*) cat "$PODMAN_LABELS/$container" 2>/dev/null ;;
	*) cat "$PODMAN_PORTS/$container" ;;
	esac
	exit 0
fi
if [ "$1" = "logs" ]; then
	cat "$PODMAN_LOGS/$2" 2>/dev/null
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
	t.Setenv("PODMAN_LABELS", t.TempDir())
	t.Setenv("PODMAN_LOGS", t.TempDir())
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

// setContainerLabel makes the fake podman answer label inspection for
// container with label.
func setContainerLabel(t *testing.T, container, label string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("PODMAN_LABELS"), container), []byte(label), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setContainerLog makes the fake podman answer logs for container with
// the given output.
func setContainerLog(t *testing.T, container, log string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("PODMAN_LOGS"), container), []byte(log), 0o644); err != nil {
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
// name, the port its container publishes, the release its state
// directory records, and readiness from a live probe, so a cluster with
// nothing answering on its port shows as not ready and one without a
// state directory yet shows an unknown release.
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
	for _, name := range []string{"booting", "ready"} {
		stateDir, err := clusterDir(name)
		if err != nil {
			t.Fatal(err)
		}
		writeStateTree(t, stateDir, name)
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
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, certgen.AdminKubeconfigFilename), []byte("kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}

	var outErr error
	out := captureStdout(t, func() { outErr = runGetKubeconfig("dev") })
	if outErr != nil {
		t.Fatal(outErr)
	}
	want := filepath.Join(stateDir, certgen.AdminKubeconfigFilename)
	if got := strings.TrimSpace(out); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// TestRunGetKubeconfigMissingCluster checks that a missing cluster is
// refused, listing the clusters that exist.
func TestRunGetKubeconfigMissingCluster(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	writeStateTree(t, filepath.Join(base, "coreshift", "clusters", "alpha"), "alpha")

	err := runGetKubeconfig("nope")
	if err == nil || !strings.Contains(err.Error(), "no cluster named nope") {
		t.Fatalf("err = %v, want a plain refusal", err)
	}
	if !strings.Contains(err.Error(), "alpha") {
		t.Fatalf("err = %v, want the existing cluster listed", err)
	}
}

// TestGetCommandsRequirePodman checks that get clusters fails naming
// podman when it is missing.
func TestGetCommandsRequirePodman(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for name, run := range map[string]func() error{
		"get clusters": func() error { return runGetClusters() },
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
	err := runDelete(context.Background(), "dev", false)
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

// TestRunDeleteStopsAndRemovesPreservingState checks the delete contract:
// the container is stopped with a timeout generous enough for the control
// plane's teardown and then removed, while the state directory and its
// marker are left intact. The state directory comes from the name; the
// container's port is never asked.
func TestRunDeleteStopsAndRemovesPreservingState(t *testing.T) {
	writeFakePodman(t)
	setContainers(t, "coreshift-dev")
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, "dev")

	var delErr error
	out := captureStdout(t, func() { delErr = runDelete(context.Background(), "dev", false) })
	if delErr != nil {
		t.Fatal(delErr)
	}
	calls := podmanCalls(t)
	want := []string{
		"container exists coreshift-dev",
		"ps -a --format {{.Names}}",
		"stop --timeout 120 coreshift-dev",
		"rm coreshift-dev",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("podman calls = %q\nwant %q", calls, want)
	}
	if !strings.Contains(out, "State preserved at "+stateDir) {
		t.Fatalf("output = %q, want the preserved state named", out)
	}
	if _, err := os.Stat(filepath.Join(stateDir, clusterMarkerFilename)); err != nil {
		t.Fatal("delete removed the state directory")
	}
}

// TestRunDeleteNotRunningPreservesState checks the delete of a cluster whose
// container is already gone but whose state was preserved: nothing is
// stopped, and the preserved state is named.
func TestRunDeleteNotRunningPreservesState(t *testing.T) {
	writeFakePodman(t)
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, "dev")

	var delErr error
	out := captureStdout(t, func() { delErr = runDelete(context.Background(), "dev", false) })
	if delErr != nil {
		t.Fatal(delErr)
	}
	want := []string{
		"container exists coreshift-dev",
		"ps -a --format {{.Names}}",
	}
	if calls := podmanCalls(t); !slices.Equal(calls, want) {
		t.Fatalf("podman calls = %q, want only the existence check and the worker listing", calls)
	}
	if !strings.Contains(out, "not running") || !strings.Contains(out, "State preserved at "+stateDir) {
		t.Fatalf("output = %q, want the cluster reported not running with its preserved state named", out)
	}
}

// TestRunDeleteCleanupRemovesState checks the cleanup flag's contract:
// the same stop-and-remove happens, and the state directory is removed
// with the same removal the cleanup command uses, with the removal said
// plainly in the output.
func TestRunDeleteCleanupRemovesState(t *testing.T) {
	writeFakePodman(t)
	setContainers(t, "coreshift-dev")
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, "dev")

	var delErr error
	out := captureStdout(t, func() { delErr = runDelete(context.Background(), "dev", true) })
	if delErr != nil {
		t.Fatal(delErr)
	}
	calls := podmanCalls(t)
	want := []string{
		"container exists coreshift-dev",
		"ps -a --format {{.Names}}",
		"stop --timeout 120 coreshift-dev",
		"rm coreshift-dev",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("podman calls = %q\nwant %q", calls, want)
	}
	if !strings.Contains(out, "State removed at "+stateDir) {
		t.Fatalf("output = %q, want the removed state named", out)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("state directory still exists after cleanup")
	}
}

// TestRunDeleteMissingCluster checks that deleting a missing cluster is
// refused plainly, with nothing stopped or removed, with and without the
// cleanup flag.
func TestRunDeleteMissingCluster(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		writeFakePodman(t)
		err := runDelete(context.Background(), "nope", cleanup)
		if err == nil || !strings.Contains(err.Error(), "no cluster named nope") {
			t.Fatalf("cleanup=%v: err = %v, want a plain refusal", cleanup, err)
		}
		if calls := podmanCalls(t); len(calls) != 1 || calls[0] != "container exists coreshift-nope" {
			t.Fatalf("cleanup=%v: podman calls = %q, want only the existence check", cleanup, calls)
		}
	}
}

// TestRunDeleteCascadesWorkers checks the cascade: deleting a cluster
// deletes each of its workers the way delete worker does, containers
// stopped and removed, before the cluster's own container goes. Another
// cluster's workers are left alone.
func TestRunDeleteCascadesWorkers(t *testing.T) {
	writeFakePodman(t)
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	setContainers(t, "coreshift-dev", "coreshift-worker-worker1", "coreshift-worker-worker2", "coreshift-worker-foreign")
	setContainerLabel(t, "coreshift-worker-worker1", "dev")
	setContainerLabel(t, "coreshift-worker-worker2", "dev")
	setContainerLabel(t, "coreshift-worker-foreign", "nope")
	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, "dev")

	var delErr error
	out := captureStdout(t, func() { delErr = runDelete(context.Background(), "dev", false) })
	if delErr != nil {
		t.Fatal(delErr)
	}
	var stops []string
	for _, call := range podmanCalls(t) {
		if strings.HasPrefix(call, "stop --timeout") {
			stops = append(stops, call)
		}
	}
	want := []string{
		"stop --timeout 120 coreshift-worker-worker1",
		"stop --timeout 120 coreshift-worker-worker2",
		"stop --timeout 120 coreshift-dev",
	}
	if !slices.Equal(stops, want) {
		t.Fatalf("stops = %q\nwant %q", stops, want)
	}
	for _, want := range []string{
		"Deleted worker worker1",
		"Deleted worker worker2",
		"Deleted cluster dev",
		"State preserved at " + stateDir,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q named", out, want)
		}
	}
	if strings.Contains(out, "foreign") {
		t.Fatalf("another cluster's worker was deleted too:\n%s", out)
	}
}

// TestRunDeleteWorkerWithoutClusterState checks the edge a cascade can
// hit too: a worker whose cluster's state directory is gone is still
// stopped and removed, with a note that its node went with the data.
func TestRunDeleteWorkerWithoutClusterState(t *testing.T) {
	writeFakePodman(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	setContainers(t, "coreshift-worker-w1")
	setContainerLabel(t, "coreshift-worker-w1", "dev")

	var delErr error
	out := captureStdout(t, func() { delErr = runDeleteWorker(context.Background(), "w1") })
	if delErr != nil {
		t.Fatal(delErr)
	}
	calls := podmanCalls(t)
	want := []string{
		"container exists coreshift-worker-w1",
		`inspect --format {{index .Config.Labels "coreshift-cluster"}} coreshift-worker-w1`,
		"stop --timeout 120 coreshift-worker-w1",
		"rm coreshift-worker-w1",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("podman calls = %q\nwant %q", calls, want)
	}
	for _, want := range []string{"Deleted worker w1", "no state left"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q named", out, want)
		}
	}
	if strings.Contains(out, "not running") {
		t.Fatalf("output = %q, want the state-gone note, not the not-running one", out)
	}
}

// TestDeleteClusterFlagDefaults checks that the delete command comes with
// the documented defaults.
func TestDeleteClusterFlagDefaults(t *testing.T) {
	cmd := newDeleteClusterCommand()
	if flag := cmd.Flags().Lookup("name"); flag == nil {
		t.Fatal("flag --name is not defined")
	} else if flag.DefValue != "coreshift" {
		t.Fatalf("--name default = %q, want coreshift", flag.DefValue)
	}
	if flag := cmd.Flags().Lookup("cleanup"); flag == nil {
		t.Fatal("flag --cleanup is not defined")
	} else if flag.DefValue != "false" {
		t.Fatalf("--cleanup default = %q, want false", flag.DefValue)
	}
}

// TestCreateWorkerFlagDefaults checks that the worker creation flags are
// accepted and come with the documented defaults — the name has none,
// because a bare creation counts it — and that the pull secret is
// optional: the cluster's stored one is the default.
func TestCreateWorkerFlagDefaults(t *testing.T) {
	cmd := newCreateWorkerCommand()
	for name, want := range map[string]string{
		"name":        "",
		"cluster":     "coreshift",
		"verbose":     "false",
		"pull-secret": "",
	} {
		if flag := cmd.Flags().Lookup(name); flag == nil {
			t.Fatalf("flag --%s is not defined", name)
		} else if flag.DefValue != want {
			t.Fatalf("--%s default = %q, want %q", name, flag.DefValue, want)
		}
	}
	if usage := cmd.Flags().Lookup("name").Usage; !strings.Contains(usage, "count") {
		t.Fatalf("--name usage = %q, want the counting default described", usage)
	}
	if _, required := cmd.Flags().Lookup("pull-secret").Annotations[cobra.BashCompOneRequiredFlag]; required {
		t.Fatal("pull secret must not be required: the cluster's stored one is the default")
	}
}

// TestDeleteWorkerFlagDefaults checks that the worker deletion comes
// with the documented default.
func TestDeleteWorkerFlagDefaults(t *testing.T) {
	cmd := newDeleteWorkerCommand()
	if flag := cmd.Flags().Lookup("name"); flag == nil {
		t.Fatal("flag --name is not defined")
	} else if flag.DefValue != "worker1" {
		t.Fatalf("--name default = %q, want worker1", flag.DefValue)
	}
}

// TestWorkerRunArgs checks the podman command line that starts a worker
// container: its name and cluster label, the host network it dials the
// control plane on, the privilege its cgroup hierarchy needs, the join
// material mounted read-only where the entrypoint reads it, and the
// host's kernel modules read-only.
func TestWorkerRunArgs(t *testing.T) {
	got := workerRunArgs("coreshift-worker-w1", "dev", "/state/api/workers/w1", "localhost/coreshift-worker:v1")
	want := []string{
		"run", "--detach",
		"--name", "coreshift-worker-w1",
		"--label", "coreshift-cluster=dev",
		"--network", "host",
		"--privileged",
		"--volume", "/state/api/workers/w1/bootstrap-kubeconfig:/etc/kubernetes/bootstrap-kubeconfig:ro,z",
		"--volume", "/state/api/workers/w1/node-name:/etc/coreshift/node-name:ro,z",
		"--volume", "/lib/modules:/lib/modules:ro",
		"localhost/coreshift-worker:v1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("podman args = %q\nwant %q", got, want)
	}
}

// TestRunCreateWorkerRefusals checks the refusals that must arrive before
// anything is minted: a name the objects cannot carry, a taken worker
// name, an unknown cluster, and a cluster whose API is not answering.
// None needs a pull secret flag, since the refusals precede its use.
func TestRunCreateWorkerRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		worker  string
		cluster string
		want    string
	}{
		"name is not a DNS label": {"Bad Name", "coreshift", "DNS label"},
		"name is empty":           {"", "coreshift", "DNS label"},
		"worker name is taken":    {"w1", "coreshift", "worker w1 already exists"},
		"cluster is unknown":      {"fresh", "nope", "no cluster named nope"},
		"cluster is not running":  {"fresh", "dev", "not running"},
	} {
		writeFakePodman(t)
		setContainers(t, "coreshift-worker-w1", "coreshift-dev")
		setContainerPort(t, "coreshift-dev", 17443)

		err := runCreateWorker(context.Background(), tc.worker, true, tc.cluster, "", false)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want a refusal containing %q", name, err, tc.want)
		}
		// A refusal before the first podman call leaves no call log.
		if calls, err := os.ReadFile(os.Getenv("PODMAN_LOG")); err == nil && strings.Contains(string(calls), "build") {
			t.Fatalf("%s: an image was minted despite the refusal", name)
		}
	}
}

// TestRunCreateWorkerRefusesMissingFlagFile checks that a pull secret
// flag pointing at no file is refused before anything is minted.
func TestRunCreateWorkerRefusesMissingFlagFile(t *testing.T) {
	writeFakePodman(t)
	err := runCreateWorker(context.Background(), "fresh", true, "coreshift", "/nonexistent/pull-secret", false)
	if err == nil || !strings.Contains(err.Error(), "read pull secret") {
		t.Fatalf("err = %v, want a refusal naming the pull secret", err)
	}
}

// TestNextWorkerName checks the counting rule a bare worker creation
// names by: the number is one past the cluster's worker count — not the
// first free slot — and skips upward past any name already in use, so
// gaps left by deletions never cause a collision.
func TestNextWorkerName(t *testing.T) {
	for name, tc := range map[string]struct {
		cluster string
		workers map[string]string // worker container → its cluster label
		want    string
	}{
		"fresh cluster": {
			cluster: "dev",
			want:    "worker1",
		},
		"one worker named foo": {
			cluster: "dev",
			workers: map[string]string{"coreshift-worker-foo": "dev"},
			want:    "worker2",
		},
		"three workers count to the fourth": {
			cluster: "dev",
			workers: map[string]string{
				"coreshift-worker-worker1": "dev",
				"coreshift-worker-worker2": "dev",
				"coreshift-worker-worker3": "dev",
			},
			want: "worker4",
		},
		"deleted middle leaves the number skipped": {
			cluster: "dev",
			workers: map[string]string{
				"coreshift-worker-worker1": "dev",
				"coreshift-worker-worker3": "dev",
			},
			want: "worker4",
		},
		"another cluster's workers do not count": {
			cluster: "dev",
			workers: map[string]string{
				"coreshift-worker-foo":     "dev",
				"coreshift-worker-worker1": "other",
			},
			want: "worker2",
		},
		"a name taken by another cluster's worker is skipped": {
			cluster: "dev",
			workers: map[string]string{
				"coreshift-worker-foo":     "dev",
				"coreshift-worker-worker2": "other",
			},
			want: "worker3",
		},
	} {
		writeFakePodman(t)
		names := make([]string, 0, len(tc.workers))
		for container, label := range tc.workers {
			names = append(names, container)
			setContainerLabel(t, container, label)
		}
		setContainers(t, names...)

		got, err := nextWorkerName(tc.cluster)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("%s: name = %q, want %q", name, got, tc.want)
		}
	}
}

// TestRunCreateWorkerBarePrintsCountedName checks that a bare creation
// picks the counted name and prints it, so the command is never
// mysterious about what it is creating.
func TestRunCreateWorkerBarePrintsCountedName(t *testing.T) {
	writeFakePodman(t)
	setContainers(t, "coreshift-worker-foo")
	setContainerLabel(t, "coreshift-worker-foo", "nope")

	var err error
	out := captureStdout(t, func() {
		err = runCreateWorker(context.Background(), "", false, "nope", "", false)
	})
	if err == nil || !strings.Contains(err.Error(), "no cluster named nope") {
		t.Fatalf("err = %v, want the unknown-cluster refusal after the pick", err)
	}
	if !strings.Contains(out, "Worker name: worker2") {
		t.Fatalf("output = %q, want the counted name printed prominently", out)
	}
}

// writeClusterPullSecret stands up a plain-HTTP stand-in for a cluster's
// API serving the pull secret read — the given secret's JSON, or
// not-found when none is given — and writes an admin kubeconfig pointing
// at it into the state directory, the way a real cluster's state
// directory holds one.
func writeClusterPullSecret(t *testing.T, stateDir string, pullSecret []byte) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/openshift-config/secrets/pull-secret", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if pullSecret == nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(metav1.Status{
				Status: metav1.StatusFailure,
				Reason: metav1.StatusReasonNotFound,
				Code:   http.StatusNotFound,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "pull-secret", Namespace: "openshift-config"},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: pullSecret},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	config := clientcmdapi.NewConfig()
	config.Clusters["coreshift"] = &clientcmdapi.Cluster{Server: server.URL}
	config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{}
	config.Contexts["admin@coreshift"] = &clientcmdapi.Context{Cluster: "coreshift", AuthInfo: "admin"}
	config.CurrentContext = "admin@coreshift"
	if err := clientcmd.WriteToFile(*config, filepath.Join(stateDir, certgen.AdminKubeconfigFilename)); err != nil {
		t.Fatal(err)
	}
}

// TestWorkerPullSecretReadsClusterSecret checks the default path: with
// no flag, the Secret the cluster stores becomes both the build's
// credentials and its throwaway auth file, which the cleanup removes.
func TestWorkerPullSecretReadsClusterSecret(t *testing.T) {
	secret := []byte(`{"auths":{"quay.io":{"auth":"dXNlcjpwYXNz"}}}`)
	stateDir := t.TempDir()
	writeClusterPullSecret(t, stateDir, secret)

	pullSecret, authfile, cleanup, err := workerPullSecret(context.Background(), "dev", stateDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pullSecret, secret) {
		t.Fatalf("pull secret = %q, want the cluster's stored one", pullSecret)
	}
	data, err := os.ReadFile(authfile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, secret) {
		t.Fatalf("auth file holds %q, want the cluster's secret", data)
	}
	cleanup()
	if _, err := os.Stat(authfile); !os.IsNotExist(err) {
		t.Fatal("cleanup left the throwaway auth file behind")
	}
}

// TestWorkerPullSecretFlagWins checks that a given flag file is used as
// the build's authfile itself, whatever the cluster stores.
func TestWorkerPullSecretFlagWins(t *testing.T) {
	stateDir := t.TempDir()
	writeClusterPullSecret(t, stateDir, []byte(`{"auths":{"other.example":{"auth":"other"}}}`))
	flagPath := writePullSecret(t)

	pullSecret, authfile, cleanup, err := workerPullSecret(context.Background(), "dev", stateDir, flagPath)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		t.Fatal("the flag's own file needs no cleanup")
	}
	if authfile != flagPath {
		t.Fatalf("authfile = %q, want the flag's file", authfile)
	}
	if want := readFile(t, filepath.Dir(flagPath), filepath.Base(flagPath)); !bytes.Equal(pullSecret, want) {
		t.Fatalf("pull secret = %q, want the flag's file content", pullSecret)
	}
}

// TestWorkerPullSecretRefusesClusterWithoutSecret checks the fallback's
// refusal: a cluster storing no pull secret, the shape one created
// before the Secret existed has, is refused naming the flag and the
// re-creation.
func TestWorkerPullSecretRefusesClusterWithoutSecret(t *testing.T) {
	stateDir := t.TempDir()
	writeClusterPullSecret(t, stateDir, nil)

	_, _, _, err := workerPullSecret(context.Background(), "dev", stateDir, "")
	if err == nil {
		t.Fatal("expected a refusal for a cluster storing no pull secret")
	}
	for _, want := range []string{"stores no pull secret", "--pull-secret", "re-create"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q:\n%v", want, err)
		}
	}
}

// TestWorkerClusterTarget checks how a worker's cluster is resolved: a
// cluster in a container through its published port, and a host-mode
// cluster, which has no container to ask, through the port its admin
// kubeconfig records.
func TestWorkerClusterTarget(t *testing.T) {
	writeFakePodman(t)
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	setContainers(t, "coreshift-dev")
	setContainerPort(t, "coreshift-dev", 7443)

	port, dir, err := workerClusterTarget("dev")
	if err != nil {
		t.Fatal(err)
	}
	if port != 7443 || dir != filepath.Join(base, "coreshift", "clusters", "dev") {
		t.Fatalf("container cluster: port %d, dir %q", port, dir)
	}

	// Host mode: no cluster container, but the cluster's state directory
	// holds the admin kubeconfig a run wrote, pointing at its port.
	stateDir := filepath.Join(base, "coreshift", "clusters", "coreshift")
	writeClusterPullSecret(t, stateDir, nil)
	config, err := clientcmd.LoadFromFile(filepath.Join(stateDir, certgen.AdminKubeconfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	config.Clusters["coreshift"].Server = "https://127.0.0.1:16443"
	if err := clientcmd.WriteToFile(*config, filepath.Join(stateDir, certgen.AdminKubeconfigFilename)); err != nil {
		t.Fatal(err)
	}
	port, dir, err = workerClusterTarget("coreshift")
	if err != nil {
		t.Fatal(err)
	}
	if port != 16443 || dir != stateDir {
		t.Fatalf("host-mode cluster: port %d, dir %q", port, dir)
	}

	if _, _, err := workerClusterTarget("nope"); err == nil || !strings.Contains(err.Error(), "no cluster named nope") {
		t.Fatalf("err = %v, want a plain refusal", err)
	}
}

// TestRunGetWorkersEmpty checks that an empty worker list is reported
// plainly.
func TestRunGetWorkersEmpty(t *testing.T) {
	writeFakePodman(t)
	var err error
	out := captureStdout(t, func() { err = runGetWorkers() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No workers") {
		t.Fatalf("output = %q, want the empty list reported", out)
	}
}

// TestRunGetWorkersListsWorkers checks the row each worker gets: its
// name, the cluster its container is labeled with, and readiness from
// its node's live condition, so a worker without a reachable cluster
// shows as not ready and one without a label shows no cluster.
func TestRunGetWorkersListsWorkers(t *testing.T) {
	writeFakePodman(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	setContainers(t, "coreshift-dev", "coreshift-worker-w1", "coreshift-worker-w2")
	setContainerPort(t, "coreshift-dev", 7443)
	setContainerLabel(t, "coreshift-worker-w1", "dev")

	var err error
	out := captureStdout(t, func() { err = runGetWorkers() })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("output has %d lines, want a header and two workers:\n%s", len(lines), out)
	}
	if fields := strings.Fields(lines[1]); len(fields) < 4 || fields[0] != "w1" || fields[1] != "dev" || strings.Join(fields[2:], " ") != "not ready" {
		t.Fatalf("w1 row = %q, want its name, cluster, and not ready", lines[1])
	}
	if fields := strings.Fields(lines[2]); len(fields) < 4 || fields[0] != "w2" || fields[1] != "-" || strings.Join(fields[2:], " ") != "not ready" {
		t.Fatalf("w2 row = %q, want its name, no cluster, and not ready", lines[2])
	}
}

// TestRunDeleteWorkerStopsAndRemoves checks the delete contract: the
// container is stopped with the same teardown time the cluster's gets
// and then removed, its cluster read from its label.
func TestRunDeleteWorkerStopsAndRemoves(t *testing.T) {
	writeFakePodman(t)
	setContainers(t, "coreshift-worker-w1")
	setContainerLabel(t, "coreshift-worker-w1", "coreshift")

	var delErr error
	captureStdout(t, func() { delErr = runDeleteWorker(context.Background(), "w1") })
	if delErr != nil {
		t.Fatal(delErr)
	}
	calls := podmanCalls(t)
	want := []string{
		"container exists coreshift-worker-w1",
		`inspect --format {{index .Config.Labels "coreshift-cluster"}} coreshift-worker-w1`,
		"stop --timeout 120 coreshift-worker-w1",
		"rm coreshift-worker-w1",
	}
	// The cluster resolution trails the removal; only the head of the
	// call sequence is the delete itself.
	if !slices.Equal(calls[:len(want)], want) {
		t.Fatalf("podman calls = %q\nwant %q at the head", calls, want)
	}
}

// TestRunDeleteWorkerMissing checks that deleting a missing worker is
// refused plainly, with nothing stopped or removed.
func TestRunDeleteWorkerMissing(t *testing.T) {
	writeFakePodman(t)
	err := runDeleteWorker(context.Background(), "nope")
	if err == nil || !strings.Contains(err.Error(), "no worker named nope") {
		t.Fatalf("err = %v, want a plain refusal", err)
	}
	if calls := podmanCalls(t); len(calls) != 1 || calls[0] != "container exists coreshift-worker-nope" {
		t.Fatalf("podman calls = %q, want only the existence check", calls)
	}
}

// TestWorkerFailureQuotesLogAndRemoves checks the failure contract: the
// error carries the container's log tail, a cgroup delegation failure
// gains the rootful remedy, and the container is removed.
func TestWorkerFailureQuotesLogAndRemoves(t *testing.T) {
	writeFakePodman(t)
	setContainerLog(t, "coreshift-worker-w1",
		"enabling the cgroup controllers failed\nthe worker container's cgroup hierarchy must be delegated to it\n")

	err := workerFailure("coreshift-worker-w1", "/state/api", "w1", fmt.Errorf("the worker did not join"))
	if err == nil {
		t.Fatal("expected a failure report")
	}
	for _, want := range []string{
		"the worker did not join",
		"recent log output:",
		"must be delegated to it",
		"sudo",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("failure missing %q:\n%v", want, err)
		}
	}
	if calls := podmanCalls(t); !slices.Contains(calls, "rm --force coreshift-worker-w1") {
		t.Fatalf("podman calls = %q, want the container removed", calls)
	}
}

// TestWorkerFailureWithoutLogKeepsErrorPlain checks that a failure with
// no log output to quote stays plain, still removing the container.
func TestWorkerFailureWithoutLogKeepsErrorPlain(t *testing.T) {
	writeFakePodman(t)

	err := workerFailure("coreshift-worker-w1", "/state/api", "w1", fmt.Errorf("the worker did not join"))
	if err == nil || err.Error() != "the worker did not join" {
		t.Fatalf("err = %v, want the plain failure", err)
	}
	if calls := podmanCalls(t); !slices.Contains(calls, "rm --force coreshift-worker-w1") {
		t.Fatalf("podman calls = %q, want the container removed", calls)
	}
}

// TestGetClustersSkipsWorkers checks that worker containers, which share
// the cluster prefix, are not listed as clusters.
func TestGetClustersSkipsWorkers(t *testing.T) {
	writeFakePodman(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	setContainers(t, "coreshift-dev", "coreshift-worker-w1")
	setContainerPort(t, "coreshift-dev", 7443)

	var err error
	out := captureStdout(t, func() { err = runGetClusters() })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "w1") {
		t.Fatalf("worker listed as a cluster:\n%s", out)
	}
	if !strings.Contains(out, "dev") {
		t.Fatalf("cluster missing from the list:\n%s", out)
	}
}
