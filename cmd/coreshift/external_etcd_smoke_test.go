package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/vkareh/coreshift/internal/certgen"
	"github.com/vkareh/coreshift/internal/warnings"
	"github.com/vkareh/coreshift/pkg/image"
)

// The pull secret used for live verification against the real registry;
// CI overrides it with SMOKE_PULL_SECRET_PATH. The test skips when the
// secret is absent, so environments without it still pass. Run it with a
// generous timeout, like the image smoke tests: a cold cache pulls the
// release payload before anything answers.
func smokePullSecretPath() string {
	if p := os.Getenv("SMOKE_PULL_SECRET_PATH"); p != "" {
		return p
	}
	return "../pull-secret"
}

// TestExternalEtcdSmoke boots the control plane against an external etcd:
// the payload's own etcd binary run as a separate process on localhost
// with TLS material the test generates, the internal etcd absent from
// the supervisor, the cluster's data landing in the external etcd, a
// user logged in through the htpasswd flow across a warm reboot, and a
// run in the other etcd mode refused against the recorded state
// directory. The state home is the test's own, so a running cluster's
// state is untouched; the image cache stays shared, the way a second
// cluster shares it.
func TestExternalEtcdSmoke(t *testing.T) {
	pullSecretPath := smokePullSecretPath()
	pullSecret, err := os.ReadFile(pullSecretPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("pull secret not found at %s", pullSecretPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("htpasswd"); err != nil {
		t.Skipf("the login probe needs the htpasswd tool: %v", err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()

	// One release resolution feeds both the etcd binary the test
	// extracts and the control plane the test boots.
	client, err := image.NewClient(pullSecret)
	if err != nil {
		t.Fatal(err)
	}
	arch, err := releaseArch(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	releaseRef, err := client.ResolveRelease(ctx, defaultReleaseRepository, "", arch, "")
	if err != nil {
		t.Fatal(err)
	}
	releaseRoot, err := client.CachedRelease(ctx, releaseRef)
	if err != nil {
		t.Fatal(err)
	}
	etcdBin, etcdctlBin := extractEtcdBinaries(t, ctx, client, releaseRoot, dir)
	release := releaseOptions{image: releaseRef}

	caFile, serverCert, serverKey, clientCert, clientKey := writeEtcdTLS(t, dir)
	endpoint := "https://127.0.0.1:" + strconv.Itoa(freePort(t))
	startExternalEtcd(t, etcdBin, endpoint, dir, serverCert, serverKey, caFile)
	waitEtcdHealthy(t, etcdHTTPClient(t, caFile, clientCert, clientKey), endpoint)

	etcd := etcdOptions{
		endpoints: endpoint,
		caFile:    caFile,
		certFile:  clientCert,
		keyFile:   clientKey,
	}
	const name = "external-etcd"
	port := freePort(t)
	stateDir, err := clusterDir(name)
	if err != nil {
		t.Fatal(err)
	}
	// A boot is stopped by canceling its context and waiting for the
	// supervisor's teardown; the cleanup runs the same stop however the
	// test ends, so the children never outlive it. Stopping a boot that
	// already ended returns how it ended, and stopping twice is safe.
	var stopControlPlane func() error
	t.Cleanup(func() { _ = stopControlPlane() })
	bootControlPlane := func() <-chan struct{} {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		var runErr error
		go func() {
			runErr = runAPI(ctx, pullSecretPath, name, port, false, release, etcd)
			close(done)
		}()
		stopControlPlane = func() error {
			cancel()
			<-done
			return runErr
		}
		return done
	}

	// The first boot.
	done := bootControlPlane()
	waitControlPlaneReady(t, stateDir, done, stopControlPlane)

	// The internal etcd is absent from the supervisor's list: it has no
	// log file, and no data directory was created for it.
	for _, path := range []string{
		filepath.Join(stateDir, "logs", "etcd.log"),
		filepath.Join(stateDir, "etcd-data"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the internal etcd ran although an external one was configured: %s exists", path)
		}
	}

	// The cluster's data landed in the external etcd: keys under
	// /registry, read with etcdctl presenting the control plane's own
	// client certificate.
	keys := etcdctlKeys(t, etcdctlBin, etcd, "/registry/")
	if len(keys) == 0 {
		t.Fatal("the external etcd holds no keys under /registry")
	}

	// The login material, the operations guide's flow, arranged now so
	// the warm reboot below renders it into the OAuth server.
	seedLoginUser(t, filepath.Join(stateDir, certgen.AdminKubeconfigFilename), dir)

	// The warm reboot: same state, same external etcd, which also proves
	// the login material survived it there.
	if err := stopControlPlane(); err != nil {
		t.Fatalf("the first boot did not shut down cleanly: %v", err)
	}
	done = bootControlPlane()
	waitControlPlaneReady(t, stateDir, done, stopControlPlane)
	loginProbe(t, stateDir, port)
	if err := stopControlPlane(); err != nil {
		t.Fatalf("the second boot did not shut down cleanly: %v", err)
	}

	// The state directory refuses the other etcd mode, naming both.
	err = runAPI(context.Background(), pullSecretPath, name, port, false, release, etcdOptions{})
	if err == nil {
		t.Fatal("expected a refusal for the other etcd mode")
	}
	for _, want := range []string{
		"cannot be reused by",
		"the supervised internal etcd",
		"external etcd at " + endpoint,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q:\n%v", want, err)
		}
	}
}

// waitControlPlaneReady waits for a boot to answer a project listing
// through the aggregated OpenShift API — the drift alarm's probe — so a
// ready answer means the API path genuinely serves, not that a stale
// availability record survived the reboot. A boot that dies first fails
// the wait with its own error.
func waitControlPlaneReady(t *testing.T, stateDir string, done <-chan struct{}, stop func() error) {
	t.Helper()
	kubeconfig := filepath.Join(stateDir, certgen.AdminKubeconfigFilename)
	deadline := time.Now().Add(containerReadyTimeout)
	for _, err := os.Stat(kubeconfig); err != nil; _, err = os.Stat(kubeconfig) {
		select {
		case <-done:
			t.Fatalf("the control plane exited before becoming ready: %v", stop())
		case <-time.After(time.Second):
		}
		if time.Now().After(deadline) {
			t.Fatal("the admin kubeconfig never appeared")
		}
	}
	config, err := warnings.Config(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	projects := schema.GroupVersionResource{Group: "project.openshift.io", Version: "v1", Resource: "projects"}
	for {
		select {
		case <-done:
			t.Fatalf("the control plane exited before becoming ready: %v", stop())
		default:
		}
		if _, err := client.Resource(projects).List(context.Background(), metav1.ListOptions{}); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the aggregated OpenShift API never answered a project listing")
		}
		time.Sleep(time.Second)
	}
}

// writeEtcdTLS generates the external etcd's certificate material: a
// CA, the server certificate etcd serves, and the client certificate
// the control plane presents — what an external etcd's operator would
// issue, generated here so the shipped certificate machinery is
// untouched.
func writeEtcdTLS(t *testing.T, dir string) (ca, serverCert, serverKey, clientCert, clientKey string) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "external-etcd-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	writePair := func(name string, der []byte, key *rsa.PrivateKey) (string, string) {
		certPath := filepath.Join(dir, name+".crt")
		keyPath := filepath.Join(dir, name+".key")
		if err := os.WriteFile(certPath, pemEncode("CERTIFICATE", der), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, pemEncode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key)), 0o600); err != nil {
			t.Fatal(err)
		}
		return certPath, keyPath
	}
	ca, _ = writePair("etcd-ca", caDER, caKey)
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(name string, serial int64, template x509.Certificate) (string, string) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		template.SerialNumber = big.NewInt(serial)
		template.NotBefore = time.Now().Add(-time.Hour)
		template.NotAfter = time.Now().Add(24 * time.Hour)
		der, err := x509.CreateCertificate(rand.Reader, &template, caCertificate, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return writePair(name, der, key)
	}
	serverCert, serverKey = issue("etcd-server", 2, x509.Certificate{
		Subject:     pkix.Name{CommonName: "external-etcd"},
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	clientCert, clientKey = issue("etcd-client", 3, x509.Certificate{
		Subject:     pkix.Name{CommonName: "external-etcd-client"},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return ca, serverCert, serverKey, clientCert, clientKey
}

// pemEncode wraps DER bytes the way the certificate files are written.
func pemEncode(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

// startExternalEtcd runs the payload's own etcd binary as a separate
// process on localhost, serving TLS and verifying client certificates,
// the way an external etcd deployment serves.
func startExternalEtcd(t *testing.T, etcdBin, endpoint, dir, serverCert, serverKey, ca string) {
	t.Helper()
	log, err := os.Create(filepath.Join(dir, "etcd.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(etcdBin,
		"--data-dir="+filepath.Join(dir, "etcd-data"),
		"--listen-client-urls="+endpoint,
		"--advertise-client-urls="+endpoint,
		"--listen-peer-urls=http://127.0.0.1:"+strconv.Itoa(freePort(t)),
		"--cert-file="+serverCert,
		"--key-file="+serverKey,
		"--client-cert-auth",
		"--trusted-ca-file="+ca,
	)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = log.Close()
	})
}

// etcdHTTPClient builds the client the probes read the external etcd
// with: TLS-verified against its CA and authenticated with the control
// plane's client certificate.
func etcdHTTPClient(t *testing.T, caFile, certFile, keyFile string) *http.Client {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	caData, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		t.Fatal("the etcd CA certificate does not parse")
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}}},
	}
}

