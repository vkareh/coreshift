# How CoreShift works

CoreShift runs a real OpenShift control plane as thirteen supervised
processes, all real upstream binaries, with the cluster-version
operator growing the cluster from the release payload the way a real
cluster's bootstrap does. This page covers the architecture. For the
pitch and the commands, see the [README](../README.md); for the
day-to-day of a running cluster, see [Operations](operations.md).

## The supervised processes

The control plane's component binaries are extracted from a real
upstream OpenShift release image and started directly — no containers,
no cluster install. Everything listens on the host, and `oc` talks to
the API server like it would to any cluster. `coreshift run cluster`
runs them here in this terminal, as ordinary processes on the host
with no container runtime involved; `coreshift create cluster` runs
the same set inside a podman container CoreShift builds, starts, and
leaves running in the background.

The thirteen children:

- etcd, kube-apiserver, kube-controller-manager, and kube-scheduler —
  the kube core. An external etcd can hold the state instead, in which
  case etcd itself is not run at all; see
  [Operations](operations.md#cluster-state).
- openshift-apiserver — the OpenShift API.
- oauth-apiserver — the OAuth API.
- oauth-server — serves logins.
- openshift-controller-manager and cluster-policy-controller — the
  OpenShift controller managers.
- cluster-machine-approver — matches joining nodes' client
  certificates against machine records.
- cluster-version-operator — applies the release payload; see
  [below](#the-version-operator).
- cluster-network-operator — deploys the cluster's network stack; see
  [below](#the-network-operator).
- cluster-dns-operator — the other node-serving operator.

A run ends on Ctrl-C (or SIGTERM), stopping the children in reverse
startup order. State survives across runs: certificates, etcd data,
and everything created through the API are still there the next time.

## The version operator

The cluster-version operator is the piece a real cluster owes its
deployed shape to: it applies the release payload's manifests to the
cluster. It runs here the way the installer's bootstrap runs it
against a fresh cluster with no nodes — pointed at the release the
run resolved, with update following off — after the boot seeds what
it cannot create itself: its namespace, the config API definitions it
reads at startup, and the initial version object, the same set the
installer's bootstrap applies. That object belongs to the operator,
not to CoreShift: the run writes it once, with nothing but the
cluster identity the API requires, so the operator adopts the cluster
instead of creating it fresh, and from its first boot it owns the
object — the status, the history, and the desired state, all written
against the release actually running. Nothing in CoreShift writes to
the object after that.

The release the run resolved is kept in lockstep across everything
that carries one: the operator is told it, the control plane's
processes run binaries from it, and the state directory's marker
records it. One resolution feeds all three, so the manifests applied,
the binaries running, and the marker recorded can never name
different releases. An in-place patch upgrade — re-creating a cluster
with its state preserved and a newer version of its stream — moves
all three together: the operator re-syncs the cluster to the new
payload, the version history gains the new release, and the processes
and marker follow.

The operator starts as soon as the kube core answers, ahead of the
OpenShift children that wait on what it applies. Through the admin
kubeconfig you can watch it work: the payload's namespaces and
definitions appear, and `oc get clusterversion version` reports the
release in its status. The rest of that status is honest about the
partial state, and the cluster reports degraded: its Failing
condition names every payload operator as not available, because
those operators run as pods in a real cluster and are not running
here at all — their Deployments sit Pending, their ClusterOperator
objects stay empty, and the few payload objects that live in
namespaces those operators would have created cannot land. The status
is the truth, not a fault to hide. The operator Deployments it
creates select master nodes, and this control plane runs none, so
they sit Pending: the payload is applied, but nothing runs from it.
The control plane stands in for the bootstrap, not for nodes.

## The gated boot

The children that need objects the payload carries wait for them
before starting: the OpenShift API server waits for the quota and
security definitions, the OAuth API server for the OAuth definition,
the controller managers for the permission pair that backs pod
security labeling, and the approver and the network and DNS operators
for their own APIs and permissions. The wait shows as one progress
line while it lasts, a timing line once it passes, and it releases
the moment the objects exist, whoever applied them:

```
Waiting for the network operator's APIs
Waited 14s for the network operator's APIs
```

so where a slow boot's seconds went is read off its output instead of
guessed. A gate that passes instantly prints nothing. The version
operator is the deliverer for the security definition and everything
the approver and the network and DNS operators wait on, so a normal
boot shows each of those lines for the few seconds its delivery
takes, and a gate the operator has already fed — delivery can land
while earlier children are still starting — shows no line at all. The
OAuth definition and the permission pair come from the boot cache, so
their gates pass instantly. The approver and the network and DNS
operators run behind the ready summary, so their waits settle in the
background rather than holding the API's readiness. If objects never
arrive, the boot fails after ten minutes, naming what was missing and
pointing at the operator's log. The budget is sized against a cold
boot's observed delivery — the security definition ten seconds after
the API server answers, the later objects under a minute — leaving an
order of magnitude of room.

## The seeds and the boot cache

The boot seeds what the operator cannot deliver, and every seed entry
the payload delivers has been audited and retired: on a fresh boot
with the operator running and the entry's seed removed, each object
was checked through the admin kubeconfig after the delivery window,
for its shape and not just its name. The operator delivers the
security, range allocation, OAuth, infrastructure, and machine
definitions; the fifteen default security context constraints (the
thirteen the seed used to apply, plus two the seed never carried, for
the insights runtime extractor and the machine API termination
handler); the grant that lets every signed-in user use the restricted
constraint; the machine approver's cluster role; and the cluster's
infrastructure object, from the payload's own create-only manifest.
None of these is seeded as an owner anymore.

Two of the audited objects return as a different class of seed, the
boot-critical cache: the OAuth definition and the
restricted-constraint grant, the two objects the API's readiness path
waits on. Unlike the permanent chicken-and-egg seeds — objects the
API server needs before it answers, which the version operator can
never deliver — these two are ordinary payload objects the operator
delivers anyway, so seeding them changes nothing about who owns them:
the seed applies the same payload manifests the operator applies,
purely as a cache for boot speed, and the operator converges the
objects to its own versions as it does everything else it owns. The
cache is updated by hand only if a future release makes a breaking
change to these objects, and that is caught loudly rather than
drifting silently: the seed reads the files the release ships, so one
that moved or reshaped them fails the boot instead of serving a stale
copy.

What the operator cannot deliver stays seeded, each with its reason
recorded where the seed lives. Two definitions must pre-date the API
server answering, because its own admission reads them: the quota
definition, whose absence breaks every namespaced write, and the role
binding restrictions definition, whose absence breaks the API
server's own bootstrap. The operator does carry the second one, but
its delivery lands seconds after the API server answers, past the
controller manager's bounded wait for the API server's health, so a
boot relying on it is a coin flip the seed removes — one audit boot
died missing it by a second, another survived. The controller
permission pairs the payload does not carry stay seeded too: the ones
the label syncers and the namespace security allocation controller
impersonate, applied in real clusters by an operator that runs as a
payload Deployment and sits Pending here, and the node bootstrap
binding, applied in real clusters by the machine config operator. The
machine approver's permissions come wholly from the payload instead:
its kubeconfig's certificate carries the payload's own
service-account identity, so the role and binding the operator
applies are the approver's, and it waits for them before starting
like the other children. The OAuth client stays because no payload
manifest carries it; the authentication operator bootstraps it in
real clusters. The pull secret stays because the operator consumes it
rather than producing it. The network operator's stand-ins are
recorded where its seed lives, with the rest of the installer output
it stands in for.

## The ready summary

The run prints its summary at API readiness, and its meaning is
honest about what is ready: the API is usable — etcd, the API server,
the aggregated servers, and the controllers up, the aggregated
availability confirmed. The machine approver, the network operator,
and the DNS operator start after the summary and converge behind it,
the way operators settle after a real cluster's API answers; their
progress and gate lines follow the summary as they settle.

## The network operator

The cluster network operator deploys the cluster's network stack: it
reads the cluster's network configuration and renders the real
OVN-Kubernetes workloads from the bindata beside its binary, the way
its payload deployment runs it in a real cluster — same flags, and
the same environment with every operand image resolved from the
release, so the workloads it creates reference the payload's own
images. It runs under the payload's own service-account identity,
whose cluster-admin the payload's binding grants, and its start waits
for the API definitions its watchers map and the network
ClusterOperator its status machinery reads, all delivered by the
version operator.

What it reads and cannot get from the payload is seeded in ahead of
it, standing in for installer output the way the pull secret and
OAuth client do: the network ranges in the cluster network object
(the payload delivers the object as a create-only empty shell), the
infrastructure record (same shell, and the operator needs the
platform and topologies), the install config record in kube-system
(the operator counts control plane replicas from it), the release's
feature gate record (the config operator maintains it in a real
cluster, and the network operator's reader is not tolerant of its
absence; it is rendered by the release's own feature set writer
before the version operator starts, so that operator's
stop-on-change controller finds it in place), and the probed host
MTU (the operator's prober job needs nodes to probe; a standard
interface's value stands in). The operator-scoped configuration
object is derived, not supplied: the operator writes it itself on its
first reconcile.

The workloads it creates are real and their status is honest: the
OVN-Kubernetes daemonsets and deployments, Multus, and the network
diagnostics all exist, and all sit unscheduled, because they select
nodes this control plane runs none of. The network ClusterOperator
reports exactly that — progressing, naming each workload not yet
scheduled on any node — and that is the truth of a deployed network
stack with nothing to run it, the same truth the payload's other
Deployments tell.

## The container image

Container mode runs the control plane inside a container image that
CoreShift builds itself from the running binary, with podman; nothing
is published to a registry. `coreshift version` prints the version
the binary was built with, derived from git at build time, and the
minted image is tagged with the same string plus a short digest of
the build's inputs, so an unchanged binary maps to the image already
built and skips the build entirely, while a rebuilt or dirty binary
gets a fresh tag. Minting is a mechanism, not a command: `coreshift
create cluster` calls it, and its `--image` flag skips it by naming
an image to run instead.

The build is quiet like the control plane: one progress line names
the image being built, and a success prints nothing more. A failed
build quotes the recent tail of the captured output and names the
file the full output is kept in, under the image cache; `--verbose`
streams the build tool's output live instead.

The image is built in two stages, both from Red Hat's UBI family and
pinned by digest. The final stage is ubi10/ubi-micro, the micro
variant of the latest UBI major line: the control plane binaries are
dynamically linked against glibc, and RHEL 9-built binaries run on
ubi10's newer glibc, verified live by running every child in the
image rather than assumed. The micro variant ships no CA
certificates, so a run inside it cannot pull its release payload: the
registry connection fails TLS. The build closes that gap inside the
family by copying the CA bundle from a ubi10/ubi-minimal stage and
pointing the standard SSL certificate environment variable at it, so
the Go binaries doing registry pulls, the coreshift binary and the
children alike, trust the registry. The bundle comes from the larger
stage at mint time, so it stays current with nothing checked into the
repository, while the final image stays micro-sized. The choice was
proven by a control plane boot inside the minted image with a cold
cache: the in-container release pull worked and the control plane
reached ready through the published port. The gpgme libraries
openshift-controller-manager needs are embedded in the binary itself
and written out at boot, so the image carries nothing but the binary
and the certificate bundle.
