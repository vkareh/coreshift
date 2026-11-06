# Operating CoreShift

The day-to-day of a running cluster: what a run's output means and
where it lands, how to log in, how to read a failure, and how to take
a cluster apart. For what runs underneath, see
[How it works](how-it-works.md); for the pitch and the commands, the
[README](../README.md).

## Logging

By default a run is quiet: one progress line as each child starts and
stops, and a summary once the control plane is ready. Host mode's
summary:

```
Control plane ready
  Name:         coreshift
  Release:      quay.io/openshift-release-dev/ocp-release:5.0.1-x86_64
  API endpoint: https://127.0.0.1:6443
  Kubeconfig:   /home/you/.local/state/coreshift/clusters/coreshift/admin.kubeconfig
  Context:      admin@coreshift
```

Container mode's summary matches it plus one line — `Container
coreshift-coreshift is running in the background` — and the container
keeps running in the background when the create command returns;
closing the terminal does not stop it. The create command streams the
container's boot output live while it waits for readiness, and the
container's own boot output is kept by podman: `podman logs
coreshift-<name>` replays it. What the summary means — what is ready
and what converges behind it — is covered in
[How it works](how-it-works.md#the-ready-summary).

By default child logs are captured to one file per child under the
`logs` folder of the state directory, written through the container's
bind mount in container mode. `--verbose` streams each child's logs
live to the terminal instead, prefixed with the child's name.

A child whose start waits on objects the version operator delivers
shows the wait as one progress line while it lasts and one timing
line once it passes; see
[the gated boot](how-it-works.md#the-gated-boot).

## Logging in

Logins use the standard OpenShift htpasswd flow. Until an identity
provider exists the OAuth server denies every login, so first create
a user and wire it up through the admin kubeconfig while the control
plane is running (the `htpasswd` tool comes from `httpd-tools` or
`apache2-utils`):

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

One honest caveat: in a full cluster the authentication operator
watches the OAuth object and rolls the OAuth server live when it
changes. Here the identity providers are rendered into the OAuth
server at startup, so changes take effect on the next restart. Once
the objects above are in place, restart the control plane —
`coreshift run cluster` again in host mode, or `coreshift delete
cluster --preserve-state` followed by `coreshift create cluster` in
container mode — then log in:

```
oc login https://127.0.0.1:6443 --certificate-authority=/home/you/.local/state/coreshift/clusters/coreshift/ca.crt -u developer -p password
```

## Reading a failed boot

When a child fails, the run tears down and the error names the failed
component, quotes the tail of its log, and points at the full log
file, so a failure can be debugged without rerunning. If a child dies
while startup is still waiting on a sibling, the error comes from the
child that died. A boot waiting on an object that never arrives fails
after ten minutes, naming what was missing and pointing at the
operator's log; see [the gated boot](how-it-works.md#the-gated-boot).

In container mode, a container that exits before the control plane
becomes ready fails the create: the error quotes the tail of the
container's log, points at the full logs in the state directory, and
the container is removed while the state is preserved. Ctrl-C during
the wait aborts the same way. A failure caused by the state directory
refusing reuse says so and names `--fresh` and a different `--name`
as the remedies.

## Cluster state

The name is the cluster's identity, `coreshift` by default: its state
lives in `clusters/<name>` under the state root
(`~/.local/state/coreshift/clusters/<name>/` by default,
`$XDG_STATE_HOME/coreshift/clusters/<name>`), the container is called
`coreshift-<name>`, and the name picks which cluster's state a run
boots — passed through to the run inside the container. Create
refuses a taken name, so a second cluster needs its own name, with
its own `--port` to publish on.

The state directory holds the generated certificates and kubeconfigs,
the etcd data, and the child logs, and it is reused across runs
whatever port the API server binds. Each directory records the
cluster's name and the release image that created it in its marker,
so it is self-describing. A newer patch of the same minor stream
reuses the directory and upgrades it in place, no question asked —
the same move a real OpenShift cluster makes on a patch update; a
different minor stream,
or an older patch, refuses, because etcd data only moves forward
within its stream. Run under another name to keep both. On a terminal
the refusal offers to remove the directory and start fresh;
everything in it is lost, including anything created through the API,
and only an explicit yes goes ahead. Without a terminal the refusal
names `coreshift cleanup` as the way to remove the directory from a
script. `--fresh` removes the state directory before starting,
without asking — the container-mode answer to a state directory that
refuses reuse. A run with no release flags reuses the release the
directory records instead of floating to the newest, and prints a
one-line note when a newer patch of that stream exists, since the
directory can be upgraded in place with `--version`. Create applies
the reuse rule before building or starting anything, so a doomed run
fails before an image is minted.

The port, 6443 by default, is not part of the cluster's identity: the
state directory does not depend on it, and a cluster re-created on a
different port reuses its state warm — with the kubeconfig's server
URL rewritten on boot to match the port in use — but etcd and the other components
always listen on the same fixed ports, so `--port` is for avoiding a
port collision, not for running two instances side by side. The
reserved ports, refused up front, are listed under
[Troubleshooting](#troubleshooting). In container mode the API is
published on `127.0.0.1:<port>` only, and the OAuth login endpoint on
the port after it, so `oc login` from the host can follow the API
server's discovery document to the OAuth server, and exposing either
to the network stays an explicit act. Create refuses an API port
whose follower is taken or reserved, and a port something already
listens on.

The pull secret is stored in cluster state, as the `pull-secret`
Secret in the `openshift-config` namespace — the global pull secret a
real cluster keeps, seeded by the startup from the file the run was
given. It lives in the state directory's etcd data and is destroyed
with it, by `delete cluster` or `coreshift cleanup`.

`coreshift get clusters` lists the clusters: name, published port,
recorded release, and readiness. Readiness requires the cluster's
container running and its API server answering a live probe: a
stopped container is not ready without probing, because the port it
published can since have been taken by another cluster, and a running
but booting or wedged one fails the probe. With no clusters it prints
`No clusters`. `coreshift get kubeconfig` prints the path to the
named cluster's admin kubeconfig — the same file host mode writes —
and refuses when no state exists for the name, listing the clusters
that do:

```
./coreshift get kubeconfig
/home/you/.local/state/coreshift/clusters/coreshift/admin.kubeconfig
```

The container bind-mounts the named cluster's state directory and the
shared image cache into itself, so everything lands exactly where
host mode puts it, and the modes share it: a release cached for one
is cached for the other, and a host-mode run of the same cluster name
reuses its certificates and state. The podman network `coreshift` —
the bridge network cluster containers run on — is created by the
create command and kept until `coreshift cleanup --cache` finds
nothing using it.

## Delete and cleanup

`coreshift delete cluster` removes the named cluster and everything
it owns: its container and its state directory — the certificate
authority keys, the admin credentials, the stored pull secret, and
the etcd data. Stopping the container tears the control plane down in
reverse startup order, like a host-mode shutdown. The command says
what was removed and where the state was, and leaves nothing behind:
no containers, no state directory, not even an empty clusters tree.
Keeping any of it is the explicit act.

```
./coreshift delete cluster
```

`--name` selects a cluster other than the default `coreshift`.

`--preserve-state` keeps the state directory; the output says the
state was preserved, with its path. This is the warm re-creation
workflow: the next create with the same name — on whatever port —
boots from the same certificates and etcd data, pulling nothing. The
shared image cache is kept either way.

A cluster whose container is already gone but whose state remains is
reported as not running, and the state follows the same rule. Failed
creates never remove state — it stays behind for inspection — so
delete is the only command that removes it by intent; `coreshift
cleanup` is the one for state orphaned by crashes or older deletes.

`coreshift cleanup` clears leftover state and cached images. It
removes the default cluster's state directory, so the next run starts
fresh: new certificates, empty etcd, nothing seeded. Delete already
removes a cluster's state with it, so cleanup is for what a crash or
an older delete left behind, and for state a delete kept on purpose.
There is no confirmation prompt; the command is explicit and safe for
scripts. Both modes keep their state in the same directories, so
cleanup also removes a container-mode cluster's state.

```
./coreshift cleanup
```

- `--name N` removes that cluster's state directory instead.
- `--all` removes every cluster's state: the whole `clusters` tree,
  and nothing else under the state root, so unrelated directories
  kept there survive.
- `--cache` additionally removes everything derived and
  rebuildable: the shared image cache, and the minted container
  images from podman's store — the `coreshift` tags — and the podman
  network cluster containers run on, when nothing uses it anymore.
  All are safe to drop: the cache is keyed by image digest and
  rebuilt by re-pulling, the images are minted from the binary again
  in seconds, and the network is re-created by the next cluster. The
  base images they were built from, such as the UBI bases, are shared
  with everything else and stay. A cluster whose state was kept
  simply mints its image again on re-create.

Each directory is listed with its size on disk before it is deleted,
each removed image is named, and the total freed is reported after.
Because images share layers with their bases, the freed space is
measured as the drop in podman's store, not as the sum of the removed
images' sizes. A missing directory is reported and is not an error.
If a control plane is running, the command refuses and asks to stop
it first, because cleaning under a live instance would pull its data
out from under it.

## The cache

Extracted control plane binaries, the release image's manifest of
component image references, and the release payload's manifest sets
are cached under `~/.cache/coreshift/`
(`$XDG_CACHE_HOME/coreshift`), keyed by image digest and shared by
everything regardless of mode or cluster. The first run pulls the
release image and its component images; a second run of the same
pinned release pulls nothing from the registry and starts straight
from the cache.

The minted container images are the same kind of thing:
deterministic from the binary and rebuilt in seconds. `coreshift
cleanup --cache` therefore removes them from podman's store along
with the cache directory, making one flag mean everything derived and
rebuildable; the base images they were built from stay. A cold cache
makes the container pull the whole release payload before booting, so
the first create is slow; the readiness wait allows half an hour for
it.

## Choosing a release

Four flags pick the release; the defaults cover the common case. With
none of them set, the newest release within the newest verified line
for the machine's architecture is resolved from the registry — unless
the state directory already records a release, which then boots again
as-is: a warm re-create, not a surprise upgrade. When the registry
holds a newer patch of that release's stream, the run says so in one
line and names `--version` as the flag that upgrades the state in
place:

```
./coreshift run cluster --pull-secret ~/pull-secret
```

Support is defined by what CoreShift has verified. The newest
verified line is the newest OpenShift minor the stand-ins have been
checked against — 5.0 today — and the unversioned default stays
inside it, so a brand-new, unverified major cannot arrive silently.
Asking for a release beyond the line works; the run just prints a
plain warning first:

```
warning: 5.1.0 is beyond the newest verified line, 5.0 — it may not work
```

`--version newest` asks for the registry's absolute newest release,
ignoring the line. It exists for the nightly drift alarm in CI, which
probes the frontier so a new release is noticed: it boots the
release, soaks it, and checks its pod security labeling, its login
chain, its aggregated API, and its node join acceptance before
shutting it down; everything else should stay on the verified
default. Raising the line is deliberate work, done together with the
stand-in updates a new line needs, and it is one constant in the
code.

`--version X.Y` pins a minor: the newest patch release of that stream
is resolved.

```
./coreshift run cluster --pull-secret ~/pull-secret --version 4.22
```

Pinning exact comes in two strengths. `--version X.Y.Z` resolves
exactly that version from the registry:

```
./coreshift run cluster --pull-secret ~/pull-secret --version 4.22.8
```

`--release-image` pins the image reference itself and skips all
registry queries — not even a tag listing happens — so use it when a
run must be reproducible bit for bit, or when the registry cannot be
reached for resolution:

```
./coreshift run cluster --pull-secret ~/pull-secret --release-image quay.io/openshift-release-dev/ocp-release:4.22.8-x86_64
```

`--registry` resolves from a private mirror, any v2-compliant
repository, instead of the default
`quay.io/openshift-release-dev/ocp-release`:

```
./coreshift run cluster --pull-secret ~/pull-secret --registry mirror.corp.example/openshift-release-dev/ocp-release
```

`--arch` defaults to the machine's architecture, as `uname -m`
reports it; pass e.g. `--arch aarch64` to resolve a payload for
another architecture. It combines freely with `--version` and
`--registry`. The startup output names the release image the run
resolved.

`coreshift create cluster` takes the same four flags and passes them
through to the run inside the container.

## Troubleshooting

**A port is taken.** The control plane's components always listen on
the same fixed ports; `--port` moves only the API server's. The
reserved ports, refused up front, are 2379, 2380 (etcd), 8443, 8444,
8445, 8446, 9191, 9192, and 10357 (the OpenShift components). Create
also refuses a port something already listens on, and an API port
whose OAuth follower is taken or reserved.

**A boot fails.** The error names the failed component, quotes the
tail of its log, and points at the full log file; see [Reading a
failed boot](#reading-a-failed-boot).

**The state directory refuses reuse.** A cross-stream move or a patch
downgrade refuses. Run under another name to keep both, answer the
terminal's fresh-start question, or use `--fresh` in container mode;
see [Cluster state](#cluster-state).

**Cleanup refuses to run.** If a control plane is running, the
command asks to stop it first, because cleaning under a live instance
would pull its data out from under it; see
[Delete and cleanup](#delete-and-cleanup).

**podman is missing.** Every container-mode command uses podman;
docker is not supported. When podman is missing, each command fails
with a clear error naming it, such as `podman is required to create
clusters`.
