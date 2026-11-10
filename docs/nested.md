# Running CoreShift inside a Kubernetes cluster

The control plane can run as one ordinary unprivileged pod inside any
Kubernetes cluster — the shape HyperShift gives its hosted control
planes. kind stands in for the host cluster throughout this page, but
nothing here is kind-specific: any cluster that can run a pod and bind
a persistent volume works identically. You reach the control plane
from your laptop through `kubectl port-forward` and the admin
kubeconfig: verified TLS, full cluster-admin, the kubeadmin
experience. It takes no Service, no new flags, and no code changes —
the container image CoreShift already mints, its baked-in entrypoint,
and two environment variables do all the work.

For the modes the binary ships as commands, see the
[README](../README.md); for what runs underneath, [How it
works](how-it-works.md).

## Getting the image

One honest wrinkle first: the container image is not published
anywhere. It is minted by `coreshift create cluster` on your machine,
into your podman store, and nothing else produces it. So the flow is a
throwaway create — pointed at a temporary state home, so your real
cluster state is never touched — followed by a delete that removes the
throwaway's state and leaves the image behind. From a checkout,
`make build` gives you the binary:

```
mkdir -p /tmp/coreshift-mint
XDG_STATE_HOME=/tmp/coreshift-mint ./coreshift create cluster --pull-secret ~/pull-secret
XDG_STATE_HOME=/tmp/coreshift-mint ./coreshift delete cluster
```

The create prints the image it built — `Building container image
localhost/coreshift:<tag>` — and the tag is the binary's version plus
a short digest of its content, so your build has its own. If the
image is already in your store the build line is skipped; `podman
images localhost/coreshift` lists the tag either way. The delete
removes the throwaway's state; the image stays in podman's store. (If
you would rather not load images around, `podman push` the minted
image to a registry your cluster can pull from and use that reference
in the manifest below instead.)

Create the host cluster, and load the image into it:

```
kind create cluster --name coreshift-nested
kind load docker-image localhost/coreshift:<tag> --name coreshift-nested
```

One kind-under-podman detail before the manifest: a kind node is
itself a podman container, capped at 2048 processes by default, and
the node hands each pod only a slice of that budget. The control
plane's thirteen processes and their threads settle around 350 and
cross the pod's slice, so the pod dies partway through its boot on a
thread-creation failure. Raise the node's cap before applying
anything — it takes effect immediately:

```
podman update --pids-limit 8192 coreshift-nested-control-plane
```

With 8192 the pod's slice is about 1200, roughly three times the
need. A cluster on real nodes carries no such cap; the limit is an
artifact of the node itself being a container.

## The manifest

Everything is one file: a namespace, the pull secret, a claim for the
state, and the deployment. Save it as `coreshift.yaml`, paste your
pull secret's JSON over the placeholder line, and put your image's tag
in the deployment:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: coreshift
---
apiVersion: v1
kind: Secret
metadata:
  name: pull-secret
  namespace: coreshift
stringData:
  pull-secret: |
    PASTE YOUR PULL SECRET'S JSON HERE, INDENTED
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: coreshift-state
  namespace: coreshift
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 2Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: coreshift
  namespace: coreshift
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: coreshift
  template:
    metadata:
      labels:
        app: coreshift
    spec:
      automountServiceAccountToken: false
      containers:
      - name: coreshift
        image: localhost/coreshift:<tag>
        imagePullPolicy: IfNotPresent
        args: ["--pull-secret", "/secret/pull-secret"]
        env:
        - name: XDG_STATE_HOME
          value: /var/lib/coreshift/state
        - name: XDG_CACHE_HOME
          value: /var/lib/coreshift/cache
        volumeMounts:
        - name: state
          mountPath: /var/lib/coreshift/state
        - name: cache
          mountPath: /var/lib/coreshift/cache
        - name: pull-secret
          mountPath: /secret
          readOnly: true
        resources:
          requests:
            cpu: 500m
            memory: 2Gi
          limits:
            memory: 4Gi
      volumes:
      - name: state
        persistentVolumeClaim:
          claimName: coreshift-state
      - name: cache
        emptyDir: {}
      - name: pull-secret
        secret:
          secretName: pull-secret
