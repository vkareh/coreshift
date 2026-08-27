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
