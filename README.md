# CoreShift

CoreShift is a lightweight tool for running a real OpenShift control plane
without a full cluster install — no VM, no kubelet, no proprietary tooling
required to actually run it. The control plane (etcd, kube-apiserver,
kube-controller-manager, kube-scheduler, plus the full OpenShift API layer:
openshift-apiserver, oauth-apiserver, oauth-server,
openshift-controller-manager, cluster-policy-controller) is built from real,
version-matched upstream OpenShift components, not reimplementations.

CoreShift runs the same real control plane two ways. `coreshift run
cluster` runs it here in this terminal, as ordinary processes on the host,
with no container runtime involved. `coreshift create cluster` runs it in
a podman container that CoreShift builds, starts, and leaves running in
the background. Both take the same release flags, both leave state that
survives, and both give a quick, disposable, real OpenShift API for
validation — RBAC/SCC checks, API compatibility testing, admission and
defaulting behavior — without the cost and complexity of a full cluster.

The command names follow one rule: run means here in this terminal;
create means in a managed container; delete removes the container and
keeps the data; cleanup destroys the data.

| | `coreshift run cluster` | `coreshift create cluster` |
|---|---|---|
| Control plane | Real | Real |
| RBAC/SCC/admission | Real | Real |
| Runtime dependency | None (bare processes) | podman |
| Lifecycle | Foreground; Ctrl-C stops it | Background container; `delete cluster` stops it |
| Several clusters at once | No (the components share fixed ports) | Yes; one per `--name` and `--port` |
| Best for | Machines without a container runtime; CI gating, PR checks | Clusters that outlive the terminal; side-by-side clusters |

## The commands

- `coreshift run cluster`: run the control plane in this terminal.
- `coreshift create cluster`: run it in a managed container and wait for
  it to be ready.
- `coreshift delete cluster [--name]`: stop a cluster's container and
  remove it, keeping the state.
- `coreshift get clusters`: list the clusters with their port, release,
  and live readiness.
- `coreshift get kubeconfig [--name]`: print the path to a cluster's
  admin kubeconfig.
- `coreshift cleanup`: remove state directories and, with `--cache`, the
  shared image cache.
- `coreshift version`: print the version the binary was built with.

## The `run cluster` command

`coreshift run cluster` runs a real OpenShift control plane as ordinary
processes on the host. The component binaries are extracted from a real
upstream OpenShift release image and started directly — no containers, no
cluster install. etcd,
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
./coreshift run cluster --pull-secret ~/pull-secret
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
  instances side by side. The reserved ports, refused up front, are 2379,
  2380 (etcd), 8443, 8444, 8445, 8446, 9191, and 10357 (the OpenShift
  components).
- `--verbose`: stream each child's logs live to the terminal, prefixed with
  the child's name. By default child logs are captured to one file per child
  under the `logs` folder of the state directory.
- `--version`: the OpenShift version to run, `X.Y` or `X.Y.Z`. Without it the
  newest stable release for the machine's architecture is picked.
- `--release-image`: an exact release image reference to run, with no
  registry queries at all.
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
  Run `coreshift cleanup` to start over. Each directory records the release
  image that created it. A newer patch of the same minor stream reuses the
  directory and upgrades it in place, the same move a real OpenShift cluster
  makes on a patch update; a different minor stream, or an older patch,
  refuses, because etcd data only moves forward within its stream. Run on
  another port to keep both. On a terminal the refusal offers to remove the
  directory and start fresh; everything in it is lost, including anything
  created through the API, and only an explicit yes goes ahead. Without a
  terminal the refusal names `coreshift cleanup` as the way to remove the
  directory from a script. Directories with etcd data from before this
  record existed get the same treatment, for the same reason. A run with
  no release flags reuses the release the directory records instead of
  floating to the newest, and prints a one-line note when a newer patch of
  that stream exists, since the directory can be upgraded in place with
  `--version`.
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
failure can be debugged without rerunning. If a child dies while startup is
still waiting on a sibling, the error comes from the child that died. `--verbose` trades the quiet for
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
the control plane — `coreshift run cluster` again in host mode, or
`coreshift delete cluster` followed by `coreshift create cluster` in container
mode — then log in with the command from the ready summary:

```
oc login https://127.0.0.1:6443 --certificate-authority=/home/you/.local/state/coreshift/api/ca.crt -u developer -p password
```

### Choosing the release

Four flags pick the release; the defaults cover the common case. With none
of them set, the newest stable release for the machine's architecture is
resolved from the registry — unless the state directory already records a
release, which then boots again as-is: a warm re-create, not a surprise
upgrade. When the registry holds a newer patch of that release's stream,
the run says so in one line and names `--version` as the flag that
upgrades the state in place:

```
./coreshift run cluster --pull-secret ~/pull-secret
```

`--version X.Y` pins a minor: the newest patch release of that stream is
resolved.

```
./coreshift run cluster --pull-secret ~/pull-secret --version 4.22
```

Pinning exact comes in two strengths. `--version X.Y.Z` resolves exactly
that version from the registry:

```
./coreshift run cluster --pull-secret ~/pull-secret --version 4.22.8
```

`--release-image` pins the image reference itself and skips all registry
queries — not even a tag listing happens — so use it when a run must be
reproducible bit for bit, or when the registry cannot be reached for
resolution:

```
./coreshift run cluster --pull-secret ~/pull-secret --release-image quay.io/openshift-release-dev/ocp-release:4.22.8-x86_64
```

`--registry` resolves from a private mirror, any v2-compliant repository,
instead of the default `quay.io/openshift-release-dev/ocp-release`:

```
./coreshift run cluster --pull-secret ~/pull-secret --registry mirror.corp.example/openshift-release-dev/ocp-release
```

`--arch` defaults to the machine's architecture, as `uname -m` reports it;
pass e.g. `--arch aarch64` to resolve a payload for another architecture.
It combines freely with `--version` and `--registry`. The startup output
names the release image the run resolved.

`coreshift create cluster` takes the same four flags and passes them
through to the run inside the container.

## The `create cluster` command

`coreshift create cluster` runs the control plane in a container: it mints
the container image from the running binary (or takes one with `--image`),
starts a detached container named `coreshift-<name>`, streams the boot
output while polling readiness from the host, and prints the same ready
summary as host mode. Before building or starting anything it reads the
state directory's recorded release and applies the same reuse rule host
mode applies: a newer patch of the same minor stream upgrades in place
with no question, while a cross-stream move or a patch downgrade is
refused up front — on a terminal with the same default-no fresh-start
question `run cluster` asks, without one with the `--fresh` remedy — so a
doomed run fails before an image is minted. State data that records no
release is refused as the leftovers of a deleted cluster. The container
keeps running in the background when the command returns; closing the
terminal does not stop it. The API is published on `127.0.0.1:<port>`
only, so exposing it to the network stays an explicit act.

### Prerequisites

- a Go toolchain (1.24 or newer) to build the `coreshift` binary
- a registry pull secret: the JSON file from the Red Hat Hybrid Cloud
  Console, with credentials for the release image registry
- podman, which every container-mode command uses; docker is not
  supported. When podman is missing, each command fails with a clear error
  naming it, such as `podman is required to create clusters`.

### Quickstart

```
make build
./coreshift create cluster --pull-secret ~/pull-secret
```

The first create builds the container image, and a cold cache makes the
container pull the whole release payload before booting, so the first run
is slow; the readiness wait allows half an hour for it. When the control
plane is ready, the summary matches host mode plus one line:

```
Control plane ready
  API endpoint: https://127.0.0.1:6443
  Kubeconfig:   /home/you/.local/state/coreshift/api/admin.kubeconfig
  Log in with:  oc login https://127.0.0.1:6443 --certificate-authority=/home/you/.local/state/coreshift/api/ca.crt -u <username> -p <password>
Container coreshift is running in the background
```