```

What the pieces do:

- The image's entrypoint is already `coreshift run cluster`, the same
  command as host mode, and the image carries the container marker
  that makes the API server bind every interface, so a forwarded port
  reaches it. The `args` append the pull secret flag to that
  entrypoint, nothing more.
- `XDG_STATE_HOME` points at the persistent claim, so certificates,
  kubeconfigs, and etcd data survive the pod. `XDG_CACHE_HOME` points
  at an emptyDir, the same rebuildable cache the host modes keep.
- The resources reflect a measured footprint: a little under 2 GiB
  resident once ready.
- There is no Service, because port-forwarding to a deployment needs
  none.

For a fully ephemeral control plane, swap the claim for a second
emptyDir: the state dies with the pod, and every fresh pod boots from
scratch.

## The recipe

Apply, then follow the logs:

```
kubectl apply -f coreshift.yaml
kubectl -n coreshift logs -f deployment/coreshift
```

The pod reports Running within seconds — the container's main process
is the whole boot, so Running only means the boot has started. The
first boot pulls the entire release payload into the pod's own cache
before the children start, the same one-time cost as a first create on
a host: minutes on a fast line, up to half an hour on a slow one.
Wait for the summary:

```
Control plane ready
  Name:         coreshift
  Release:      quay.io/openshift-release-dev/ocp-release:5.0.1-x86_64
  API endpoint: https://127.0.0.1:6443
  Kubeconfig:   /var/lib/coreshift/state/coreshift/clusters/coreshift/admin.kubeconfig
  Context:      admin@coreshift
```

In one terminal, forward the API port:

```
kubectl -n coreshift port-forward deployment/coreshift 6443:6443
```

In another, pull the admin kubeconfig out of the pod's state and use
it:

```
kubectl -n coreshift exec deployment/coreshift -- cat /var/lib/coreshift/state/coreshift/clusters/coreshift/admin.kubeconfig > coreshift-admin.kubeconfig
export KUBECONFIG=$PWD/coreshift-admin.kubeconfig
oc get ns
oc get projects
```

`oc get projects` answers from the OpenShift API — the aggregated
servers run inside the same pod — so this is the full API surface,
not a kubectl lookalike.

The TLS verifies with no flags and no exceptions. The forward presents
the API at localhost, which the serving certificate names alongside
127.0.0.1, and the kubeconfig embeds both the certificate authority
that signed the server and the admin client certificate, so `oc`
proves the server's identity and authenticates as cluster-admin in
one step.

## Caveats

- **The tether.** The port-forward lives in one terminal and dies
  with it — Ctrl-C, a dropped connection, a sleeping laptop. Any
  client on the machine can use it while it runs, but nothing else
  reaches the control plane; re-run the forward to reconnect.
- **State.** On the claim, everything survives: a restarted pod
  re-boots warm from the same certificates and etcd data, and
  anything created through the API is still there. On an emptyDir it
  is all ephemeral. The cache is an emptyDir either way, so every
  newly created pod re-pulls the release payload once.
- **Image versioning.** The tag encodes the binary's version and a
  digest of its content: rebuild the binary and the next create mints
  a new tag. After a rebuild, load the new image and update the
  manifest; the running pod keeps its old image until then.
- **One control plane per pod.** The components listen on fixed ports
  and the state has one writer, so replicas stays 1. To run several
  control planes, run several pods, each with its own claim.

## What this does not give

Two things a HyperShift guest gets are out of reach here, and both
wait on the same work. A Service in front of the pod would serve the
API inside the host cluster — at `https://coreshift.coreshift.svc` —
but verified TLS on that address needs the serving certificate to
name it, and that certificate work, opt-in exposure with matching
certificate names, is what the roadmap's Phase 9 scopes. `oc login`
needs the same work plus the OAuth discovery chain: the discovery
document advertises the OAuth server on the port after the API port,
which a single forwarded port does not carry. Until that phase lands,
the admin kubeconfig through the forward is the supported path; do
not bridge the gap with an insecure flag, which is why this boundary
is written down.