// waitEtcdHealthy waits for the external etcd to answer its health
// endpoint over TLS.
func waitEtcdHealthy(t *testing.T, client *http.Client, endpoint string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		resp, err := client.Get(endpoint + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = fmt.Errorf("status %s", resp.Status)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the external etcd never answered its health endpoint: %v", err)
		}
		time.Sleep(time.Second)
	}
}

// extractEtcdBinaries pulls the payload's etcd image and extracts its
// two binaries: the etcd the test runs externally, and the etcdctl the
// verification reads it with.
func extractEtcdBinaries(t *testing.T, ctx context.Context, client *image.Client, releaseRoot, dir string) (etcd, etcdctl string) {
	t.Helper()
	etcd, err := client.ExtractEtcd(ctx, releaseRoot)
	if err != nil {
		t.Fatal(err)
	}
	imageRef, err := image.ResolveComponentImage(releaseRoot, "etcd")
	if err != nil {
		t.Fatal(err)
	}
	filesystem := t.TempDir()
	if err := client.PullAndUnpack(ctx, imageRef, filesystem); err != nil {
		t.Fatal(err)
	}
	if err := image.ExtractBinary(filesystem, "/usr/bin/etcdctl", dir); err != nil {
		t.Fatal(err)
	}
	return etcd, filepath.Join(dir, "etcdctl")
}

