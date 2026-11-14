# BSDKubernetes

**Goal:** run Kubernetes *natively* on FreeBSD worker nodes, so the normal Kubernetes ecosystem
(kubectl, Helm, controllers, operators, GitOps) works against them. Pods run as **jails** by default,
as **Linux containers** where the Linuxulator can handle them, and inside **bhyve microVMs** where it can't.

> Status: research and planning (Nov 2026). Nothing here is production software.

## Start here

| Doc | What it is |
|---|---|
| [docs/PLAN.md](docs/PLAN.md) | **Master plan.** Architecture, phases, blockers and solutions, what to drop, kill criteria |
| [docs/MAC-DEV-VM.md](docs/MAC-DEV-VM.md) | **FreeBSD arm64 dev VM on Apple Silicon** (QEMU+HVF), arm64 control-plane images, running the demo cluster |
| [docs/FORKS.md](docs/FORKS.md) | **Fork-first workflow.** Public mirror-forks (kubernetes, cri-o, ocijail, cni-plugins, kubernetes-demo), branches, sync, upstreaming |
| [research/00-strategy-prior-art.md](research/00-strategy-prior-art.md) | Prior art (Doug Rabson's working demo), upstream politics, which BSD, strategy options |
| [research/01-kubelet-cri-oci.md](research/01-kubelet-cri-oci.md) | kubelet / CRI / OCI runtime port; cross-compile results |
| [research/02-resource-isolation.md](research/02-resource-isolation.md) | cgroups v2 vs RACCT/RCTL/cpuset; 14 kernel proposals (K0–K13) |
| [research/03-networking.md](research/03-networking.md) | CNI on VNET, pf-based kube-proxy, NetworkPolicy, eBPF/Cilium alternatives |
| [research/04-linux-compat-bhyve.md](research/04-linux-compat-bhyve.md) | Linuxulator gap analysis; bhyve as a Kata-style microVM runtime |
| [research/05-storage-security-controlplane.md](research/05-storage-security-controlplane.md) | ZFS snapshotters, CSI, security mapping, control plane, ecosystem |
| [research/06-resource-limits.md](research/06-resource-limits.md) | **Implemented:** pod CPU/memory limits → rctl. Measured rctl semantics (`memoryuse:sigkill`, `pcpu` accuracy) |
| [research/UPSTREAM-PINS.md](research/UPSTREAM-PINS.md) | Exact upstream commits the research was done against |
| [patches/kubernetes/v1.36.1/](patches/kubernetes/v1.36.1/) | Doug Rabson's FreeBSD patch series for kubelet, kube-proxy and kubeadm (Apache-2.0, credit: dfr) |

## Reproduce the source workspace

```sh
scripts/fetch-upstream.sh ~/bsdk8s/upstream   # shallow-clones FreeBSD/OpenBSD/NetBSD/DragonFly, k8s, containerd, …
scripts/build-freebsd-k8s.sh                  # applies the patch series to v1.36.1 and cross-compiles for FreeBSD
```