Log in with `oc` as described in [Logging in](#logging-in); the restart it
asks for is `coreshift delete cluster` followed by `coreshift create
cluster`. Then check what is running, and stop it without losing the data:

```
./coreshift get clusters
./coreshift delete cluster
```

`delete` prints where the state was kept, and the next create on the same
port picks it back up.

### Flags

- `--pull-secret` (required): path to the pull secret JSON file.
- `--port`: the port the API is published on, 6443 by default. Like host
  mode, a non-default port gets its own state directory. Create refuses a
  port something already listens on, and one of the control plane's
  reserved ports listed under host mode.
- `--name`: the cluster's name, `coreshift` by default; the container is
  called `coreshift-<name>`. Create refuses a taken name, so a second
  cluster needs its own name and port.
- `--fresh`: remove the state directory before starting, without asking.
  This is the container-mode answer to a state directory that refuses
  reuse: a cross-stream move or a patch downgrade.
- `--image`: the container image to run the cluster in, minted from the
  running binary when not given; see
  [The container image](#the-container-image).
- `--verbose`: stream the image build's output to the terminal. Without
  it the build is quiet, and a failed build quotes the recent tail of
  its captured output and names the file the full output is kept in.
- The release flags are the same as host mode's — `--version`,
  `--release-image`, `--registry`, `--arch` — and are passed through to
  the run inside the container; see
  [Choosing the release](#choosing-the-release).

### Where things live

The container bind-mounts the host's state and cache roots into itself,
so everything lands exactly where host mode puts it, and the modes share
it: a release cached for one is cached for the other, and a host-mode run
on the same port reuses a cluster's certificates and data.

- State directory: `~/.local/state/coreshift/api/` by default
  (`$XDG_STATE_HOME/coreshift/api`), with `/<port>` appended for a
  non-default port — the same directories as host mode.
- Child logs: one file per child under the `logs` folder of the state
  directory, written through the bind mount.
- Cached images: `~/.cache/coreshift/`, shared by everything; see
  [Cache](#cache).
- The container's own boot output: kept by podman; `podman logs
  coreshift-<name>` replays it, and the create command streams it live
  while it waits.

### When a boot fails

A container that exits before the control plane becomes ready fails the
create: the error quotes the tail of the container's log, points at the
full logs in the state directory, and the container is removed while the
state is kept. Ctrl-C during the wait aborts the same way. A failure
caused by the state directory refusing reuse says so and names `--fresh`
and a different `--port` as the remedies.

## `run cluster` or `create cluster`

Use `run cluster` on machines without a container runtime — it needs
nothing but the Go toolchain — and whenever the control plane belongs to
one terminal or one CI step: it runs in the foreground and stops on
Ctrl-C. Use `create cluster` when podman is available and the cluster
should outlive the terminal, or when several clusters have to run side by
side, each with its own name and port. Both answer the question "does it
behave like OpenShift at the API level" equally well; neither runs
workloads, since there is no node, CNI, DNS, or ingress in either mode.

## The `delete cluster` command

`coreshift delete cluster` stops the named cluster's container and removes
it. Stopping the container tears the control plane down in reverse startup
order, like a host-mode shutdown. The state directory is deliberately
kept — deleting a cluster destroys nothing — and the command prints where
the state is, with `coreshift cleanup` named as the way to remove it.

```
./coreshift delete cluster
```

`--name` selects a cluster other than the default `coreshift`.

`--cleanup` opts into destroying the data as well: once the container is
down, the state directory is removed with the same removal the cleanup
command uses, while the shared image cache is kept either way. The
command says in one line whether the state was kept or removed, and
where.

## The `get` command

`coreshift get clusters` lists the clusters: name, published port,
recorded release, and readiness. Readiness is a live probe of each
cluster's API server rather than the container's running state, so a
booting or wedged cluster shows as not ready. With no clusters it prints
`No clusters`.

`coreshift get kubeconfig` prints the path to the named cluster's admin
kubeconfig — the same file host mode writes — and refuses when no cluster
with that name exists. It resolves the right state directory for a cluster
on a non-default port:

```
./coreshift get kubeconfig
/home/you/.local/state/coreshift/api/admin.kubeconfig
```

## The `version` command

`coreshift version` prints the version the binary was built with, derived
from git at build time. The container image minted from the binary is
tagged with the same string, plus a short digest of the binary's content.

## The container image

Container mode runs the control plane inside a container image that
CoreShift builds itself from the running binary, with podman; nothing is
published to a registry, and docker is not supported. The tag combines
the binary's version with a short digest of the binary's content, so an
unchanged binary maps to the image already built and skips the build
entirely, while a rebuilt or dirty binary gets a fresh tag. Minting is a
mechanism, not a command: `coreshift create cluster` calls it, and its
`--image` flag skips it by naming an image to run instead.

The build is quiet like the control plane: one progress line names the
image being built, and a success prints nothing more. A failed build
quotes the recent tail of the captured output and names the file the
full output is kept in, under the image cache; `--verbose` streams the
build tool's output live instead.

The image is built in two stages, both from Red Hat's UBI family and
pinned by digest. The final stage is ubi10/ubi-micro, the micro variant
of the latest UBI major line: the control plane binaries are dynamically
linked against glibc, and RHEL 9-built binaries run on ubi10's newer
glibc, verified live by running all nine children in the image rather
than assumed. The micro variant ships no CA certificates, so a run
inside it cannot pull its release payload: the registry connection
fails TLS. The build closes that gap inside the family by copying the
CA bundle from a ubi10/ubi-minimal stage and pointing the standard SSL
certificate environment variable at it, so the Go binaries doing
registry pulls, the coreshift binary and the children alike, trust the
registry. The bundle comes from the larger stage at mint time, so it
stays current with nothing checked into the repository, while the final
image stays micro-sized. The choice was proven by a control plane boot
inside the minted image with a cold cache: the in-container release
pull worked and the control plane reached ready through the published
port. The gpgme libraries openshift-controller-manager needs are
embedded in the binary itself and written out at boot, so the image
carries nothing but the binary and the certificate bundle.

## The `cleanup` command

`coreshift cleanup` removes the state directory of the default port, so the
next run starts fresh: new certificates, empty etcd, nothing seeded. There
is no confirmation prompt; the command is explicit and safe for scripts.
Both modes keep their state in the same directories, so cleanup also
removes a container-mode cluster's state.

```
./coreshift cleanup
```

- `--port N` removes that port's state directory instead.
- `--all` removes every state directory: the default one and all per-port
  ones.
- `--cache` additionally removes the shared image cache. That is safe to
  drop; it is keyed by image digest and fully rebuilt by re-pulling on the
  next run.

Each directory is listed with its size on disk before it is deleted, and
the total freed is reported after. A missing directory is reported and is
not an error. If a control plane is running, the command refuses and asks
to stop it first, because cleaning under a live instance would pull its
data out from under it.

## Cache

Extracted control plane binaries and the release image's manifest of
component image references are cached under `~/.cache/coreshift/`
(`$XDG_CACHE_HOME/coreshift`), keyed by image digest and shared by
everything regardless of mode or port. A second run of the
same pinned release pulls nothing from the registry and starts straight
from the cache. Run `coreshift cleanup --cache` to clear it.
