# CoreShift

CoreShift is a lightweight tool for running a real OpenShift control plane
without a full cluster install — no VM, no kubelet, no proprietary tooling
required to actually run it. The control plane (etcd, kube-apiserver,
kube-controller-manager, kube-scheduler, plus the full OpenShift API layer:
openshift-apiserver, oauth-apiserver, oauth-server,
openshift-controller-manager, cluster-policy-controller, the
cluster-machine-approver, the cluster-version-operator, the
cluster-network-operator, and the cluster-dns-operator) is built from
real, version-matched upstream OpenShift components, not reimplementations.

CoreShift runs the same real control plane two ways. `coreshift run
cluster` runs it here in this terminal, as ordinary processes on the host,
with no container runtime involved. `coreshift create cluster` runs it in
a podman container that CoreShift builds, starts, and leaves running in
the background. Both take the same release flags, both leave state that
survives, and both give a quick, disposable, real OpenShift API for
validation — RBAC/SCC checks, API compatibility testing, admission and
defaulting behavior — without the cost and complexity of a full cluster.

The command names follow one rule: run means here in this terminal;
create means in a managed container; delete removes the cluster and its
state; cleanup clears leftover state and cached images.

| | `coreshift run cluster` | `coreshift create cluster` |
|---|---|---|
| Control plane | Real | Real |
| RBAC/SCC/admission | Real | Real |
| Runtime dependency | None (bare processes) | podman |
| Lifecycle | Foreground; Ctrl-C stops it | Background container; `delete cluster` removes it |
| Several clusters at once | No (the components share fixed ports) | Yes; one per `--name`, each published on its own `--port` |
| Best for | Machines without a container runtime; CI gating, PR checks | Clusters that outlive the terminal; side-by-side clusters |

## The commands

- `coreshift run cluster`: run the control plane in this terminal.
- `coreshift create cluster`: run it in a managed container and wait for
  it to be ready.
- `coreshift delete cluster [--name]`: remove a cluster and its state;
  `--preserve-state` keeps the state.
- `coreshift get clusters`: list the clusters with their port, release,
  and live readiness.
- `coreshift get kubeconfig [--name]`: print the path to a cluster's
  admin kubeconfig.
- `coreshift cleanup [--name | --all]`: remove cluster state and, with
  `--cache`, everything derived and rebuildable: the image cache and the
  minted container images.
- `coreshift version`: print the version the binary was built with.

## The `run cluster` command

`coreshift run cluster` runs a real OpenShift control plane as ordinary
processes on the host. The component binaries are extracted from a real
upstream OpenShift release image and started directly — no containers, no
cluster install. etcd,
kube-apiserver, kube-controller-manager, and kube-scheduler run alongside the
full OpenShift API layer: openshift-apiserver, oauth-apiserver, oauth-server,
openshift-controller-manager, cluster-policy-controller,
cluster-machine-approver, cluster-version-operator,
cluster-network-operator, and cluster-dns-operator. Everything
listens on the host, and `oc` talks to the API server like it would to any
cluster.

### The version operator

The cluster-version operator is the piece a real cluster owes its
deployed shape to: it applies the release payload's manifests to the
cluster. It runs here the way the installer's bootstrap runs it against
a fresh cluster with no nodes — pointed at the release the run resolved,
with update following off — after the boot seeds what it cannot create
itself: its namespace, the config API definitions it reads at startup,
and the initial version object, the same set the installer's bootstrap
applies. That object belongs to the operator, not to CoreShift: the run
writes it once, with nothing but the cluster identity the API requires,
so the operator adopts the cluster instead of creating it fresh, and
from its first boot it owns the object — the status, the history, and
the desired state, all written against the release actually running.
Nothing in CoreShift writes to the object after that.

The release the run resolved is kept in lockstep across everything that
carries one: the operator is told it, the control plane's processes run
binaries from it, and the state directory's marker records it. One
resolution feeds all three, so the manifests applied, the binaries
running, and the marker recorded can never name different releases. An
in-place patch upgrade — re-creating a cluster with its state preserved
and a newer version of its stream — moves all three together: the
operator re-syncs the cluster to the new payload, the version history
gains the new release, and the processes and marker follow.

