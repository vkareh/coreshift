# CoreShift

A real OpenShift control plane from one binary — no VM, no cluster, no nodes.

- **Real**: version-matched components from the current release payload — real API server, real admission, real OAuth logins
- **Light**: rootless, disposable, API-ready in under a minute
- **For**: CI gating, operator dev loops, API compatibility testing

Nodes don't join and workloads don't run — [why](docs/how-it-works.md).

## Quickstart

You need a [pull secret](https://console.redhat.com/openshift/create/token) and podman. That's the whole prerequisite list.

```
go install github.com/vkareh/coreshift/cmd/coreshift@latest
coreshift create cluster --pull-secret ~/pull-secret
export KUBECONFIG=$(coreshift get kubeconfig)
oc get clusterversion
coreshift delete cluster
```

The first `create cluster` pulls the entire release payload — allow half an hour, once; every later boot is under a minute. To log in as a user instead of admin: [logging in](docs/operations.md#logging-in).

No podman, or prefer foreground? `coreshift run cluster --pull-secret ~/pull-secret` runs the same control plane in this terminal; Ctrl-C stops it. From source: `make build`.

## Commands

One rule: `run` means this terminal, `create` means a managed container, `delete` removes the cluster and its state, `cleanup` clears leftovers.

| Command | Flags | |
|---|---|---|
| `run cluster` | `--pull-secret` (required), `--name`, `--port`, `--verbose`, [release flags](#choosing-a-release) | Run the control plane in this terminal. |
| `create cluster` | `--pull-secret` (required), `--name`, `--port`, `--fresh`, `--verbose`, `--image`, [release flags](#choosing-a-release) | Run it in a managed container, wait for ready. |
| `delete cluster` | `--name`, `--preserve-state` | Remove the cluster and its state; `--preserve-state` keeps the state. |
| `get clusters` | | List clusters: port, release, live readiness. |
| `get kubeconfig` | `--name` | Print a cluster's admin kubeconfig path. |
| `cleanup` | `--name`, `--all`, `--cache` | Remove state; `--cache` also clears everything rebuildable (image cache, minted images). |
| `version` | | Print the build version. |

## The two modes

| | `run cluster` | `create cluster` |
|---|---|---|
| Runtime dependency | None (bare processes) | podman |
| Lifecycle | Foreground; Ctrl-C stops it | Background container |
| Side-by-side clusters | No (fixed component ports) | Yes — one per `--name`/`--port` |
| Best for | Machines without a container runtime | Clusters that outlive the terminal |

Both run the same thirteen real processes and answer identically at the API level.

Nodes cannot join: no join credentials ship. The machinery that would accept a node — the machine approver, certificate signing, the node-serving operators — is retained and waits for a real-node path this repository does not yet build; see [how it works](docs/how-it-works.md).

## Where things live

- State (certs, kubeconfigs, etcd, per-child logs): `~/.local/state/coreshift/clusters/<name>/`
- Image cache (shared by everything): `~/.cache/coreshift/`

`$XDG_STATE_HOME` and `$XDG_CACHE_HOME` are honored. State survives re-creates; `cleanup` removes it.

## Choosing a release

`--version X.Y` pins a minor, `--version X.Y.Z` an exact release, `--version newest` the registry's absolute newest, `--release-image` an exact reference with no registry queries. Unset: the newest release of the newest **verified** line — 5.0 today — so a brand-new major cannot arrive silently; requesting beyond it warns first. Recorded state re-boots its own release as-is. Detail, including the nightly drift alarm: [operations](docs/operations.md#choosing-a-release).

## Documentation

- [How it works](docs/how-it-works.md) — architecture
- [Operations](docs/operations.md) — logging in, failures, state, releases, troubleshooting
- [Nested in a cluster](docs/nested.md) — the control plane as a pod in a host cluster
