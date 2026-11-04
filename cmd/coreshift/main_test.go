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
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
// writeNetworkOperatorRelease writes the release records the network
// operator's environment is built from: the payload's version record
// and the release manifest resolving every component the operator's
// deployment names.
func writeNetworkOperatorRelease(t *testing.T) (releaseRoot, payloadDir string) {
	t.Helper()
	components := make([]string, 0, len(networkOperatorImages))
	for _, entry := range networkOperatorImages {
		components = append(components, fmt.Sprintf(
			`{"name": %q, "from": {"kind": "DockerImage", "name": "quay.io/example/%s@sha256:%s"}}`,
			entry.component, entry.component, entry.component))
	}
	releaseRoot = t.TempDir()
	manifest := filepath.Join(releaseRoot, "release-manifests", "image-references")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{"spec": {"tags": [`+strings.Join(components, ",")+`]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	payloadDir = t.TempDir()
	metadata := filepath.Join(payloadDir, "release-manifests", "release-metadata")
	if err := os.MkdirAll(filepath.Dir(metadata), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(`{"version": "4.22.21"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return releaseRoot, payloadDir
}

func TestNetworkOperatorEnv(t *testing.T) {
	releaseRoot, payloadDir := writeNetworkOperatorRelease(t)

	env, err := networkOperatorEnv(releaseRoot, payloadDir)
	if err != nil {
		t.Fatal(err)
	}

	variables := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		variables[name] = value
	}
	if variables["RELEASE_VERSION"] != "4.22.21" {
		t.Fatalf("RELEASE_VERSION = %q, want 4.22.21", variables["RELEASE_VERSION"])
	}
	for _, entry := range networkOperatorImages {
		if want := "quay.io/example/" + entry.component + "@sha256:" + entry.component; variables[entry.env] != want {
			t.Fatalf("%s = %q, want %q", entry.env, variables[entry.env], want)
		}
	}
	for _, tuning := range networkOperatorTunings {
		if !slices.Contains(env, tuning) {
			t.Fatalf("environment is missing the deployment's %s", tuning)
		}
	}
}

func TestNetworkOperatorEnvMissingComponent(t *testing.T) {
	releaseRoot, payloadDir := writeNetworkOperatorRelease(t)
	if err := os.Remove(filepath.Join(releaseRoot, "release-manifests", "image-references")); err != nil {
		t.Fatal(err)
	}

	if _, err := networkOperatorEnv(releaseRoot, payloadDir); err == nil {
		t.Fatal("expected an error naming the unresolvable image")
	}
}

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

// TestKubeAPIServerArgsKubeletConnection checks that kube-apiserver dials
// kubelets by their internal IP first and presents the client certificate
// kubelets authenticate: nothing in this cluster resolves a node's bare
// name, so the hostname-first default leaves logs, exec, and
// port-forwarding failing on name resolution, and without the client
// certificate every kubelet-proxied path fails on authentication.
func TestKubeAPIServerArgsKubeletConnection(t *testing.T) {
	args := kubeAPIServerArgs("/state", "http://127.0.0.1:2379", defaultAPIPort,
		"/state/kube-apiserver-config.json", "/state/token-review.kubeconfig")
	var addressTypes, clientCert, clientKey bool
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--kubelet-preferred-address-types="):
			addressTypes = true
			if value := strings.TrimPrefix(arg, "--kubelet-preferred-address-types="); value != "InternalIP,Hostname,InternalDNS,ExternalDNS,ExternalIP" {
				t.Fatalf("kubelet preferred address types = %q, want the node's internal IP first", value)
			}
		case arg == "--kubelet-client-certificate=/state/"+certgen.KubeletClientCertFilename:
			clientCert = true
		case arg == "--kubelet-client-key=/state/"+certgen.KubeletClientKeyFilename:
			clientKey = true
		}
	}
	if !addressTypes {
		t.Fatalf("kube-apiserver args carry no kubelet preferred address types:\n%q", args)
	}
	if !clientCert || !clientKey {
		t.Fatalf("kube-apiserver args carry no kubelet client certificate pair:\n%q", args)
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
	fakeProc(t)
	stateDir, err := clusterDir(defaultClusterName)
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, defaultClusterName)

	if err := runCleanup(context.Background(), defaultClusterName, false, false); err != nil {
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
	fakeProc(t)

	if err := runCleanup(context.Background(), "dev", false, false); err != nil {
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
	fakeProc(t)

	if err := runCleanup(context.Background(), defaultClusterName, true, false); err != nil {
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
	fakeProc(t)
	if err := runCleanup(context.Background(), defaultClusterName, false, false); err != nil {
		t.Fatalf("missing state directory must not fail: %v", err)
	}
}

func TestCleanupCache(t *testing.T) {
	// The fake podman keeps the cleanup's image half from touching any
	// real images the machine running the tests holds.
	writeFakePodman(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cacheBase := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheBase)
	cacheDir := filepath.Join(cacheBase, "coreshift")
	writeStateTree(t, cacheDir, defaultClusterName)
	fakeProc(t)

	if err := runCleanup(context.Background(), defaultClusterName, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatal("image cache still exists")
	}
}

// TestCleanupCacheRemovesMintedImages checks the cache flag's widened
// meaning: the minted images in both of the project's repositories are
// removed alongside the cache directory, and the state removal still
// happens with them.
func TestCleanupCacheRemovesMintedImages(t *testing.T) {
	writeFakePodman(t)
	setRepositoryImages(t, "localhost/coreshift", "localhost/coreshift:v1-a")
	setRepositoryImages(t, "localhost/coreshift-worker", "localhost/coreshift-worker:v1-c")
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	cacheBase := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheBase)
	stateDir, err := clusterDir(defaultClusterName)
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, defaultClusterName)
	fakeProc(t)

	var cleanupErr error
	out := captureStdout(t, func() { cleanupErr = runCleanup(context.Background(), defaultClusterName, false, true) })
	if cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	calls := podmanCalls(t)
	want := []string{
		"images --format {{.Repository}}:{{.Tag}} localhost/coreshift",
		"images --format {{.Repository}}:{{.Tag}} localhost/coreshift-worker",
		"system df --format json",
		"rmi localhost/coreshift:v1-a",
		"rmi localhost/coreshift-worker:v1-c",
		"system df --format json",
		"network exists coreshift",
		"network rm coreshift",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("podman calls = %q\nwant %q", calls, want)
	}
	for _, want := range []string{
		"Removing image localhost/coreshift:v1-a",
		"Removing image localhost/coreshift-worker:v1-c",
		"No image cache at ",
		"Removed network coreshift",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q named", out, want)
		}
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("state directory still exists")
	}
}

// TestCleanupWithoutCacheLeavesImagesAlone checks the other side of the
// widened meaning: without the cache flag no image is listed or removed.
func TestCleanupWithoutCacheLeavesImagesAlone(t *testing.T) {
	writeFakePodman(t)
	setRepositoryImages(t, "localhost/coreshift", "localhost/coreshift:v1-a")
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := clusterDir(defaultClusterName)
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, defaultClusterName)
	fakeProc(t)

	if err := runCleanup(context.Background(), defaultClusterName, false, false); err != nil {
		t.Fatal(err)
	}
	for _, call := range podmanCalls(t) {
		if strings.HasPrefix(call, "images") || strings.HasPrefix(call, "rmi") {
			t.Fatalf("podman calls = %q, want no image commands without the cache flag", podmanCalls(t))
		}
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("state directory still exists")
	}
}

func TestCleanupRefusesAllWithName(t *testing.T) {
	cmd := newCleanupCommand()
	cmd.SetArgs([]string{"--all", "--name", "dev"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("err = %v, want --all and --name together refused", err)
	}
}

// fakeProc points the running-control-plane scan at an empty directory of
// the test's own, so anything live on the machine, a developer's cluster
// included, is invisible to the test. It returns the directory, where
// writeProcEntry plants the stand-ins the test wants the scan to see.
func fakeProc(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = previous })
	return dir
}

// writeProcEntry plants a process command line in a fake proc directory,
// in the shape the scan reads: a numbered directory whose cmdline file
// carries the arguments separated by NUL bytes. Numbers stay past the test
// process's own, which the scan skips.
func writeProcEntry(t *testing.T, dir string, args ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dir, strconv.Itoa(os.Getpid()+len(entries)+1))
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.Join(args, "\x00") + "\x00")
	if err := os.WriteFile(filepath.Join(entry, "cmdline"), data, 0o644); err != nil {
		t.Fatal(err)
	}
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
	proc := fakeProc(t)
	writeProcEntry(t, proc, "etcd", "--data-dir="+filepath.Join(stateDir, "etcd-data"))

	err = runCleanup(context.Background(), "dev", false, false)
	if err == nil || !strings.Contains(err.Error(), "Stop it first") {
		t.Fatalf("err = %v, want a refusal naming the running control plane", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(stateDir, "etcd-data")) {
		t.Fatalf("err = %v, want the refusal to name the test's own stand-in", err)
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
// runnable things and succeeds: the worker entrypoint is the worker
// image's own, not something a user types, so it stays out of the
// listing.
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
	if strings.Contains(out.String(), "worker") {
		t.Fatalf("run help lists the worker entrypoint, which users never type:\n%s", out.String())
	}
}

// TestWorkerCommandHiddenButRunnable checks that the worker entrypoint
// is hidden from the listings while remaining resolvable: it is the
// worker image's entrypoint, typed by no user but run by every worker
// container.
func TestWorkerCommandHiddenButRunnable(t *testing.T) {
	root := newRootCommand()
	worker, _, err := root.Find([]string{"run", "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if worker.Name() != "worker" {
		t.Fatalf("found %q, want the worker entrypoint", worker.Name())
	}
	if !worker.Hidden {
		t.Fatal("the worker entrypoint must be hidden: it is the image's own, not a user command")
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
		"--network", "coreshift",
		"--publish", "127.0.0.1:7443:7443",
		"--publish", "127.0.0.1:7444:8445",
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

// TestRefuseOAuthPort checks that an API port whose following port, the
// OAuth login endpoint's, is reserved or taken is refused, naming that
// port, and that a free one passes.
func TestRefuseOAuthPort(t *testing.T) {
	for _, api := range []int{etcdPeerPort - 1, oauthAPIServerPort - 1} {
		err := refuseOAuthPort(api)
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(api+1)) {
			t.Fatalf("API port %d: err = %v, want a refusal naming port %d", api, err, api+1)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	err = refuseOAuthPort(port - 1)
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Fatalf("err = %v, want a refusal naming port %d", err, port)
	}
}

// TestOAuthMetadataEndpointsFollowMode checks that the OAuth login
// endpoints the discovery document advertises sit on the OAuth server's
// fixed port on the host and on the port after the API port inside a
// container, where the container publishes the server.
func TestOAuthMetadataEndpointsFollowMode(t *testing.T) {
	stateDir := t.TempDir()
	const port = 7443
	t.Setenv(containerEnv, "")
	if _, err := writeOAuthMetadata(stateDir, port); err != nil {
		t.Fatal(err)
	}
	metadata := string(readFile(t, stateDir, "oauth-metadata.json"))
	if !strings.Contains(metadata, fmt.Sprintf("https://127.0.0.1:%d/oauth/authorize", oauthServerPort)) {
		t.Fatalf("host metadata does not point at the OAuth server's port:\n%s", metadata)
	}

	t.Setenv(containerEnv, "1")
	if _, err := writeOAuthMetadata(stateDir, port); err != nil {
		t.Fatal(err)
	}
	metadata = string(readFile(t, stateDir, "oauth-metadata.json"))
	if !strings.Contains(metadata, fmt.Sprintf("https://127.0.0.1:%d/oauth/authorize", port+1)) {
		t.Fatalf("container metadata does not point at the published OAuth port:\n%s", metadata)
	}
}

// TestOAuthServerConfigBindsAllInterfacesInContainer checks that the
// OAuth server's serving address follows where it runs, like the API
// server's: every interface inside a container, so the published login
// port reaches it, and loopback only on the host.
func TestOAuthServerConfigBindsAllInterfacesInContainer(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv(containerEnv, "1")

	path, err := writeOAuthServerConfig(stateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := string(readFile(t, filepath.Dir(path), filepath.Base(path)))
	if !strings.Contains(config, fmt.Sprintf("bindAddress: 0.0.0.0:%d", oauthServerPort)) {
		t.Fatalf("config does not bind all interfaces:\n%s", config)
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

// TestBootReleaseWarnsBeyondVerifiedLine checks that any explicitly
// requested release from a line newer than the verified one — a
// version, the newest value, or a pinned image — warns, and that an
// in-line request stays quiet.
func TestBootReleaseWarnsBeyondVerifiedLine(t *testing.T) {
	for name, tc := range map[string]struct {
		release releaseOptions
		refHas  string
		warns   bool
	}{
		"version":         {releaseOptions{version: "5.0.0", arch: "x86_64"}, "5.0.0", true},
		"newest":          {releaseOptions{version: "newest", arch: "x86_64"}, "5.0.0", true},
		"pinned image":    {releaseOptions{image: "reg.example/ocp-release:5.0.0-x86_64"}, "5.0.0", true},
		"in-line version": {releaseOptions{version: "4.22.11", arch: "x86_64"}, "4.22.11", false},
	} {
		tc.release.registry = newTagRegistry(t, "4.22.11-x86_64", "5.0.0-x86_64")

		ref, note, err := bootRelease(context.Background(), &image.Client{}, tc.release, t.TempDir())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(ref, tc.refHas) {
			t.Fatalf("%s: ref = %q, want it to carry %s", name, ref, tc.refHas)
		}
		if !tc.warns {
			if note != "" {
				t.Fatalf("%s: note = %q, want none", name, note)
			}
			continue
		}
		if !strings.Contains(note, "beyond the newest verified line") {
			t.Fatalf("%s: note = %q, want the beyond-line warning", name, note)
		}
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
// answers with the per-container port file from PODMAN_PORTS, with the
// per-container label file from PODMAN_LABELS when asked for labels, and
// with the per-container running file from PODMAN_STATE, defaulting to
// stopped, when asked for the running state; logs answers with the
// per-container file from PODMAN_LOGS. The image side: images answers
// with the per-repository file from PODMAN_IMAGES, system df reports an
// empty store, and rmi answers with success.
// freePort returns a port a listener on loopback can take, for tests
// that need a port nothing answers on beside live clusters.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

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
	*Running*) cat "$PODMAN_STATE/$container" 2>/dev/null || echo false ;;
	*Pid*) cat "$PODMAN_PIDS/$container" 2>/dev/null || echo 0 ;;
	*) cat "$PODMAN_PORTS/$container" ;;
	esac
	exit 0
fi
if [ "$1" = "logs" ]; then
	cat "$PODMAN_LOGS/$2" 2>/dev/null
	exit 0
fi
if [ "$1" = "images" ]; then
	cat "$PODMAN_IMAGES/$4" 2>/dev/null
	exit 0
fi
if [ "$1 $2" = "system df" ]; then
	echo '[{"Type":"Images","RawSize":0}]'
	exit 0
fi
if [ "$1" = "rmi" ]; then
	echo "Untagged: $2"
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
	t.Setenv("PODMAN_STATE", t.TempDir())
	t.Setenv("PODMAN_LOGS", t.TempDir())
	t.Setenv("PODMAN_IMAGES", t.TempDir())
	t.Setenv("PODMAN_PIDS", t.TempDir())
}

// writeFakeSudo stands in for sudo, recording its calls. A read against
// root's store (-n) is answered by running the call against the
// container set SUDO_CONTAINERS names, so the root side is a world of
// its own, unless SUDO_REFUSE names a file: then it is refused, the way
// a real sudo refuses without a cached password.
func writeFakeSudo(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
echo "sudo $@" >> "$SUDO_LOG"
if [ "$1" = "-n" ]; then
	if [ -e "$SUDO_REFUSE" ]; then
		echo "sudo: a password is required" >&2
		exit 1
	fi
	shift
	exec env PODMAN_CONTAINERS="$SUDO_CONTAINERS" "$@"
fi
exec "$@"
`
	path := filepath.Join(dir, "sudo")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("SUDO_LOG", filepath.Join(t.TempDir(), "calls"))
	containers := filepath.Join(t.TempDir(), "containers")
	if err := os.WriteFile(containers, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUDO_CONTAINERS", containers)
}

// setRootfulContainers makes the fake sudo's root-side podman report
// exactly these containers as existing.
func setRootfulContainers(t *testing.T, names ...string) {
	t.Helper()
	if err := os.WriteFile(os.Getenv("SUDO_CONTAINERS"), []byte(strings.Join(names, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setContainerPID makes the fake podman answer pid inspection for
// container with the given process id.
func setContainerPID(t *testing.T, container string, pid int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("PODMAN_PIDS"), container), []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sudoCalls returns the calls the fake sudo recorded; a fake sudo never
// invoked recorded none.
func sudoCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("SUDO_LOG"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// setRepositoryImages makes the fake podman answer the images listing
// for one repository with the given references.
func setRepositoryImages(t *testing.T, repo string, refs ...string) {
	t.Helper()
	path := filepath.Join(os.Getenv("PODMAN_IMAGES"), repo)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(refs, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
// with an identity port mapping, the way a created cluster publishes its
// API.
func setContainerPort(t *testing.T, container string, port int) {
	t.Helper()
	mapping := fmt.Sprintf("%d %d/tcp\n", port, port)
	if err := os.WriteFile(filepath.Join(os.Getenv("PODMAN_PORTS"), container), []byte(mapping), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setContainerRunning makes the fake podman answer state inspection for
// container: running, or stopped when false.
func setContainerRunning(t *testing.T, container string, running bool) {
	t.Helper()
	state := "false\n"
	if running {
		state = "true\n"
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("PODMAN_STATE"), container), []byte(state), 0o644); err != nil {
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

// podmanCalls returns the calls the fake podman recorded; a fake podman
// never invoked recorded none.
func podmanCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("PODMAN_LOG"))
	if os.IsNotExist(err) {
		return nil
	}
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
// directory records, and readiness from its container running and a
// live probe agreeing, so a running cluster with nothing answering on
// its port shows as not ready and one without a state directory yet
// shows an unknown release.
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
	// A port nothing answers on, so the cluster reads as not ready
	// beside whatever a live cluster publishes.
	deadPort := freePort(t)
	setContainerPort(t, "coreshift-early", deadPort)
	setContainerPort(t, "coreshift-ready", readyPort)
	for _, name := range []string{"booting", "early", "ready"} {
		setContainerRunning(t, "coreshift-"+name, true)
	}
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
	if got := rowFor("early"); len(got) < 4 || got[1] != strconv.Itoa(deadPort) || got[2] != "-" || readiness(got) != "not ready" {
		t.Fatalf("early row = %q, want its port, an unknown release, and not ready", got)
	}
	if got := rowFor("ready"); len(got) != 4 || got[1] != strconv.Itoa(readyPort) || got[3] != "ready" {
		t.Fatalf("ready row = %q, want its port and ready", got)
	}
}

// TestRunGetClustersStoppedContainerNotReady checks the row a stopped
// container gets: not ready, without probing, even when its published
// port is held by a live cluster whose API would answer the probe for
// it, which is how a dead cluster used to show as ready.
func TestRunGetClustersStoppedContainerNotReady(t *testing.T) {
	writeFakePodman(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var probes atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
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
	// Both containers publish the live server's port; only one runs.
	setContainers(t, "coreshift-dead", "coreshift-live")
	setContainerPort(t, "coreshift-dead", readyPort)
	setContainerPort(t, "coreshift-live", readyPort)
	setContainerRunning(t, "coreshift-live", true)

	var outErr error
	out := captureStdout(t, func() { outErr = runGetClusters() })
	if outErr != nil {
		t.Fatal(outErr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("output has %d lines, want a header and two clusters:\n%s", len(lines), out)
	}
	if fields := strings.Fields(lines[1]); len(fields) < 4 || fields[0] != "dead" || strings.Join(fields[3:], " ") != "not ready" {
		t.Fatalf("dead row = %q, want not ready", lines[1])
	}
	if fields := strings.Fields(lines[2]); len(fields) < 3 || fields[0] != "live" || strings.Join(fields[3:], " ") != "ready" {
		t.Fatalf("live row = %q, want ready", lines[2])
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("API probed %d times, want once, for the live container only", got)
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

// TestPrintReadySummary checks the ready summary: five information lines
// — the cluster's name, its release, the API endpoint, the admin
// kubeconfig, and the context it selects — and no command suggestion;
// the login flow is the documentation's to teach.
func TestPrintReadySummary(t *testing.T) {
	out := captureStdout(t, func() {
		printReadySummary("/state/dev", "dev", "reg.example/ocp-release:4.22.8-x86_64", 6443)
	})
	want := "\nControl plane ready\n" +
		"  Name:         dev\n" +
		"  Release:      reg.example/ocp-release:4.22.8-x86_64\n" +
		"  API endpoint: https://127.0.0.1:6443\n" +
		"  Kubeconfig:   /state/dev/admin.kubeconfig\n" +
		"  Context:      admin@dev\n"
	if out != want {
		t.Fatalf("output =\n%s\nwant\n%s", out, want)
	}
}

// TestPrintWorkerSummary checks the joined worker's summary: it reports
// the node and its state, and a rootless worker states its limit as the
// fact alone, without coaching the remedy.
func TestPrintWorkerSummary(t *testing.T) {
	out := captureStdout(t, func() {
		printWorkerSummary("coreshift-worker-worker1", "/state/dev", "worker1", "container:coreshift-dev", true)
	})
	for _, want := range []string{
		"Worker joined\n",
		"  Node:       worker1, with the OVN-Kubernetes pods running on it\n",
		"  Kubeconfig: /state/dev/admin.kubeconfig\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output =\n%s\nwant it to contain %q", out, want)
		}
	}
	if strings.Contains(out, "sudo") || strings.Contains(out, "oc ") {
		t.Fatalf("output =\n%s\nwant no command suggestion", out)
	}
	if os.Geteuid() != 0 {
		limit := "  Limit:      workloads whose user IDs the worker's user namespace cannot express will not start\n"
		if !strings.Contains(out, limit) {
			t.Fatalf("output =\n%s\nwant it to contain %q", out, limit)
		}
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
	err := runDelete(context.Background(), "dev", false, false)
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

// TestRunDeleteStopsAndRemovesState checks the delete contract: the
// container is stopped with a timeout generous enough for the control
// plane's teardown and then removed, and the state directory goes with
// the cluster, leaving nothing behind — no state directory, no empty
// clusters tree. The state directory comes from the name; the
// container's port is never asked.
func TestRunDeleteStopsAndRemovesState(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	setContainers(t, "coreshift-dev")
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, "dev")

	var delErr error
	out := captureStdout(t, func() { delErr = runDelete(context.Background(), "dev", false, false) })
	if delErr != nil {
		t.Fatal(delErr)
	}
	calls := podmanCalls(t)
	want := []string{
		"container exists coreshift-dev",
		"ps -a --format {{.Names}}",
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
		t.Fatal("delete left the state directory behind")
	}
	if _, err := os.Stat(filepath.Join(base, "coreshift")); !os.IsNotExist(err) {
		t.Fatal("delete left an empty state root behind")
	}
}

// TestRunDeleteNotRunningRemovesState checks the delete of a cluster whose
// container is already gone: nothing is stopped, and the state is removed
// with the cluster reported not running.
func TestRunDeleteNotRunningRemovesState(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, "dev")

	var delErr error
	out := captureStdout(t, func() { delErr = runDelete(context.Background(), "dev", false, false) })
	if delErr != nil {
		t.Fatal(delErr)
	}
	want := []string{
		"container exists coreshift-dev",
		"ps -a --format {{.Names}}",
		"ps -a --format {{.Names}}",
	}
	if calls := podmanCalls(t); !slices.Equal(calls, want) {
		t.Fatalf("podman calls = %q, want only the existence check and the worker listings", calls)
	}
	if !strings.Contains(out, "not running") || !strings.Contains(out, "State removed at "+stateDir) {
		t.Fatalf("output = %q, want the cluster reported not running with its removed state named", out)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("delete left the state directory behind")
	}
}

// TestRunDeletePreserveStateKeepsState checks the preserve-state flag's
// contract: the same stop-and-remove happens, while the state directory
// and its marker stay, with the preservation said plainly in the output.
func TestRunDeletePreserveStateKeepsState(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	setContainers(t, "coreshift-dev")
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, "dev")

	var delErr error
	out := captureStdout(t, func() { delErr = runDelete(context.Background(), "dev", true, false) })
	if delErr != nil {
		t.Fatal(delErr)
	}
	calls := podmanCalls(t)
	want := []string{
		"container exists coreshift-dev",
		"ps -a --format {{.Names}}",
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

// TestRunDeleteMissingCluster checks that deleting a missing cluster is
// refused plainly, with nothing stopped or removed, with and without the
// preserve-state flag.
func TestRunDeleteMissingCluster(t *testing.T) {
	// The state home is the test's own, so the refusal names none of
	// the machine's real clusters.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, preserveState := range []bool{false, true} {
		writeFakePodman(t)
		err := runDelete(context.Background(), "nope", preserveState, false)
		if err == nil || !strings.Contains(err.Error(), "no cluster named nope") {
			t.Fatalf("preserveState=%v: err = %v, want a plain refusal", preserveState, err)
		}
		if calls := podmanCalls(t); len(calls) != 1 || calls[0] != "container exists coreshift-nope" {
			t.Fatalf("preserveState=%v: podman calls = %q, want only the existence check", preserveState, calls)
		}
	}
}

// writeWorkerMaterial stands up a cluster state directory holding one
// worker's join material, the record an unprivileged side has of a
// rootful worker.
func writeWorkerMaterial(t *testing.T, stateDir, worker string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(stateDir, workersDirName, worker), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestRunDeleteNamesSudoForWorkers checks the invoker's-act rule on the
// cluster side: a cluster whose state records workers is not deleted
// unprivileged, because a rootful worker's container lives in root's
// store; the command refuses naming sudo before touching anything, with
// the workers preserved or not.
func TestRunDeleteNamesSudoForWorkers(t *testing.T) {
	for _, preserveWorkers := range []bool{false, true} {
		writeFakePodman(t)
		writeFakeSudo(t)
		base := t.TempDir()
		t.Setenv("XDG_STATE_HOME", base)
		stateDir, err := clusterDir("dev")
		if err != nil {
			t.Fatal(err)
		}
		writeStateTree(t, stateDir, "dev")
		writeWorkerMaterial(t, stateDir, "worker1")

		err = runDelete(context.Background(), "dev", false, preserveWorkers)
		if err == nil || !strings.Contains(err.Error(), "sudo") {
			t.Fatalf("preserveWorkers=%v: err = %v, want a refusal naming sudo", preserveWorkers, err)
		}
		if calls := podmanCalls(t); len(calls) != 0 {
			t.Fatalf("preserveWorkers=%v: podman calls = %q, want nothing touched", preserveWorkers, calls)
		}
		if _, err := os.Stat(stateDir); err != nil {
			t.Fatalf("preserveWorkers=%v: the refusal path removed the state", preserveWorkers)
		}
	}
}

// TestRunDeleteWithoutWorkersStaysUnelevated checks that a cluster
// without workers deletes without asking for elevation: no material,
// nothing rootful to touch.
func TestRunDeleteWithoutWorkersStaysUnelevated(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, "dev")

	var delErr error
	captureStdout(t, func() { delErr = runDelete(context.Background(), "dev", false, false) })
	if delErr != nil {
		t.Fatal(delErr)
	}
	// The non-prompting read of root's store may happen — that is how
	// the deletion looks for rootful workers — but every sudo call
	// stays a read: the tool never escalates itself.
	for _, call := range sudoCalls(t) {
		if !strings.HasPrefix(call, "sudo -n ") {
			t.Fatalf("sudo calls = %q, want only non-prompting reads", sudoCalls(t))
		}
	}
}

// TestRunDeleteCascadesWorkers checks the cascade: deleting a cluster
// deletes each of its workers the way delete worker does, containers
// stopped and removed, before the cluster's own container goes. Another
// cluster's workers are left alone.
func TestRunDeleteCascadesWorkers(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
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
	out := captureStdout(t, func() { delErr = runDelete(context.Background(), "dev", false, false) })
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
		"State removed at " + stateDir,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q named", out, want)
		}
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("delete left the cluster's state behind")
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
		"container exists coreshift-worker-w1",
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

// TestRunDeleteWorkerRemovesMaterial checks the delete contract's state
// side: the join material directory goes with the worker's container
// even when the cluster is not running to remove the node, so nothing
// of the worker stays in the cluster's state tree.
func TestRunDeleteWorkerRemovesMaterial(t *testing.T) {
	writeFakePodman(t)
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	setContainers(t, "coreshift-worker-w1")
	setContainerLabel(t, "coreshift-worker-w1", "dev")
	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, "dev")
	writeWorkerMaterial(t, stateDir, "w1")

	var delErr error
	out := captureStdout(t, func() { delErr = runDeleteWorker(context.Background(), "w1") })
	if delErr != nil {
		t.Fatal(delErr)
	}
	if _, err := os.Stat(filepath.Join(stateDir, workersDirName, "w1")); !os.IsNotExist(err) {
		t.Fatal("delete left the worker's join material behind")
	}
	for _, want := range []string{"Deleted worker w1", "not running"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q named", out, want)
		}
	}
}

// TestRunDeletePreserveWorkersLeavesWorkers checks the flag's contract:
// the cluster's container goes, its workers stay untouched, and the state
// is preserved with them — the flag implies it, since the workers could
// never rejoin a cluster re-created without its state — so they reconnect
// when the cluster returns.
func TestRunDeletePreserveWorkersLeavesWorkers(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	setContainers(t, "coreshift-dev", "coreshift-worker-worker1", "coreshift-worker-worker2")
	setContainerLabel(t, "coreshift-worker-worker1", "dev")
	setContainerLabel(t, "coreshift-worker-worker2", "dev")
	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	writeStateTree(t, stateDir, "dev")

	var delErr error
	out := captureStdout(t, func() { delErr = runDelete(context.Background(), "dev", false, true) })
	if delErr != nil {
		t.Fatal(delErr)
	}
	calls := podmanCalls(t)
	want := []string{
		"container exists coreshift-dev",
		"ps -a --format {{.Names}}",
		`inspect --format {{index .Config.Labels "coreshift-cluster"}} coreshift-worker-worker1`,
		`inspect --format {{index .Config.Labels "coreshift-cluster"}} coreshift-worker-worker2`,
		"ps -a --format {{.Names}}",
		"stop --timeout 120 coreshift-dev",
		"rm coreshift-dev",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("podman calls = %q\nwant %q", calls, want)
	}
	for _, want := range []string{
		"Workers left running: worker1, worker2",
		"State preserved at " + stateDir,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q named", out, want)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, clusterMarkerFilename)); err != nil {
		t.Fatal("preserve-workers removed the state directory")
	}
}

// TestDeleteClusterFlagDefaults checks that the delete command comes with
// the documented defaults, and that the retired cleanup flag is gone.
func TestDeleteClusterFlagDefaults(t *testing.T) {
	cmd := newDeleteClusterCommand()
	for name, want := range map[string]string{
		"name":             "coreshift",
		"preserve-state":   "false",
		"preserve-workers": "false",
	} {
		if flag := cmd.Flags().Lookup(name); flag == nil {
			t.Fatalf("flag --%s is not defined", name)
		} else if flag.DefValue != want {
			t.Fatalf("--%s default = %q, want %q", name, flag.DefValue, want)
		}
	}
	if flag := cmd.Flags().Lookup("cleanup"); flag != nil {
		t.Fatal("flag --cleanup is gone: delete removes the state by default")
	}
}

// TestCreateWorkerFlagDefaults checks that the worker creation flags are
// accepted and come with the documented defaults — the name has none,
// because a bare creation counts it. The rootless launch is the bare
// default; the retired flag is gone.
func TestCreateWorkerFlagDefaults(t *testing.T) {
	cmd := newCreateWorkerCommand()
	for name, want := range map[string]string{
		"name":    "",
		"cluster": "coreshift",
		"verbose": "false",
	} {
		if flag := cmd.Flags().Lookup(name); flag == nil {
			t.Fatalf("flag --%s is not defined", name)
		} else if flag.DefValue != want {
			t.Fatalf("--%s default = %q, want %q", name, flag.DefValue, want)
		}
	}
	if flag := cmd.Flags().Lookup("rootless"); flag != nil {
		t.Fatal("flag --rootless is gone: the rootless launch is the default")
	}
	if usage := cmd.Flags().Lookup("name").Usage; !strings.Contains(usage, "count") {
		t.Fatalf("--name usage = %q, want the counting default described", usage)
	}
	if flag := cmd.Flags().Lookup("pull-secret"); flag != nil {
		t.Fatal("flag --pull-secret is gone: the cluster's stored secret is the only source")
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
// container for a host-mode cluster: its name and cluster label, the
// host network it dials the control plane on, the privilege its cgroup
// hierarchy needs, the join material and pull secret mounted read-only
// where the entrypoint and runtime read them, and the host's kernel
// modules read-only.
func TestWorkerRunArgs(t *testing.T) {
	got := workerRunArgs("coreshift-worker-w1", "dev", "/state/api/workers/w1", "localhost/coreshift-worker:v1", "", false)
	want := []string{
		"run", "--detach",
		"--name", "coreshift-worker-w1",
		"--label", "coreshift-cluster=dev",
		"--network", "host",
		"--privileged",
		"--volume", "/state/api/workers/w1/bootstrap-kubeconfig:/etc/kubernetes/bootstrap-kubeconfig:ro,z",
		"--volume", "/state/api/workers/w1/node-name:/etc/coreshift/node-name:ro,z",
		"--volume", "/state/api/workers/w1/pull-secret:/etc/coreshift/pull-secret:ro,z",
		"--volume", "/lib/modules:/lib/modules:ro",
		"localhost/coreshift-worker:v1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("podman args = %q\nwant %q", got, want)
	}
}

// TestWorkerRunArgsContainerMode checks the podman command line that
// starts a rootful worker joining a cluster in a container: it joins the
// cluster container's network namespace by its path — the worker runs
// rootful, the cluster container rootless, and the two podman stores
// cannot see each other's containers — and carries the marker that
// tells the worker its network is the tool's own to configure.
func TestWorkerRunArgsContainerMode(t *testing.T) {
	got := workerRunArgs("coreshift-worker-w1", "dev", "/state/api/workers/w1", "localhost/coreshift-worker:v1", "ns:/proc/4242/ns/net", true)
	for i, arg := range got {
		switch arg {
		case "--network":
			if got[i+1] != "ns:/proc/4242/ns/net" {
				t.Fatalf("worker network = %q, want the cluster container's namespace by path", got[i+1])
			}
		case "--volume":
			if got[i+1] == "/state/api/workers/w1/network-isolated:/etc/coreshift/network-isolated:ro,z" {
				return
			}
		}
	}
	t.Fatalf("podman args = %q, want the network marker mounted", got)
}

// TestWorkerRunArgsRootlessContainerMode checks the podman command line
// that starts a rootless worker joining a cluster in a container: both
// containers live in the user's one store, so the join is by the
// cluster container's name, with no path gymnastics.
func TestWorkerRunArgsRootlessContainerMode(t *testing.T) {
	got := workerRunArgs("coreshift-worker-w1", "dev", "/state/api/workers/w1", "localhost/coreshift-worker:v1", "container:coreshift-dev", true)
	for i, arg := range got {
		switch arg {
		case "--network":
			if got[i+1] != "container:coreshift-dev" {
				t.Fatalf("worker network = %q, want the cluster container by name", got[i+1])
			}
		case "--volume":
			if got[i+1] == "/state/api/workers/w1/network-isolated:/etc/coreshift/network-isolated:ro,z" {
				return
			}
		}
	}
	t.Fatalf("podman args = %q, want the network marker mounted", got)
}

// TestStdinIsTerminalOnPipe checks the fresh-start question's terminal
// detection: a pipe on standard input is not a terminal, so no question
// is ever asked on one.
func TestStdinIsTerminalOnPipe(t *testing.T) {
	noTerminal(t)
	if stdinIsTerminal() {
		t.Fatal("a pipe on standard input must not count as a terminal")
	}
}

// TestInvokingUserHomes checks the sudo-side home resolution: the homes
// come from the invoking user's own entry, so a run under sudo reads
// the same state and cache the bare one does, and an unknown user is
// refused.
func TestInvokingUserHomes(t *testing.T) {
	uid := strconv.Itoa(os.Getuid())
	stateHome, cacheHome, err := invokingUserHomes(uid)
	if err != nil {
		t.Fatal(err)
	}
	invoking, err := user.LookupId(uid)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(invoking.HomeDir, ".local", "state"); stateHome != want {
		t.Fatalf("state home = %q, want %q", stateHome, want)
	}
	if want := filepath.Join(invoking.HomeDir, ".cache"); cacheHome != want {
		t.Fatalf("cache home = %q, want %q", cacheHome, want)
	}
	if _, _, err := invokingUserHomes("no-such-uid"); err == nil {
		t.Fatal("expected an error for an unknown user")
	}
}

// TestCreateWorkerBareStaysUnelevated checks the default's promise: a
// bare worker creation asks for no elevation at all, running in the
// user's own podman, so with every way to elevate refused it still
// proceeds to the cluster resolution. The state home is the test's
// own, so that resolution cannot reach a live default-named cluster.
func TestCreateWorkerBareStaysUnelevated(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	noTerminal(t)
	refusal := filepath.Join(t.TempDir(), "no-passwordless")
	if err := os.WriteFile(refusal, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUDO_REFUSE", refusal)

	root := newRootCommand()
	root.SetArgs([]string{"create", "worker"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "no cluster named coreshift") {
		t.Fatalf("err = %v, want the cluster resolution", err)
	}
	if sudo := sudoCalls(t); len(sudo) != 0 {
		t.Fatalf("sudo calls = %q, want none: the rootless worker needs no elevation", sudo)
	}
}

// TestClusterNetworkNamespace checks the namespace path a worker joins:
// the cluster container's main process, whose namespace root joins by
// path because the two podman stores cannot see each other's
// containers. A stopped container has no process left to join.
func TestClusterNetworkNamespace(t *testing.T) {
	writeFakePodman(t)
	setContainers(t, "coreshift-dev")
	setContainerPID(t, "coreshift-dev", 4242)

	got, err := clusterNetworkNamespace("dev")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/proc/4242/ns/net" {
		t.Fatalf("namespace = %q, want the cluster container's by path", got)
	}

	setContainerPID(t, "coreshift-dev", 0)
	if _, err := clusterNetworkNamespace("dev"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err = %v, want the stopped cluster refused", err)
	}
}

// TestRunCreateWorkerRefusals checks the refusals that must arrive before
// anything is minted: a name the objects cannot carry, a taken worker
// name, an unknown cluster, and a cluster whose API is not answering.
// None needs a pull secret flag, since the refusals precede its use.
// The state home is the test's own, so the refusals read no real
// cluster state.
func TestRunCreateWorkerRefusals(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
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

		err := runCreateWorker(context.Background(), tc.worker, true, tc.cluster, false)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want a refusal containing %q", name, err, tc.want)
		}
		// A refusal before the first podman call leaves no call log.
		if calls, err := os.ReadFile(os.Getenv("PODMAN_LOG")); err == nil && strings.Contains(string(calls), "build") {
			t.Fatalf("%s: an image was minted despite the refusal", name)
		}
	}
}

// TestNextWorkerName checks the counting rule a bare worker creation
// names by: the number is one past the cluster's node count — not the
// first free slot — and skips upward past any name already in use, as a
// node or as a container, so gaps left by deletions never cause a
// collision. The nodes are the truth across both worker kinds, since a
// cluster may hold either.
func TestNextWorkerName(t *testing.T) {
	for name, tc := range map[string]struct {
		nodes      []string
		containers []string
		want       string
	}{
		"fresh cluster": {
			want: "worker1",
		},
		"one worker named foo": {
			nodes: []string{"foo"},
			want:  "worker2",
		},
		"three nodes count to the fourth": {
			nodes: []string{"worker1", "worker2", "worker3"},
			want:  "worker4",
		},
		"deleted middle leaves the number skipped": {
			nodes: []string{"worker1", "worker3"},
			want:  "worker4",
		},
		"a name taken by a container is skipped": {
			nodes:      []string{"worker1"},
			containers: []string{"coreshift-worker-worker2"},
			want:       "worker3",
		},
	} {
		writeFakePodman(t)
		setContainers(t, tc.containers...)

		if got := nextWorkerName(tc.nodes); got != tc.want {
			t.Fatalf("%s: name = %q, want %q", name, got, tc.want)
		}
	}
}

// writeClusterNodes stands up a TLS stand-in for a cluster's API serving
// the node list — the given names — and its health endpoint, and writes
// an admin kubeconfig pointing at it into the state directory, the way a
// real cluster's state directory holds one. The stand-in's own
// certificate is the kubeconfig's authority, so the readiness probe and
// the API clients trust it alike, and the fake cluster container
// publishes the stand-in's port.
func writeClusterNodes(t *testing.T, stateDir, cluster string, names ...string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		items := make([]corev1.Node, 0, len(names))
		for _, name := range names {
			items = append(items, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
		}
		_ = json.NewEncoder(w).Encode(corev1.NodeList{Items: items})
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	config := clientcmdapi.NewConfig()
	config.Clusters["coreshift"] = &clientcmdapi.Cluster{
		Server:                   server.URL,
		CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
	}
	config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{}
	config.Contexts["admin@coreshift"] = &clientcmdapi.Context{Cluster: "coreshift", AuthInfo: "admin"}
	config.CurrentContext = "admin@coreshift"
	if err := clientcmd.WriteToFile(*config, filepath.Join(stateDir, certgen.AdminKubeconfigFilename)); err != nil {
		t.Fatal(err)
	}
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(address.Port())
	if err != nil {
		t.Fatal(err)
	}
	setContainerPort(t, clusterContainerPrefix+cluster, port)
}

// TestRunCreateWorkerBarePrintsCountedName checks that a bare creation
// picks the counted name from the cluster's nodes and prints it, so the
// command is never mysterious about what it is creating.
func TestRunCreateWorkerBarePrintsCountedName(t *testing.T) {
	writeFakePodman(t)
	setContainers(t, "coreshift-nope")
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir, err := clusterDir("nope")
	if err != nil {
		t.Fatal(err)
	}
	writeClusterNodes(t, stateDir, "nope", "worker1")

	var runErr error
	out := captureStdout(t, func() {
		runErr = runCreateWorker(context.Background(), "", false, "nope", false)
	})
	// The stand-in cluster serves no join endpoints, so the mint that
	// follows the pick fails; the pick itself is what is tested.
	if runErr == nil {
		t.Fatal("want the join to fail against the stand-in cluster")
	}
	if !strings.Contains(out, "Worker name: worker2") {
		t.Fatalf("output = %q, want the counted name printed prominently", out)
	}
}

// TestWorkerRunArgsSecondWorkerBesideNetwork checks the podman command
// line that starts a worker joining beside the one that runs the
// cluster's network: it still joins the cluster container's namespace,
// but carries no marker, so it reconfigures no interfaces.
func TestWorkerRunArgsSecondWorkerBesideNetwork(t *testing.T) {
	got := workerRunArgs("coreshift-worker-w2", "dev", "/state/api/workers/w2", "localhost/coreshift-worker:v1", "container:coreshift-dev", false)
	for i, arg := range got {
		switch arg {
		case "--network":
			if got[i+1] != "container:coreshift-dev" {
				t.Fatalf("worker network = %q, want the cluster container by name", got[i+1])
			}
		case "--volume":
			if got[i+1] == "/state/api/workers/w2/network-isolated:/etc/coreshift/network-isolated:ro,z" {
				t.Fatalf("podman args = %q, want no network marker beside the first worker", got)
			}
		}
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

// TestWorkerPullSecretReadsClusterSecret checks that the Secret the
// cluster stores becomes both the build's credentials and its throwaway
// auth file, which the cleanup removes.
func TestWorkerPullSecretReadsClusterSecret(t *testing.T) {
	secret := []byte(`{"auths":{"quay.io":{"auth":"dXNlcjpwYXNz"}}}`)
	stateDir := t.TempDir()
	writeClusterPullSecret(t, stateDir, secret)

	pullSecret, authfile, cleanup, err := workerPullSecret(context.Background(), stateDir)
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
// plainly, with the root side read without a prompt: nothing there and
// nothing here means no workers, and no note.
func TestRunGetWorkersEmpty(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	var err error
	out := captureStdout(t, func() { err = runGetWorkers() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No workers") {
		t.Fatalf("output = %q, want the empty list reported", out)
	}
	if strings.Contains(out, "elevation") {
		t.Fatalf("output = %q, want no elevation note: the root side was read", out)
	}
}

// TestRunGetWorkersListsWorkers checks the row each worker gets: its
// name, the cluster its container is labeled with, its kind, and
// readiness from its container running and its node's live condition
// agreeing, so a running worker without a reachable cluster shows as
// not ready and one without a label shows no cluster. A worker in the
// user's own store is the rootless kind.
func TestRunGetWorkersListsWorkers(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	setContainers(t, "coreshift-dev", "coreshift-worker-w1", "coreshift-worker-w2")
	setContainerPort(t, "coreshift-dev", 7443)
	setContainerLabel(t, "coreshift-worker-w1", "dev")
	setContainerRunning(t, "coreshift-worker-w1", true)

	var err error
	out := captureStdout(t, func() { err = runGetWorkers() })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("output has %d lines, want a header and two workers:\n%s", len(lines), out)
	}
	if fields := strings.Fields(lines[1]); len(fields) < 4 || fields[0] != "w1" || fields[1] != "dev" || fields[2] != "rootless" || strings.Join(fields[3:], " ") != "not ready" {
		t.Fatalf("w1 row = %q, want its name, cluster, rootless kind, and not ready", lines[1])
	}
	if fields := strings.Fields(lines[2]); len(fields) < 4 || fields[0] != "w2" || fields[1] != "-" || fields[2] != "rootless" || strings.Join(fields[3:], " ") != "not ready" {
		t.Fatalf("w2 row = %q, want its name, no cluster, rootless kind, and not ready", lines[2])
	}
}

// TestRunGetWorkersListsRootfulWorkers checks the split the listing is
// honest about: rootful worker containers live in root's store, read
// through the non-interactive elevation, and a worker only that side
// can see is listed as the rootful kind, with its row read the same
// way.
func TestRunGetWorkersListsRootfulWorkers(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	setContainers(t, "coreshift-dev")
	setRootfulContainers(t, "coreshift-worker-w1")
	setContainerPort(t, "coreshift-dev", 7443)
	setContainerLabel(t, "coreshift-worker-w1", "dev")
	setContainerRunning(t, "coreshift-worker-w1", true)

	var err error
	out := captureStdout(t, func() { err = runGetWorkers() })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("output has %d lines, want a header and the rootful worker:\n%s", len(lines), out)
	}
	if fields := strings.Fields(lines[1]); len(fields) < 4 || fields[0] != "w1" || fields[1] != "dev" || fields[2] != "rootful" || strings.Join(fields[3:], " ") != "not ready" {
		t.Fatalf("w1 row = %q, want its name, cluster, rootful kind, and not ready", lines[1])
	}
	// The row was read through the root side: the label read went
	// through elevation, not plain podman. (The fake podman logs both
	// paths alike; the sudo log is the one that tells them apart.)
	if sudo := sudoCalls(t); !slices.ContainsFunc(sudo, func(call string) bool {
		return strings.Contains(call, `inspect --format {{index .Config.Labels "coreshift-cluster"}} coreshift-worker-w1`)
	}) {
		t.Fatalf("sudo calls = %q, want the rootful worker's label read elevated", sudo)
	}
	if strings.Contains(out, "elevation") {
		t.Fatalf("output = %q, want no elevation note: the root side was read", out)
	}
}

// TestRunGetWorkersListsBothKinds checks the row each kind of one
// cluster's workers gets: a cluster may hold rootless and rootful
// workers at once, and the listing shows both, each labeled with its
// kind.
func TestRunGetWorkersListsBothKinds(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	setContainers(t, "coreshift-dev", "coreshift-worker-worker1")
	setRootfulContainers(t, "coreshift-worker-worker2")
	setContainerPort(t, "coreshift-dev", 7443)
	setContainerLabel(t, "coreshift-worker-worker1", "dev")
	setContainerLabel(t, "coreshift-worker-worker2", "dev")

	var err error
	out := captureStdout(t, func() { err = runGetWorkers() })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("output has %d lines, want a header and both workers:\n%s", len(lines), out)
	}
	if fields := strings.Fields(lines[1]); fields[0] != "worker1" || fields[1] != "dev" || fields[2] != "rootless" {
		t.Fatalf("worker1 row = %q, want the rootless kind labeled", lines[1])
	}
	if fields := strings.Fields(lines[2]); fields[0] != "worker2" || fields[1] != "dev" || fields[2] != "rootful" {
		t.Fatalf("worker2 row = %q, want the rootful kind labeled", lines[2])
	}
}

// TestRunGetWorkersNotesRefusedElevation checks the fallback's honesty:
// when the non-interactive read of root's store is refused — no cached
// credentials — the listing reports only what the unprivileged side
// can see and says plainly that rootful workers need elevation.
func TestRunGetWorkersNotesRefusedElevation(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	refusal := filepath.Join(t.TempDir(), "refused")
	if err := os.WriteFile(refusal, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUDO_REFUSE", refusal)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var err error
	out := captureStdout(t, func() { err = runGetWorkers() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No workers") {
		t.Fatalf("output = %q, want the empty list reported", out)
	}
	if !strings.Contains(out, "needs elevation") {
		t.Fatalf("output = %q, want the elevation note", out)
	}
}

// TestRunGetWorkersStoppedContainerNotReady checks the row a stopped
// worker gets: not ready, without probing, even when its node's
// condition still reads ready in the cluster's API, which is the shape
// a just-stopped worker leaves behind until the cluster notices.
func TestRunGetWorkersStoppedContainerNotReady(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)

	// The cluster's API reports every node ready, stopped ones included.
	var mu sync.Mutex
	probes := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/nodes/")
		mu.Lock()
		probes[name]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(corev1.Node{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			}}},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	stateDir, err := clusterDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := clientcmdapi.NewConfig()
	config.Clusters["dev"] = &clientcmdapi.Cluster{Server: server.URL}
	config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{}
	config.Contexts["admin@dev"] = &clientcmdapi.Context{Cluster: "dev", AuthInfo: "admin"}
	config.CurrentContext = "admin@dev"
	if err := clientcmd.WriteToFile(*config, filepath.Join(stateDir, certgen.AdminKubeconfigFilename)); err != nil {
		t.Fatal(err)
	}

	setContainers(t, "coreshift-dev", "coreshift-worker-w1", "coreshift-worker-w3")
	setContainerPort(t, "coreshift-dev", 7443)
	setContainerLabel(t, "coreshift-worker-w1", "dev")
	setContainerLabel(t, "coreshift-worker-w3", "dev")
	setContainerRunning(t, "coreshift-worker-w1", true)

	var outErr error
	out := captureStdout(t, func() { outErr = runGetWorkers() })
	if outErr != nil {
		t.Fatal(outErr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("output has %d lines, want a header and two workers:\n%s", len(lines), out)
	}
	if fields := strings.Fields(lines[1]); len(fields) < 3 || fields[0] != "w1" || strings.Join(fields[3:], " ") != "ready" {
		t.Fatalf("w1 row = %q, want ready", lines[1])
	}
	if fields := strings.Fields(lines[2]); len(fields) < 4 || fields[0] != "w3" || strings.Join(fields[3:], " ") != "not ready" {
		t.Fatalf("w3 row = %q, want not ready", lines[2])
	}
	mu.Lock()
	defer mu.Unlock()
	if probes["w3"] != 0 {
		t.Fatalf("the stopped worker's node was probed %d times, want none", probes["w3"])
	}
	if probes["w1"] != 1 {
		t.Fatalf("the running worker's node was probed %d times, want once", probes["w1"])
	}
}

// TestRunDeleteWorkerStopsAndRemoves checks the delete contract: the
// container is stopped with the same teardown time the cluster's gets
// and then removed, its cluster read from its label — all through the
// user's own store, with no elevation anywhere. The state home is the
// test's own: the label names the default cluster, whose real state
// the node removal must never reach.
func TestRunDeleteWorkerStopsAndRemoves(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
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
		"container exists coreshift-worker-w1",
		"stop --timeout 120 coreshift-worker-w1",
		"rm coreshift-worker-w1",
	}
	// The cluster resolution trails the removal; only the head of the
	// call sequence is the delete itself.
	if !slices.Equal(calls[:len(want)], want) {
		t.Fatalf("podman calls = %q\nwant %q at the head", calls, want)
	}
	if sudo := sudoCalls(t); len(sudo) != 0 {
		t.Fatalf("sudo calls = %q, want none: a rootless worker deletes unelevated", sudo)
	}
}

// TestRunDeleteWorkerRootfulNeedsSudo checks the invoker's-act rule: a
// worker the invoking user's podman cannot see — rootful, or not there
// at all — is not deleted unprivileged; the command refuses naming sudo
// and touches nothing beyond the user-store existence check.
func TestRunDeleteWorkerRootfulNeedsSudo(t *testing.T) {
	writeFakePodman(t)
	writeFakeSudo(t)

	var err error
	captureStdout(t, func() { err = runDeleteWorker(context.Background(), "w1") })
	if err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("err = %v, want a refusal naming sudo", err)
	}
	if calls := podmanCalls(t); len(calls) != 1 || calls[0] != "container exists coreshift-worker-w1" {
		t.Fatalf("podman calls = %q, want only the user-store existence check", calls)
	}
}

// TestWorkerFailureQuotesLogAndRemoves checks the failure contract: the
// error carries the container's log tail, a join that failed on the
// cgroup hierarchy gains the rootful remedy, and the container is
// removed.
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
		"under sudo",
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