The operator starts as soon as the kube core answers, ahead of the
OpenShift children that wait on what it applies. Through the admin
kubeconfig you can watch it work: the payload's namespaces and
definitions appear, and `oc get clusterversion version` reports the
release in its status. The rest of that status is honest about the
partial state, and the cluster reports degraded: its Failing condition
names every payload operator as not available, because those operators
run as pods in a real cluster and are not running here at all — their
Deployments sit Pending, their ClusterOperator objects stay empty, and
the few payload objects that live in namespaces those operators would
have created cannot land. The status is the truth, not a fault to
hide. The operator Deployments it creates select master nodes, and
this control plane runs none, so they sit Pending: the payload is
applied, but nothing runs from it. The control plane stands in for the
bootstrap, not for nodes.

The children that need objects the payload carries wait for them before
starting: the OpenShift API server waits for the quota and security
definitions, the OAuth API server for the OAuth definition, the
controller managers for the permission pair that backs pod security
labeling, and the approver and the network and DNS operators for their
own APIs and permissions. The wait shows as one progress line while it
lasts, a timing line once it passes, and it releases the moment the
objects exist, whoever applied them. The version operator is the
deliverer for the security definition and everything the approver and
the network and DNS operators wait on, so a normal boot shows each of
those lines for the few seconds its delivery takes, and a gate the
operator has already fed — delivery can land while earlier children are
still starting — shows no line at all. The OAuth definition and the
permission pair come from the boot cache, so their gates pass
instantly. The approver and the network and DNS operators run behind
the ready summary, so their waits settle in the background rather than
holding the API's readiness. If objects never arrive, the boot fails
after ten minutes, naming what was missing and pointing at the
operator's log. The budget is sized against a cold boot's observed
delivery — the security definition ten seconds after the API server
answers, the later objects under a minute — leaving an order of
magnitude of room.

Every seed entry the payload delivers has been audited and retired: on
a fresh boot with the operator running and the entry's seed removed,
each object was checked through the admin kubeconfig after the delivery
window, for its shape and not just its name. The operator delivers the
security, range allocation, OAuth, infrastructure, and machine
definitions; the fifteen default security context constraints (the
thirteen the seed used to apply, plus two the seed never carried, for
the insights runtime extractor and the machine API termination handler);
the grant that lets every signed-in user use the restricted constraint;
the machine approver's cluster role; and the cluster's infrastructure
object, from the payload's own create-only manifest. None of these is
seeded as an owner anymore.

Two of the audited objects return as a different class of seed, the
boot-critical cache: the OAuth definition and the
restricted-constraint grant, the two objects the API's readiness path
waits on. Unlike the permanent chicken-and-egg seeds — objects the API
server needs before it answers, which the version operator can never
deliver — these two are ordinary payload objects the operator delivers
anyway, so seeding them changes nothing about who owns them: the seed
applies the same payload manifests the operator applies, purely as a
cache for boot speed, and the operator converges the objects to its own
versions as it does everything else it owns. The cache is updated by
hand only if a future release makes a breaking change to these objects,
and that is caught loudly rather than drifting silently: the seed reads
the files the release ships, so one that moved or reshaped them fails
the boot instead of serving a stale copy.

What the operator cannot deliver stays seeded, each with its reason
recorded where the seed lives. Two definitions must pre-date the API
server answering, because its own admission reads them: the quota
definition, whose absence breaks every namespaced write, and the role
binding restrictions definition, whose absence breaks the API server's
own bootstrap. The operator does carry the second one, but its delivery
lands seconds after the API server answers, past the controller
manager's bounded wait for the API server's health, so a boot relying
on it is a coin flip the seed removes — one audit boot died missing it
by a second, another survived. The controller permission pairs the
payload does not carry stay seeded too: the ones the label syncers and
the namespace security allocation controller impersonate, applied in
real clusters by an operator that runs as a payload Deployment and sits
Pending here, and the node bootstrap binding, applied in real clusters
by the machine config operator. The machine approver's permissions come
wholly from the payload instead: its kubeconfig's certificate carries
the payload's own service-account identity, so the role and binding the
operator applies are the approver's, and it waits for them before
starting like the other children. The OAuth client stays
because no payload manifest carries it; the authentication operator
bootstraps it in real clusters. The pull secret stays because the
operator consumes it rather than producing it. The network operator's
stand-ins are recorded where its seed lives, with the rest of the
installer output it stands in for.