// etcdctlKeys returns the keys etcdctl lists under the prefix, read with
// the control plane's own client certificate and CA.
func etcdctlKeys(t *testing.T, etcdctl string, etcd etcdOptions, prefix string) []string {
	t.Helper()
	out, err := exec.Command(etcdctl,
		"--endpoints="+etcd.endpoints,
		"--cacert="+etcd.caFile,
		"--cert="+etcd.certFile,
		"--key="+etcd.keyFile,
		"get", prefix, "--prefix", "--keys-only",
	).Output()
	if err != nil {
		t.Fatalf("list %s in the external etcd: %v\n%s", prefix, err, out)
	}
	var keys []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, prefix) {
			keys = append(keys, line)
		}
	}
	return keys
}

// seedLoginUser arranges the htpasswd login material through the API,
// the operations guide's flow: a user file, the secret carrying it, and
// the OAuth object naming it. The objects land once the version
// operator has applied the payload's copy of them, so the writes retry
// until they do. The material is rendered into the OAuth server at the
// next boot, like the guide's restart.
func seedLoginUser(t *testing.T, kubeconfigPath, dir string) {
	t.Helper()
	users := filepath.Join(dir, "users.htpasswd")
	if out, err := exec.Command("htpasswd", "-c", "-B", "-b", users, "smoke-user", "smoke-password").CombinedOutput(); err != nil {
		t.Fatalf("create the htpasswd file: %v\n%s", err, out)
	}
	data, err := os.ReadFile(users)
	if err != nil {
		t.Fatal(err)
	}
	config, err := warnings.Config(kubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "smoke-htpasswd", Namespace: "openshift-config"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"htpasswd": data},
	}
	patch := []byte(`{"spec":{"identityProviders":[{"name":"htpasswd","type":"HTPasswd","mappingMethod":"claim","htpasswd":{"fileData":{"name":"smoke-htpasswd"}}}]}}`)
	seed := func() error {
		if _, err := kubeClient.CoreV1().Secrets("openshift-config").Create(context.Background(), secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		_, err := dynamicClient.Resource(oauths).Patch(context.Background(), "cluster", types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		err := seed()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the login material never landed: %v", err)
		}
		time.Sleep(5 * time.Second)
	}
}

