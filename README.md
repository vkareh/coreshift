# CoreShift

CoreShift is a lightweight tool for running a real OpenShift control plane
without a full cluster install — no VM, no kubelet, no proprietary tooling
required to actually run it. The control plane (etcd, kube-apiserver,
kube-controller-manager, kube-scheduler, plus the full OpenShift API layer:
openshift-apiserver, oauth-apiserver, oauth-server,
openshift-controller-manager, cluster-policy-controller) is built from real,
version-matched upstream OpenShift components, not reimplementations.

CoreShift ships two ways to get a real control plane running: `coreshift api`,
a bare-process mode with no container runtime dependency, fast to boot,
control-plane-only — and `coreshift up`, a podman-based mode running the full
stack including a real joined worker, CNI, DNS, and ingress. Both are backed by
convenience up/down/status commands that apply, wait for the API to become
healthy, and tear down — useful for CI pipelines and local development where
a quick, disposable, real OpenShift API is needed for validation (RBAC/SCC
checks, API compatibility testing, admission/defaulting behavior) without the
cost and complexity of a full cluster.

| | `coreshift api` | `coreshift up` (podman) |
|---|---|---|
| Control plane | Real | Real |
| RBAC/SCC/admission | Real | Real |
| Runtime dependency | None (bare process) | podman/docker |
| Boot time | Seconds | Longer (real containers, real join) |
| Workloads actually run | No | Yes |
| Real node/CNI/DNS/ingress | No | Yes |
| Best for | CI gating, PR checks, operator dev loop | Local dev, demos, "does it really work" |

`coreshift render` sits underneath both — a scope-agnostic YAML artifact usable
by podman, kubectl, or other tooling.

## The `api` command

`coreshift api` runs a real OpenShift control plane as ordinary processes on
the host. The component binaries are extracted from a real upstream OpenShift
release image and started directly — no containers, no cluster install. etcd,
kube-apiserver, kube-controller-manager, and kube-scheduler run alongside the
full OpenShift API layer: openshift-apiserver, oauth-apiserver, oauth-server,
openshift-controller-manager, and cluster-policy-controller. Everything
listens on the host, and `oc` talks to the API server like it would to any
cluster.

### Prerequisites

- a Go toolchain (1.24 or newer) to build the `coreshift` binary
- a registry pull secret: the JSON file from the Red Hat Hybrid Cloud
  Console, with credentials for the release image registry

Deliberately absent: no podman, no docker, no cluster to install. Nothing on
the host is modified outside the directories listed below.

### Running it

```
make build
./coreshift api --pull-secret ~/pull-secret
```

The first run pulls the release image and its component images; later runs
start straight from the [cache](#cache). The run ends on Ctrl-C (or SIGTERM),
stopping the children in reverse startup order. State survives across runs:
certificates, etcd data, and everything created through the API are still
there the next time.

### Flags

- `--pull-secret` (required): path to the pull secret JSON file.
- `--port`: the port the API server listens on, 6443 by default. Use it when
  something else already occupies 6443. A non-default port gets its own state
  directory, but etcd and the other components always listen on the same
  fixed ports, so this is for avoiding a port collision, not for running two
  instances side by side.
- `--verbose`: stream each child's logs live to the terminal, prefixed with
  the child's name. By default child logs are captured to one file per child
  under the `logs` folder of the state directory.
- `--version`: the OpenShift version to run, `X.Y` or `X.Y.Z`. Without it the
  newest stable release for the machine's architecture is picked.
- `--release-image`: an exact release image reference to run, skipping
  version resolution.
- `--registry`: the repository release versions are resolved from,
  `quay.io/openshift-release-dev/ocp-release` by default.
- `--arch`: the release payload architecture, as `uname -m` reports it,
  defaulting to the machine's architecture.

### Where things live

- State directory: `~/.local/state/coreshift/api/` by default
  (`$XDG_STATE_HOME/coreshift/api`). It holds the generated certificates and
  kubeconfigs, the etcd data, and the child logs, and it is reused across
  runs. A non-default port uses `~/.local/state/coreshift/api/<port>/`
  instead, so runs on different ports never share certificates or data.
  Delete the directory to start over.
- Image cache: `~/.cache/coreshift/`, shared by all runs regardless of port;
  see [Cache](#cache).

### Logging

By default a run is quiet: one progress line as each child starts and stops,
and a summary once the control plane is ready:

```
Control plane ready
  API endpoint: https://127.0.0.1:6443
  Kubeconfig:   /home/you/.local/state/coreshift/api/admin.kubeconfig
  Log in with:  oc login https://127.0.0.1:6443 --certificate-authority=/home/you/.local/state/coreshift/api/ca.crt -u <username> -p <password>
```

When a child fails, the run tears down and the error names the failed
component, quotes the tail of its log, and points at the full log file, so a
failure can be debugged without rerunning. `--verbose` trades the quiet for
live child output.

### Logging in

Logins use the standard OpenShift htpasswd flow. Until an identity provider
exists the OAuth server denies every login, so first create a user and wire
it up through the admin kubeconfig while the control plane is running (the
`htpasswd` tool comes from `httpd-tools` or `apache2-utils`):

```
htpasswd -c -B -b users.htpasswd developer password

export KUBECONFIG=~/.local/state/coreshift/api/admin.kubeconfig
oc create namespace openshift-config
oc create secret generic htpasswd-secret --from-file=htpasswd=users.htpasswd -n openshift-config
oc apply -f - <<'EOF'
apiVersion: config.openshift.io/v1
kind: OAuth
metadata:
  name: cluster
spec:
  identityProviders:
  - name: htpasswd
    type: HTPasswd
    mappingMethod: claim
    htpasswd:
      fileData:
        name: htpasswd-secret
EOF
```

One honest caveat: in a full cluster the authentication operator watches the
OAuth object and rolls the OAuth server live when it changes. Here the
identity providers are rendered into the OAuth server at startup, so changes
take effect on the next restart. Once the objects above are in place, restart
`coreshift api`, then log in with the command from the ready summary:

```
oc login https://127.0.0.1:6443 --certificate-authority=/home/you/.local/state/coreshift/api/ca.crt -u developer -p password
```

### Choosing the release

By default the run resolves the newest stable release for the machine's
architecture from the registry. `--version 4.22` picks the newest patch
release of that minor stream, `--version 4.22.8` pins an exact version, and
`--release-image` runs an exact image reference with no resolution at all.
The startup output names the release image the run resolved.

### `api` or `up`

The comparison table above is the short answer. Use `api` when an OpenShift
API is what you need — CI gating, PR checks, and operator dev loops — where
its bare-process, seconds-to-boot nature is the fit. Use `up`, the
container-based mode, when workloads have to actually run against a real
node, CNI, DNS, and ingress. If the question is only "does it behave like
OpenShift at the API level," `api` answers it faster and cheaper.

## Cache

Extracted control plane binaries and the release image's manifest of
component image references are cached under `~/.cache/coreshift/`
(`$XDG_CACHE_HOME/coreshift`), keyed by image digest. A second run of the
same pinned release pulls nothing from the registry and starts straight
from the cache. Delete the directory to clear it.

## Nightly smoke tests

CI runs the live, pull-secret-gated smoke tests nightly on a schedule.
The nightly job needs a repository secret named `PULL_SECRET` holding the
JSON pull secret; a maintainer must add it under Settings -> Secrets and
variables -> Actions to enable the run. Without the secret the job skips
cleanly instead of failing.
