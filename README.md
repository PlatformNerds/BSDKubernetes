# BSDKubernetes

**Goal:** run Kubernetes *natively* on FreeBSD worker nodes, so the normal Kubernetes ecosystem
(kubectl, Helm, controllers, operators, GitOps) works against them. Pods run as **jails** by default,
as **Linux containers** where the Linuxulator can handle them, and inside **bhyve microVMs** where it can't.

> Status (Nov 2026): a two-node FreeBSD-native cluster passes **437/446 Kubernetes conformance tests (98.0%)**; the 9 failures are 6 in-place resize tests that read Linux cgroups and 3 `[LinuxOnly]` tests (run 17, dual-stack, research/07 and 26). Experimental, not production software.

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
| [research/07-conformance.md](research/07-conformance.md) | **Conformance (2 nodes): 437/446 (98.0%)**; runs 4-17, sweeps beyond the conformance set (per SIG, NodeConformance, Serial, Slow, NFS, DRA 92/93), remaining gaps |
| [research/08-allow-pf.md](research/08-allow-pf.md) | **Kernel K14 `allow.pf` implemented:** kube-proxy runs as a normal pod (kernel + ocijail + CRI-O) |
| [research/09-host-clusterip.md](research/09-host-clusterip.md) | Host-network clients and ClusterIPs on pf |
| [research/10-linux-images.md](research/10-linux-images.md) | Linux container images on FreeBSD nodes via the Linuxulator |
| [research/11-bsd-placement.md](research/11-bsd-placement.md) | bsd-placement: unmodified Linux manifests on FreeBSD nodes |
| [research/12-pod-teardown.md](research/12-pod-teardown.md) | Deleted pods lingered: volumes held by stopped containers |
| [research/13-ingress-nginx.md](research/13-ingress-nginx.md) | ingress-nginx unmodified |
| [research/14-gateway-api.md](research/14-gateway-api.md) | Gateway API with Envoy Gateway |
| [research/15-cgroup-view.md](research/15-cgroup-view.md) | A cgroup v2 view of rctl for Linux containers |
| [research/16-nodejs.md](research/16-nodejs.md) | Node.js under the Linuxulator on arm64: three kernel bugs |
| [research/17-pod-os-field.md](research/17-pod-os-field.md) | Pods with `spec.os.name: linux` on FreeBSD nodes; in-place resize |
| [research/18-proc-pid1-fd.md](research/18-proc-pid1-fd.md) | `/proc/1` and `/proc/<pid>/fd` for Linux containers |
| [research/19-traffic-policy.md](research/19-traffic-policy.md) | Service traffic policies in the pf kube-proxy |
| [research/20-volumes-hostports.md](research/20-volumes-hostports.md) | Volumes (fsGroup, hostPath submounts and sockets, NFS servers and dynamic NFS, block volumes, growing file systems, device nodes, the subPath escape fix, leaked CSI volume directories, node log rotation), host ports |
| [research/21-rmlock-epoch-deadlock.md](research/21-rmlock-epoch-deadlock.md) | Node freeze under pod churn: an rmlock lost wakeup (kernel fix) |
| [research/22-networkpolicy.md](research/22-networkpolicy.md) | NetworkPolicy with pf (bsd-netpol) |
| [research/23-sctp.md](research/23-sctp.md) | SCTP: Services, NetworkPolicy, HostPorts |
| [research/24-csi.md](research/24-csi.md) | **CSI on FreeBSD:** a mount proxy (like Windows' csi-proxy) instead of mount propagation; csi-hostpath e2e 36/36 (filesystem and block volumes), 87/87 with Slow, Serial 9/9, snapshots 8/8, CSI Mock 47/47 |
| [research/25-loadbalancer.md](research/25-loadbalancer.md) | LoadBalancer Services on bare metal (bsd-lb: address pool, ARP speaker via bpf); session affinity for host clients; LoadBalancer e2e 15/18 |
| [research/26-dual-stack.md](research/26-dual-stack.md) | IPv4/IPv6 dual-stack: converting the cluster, IPv6 NodePort from the node (kernel fix), kube-proxy's two families serialized on pf; IPv6DualStack e2e 23/23; SCTP Services and multi-homed pods (limit) |
| [patches/README.md](patches/README.md) | **Every patch, per project** (kubernetes, CRI-O, ocijail, FreeBSD, CNI plugins, nfs-provisioner, unfs3) |
| [research/UPSTREAM-PINS.md](research/UPSTREAM-PINS.md) | Exact upstream commits the research was done against |
| [patches/kubernetes/v1.36.1/](patches/kubernetes/v1.36.1/) | Doug Rabson's FreeBSD patch series for kubelet, kube-proxy and kubeadm (Apache-2.0, credit: dfr), and ours on top (`bsdk8s/`) |

## Reproduce the source workspace

```sh
scripts/fetch-upstream.sh ~/bsdk8s/upstream   # shallow-clones FreeBSD/OpenBSD/NetBSD/DragonFly, k8s, containerd, …
scripts/build-freebsd-k8s.sh                  # applies the patch series to v1.36.1 and cross-compiles for FreeBSD
```
