# CoreShift

CoreShift is a lightweight tool for running a real OpenShift control
plane without a full cluster install — no VM, no kubelet, no
proprietary tooling required to actually run it. One binary runs
thirteen real, version-matched upstream processes — etcd,
kube-apiserver, kube-controller-manager, kube-scheduler, and the full
OpenShift API layer — grown from a current upstream release payload,
not reimplementations. It gives a quick, disposable, real OpenShift
API for validation — RBAC/SCC checks, API compatibility testing,
admission and defaulting behavior — without the cost and complexity
of a full cluster.

## Quickstart

You need a Go toolchain (1.24 or newer), a registry pull secret (the
JSON file from the Red Hat Hybrid Cloud Console, with credentials for
the release image registry), and podman. Nothing else is installed,
and nothing on the host is modified outside the directories under
[Where things live](#where-things-live). Once release binaries exist,
download one, make it executable, and run it in place of the build
step.

```
make build
./coreshift create cluster --pull-secret ~/pull-secret
export KUBECONFIG=$(./coreshift get kubeconfig)
oc get clusterversion
./coreshift delete cluster
```

The first create pulls the whole release payload before booting, so
it is slow; the readiness wait allows half an hour for it. When the
control plane is ready, it prints its summary:

```
Control plane ready
  Name:         coreshift
  Release:      quay.io/openshift-release-dev/ocp-release:5.0.1-x86_64
  API endpoint: https://127.0.0.1:6443
  Kubeconfig:   /home/you/.local/state/coreshift/clusters/coreshift/admin.kubeconfig
  Context:      admin@coreshift
Container coreshift-coreshift is running in the background
```

`oc get clusterversion` answers through the exported kubeconfig; the
version operator keeps converging behind the summary, and the status
it reports is the honest truth of a control plane with no nodes —
see [How it works](docs/how-it-works.md). To log in as a user, see
[Logging in](docs/operations.md#logging-in). No podman?
`./coreshift run cluster --pull-secret ~/pull-secret` runs the same
control plane in this terminal; Ctrl-C stops it.

## What's real

CoreShift runs the same real control plane two ways. `coreshift run
cluster` runs it here in this terminal, as ordinary processes on the
host, with no container runtime involved. `coreshift create cluster`
runs it in a podman container that CoreShift builds, starts, and
leaves running in the background. Both take the same release flags,
both leave state that survives, and both answer the question "does it
behave like OpenShift at the API level" equally well.

| | `coreshift run cluster` | `coreshift create cluster` |
|---|---|---|
| Control plane | Real | Real |
| RBAC/SCC/admission | Real | Real |
| Runtime dependency | None (bare processes) | podman |
| Lifecycle | Foreground; Ctrl-C stops it | Background container; `delete cluster` removes it |
| Several clusters at once | No (the components share fixed ports) | Yes; one per `--name`, each published on its own `--port` |
| Best for | Machines without a container runtime; CI gating, PR checks | Clusters that outlive the terminal; side-by-side clusters |

One honest limit: nodes cannot join. No tooling ships the credentials
a join needs — no bootstrap identity to present, no machine record
for an approver to match against — so nothing in this repository
advertises node-joining capability. What the control plane keeps is
the machinery that would accept a real node: the machine approver,
the certificate signing for node client certificates, the
node-bootstrap permissions, and the node-serving network and DNS
operators rendering their workloads harmlessly with no nodes to
serve. That accepting machinery waits for the planned real-node join
path. How the control plane is built from the payload is in
[How it works](docs/how-it-works.md).

## Commands

The names follow one rule: run means here in this terminal; create
means in a managed container; delete removes the cluster and its
state; cleanup clears leftover state and cached images.

| Command | Flags | Meaning |
|---|---|---|
| `run cluster` | `--pull-secret` (required), `--name`, `--port`, `--verbose`, `--version`, `--release-image`, `--registry`, `--arch` | Run the control plane in this terminal. |
| `create cluster` | `--pull-secret` (required), `--name`, `--port`, `--fresh`, `--verbose`, `--image`, `--version`, `--release-image`, `--registry`, `--arch` | Run it in a managed container and wait for it to be ready. |
| `delete cluster` | `--name`, `--preserve-state` | Remove a cluster and its state; `--preserve-state` keeps the state. |
| `get clusters` | | List the clusters with their port, release, and live readiness. |
| `get kubeconfig` | `--name` | Print the path to a cluster's admin kubeconfig. |
| `cleanup` | `--name`, `--all`, `--cache` | Remove cluster state and, with `--cache`, everything derived and rebuildable: the image cache and the minted container images. |
| `version` | | Print the version the binary was built with. |

## Where things live

- State: `~/.local/state/coreshift/clusters/<name>/` by default
  (`$XDG_STATE_HOME/coreshift/clusters/<name>`). It holds the
  generated certificates and kubeconfigs, the etcd data, and the
  child logs, and it is reused across runs whatever port the API
  server is served on.
- Logs: one file per child under the `logs` folder of the state
  directory.
- Image cache: `~/.cache/coreshift/` (`$XDG_CACHE_HOME/coreshift`),
  shared by all runs regardless of cluster or mode.

Container mode lands in the same places: the container bind-mounts
the state directory and the cache. The rules for reusing and removing
state are in [Operations](docs/operations.md#cluster-state).

## Choosing a release

Four flags pick the release; the defaults cover the common case.
`--version X.Y` pins a minor, `--version X.Y.Z` an exact release,
`--version newest` the registry's absolute newest, and
`--release-image` an exact image reference with no registry queries
at all; `--registry` and `--arch` steer the resolution. With none
set, the newest release within the newest verified line is picked —
the newest OpenShift minor CoreShift has verified its stand-ins
against, 5.0 today — so a brand-new, unverified major cannot arrive
silently; asking beyond it works, with a plain warning first. A state
directory that already records a release boots it again as-is. The
detail, including the nightly drift alarm that probes the frontier,
is in [Operations](docs/operations.md#choosing-a-release).

## Documentation

- [How it works](docs/how-it-works.md) — the thirteen supervised
  processes, the version operator growing the cluster from the
  payload, the seeds and the boot cache, the gated boot, the ready
  summary, and the container image.
- [Operations](docs/operations.md) — logging, logging in, failed
  boots, delete and cleanup semantics, the cache, and choosing a
  release in detail.

When something fails, start with
[troubleshooting](docs/operations.md#troubleshooting).
