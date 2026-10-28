# The rhel-coreos payload image

Facts about the node-side image the worker image will be built `FROM`,
recorded so the worker image task does not have to rediscover them.
Everything below was measured on the release the image machinery
resolves with nothing pinned: `quay.io/openshift-release-dev/ocp-release:4.22.20-x86_64`
(digest `sha256:de2f336eccccd7fb1ea129be52c9f0707c4f76e22c470c044fcbbe25cc77f813`).
Component pullspecs come from the release image's component map in the
image cache; the image itself was pulled with the existing machinery
and the pull secret, and exercised under podman.

## Which rhel-coreos image

The component map carries four rhel-coreos-named components:

| component | pullspec |
| --- | --- |
| rhel-coreos | `quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:e5c3fcc3825ce32152577a868d78f9c6bd9268d0439b842d565a74fab52f1d16` |
| rhel-coreos-extensions | `quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:0ea216443e6659e9d9e26d2acf7c59e65bb6072e54230b6649fc217319f04a2d` |
| rhel-coreos-10 | `quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:d6c64e04248735746b2cb036804affb8b48a9236a61641c2525c6f973923e200` |
| rhel-coreos-10-extensions | `quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:32f0f9e5a87bf26b2d4dea5e9f15d04b546d329a481910340f35e264a6cbe21f` |

The chosen image is **rhel-coreos**, the first row, digest-pinned.
Why: the release's own boot artifact, the `machine-os-images`
component that carries the live install ISO, is built from the
rhel-9.8 stream, and rhel-coreos is RHCOS `9.8.20261023-0 (Plow)`
on `platform:el9` — the same OS the release installs on its nodes.
rhel-coreos-10 is RHCOS `10.2.20261022-1 (Coughlan)` on `platform:el10`,
a parallel stream for the newer base. The machine-config-operator's
node-OS configmap also names the rhel-coreos and rhel-coreos-extensions
components as the node's OS and extension layers. The 9-based image
additionally ships the whole node runtime stack in its base layer,
while the 10-based one ships only kubelet, crio, and crun — no runc
and no CNI plugins.

## Node binaries

Versions below are the commands' own output, executed inside the
image running as a plain container (see the runnability check).

| binary | path in image | version |
| --- | --- | --- |
| kubelet | `/usr/bin/kubelet` | `Kubernetes v1.35.9` |
| crio | `/usr/bin/crio` | `crio version 1.35.10-3.rhaos4.22.git9383502.el9` |
| runc | `/usr/bin/runc` | `runc version 1.4.2` |
| crun | `/usr/bin/crun` | `crun version 1.27` |
| conmon | `/usr/bin/conmon` | (symlinked from `/usr/libexec/crio/conmon`) |
| CNI plugins | `/usr/libexec/cni/` | `containernetworking-plugins 1.9.0` (RPM) |

The image's kubelet matches the release's control plane binaries
(hyperkube's kube-apiserver reports the same `v1.35.9`), so building
the worker image `FROM` this image keeps node-side versions
self-matched to the release, as planned.

The CNI plugin directory holds 19 plugins: bandwidth, bridge, dhcp,
dummy, firewall, host-device, host-local, ipvlan, loopback, macvlan,
portmap, ptp, sample, sbr, static, tap, tuning, vlan, vrf. Their
`--version` output reports `version unknown` — the plugins do not
embed a version string; the RPM version above is the honest one.

**containerd is absent.** This release's node CRI runtime is CRI-O:
the machine-config-operator's kubelet unit requires `crio.service`
and runs kubelet with
`--container-runtime-endpoint=/var/run/crio/crio.sock`. The verbatim
result of asking for containerd inside the image:

    Error: crun: executable file `/usr/bin/containerd` not found: No such file or directory: OCI runtime attempted to invoke a command that was not found

Containerd is not in the extensions images either; those carry kernel
and driver RPMs under `/usr/share/rpm-ostree/extensions/`. The worker
image task should therefore run crio, not containerd.

## Other node-side contents

- The image is built to boot (`Cmd=/sbin/init`, labels
  `ostree.bootable=1` and `containers.bootc=1`,
  `com.coreos.stream=rhel-9.8`) but runs fine as a plain container.
- A CRI-O systemd unit ships in the image at
  `/usr/lib/systemd/system/crio.service`, with config at
  `/etc/crio/crio.conf`.
- The machine-config-operator's rendered CRI-O config for real nodes
  sets `default_runtime = "crun"` with both crun and runc configured,
  `pause_image` to the payload's `pod` component
  (`quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:e5fc73fc66cd2ff4e781d9af380348be759b6917c3703325d340669c3764e136`,
  command `/usr/bin/pod`), the pull secret at
  `/var/lib/kubelet/config.json`, the CNI network directory at
  `/etc/kubernetes/cni/net.d/` (present in the image, empty), and the
  CNI plugin directory at `/var/lib/cni/bin` (not in the image; the
  plugins ship in `/usr/libexec/cni`, and real nodes get
  `/var/lib/cni/bin` populated at runtime).
- `/etc/cni` does not exist; `/etc/kubernetes/cni/net.d` is the
  OpenShift CNI config directory.
- SELinux targeted policy is present
  (`/etc/selinux/targeted/policy.33`), and the rendered CRI-O config
  runs with `selinux = true`.
- podman 5.8.2, netavark 1.17.2, containers-common 5.8, and skopeo
  1.22.2 are in the base image.
- Real nodes run kubelet under `/usr/local/bin/kubenswrapper`
  (delivered by machine config, not present in the image); the
  wrapper is an nsenter helper, not something kubelet needs to start.

## Runnability check

The image starts under podman as a plain container with the
entrypoint overridden, and every node binary executed inside it:

    podman run --rm --entrypoint /usr/bin/kubelet <image> --version
    Kubernetes v1.35.9

    podman run --rm --entrypoint /usr/bin/crio <image> --version
    crio version 1.35.10-3.rhaos4.22.git9383502.el9

    podman run --rm --entrypoint /usr/bin/runc <image> --version
    runc version 1.4.2

    podman run --rm --entrypoint /usr/bin/crun <image> --version
    crun version 1.27

    podman run --rm --entrypoint /usr/libexec/cni/host-local <image> --version
    CNI host-local plugin version unknown
    CNI protocol versions supported: 0.1.0, 0.2.0, 0.3.0, 0.3.1, 0.4.0, 1.0.0, 1.1.0

A plain `bash` also runs and reads `/etc/os-release`. The plan of
building the worker image `FROM` this image survives contact with
reality: the image is runnable as a container and its node binaries
work inside it.