// oauths is the OAuth object the login material is arranged through.
var oauths = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "oauths"}

// loginProbe walks the login flow the drift alarm probes: the OAuth
// server's authorize endpoint authenticates the generated user and
// redirects with an access token, which names its holder through the
// user API.
func loginProbe(t *testing.T, stateDir string, port int) {
	t.Helper()
	caData, err := os.ReadFile(filepath.Join(stateDir, certgen.CACertFilename))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		t.Fatal("the cluster CA certificate does not parse")
	}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	query := url.Values{
		"client_id":     {"openshift-challenging-client"},
		"response_type": {"token"},
		"redirect_uri":  {apiServerURL(port) + "/oauth/token/implicit"},
	}
	authorize := fmt.Sprintf("https://127.0.0.1:%d/oauth/authorize?%s", oauthServerPort, query.Encode())

	// Up to two minutes: the OAuth server settles right around the
	// ready summary.
	var token string
	deadline := time.Now().Add(2 * time.Minute)
	for token == "" {
		req, err := http.NewRequest("GET", authorize, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("smoke-user", "smoke-password")
		resp, err := client.Do(req)
		if err != nil {
			retryLogin(t, deadline, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			retryLogin(t, deadline, fmt.Errorf("authorize answered %s", resp.Status))
			continue
		}
		location, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			retryLogin(t, deadline, err)
			continue
		}
		values, err := url.ParseQuery(location.Fragment)
		if err != nil {
			retryLogin(t, deadline, err)
			continue
		}
		if token = values.Get("access_token"); token == "" {
			retryLogin(t, deadline, fmt.Errorf("the redirect carries no token: %s", location))
		}
	}

	req, err := http.NewRequest("GET", apiServerURL(port)+"/apis/user.openshift.io/v1/users/~", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "smoke-user") {
		t.Fatalf("the token did not name its holder: %s\n%s", resp.Status, body)
	}
}

// retryLogin sleeps and lets the caller retry until the deadline, then
// fails the test; the OAuth server needs a moment after the ready
// summary.
func retryLogin(t *testing.T, deadline time.Time, err error) {
	t.Helper()
	if time.Now().After(deadline) {
		t.Fatalf("the login flow never completed: %v", err)
	}
	time.Sleep(5 * time.Second)
}