### The network operator

The cluster network operator deploys the cluster's network stack: it
reads the cluster's network configuration and renders the real
OVN-Kubernetes workloads from the bindata beside its binary, the way
its payload deployment runs it in a real cluster — same flags, and the
same environment with every operand image resolved from the release,
so the workloads it creates reference the payload's own images. It runs
under the payload's own service-account identity, whose cluster-admin
the payload's binding grants, and its start waits for the API
definitions its watchers map and the network ClusterOperator its status
machinery reads, all delivered by the version operator.

What it reads and cannot get from the payload is seeded in ahead of it,
standing in for installer output the way the pull secret and OAuth
client do: the network ranges in the cluster network object (the
payload delivers the object as a create-only empty shell), the
infrastructure record (same shell, and the operator needs the platform
and topologies), the install config record in kube-system (the operator
counts control plane replicas from it), the release's feature gate
record (the config operator maintains it in a real cluster, and the
network operator's reader is not tolerant of its absence; it is rendered
by the release's own feature set writer before the version operator
starts, so that operator's stop-on-change controller finds it in
place), and the probed host MTU (the operator's prober job needs nodes
to probe; a standard interface's value stands in). The operator-scoped
configuration object is derived, not supplied: the operator writes it
itself on its first reconcile.

The workloads it creates are real and their status is honest: the
OVN-Kubernetes daemonsets and deployments, Multus, and the network
diagnostics all exist, and all sit unscheduled, because they select
nodes this control plane runs none of. The network ClusterOperator
reports exactly that — progressing, naming each workload not yet
scheduled on any node — and that is the truth of a deployed network
stack with nothing to run it, the same truth the payload's other
Deployments tell.

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
- `--name`: the cluster's name, `coreshift` by default. The name is the
  cluster's identity: its state lives in `clusters/<name>` under the
  state root, and the name picks which cluster's state a run boots.
- `--port`: the port the API server listens on, 6443 by default. Use it
  when something else already occupies 6443. The port is not part of the
  cluster's identity — the state directory does not depend on it, and a
  cluster re-created on a different port reuses its state warm — but etcd
  and the other components always listen on the same fixed ports, so this
  is for avoiding a port collision, not for running two instances side by
  side. The reserved ports, refused up front, are 2379,
  2380 (etcd), 8443, 8444, 8445, 8446, 9191, and 10357 (the OpenShift
  components).
- `--verbose`: stream each child's logs live to the terminal, prefixed with
  the child's name. By default child logs are captured to one file per child
  under the `logs` folder of the state directory.
- `--version`: the OpenShift version to run, `X.Y`, `X.Y.Z`, or `newest` for
  the registry's absolute newest release. Without it the newest release
  within the newest verified line is picked; see
  [Choosing the release](#choosing-the-release).
- `--release-image`: an exact release image reference to run, with no
  registry queries at all.
- `--registry`: the repository release versions are resolved from,
  `quay.io/openshift-release-dev/ocp-release` by default.
- `--arch`: the release payload architecture, as `uname -m` reports it,
  defaulting to the machine's architecture.

### Where things live

- State directory: `~/.local/state/coreshift/clusters/<name>/` by
  default (`$XDG_STATE_HOME/coreshift/clusters/<name>`). It holds the
  generated certificates and kubeconfigs, the etcd data, and the child
  logs, and it is reused across runs whatever port the API server binds.
  Run `coreshift cleanup` to start over. Each directory records the
  cluster's name and the release image that created it in its marker, so
  it is self-describing. A newer patch of the same minor stream reuses the
  directory and upgrades it in place, the same move a real OpenShift
  cluster makes on a patch update; a different minor stream, or an older
  patch, refuses, because etcd data only moves forward within its stream.
  Run under another name to keep both. On a terminal the refusal offers to
  remove the directory and start fresh; everything in it is lost,
  including anything created through the API, and only an explicit yes
  goes ahead. Without a terminal the refusal names `coreshift cleanup`
  as the way to remove the directory from a script. A run with no release
  flags reuses the release the directory records instead of floating to
  the newest, and prints a one-line note when a newer patch of that
  stream exists, since the directory can be upgraded in place with
  `--version`.
- Pull secret: stored in cluster state, as the `pull-secret` Secret in
  the `openshift-config` namespace — the global pull secret a real
  cluster keeps, seeded by the startup from the file the run was given.
  It lives in the state directory's etcd data and is destroyed with it,
  by `delete cluster` or `coreshift cleanup`.
- Image cache: `~/.cache/coreshift/`, shared by all runs regardless of
  cluster; see [Cache](#cache).

### Logging

By default a run is quiet: one progress line as each child starts and stops,
and a summary once the control plane is ready:

```
Control plane ready
  Name:         coreshift
  Release:      quay.io/openshift-release-dev/ocp-release:4.22.21-x86_64
  API endpoint: https://127.0.0.1:6443
  Kubeconfig:   /home/you/.local/state/coreshift/clusters/coreshift/admin.kubeconfig
  Context:      admin@coreshift
```

The summary's meaning is honest about what is ready: the API is usable —
etcd, the API server, the aggregated servers, and the controllers up, the
aggregated availability confirmed. The machine approver, the network
operator, and the DNS operator start after the summary and converge behind
it, the way operators settle after a real cluster's API answers; their
progress and gate lines follow the summary as they settle.

When a child fails, the run tears down and the error names the failed
component, quotes the tail of its log, and points at the full log file, so a
failure can be debugged without rerunning. If a child dies while startup is
still waiting on a sibling, the error comes from the child that died. `--verbose` trades the quiet for
live child output.

A child whose start waits for objects the version operator delivers — see
[The version operator](#the-version-operator) — shows the wait as one progress
line while it lasts, and once it passes, one timing line saying how long the
gate held the start:

```
Waiting for the network operator's APIs
Waited 14s for the network operator's APIs
```

so where a slow boot's seconds went is read off its output instead of
guessed. A gate that passes instantly prints nothing.

### Logging in

Logins use the standard OpenShift htpasswd flow. Until an identity provider
exists the OAuth server denies every login, so first create a user and wire
it up through the admin kubeconfig while the control plane is running (the
`htpasswd` tool comes from `httpd-tools` or `apache2-utils`):

```
htpasswd -c -B -b users.htpasswd developer password

export KUBECONFIG=~/.local/state/coreshift/clusters/coreshift/admin.kubeconfig
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
`coreshift delete cluster --preserve-state` followed by `coreshift create
cluster` in container mode — then log in:

```
oc login https://127.0.0.1:6443 --certificate-authority=/home/you/.local/state/coreshift/clusters/coreshift/ca.crt -u developer -p password
```

### Choosing the release

Four flags pick the release; the defaults cover the common case. With none
of them set, the newest release within the newest verified line for the
machine's architecture is resolved from the registry — unless the state
directory already records a release, which then boots again as-is: a warm
re-create, not a surprise upgrade. When the registry holds a newer patch
of that release's stream, the run says so in one line and names
`--version` as the flag that upgrades the state in place:

```
./coreshift run cluster --pull-secret ~/pull-secret
```

Support is defined by what CoreShift has verified. The newest verified
line is the newest OpenShift minor the stand-ins have been checked
against — 4.22 today — and the unversioned default stays inside it, so a
brand-new, unverified major cannot arrive silently. Asking for a release
beyond the line works; the run just prints a plain warning first:

```
warning: 5.0.0 is beyond the newest verified line, 4.22 — it may not work
```

`--version newest` asks for the registry's absolute newest release,
ignoring the line. It exists for the nightly drift alarm in CI, which
probes the frontier so a new release is noticed: it boots the release,
soaks it, and checks its pod security labeling, its login chain, its
aggregated API, and its node join acceptance before shutting it down;
everything else should stay on the verified default. Raising the line
is deliberate work, done together with the stand-in updates a new
line needs, and it is one constant in the code.

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
named cluster's recorded release and applies the same reuse rule host
mode applies: a newer patch of the same minor stream upgrades in place
with no question, while a cross-stream move or a patch downgrade is
refused up front — on a terminal with the same default-no fresh-start
question `run cluster` asks, without one with the `--fresh` remedy — so a
doomed run fails before an image is minted. The container
keeps running in the background when the command returns; closing the
terminal does not stop it. The API is published on `127.0.0.1:<port>`
only, and the OAuth login endpoint on the port after it, so exposing
either to the network stays an explicit act.

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
  Name:         coreshift
  Release:      quay.io/openshift-release-dev/ocp-release:4.22.21-x86_64
  API endpoint: https://127.0.0.1:6443
  Kubeconfig:   /home/you/.local/state/coreshift/clusters/coreshift/admin.kubeconfig
  Context:      admin@coreshift
Container coreshift-coreshift is running in the background
```

Log in with `oc` as described in [Logging in](#logging-in); the restart it
asks for is `coreshift delete cluster --preserve-state` followed by
`coreshift create cluster`. Then check what is running, and remove it all
when done:

```
./coreshift get clusters
./coreshift delete cluster
```

`delete` removes the cluster and its state, saying what was
removed and where the state was. To keep the state for a warm re-create,
pass `--preserve-state`: the output then says the state was preserved,
and the next create with the same name picks it back up — on whatever
port, warm.

### Flags

- `--pull-secret` (required): path to the pull secret JSON file.
- `--port`: the port the API is published on, 6443 by default. The state
  directory does not depend on it, so a cluster deleted with
  `--preserve-state` and re-created on a different port reuses its state
  warm, with the kubeconfig's server URL rewritten on boot to match the
  port in use. The OAuth login endpoint publishes on the port after the
  API port, so `oc login` from the host can follow the API server's
  discovery document to the OAuth server; create refuses an API port
  whose follower is taken or reserved. Create also refuses a port
  something already listens on, and one of the control plane's reserved
  ports listed under host mode.
- `--name`: the cluster's name, `coreshift` by default; the container is
  called `coreshift-<name>` and the state lives in
  `clusters/<name>` under the state root, the same directory host mode
  uses. The name is passed through to the run inside the container.
  Create refuses a taken name, so a second cluster needs its own name,
  with its own port to publish on.
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

The container bind-mounts the named cluster's state directory and the
shared image cache into itself, so everything lands exactly where host
mode puts it, and the modes share it: a release cached for one is cached
for the other, and a host-mode run of the same cluster name reuses its
certificates and state.

- State directory: `~/.local/state/coreshift/clusters/<name>/` by
  default (`$XDG_STATE_HOME/coreshift/clusters/<name>`) — the same
  directory host mode uses, whatever port the API is published on.
- Child logs: one file per child under the `logs` folder of the state
  directory, written through the bind mount.
- Pull secret: stored in cluster state, as the `pull-secret` Secret in
  the `openshift-config` namespace — the same object host mode stores,
  seeded by the run inside the container. It lives in the state
  directory's etcd data and is destroyed with it, by `delete cluster` or
  `coreshift cleanup`.
- Cached images: `~/.cache/coreshift/`, shared by everything; see
  [Cache](#cache).
- The podman network `coreshift`: the bridge network cluster containers
  run on, created by the create command and kept until
  `coreshift cleanup --cache` finds nothing using it.
- The container's own boot output: kept by podman; `podman logs
  coreshift-<name>` replays it, and the create command streams it live
  while it waits.

### When a boot fails

A container that exits before the control plane becomes ready fails the
create: the error quotes the tail of the container's log, points at the
full logs in the state directory, and the container is removed while the
state is preserved. Ctrl-C during the wait aborts the same way. A failure
caused by the state directory refusing reuse says so and names `--fresh`
and a different `--name` as the remedies.

## Nodes

Nodes cannot currently join a CoreShift control plane. No tooling ships
the credentials a join needs — no bootstrap identity to present, no
machine record for an approver to match against — so nothing in this
repository advertises node-joining capability.

What the control plane keeps is the machinery that would accept a real
node: the machine approver running behind the ready summary, the
controller manager's certificate signing for node client certificates,
the node-bootstrap permissions, and the node-serving operators — network
and DNS — rendering their workloads harmlessly with no nodes to serve.
That layer is the accepting half of a real cluster's join path, kept for
the planned real-node join the roadmap defers to a later phase.

## `run cluster` or `create cluster`

Use `run cluster` on machines without a container runtime — it needs
nothing but the Go toolchain — and whenever the control plane belongs to
one terminal or one CI step: it runs in the foreground and stops on
Ctrl-C. Use `create cluster` when podman is available and the cluster
should outlive the terminal, or when several clusters have to run side by
side, each with its own name. Both answer the question "does it
behave like OpenShift at the API level" equally well.

## The `delete cluster` command

`coreshift delete cluster` removes the named cluster and everything it
owns: its container and its state directory — the
certificate authority keys, the admin credentials, the stored pull
secret, and the etcd data. Stopping the container tears the control
plane down in reverse startup order, like a host-mode shutdown. The
command says what was removed
and where the state was, and leaves nothing behind: no containers, no
state directory, not even an empty clusters tree. Keeping any of it is
the explicit act.

```
./coreshift delete cluster
```

`--name` selects a cluster other than the default `coreshift`.

`--preserve-state` keeps the state directory; the output says the state
was preserved, with its path. This is the warm re-creation workflow: the
next create with the same name — on whatever port — boots from the same
certificates and etcd data, pulling nothing. The shared image cache is
kept either way.

A cluster whose container is already gone but whose state remains is
reported as not running, and the state follows the same rule. Failed
creates never remove state — it stays behind
for inspection — so delete is the only command that removes it by intent;
`coreshift cleanup` is the one for state orphaned by crashes or older
deletes.

## The `get` command

`coreshift get clusters` lists the clusters: name, published port,
recorded release, and readiness. Readiness requires the cluster's
container running and its API server answering a live probe: a stopped
container is not ready without probing, because the port it published
can since have been taken by another cluster, and a running but booting
or wedged one fails the probe. With no clusters it prints
`No clusters`.

`coreshift get kubeconfig` prints the path to the named cluster's admin
kubeconfig — the same file host mode writes — and refuses when no state
exists for the name, listing the clusters that do:

```
./coreshift get kubeconfig
/home/you/.local/state/coreshift/clusters/coreshift/admin.kubeconfig
```

## The `version` command

`coreshift version` prints the version the binary was built with, derived
from git at build time. The container image minted from the binary is
tagged with the same string, plus a short digest of the build's inputs.

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
glibc, verified live by running every child in the image rather
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

`coreshift cleanup` clears leftover state and cached images. It removes
the default cluster's state directory, so the next run starts fresh: new
certificates, empty etcd, nothing seeded. Delete already removes a
cluster's state with it, so cleanup is for what a crash or an older
delete left behind, and for state a delete kept on purpose. There is no
confirmation prompt; the command is explicit and safe for scripts. Both
modes keep their state in the same directories, so cleanup also removes
a container-mode cluster's state.

```
./coreshift cleanup
```

- `--name N` removes that cluster's state directory instead.
- `--all` removes every cluster's state: the whole `clusters` tree, and
  nothing else under the state root, so unrelated directories kept there
  survive.
- `--cache` additionally removes everything derived and rebuildable: the
  shared image cache, and the minted container images from podman's store
  — the `coreshift` tags — and the podman network
  cluster containers run on, when nothing uses it anymore. All are safe
  to drop: the cache is keyed by image digest and rebuilt by re-pulling,
  the images are minted from the binary again in seconds, and the
  network is re-created by the next cluster. The base images
  they were built from, such as the UBI bases, are shared
  with everything else and stay. A cluster whose state was kept simply
  mints its image again on re-create.

Each directory is listed with its size on disk before it is deleted,
each removed image is named, and the total freed is reported after.
Because images share layers with their bases, the freed space is
measured as the drop in podman's store, not as the sum of the removed
images' sizes. A missing directory is reported and is not an error. If a
control plane is running, the command refuses and asks to stop it first,
because cleaning under a live instance would pull its data out from
under it.

## Cache

Extracted control plane binaries, the release image's manifest of
component image references, and the release payload's manifest sets are
cached under `~/.cache/coreshift/`
(`$XDG_CACHE_HOME/coreshift`), keyed by image digest and shared by
everything regardless of mode or cluster. A second run of the
same pinned release pulls nothing from the registry and starts straight
from the cache.

The minted container images are the same kind of thing: deterministic
from the binary and rebuilt in seconds. `coreshift cleanup --cache`
therefore removes them from podman's store along with the cache
directory, making one flag mean everything derived and rebuildable; the
base images they were built from stay.
