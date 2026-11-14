# BSDKubernetes: master plan

*Native Kubernetes on FreeBSD. Synthesised from the six research reports in `../research/` (Nov 2026).
Effort and timeline figures are estimates. Anything tagged **[verified]** was checked by building or reading code in this repo's workspace.*

---

## 1. The honest one-paragraph answer

**Native Kubernetes on FreeBSD is feasible, and part of it already works.**

Doug Rabson (FreeBSD committer, author of ocijail and the Podman/Buildah port) maintains `dfr/kubernetes-demo`. It brings up a **single-node, FreeBSD-only cluster on Kubernetes v1.36.1** using:
- CRI-O (partly upstream; the pod-level FreeBSD work is on dfr's `freebsd-wip-1.37.0` branch)
- ocijail as the OCI runtime
- a patched kubelet (~2.2 kLOC)
- a pf-based kube-proxy (~1 kLOC)
- a patched kubeadm (~0.6 kLOC)

We imported that patch series, merged it, fixed it so it builds without cgo, and **cross-compiled every Kubernetes binary for FreeBSD amd64 and arm64 [verified]**. `scripts/build-freebsd-k8s.sh` reproduces this from a clean v1.36.1 checkout.

So the hard part is not compiling. It is these five things:
1. **Resource isolation that actually means something.** cgroups semantics are not available on FreeBSD today.
2. **Linux images.** Almost nothing ships a FreeBSD image.
3. **Ecosystem assumptions:** `spec.os` only accepts linux/windows, Helm charts pin `kubernetes.io/os: linux`, and Cilium, Envoy and CSI expect Linux.
4. **Upstream policy.** FreeBSD is capped at "community-supported", with no official CI or builds.
5. **Finding enough people to maintain all of it.**

Every one of these has a workable answer below. Some answers involve **dropping capabilities**, and this plan says so explicitly.

## 2. Keeping you honest: what "native" can and cannot mean

| Claim you might want to make | Reality | Verdict |
|---|---|---|
| "Any Kubernetes controller/operator works" | **True for controllers.** They talk to the API server, which doesn't care about node OS. The Linux control plane works today, and the control plane also builds for FreeBSD. | ✅ |
| "Any Helm chart / container image works on FreeBSD nodes" | **False.** Images are Linux ELF. Charts pin `kubernetes.io/os: linux`, and the API only allows `spec.os.name ∈ {linux, windows}`. Kubernetes never solved this for Windows either. | ❌ Needs Linux compatibility (§5.4) and a scheduling story (§5.6). |
| "Cilium/eBPF works" | **No eBPF on FreeBSD.** Only classic BPF (packet capture), DTrace, pf, ipfw, netgraph and netmap. | ❌ Replace (§5.3) |
| "Same isolation guarantees as Linux pods" | **No.** Missing: CPU weights, real memory limits, per-pod OOM, PSI, seccomp, user namespaces. | ⚠️ Jails are best-effort. Use bhyve for hard isolation (§5.2). |
| "Kernel mods make it equal" | **Partially.** About 4–6 kLOC of kernel work gets an honest beta. Real memory-cgroup semantics touch the VM page daemon and are a moonshot. | ⚠️ |
| "Upstream Kubernetes will support it" | **Not soon.** It caps at community tier, with no release artifacts and no CI. Earlier FreeBSD PRs were closed with "write a KEP first". | ⚠️ Maintain a patch series and earn a KEP. |

## 3. Which BSD? FreeBSD, and it's not close

| | FreeBSD | DragonFly | NetBSD | OpenBSD |
|---|---|---|---|---|
| Container primitive | Jails, VNET, hierarchical, mature | Old jails, no VNET-grade isolation | None (rump kernels and secmodel are interesting but not containers) | None (pledge/unveil are per-process) |
| Hypervisor | bhyve (amd64 + arm64) | vkernel/nvmm | nvmm | vmm (limited) |
| Firewall | pf (VNET-aware, netlink API) + ipfw | pf (old) + ipfw | npf | pf (best syntax, but no VNET) |
| Storage | ZFS (clones are the overlayfs answer) | HAMMER2 | ZFS (older) | FFS only |
| Linux compat | Linuxulator (actively maintained) | Old | Old compat_linux | Removed |
| OCI/CRI ecosystem | ocijail, runj, CRI-O, containerd, Podman | None | None | None |

**Steal ideas, not bases:** OpenBSD's pf syntax and pledge model (as inspiration for a capability MAC module), NetBSD's rump kernels (for a userspace driver sandbox), DragonFly's variant symlinks. See `research/02` §4 and `research/00` §6.

## 4. Target architecture

```
                 ┌──────────────── Control plane (Phase 1: stock Linux; Phase 3: FreeBSD) ────────────────┐
                 │ kube-apiserver · etcd · scheduler · controller-manager · CoreDNS · your operators      │
                 └───────────────────────────────────────────┬────────────────────────────────────────────┘
                                                             │ (unchanged k8s API)
┌───────────────────────────────────── FreeBSD worker node ─┴───────────────────────────────────────────────┐
│ kubelet (FreeBSD build: rctl stats, jail container-manager, nullfs mounter)                               │
│   │ CRI                                                                                                   │
│ CRI-O (FreeBSD WIP, our fork)     ──RuntimeClass──►  ┌───────────────┬────────────────────┬─────────────┐ │
│                                                      │ jail (default)│ linux-jail         │ bhyve       │ │
│                                                      │ ocijail       │ ocijail+Linuxulator│ kata-bhyve  │ │
│                                                      │ FreeBSD images│ allow-listed Linux │ any Linux   │ │
│                                                      └──────┬────────┴─────────┬──────────┴──────┬──────┘ │
│  Pod sandbox = VNET jail ("pause") ── containers = child jails sharing its vnet  │  microVM per pod       │
│                                                                                                           │
│ Networking: bridge0 ⇄ epairN ⇄ pod VNET   │ kube-proxy "pf" mode (anchors + tables, atomic netlink txns)  │
│             overlay: VXLAN (flannel port) or BGP (FRR/BIRD)  │ NetworkPolicy: pf on the host-side epair   │
│ Storage: ZFS snapshotter (image layers = clones) · nullfs bind mounts · csi-proxy-bsd (host-side mounts)  │
│ Limits: RCTL/RACCT per pod jail + cpuset/domainset · K-series kernel patches (§6)                         │
│ Observability: DTrace + pf flow export (Hubble-like) · node_exporter · kubelet /stats/summary from racct  │
└───────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

## 5. Linux → FreeBSD mapping and blockers, with solutions

### 5.1 Node agent (kubelet / CRI / OCI): `research/01`
| Blocker | Severity | Solution | Effort |
|---|---|---|---|
| Upstream kubelet compiles but runs only stubs (cAdvisor exits at startup; no container manager or mounter) | High | **Rebase dfr's `freebsd-kubelet` series** (FreeBSD cadvisor from racct, container manager, mount-utils, subpath, emptyDir). Already in `patches/`. | Done for v1.36.1. Ongoing rebases. |
| kubelet FreeBSD build needed cgo, so no cross-compile | Low | **Fixed [verified]**: `patches/.../bsdk8s/0001`. Also fixes 5 bugs in rctl racct parsing. | Done |
| containerd CRI: netns is a stub on FreeBSD, so every non-hostNetwork pod fails | High | **Use CRI-O for the MVP**, proven by dfr's demo. Note: the pod-level FreeBSD bits (stats, devices, RunAsUser, sandbox validation, ~1.4 kLOC) live on `dfr/cri-o` `freebsd-wip-1.37.0`, **not upstream yet**. We carry them in `PlatformNerds/cri-o` `bsdk8s-1.37` (cross-compiles ✅) and upstream them. Revisit containerd + a shim for ocijail later. | MVP: carry the WIP; upstreaming ~1–2 months |
| runj is a self-described unaudited PoC (no Stats/Update, no parent jails) | Med | Standardise on **ocijail** (supports pods via parent-jail annotation). | – |
| `spec.os.name` only allows linux/windows | Med | Phase 1: leave `spec.os` empty and schedule with `kubernetes.io/os=freebsd` label, taint and RuntimeClass `scheduling`. Phase 3: KEP. | KEP: months of process |
| OCI runtime-spec `freebsd` section has no resources object | Med | Propose a `linux.resources`-style `freebsd.resources` (rctl rules + cpuset) to OCI. Until then, pass limits through CRI-O annotations → ocijail → rctl. | 2–4 wks code + spec process |
| Strategy: real kubelet vs virtual-kubelet | – | **Real kubelet.** Virtual-kubelet still needs every runtime piece and loses features and conformance. Use VK only for a 1-week demo, if ever. | – |

### 5.2 Resource isolation (cgroups → RCTL/RACCT/cpuset): `research/02`
| Kubernetes feature | Linux | FreeBSD today | Plan |
|---|---|---|---|
| CPU limit (`cpu.max`) | CFS bandwidth | rctl `pcpu`: 1 Hz on/off duty cycle. **Measured:** 500m → 50.3% ✅, 200m → 40% (2× overshoot). | ✅ **Implemented** (kubelet → CRI-O → ocijail `pcpu:deny`, [research/06](../research/06-resource-limits.md)). Accurate ≥ ~500m. Kernel **K2** for small limits. |
| CPU request (`cpu.weight`) | CFS shares | **Nothing** (no group scheduling) | **K3-B**: ULE priority bias per jail (cheap). K3-A: fair-share scheduler (research). |
| Memory limit | `memory.max` + OOM | **Measured:** `memoryuse:deny` doesn't enforce (no swap → nothing to reclaim). `swapuse:deny` charges at mmap time, so **Go dies at startup** (heap arena map). `memoryuse:sigkill` kills on RSS > limit ✅. | ✅ **Implemented** as `memoryuse:sigkill` (exit 137; 1 Hz check, so fast allocators overshoot). Reason shows `Error`, not `OOMKilled`. **K5**: per-jail OOM event + `oom_score_adj`. **K4**: page-owner accounting (moonshot). |
| Memory protection (min/low/high) | yes | none | K4 (moonshot). Reserve kube-reserved and cap ZFS ARC (`vfs.zfs.arc_max`). |
| pids | `pids.max` | `maxproc`/`nthr` | Works ✅ |
| cpuset / NUMA (CPU and memory managers) | cpuset cgroup | `cpuset(2)` + `domainset` per jail | Works ✅ (better than Linux) |
| hugepages | hugetlb | no accounting | **K7** (~150 LOC) |
| PSI / eviction pressure | `*.pressure` | none | **K6** |
| Hierarchy | cgroup tree | nested jails roll up usage, but rule relink bug suspected (`kern_rctl.c:1992`) | **K0** fix (~60 LOC), **K1** hierarchical rctl |
| Linux apps reading `/sys/fs/cgroup` (JVM, .NET, Go GOMAXPROCS) | native | absent, so apps size themselves to the whole host | **K10**: read-only cgroupfs in linsysfs |
| securityContext capabilities | caps | jail `allow.*` params only | **K13** `mac_capabilities` (PRIV_* ↔ CAP_* map) |
| seccomp | seccomp-bpf | none (Linuxulator `seccomp` is a stub) | **K12**: per-jail syscall bitmap. Optionally Linux seccomp via cBPF (reuse `bpf_filter.c`). |
| user namespaces (`hostUsers:false`) | userns | none | **K9a**: nullfs uid/gid map (idmapped-mount analogue). Otherwise **drop** and push those pods to bhyve. |

**Rule for the scheduler:** RuntimeClass `jail` = cooperative workloads, best-effort isolation, documented. RuntimeClass `bhyve` = hard limits, untrusted code, `hostUsers:false`.

### 5.3 Networking: iptables / Cilium / eBPF → pf / VNET / DTrace (`research/03`)
| Linux piece | FreeBSD replacement | Notes |
|---|---|---|
| netns + veth | VNET jail + `epair(4)` | Pod sandbox is a persistent VNET jail. Podman already uses this convention. |
| CNI bridge/host-local/loopback/portmap | **dfr/plugins `freebsd` branch** (FreeBSD's `net/containernetworking-plugins` port) | Upstream CNI plugins import Linux netlink and iptables directly and don't build. |
| flannel (VXLAN) | port to `if_vxlan` + route sockets | ~3–5 wks. Alternative: pure BGP with FRR/BIRD (no overlay, best performance). |
| kube-proxy iptables/ipvs/nftables | **kube-proxy `pf` mode** (dfr patch, in `patches/`): `rdr` to tables with round-robin, random or source-hash, sticky-address. Endpoint churn = atomic table replace over pf netlink. | Linear rule evaluation degrades past ~5k Services. Short-term fix: nested anchors bucketed by ClusterIP. Real fix: **pf "service map" lookup** (one hash hit per new connection, 2–3 months, upstreamable) or an ipfw-based IPVS-like module. |
| NetworkPolicy | pf rules on the **host-side** epair, with selectors as pf tables | Don't rely on pf inside the pod's VNET, because a privileged pod can flush it. |
| conntrack | pf state table | ✅ |
| MetalLB | CARP or BGP (FRR, OpenBGPD) | ✅ |
| Ingress | HAProxy, Traefik, nginx ✅. **Envoy is BROKEN in ports.** | Drop Envoy-based Gateway API and service mesh on FreeBSD nodes, or run them in bhyve. Fixing Envoy's FreeBSD port is a worthy side project. |
| Cilium / Calico-eBPF / Hubble / Tetragon / Falco-eBPF | **Not portable.** | Replace with: Services = pf, policy = pf, observability = **DTrace + pf flow export (`pflow`/netlink)**, a Hubble-like flow UI. |

**"Is there an eBPF equivalent on FreeBSD?"** Not one tool; three replacements:
1. **Tracing / debugging:** DTrace + `kinst` (arguably better than bpftrace).
2. **Packet filtering / redirect:** pf / ipfw (pf already has 71 SDT probes).
3. **Programmable datapath:** netgraph (`ng_bpf`, which is classic BPF) and netmap/VALE. Nothing is loaded and verified at runtime the way eBPF is.

A real eBPF VM in the kernel (the old generic-ebpf project, hooked into pf/ipfw) is a **multi-year research track (R1)**, not the MVP.

### 5.4 Linux containers on FreeBSD: Linuxulator vs bhyve (`research/04`)
- **Linuxulator** (FreeBSD 16-CURRENT):
  - **Works:** inotify (native since 15), memfd, statx, close_range, membarrier, clone3, epoll, eventfd, timerfd.
  - **Missing (ENOSYS):** unshare/setns, io_uring, aio, signalfd, pidfd, userfaultfd, bpf, keyctl, landlock.
  - **Bugs:** namespace clone flags are *silently ignored* (dangerous). `EPOLLEXCLUSIVE` is rejected (nginx with multiple workers).
  - **Real-world reports:** Go static binaries, nginx, redis and Node work. Postgres needs `sysvshm`. systemd, s6-overlay and Docker-in-Docker fail.
- **bhyve today:**
  - **Missing:** vsock, virtio-fs, balloon, hotplug, direct Linux kernel boot (a 2024 prototype exists), nested virtualization.
  - **Has:** virtio-blk/net/9p/console/rnd. Can run *inside a jail*.
- **Plan (three RuntimeClasses):**
  1. **`bhyve-hybrid`, now:** one long-lived Linux VM per node runs a Linux kubelet sidecar-style, so the node is effectively "two nodes in one box". Works today. Useful for Linux-only DaemonSets and charts.
  2. **`linux-jail`, opt-in:** an allow-list of images.
     - **Fix first:** make namespace flags *fail loudly*, add `EPOLLEXCLUSIVE`, add signalfd (~1.3 kLOC total).
     - **Then:** K10 cgroupfs.
  3. **`kata-bhyve`, default for Linux images on bare metal:**
     - **Add to bhyve:** a userspace **virtio-vsock** using the Firecracker Unix-socket protocol (~1.5 kLOC, no kernel vsock needed), direct kernel boot, and a minimal device profile.
     - **Rootfs:** ZFS zvol clones, with no virtio-fs needed.
     - **Shim:** a new FreeBSD containerd/CRI-O shim for Kata.
     - **Effort:** ~4–6 engineer-months.
- **Hard truth:** cloud VMs have no nested virtualization, so on EC2/GCE FreeBSD nodes Linux images only get the Linuxulator path. Bare metal gets bhyve.

### 5.5 Storage & images (`research/05`)
| Need | FreeBSD answer |
|---|---|
| overlayfs | **ZFS snapshotter / clones** (containerd and containers/storage both have FreeBSD ZFS support). Not unionfs: its 16-CURRENT man page still warns it "may destroy data". |
| bind mounts / hostPath / configMap / secret | nullfs + tmpfs; the AtomicWriter lchown fix is in the patch series |
| emptyDir | tmpfs or a ZFS dataset per pod (quota = sizeLimit) |
| CSI | No mount propagation, and jails can't mount NFS/UFS. Build **csi-proxy-bsd**, a host-side mount helper modeled on Windows' `csi-proxy`. CSI node plugins call it over gRPC. Plus our own **zfs-localpv CSI** (datasets with `jailed=on`, delegated into the pod). Kernel **K8** (shared nullfs propagation) is the long-term fix. |
| block / iSCSI / NFS | ctld/iscsictl, NFS client on the host via csi-proxy-bsd |
| images | Official FreeBSD OCI base images (since 14.2). Build a **"freebsd-images" factory** (Kaniko/Buildah-on-FreeBSD in CI) for the top 50 images. Pin `platform` per RuntimeClass so Linux images don't silently fall back. |

### 5.6 Ecosystem & scheduling story (the hardest non-code problem)
1. **Nodes:** label `kubernetes.io/os=freebsd`, taint `os=freebsd:NoSchedule`. This is exactly how Windows nodes were introduced.
2. **RuntimeClasses** `jail`, `linux-jail`, `kata-bhyve`, each with `scheduling.nodeSelector` + tolerations, so a pod that names a RuntimeClass lands correctly.
3. **Mutating admission webhook `bsd-placement`:**
   - If an image's manifest has a `freebsd/*` platform, allow FreeBSD nodes with RuntimeClass `jail`.
   - If the image is Linux-only and on the Linuxulator allow-list, use `linux-jail`.
   - Else use `kata-bhyve`, or leave the pod on Linux nodes.

   This is the **"out-of-the-box" piece** that makes "unchanged Helm charts" mostly true without lying in `spec.os`.
4. **DaemonSets:** FreeBSD-native builds of kube-proxy (pf), the CNI and node_exporter. Everything else gets excluded by the taint.

## 6. Kernel work plan (FreeBSD), prioritised
Details, file lists and LOC are in `research/02` §5. The upstream-friendly items come first.

| ID | Change | LOC | Upstream odds | Phase |
|---|---|---|---|---|
| K0 | rctl hierarchical relink bug fix + ATF test | ~60 | High | 1 |
| K5 | per-jail OOM kill + `oom_score_adj` (procctl) + jail OOM knote | 500–900 | High | 2 |
| K7 | hugepage (largepage) racct | 100–200 | High | 2 |
| K10 | read-only cgroupfs in linsysfs/linprocfs | 1–1.8k | High | 2 |
| K13 | `mac_capabilities` MAC module (PRIV_* ↔ CAP_*) | 0.8–1.5k | High (module) | 2 |
| K1 | hierarchical rctl semantics, delegation, in-jail view | 0.8–1.5k | Likely | 2 |
| K12b | per-jail syscall deny bitmap (RuntimeDefault-like) | 0.4–0.8k | Contentious | 2 |
| **K14** | **`allow.pf` (and `allow.ipfw`) jail permission**: grant `PRIV_NETINET_PF` to a non-VNET (hostNetwork) jail, so kube-proxy / CNI / NetworkPolicy agents can run as pods. Also make pf's ioctl and netlink paths enforce the same privilege. **Found in the first real cluster run** (MAC-DEV-VM.md §4 #4). | ~50–100 | High | **1** |
| N1 | pf "service map" O(1) lookup for kube-proxy | ~2–3 mo | Plausible | 3 |
| V1 | bhyve virtio-vsock (userspace, FC protocol) + direct kernel boot | ~1.5k + ~1k | High | 2 |
| K6 | PSI for racct | 0.8–1.5k | Plausible | 3 |
| K2 | CPU bandwidth controller in ULE | 1.5–2.5k | Possible (option) | 3 |
| K3-B | CPU weights via ULE priority bias | 0.5–0.9k | Debatable | 3 |
| K8 | shared nullfs mount propagation | 0.8–1.5k | Maybe | 3 |
| K9a | nullfs uid/gid mapping | 0.6–1k | Plausible | 3 |
| K4 | page-owner memory cgroup semantics | 3–6k | Unlikely | R (moonshot) |
| R1 | eBPF VM hooked into pf/ipfw | years | ? | R |

Honest-beta kernel set = **K0, K1, K5, K7, K10, K13** ≈ 4–6 kLOC ≈ **1 person-year** with tests.

## 7. Phased roadmap

**Phase 0 — Foundation (weeks 0–4)**
- [x] Research and the source workspace (this repo).
- [x] Import the dfr patch series; integrate; cgo-free kubelet; cross-compile all binaries for amd64/arm64 **[verified]**.
- [ ] **Talk to people before writing more code:** Doug Rabson (dfr), Samuel Karp (runj/containerd), the FreeBSD Foundation's container/cloud working group. Offer to co-maintain rather than fork. *The number-one risk is maintainer bandwidth, not technology.*
- [ ] CI: nightly GitHub Actions job that builds the patch series against Kubernetes `master` and the latest release (cross-compile). Plus a FreeBSD VM job (`vmactions/freebsd-vm`) that runs `build-cluster.sh` and a smoke test.
- [x] FreeBSD 15.1 arm64 dev VM on Apple Silicon (QEMU+HVF), with arm64 control-plane images built locally. See [MAC-DEV-VM.md](MAC-DEV-VM.md).
- [ ] An x86 bare-metal FreeBSD 15.x host for bhyve / kata-bhyve work (needs real virtualization; not possible in the Mac VM).

**Phase 1 — MVP: Linux control plane + FreeBSD workers (months 1–3)**
- CRI-O + ocijail + patched kubelet + pf kube-proxy, joined to a stock Linux control plane via kubeadm.
- FreeBSD-native images only, RuntimeClass `jail`, taints and labels.
- CNI: bridge + host-local (dfr plugins), multi-node via BGP (FRR) first, flannel-VXLAN port second.
- K0 upstreamed. Memory hard cap via `swapuse`. CPU limits documented as best-effort.
- **Deliverable:** conformance run with a published pass rate (expect the ~60 Linux-only tests to be skipped). Demo: Argo CD deploying a FreeBSD nginx/Go app plus NetworkPolicy.

**Phase 2 — Isolation & Linux workloads (months 3–9)**
- Kernel: K1, K5, K7, K10, K13, K12b. Add a `freebsd.resources` proposal to OCI runtime-spec, implemented in ocijail.
- `bhyve-hybrid` node mode, then the `kata-bhyve` runtime (vsock + direct boot + FreeBSD Kata shim).
- Linuxulator fixes (loud namespace failures, EPOLLEXCLUSIVE, signalfd) + `linux-jail` allow-list with tests.
- Storage: csi-proxy-bsd + zfs-localpv CSI. Placement webhook `bsd-placement`.

**Phase 3 — Distribution & scale (months 9–18)**
- `kbsd`: a single-binary, k3s-like node bundle (kubelet + CRI-O + CNI + kube-proxy-pf) shipped as a FreeBSD package and a `pkgbase` set, with a FreeBSD-hosted control plane option.
- pf service-map (N1), PSI (K6), CPU bandwidth (K2), CPU weight (K3-B), mount propagation (K8).
- Hubble-like flow observability from DTrace + pf.

**Phase 4 — Upstream (18 months +, demand-gated)**
- KEP: `spec.os.name=freebsd`, CRI FreeBSD fields, community-tier platform with SIG Node and SIG Release.

## 8. What we deliberately drop (at least for MVP)
- Cilium, Calico-eBPF, Hubble, Tetragon, Pixie, Falco-eBPF, and anything eBPF.
- Envoy-based ingress, Gateway API and service mesh on FreeBSD nodes (until the Envoy port is fixed).
- `hostUsers:false` on jails (bhyve only). seccomp profiles beyond RuntimeDefault-ish. AppArmor/SELinux fields (rejected by admission).
- Memory QoS (min/low/high), real CPU weights, and PSI-based eviction until Phase 3.
- Bidirectional mount propagation (CSI goes through csi-proxy-bsd instead).
- Linux images in jails by default (allow-list only). systemd-in-container, Docker-in-Docker.
- containerd as the default CRI (CRI-O first; containerd later).
- k3s port (write `kbsd` instead). Nomad / API shims. Pretending FreeBSD nodes are Linux.

## 9. Kill criteria (stay honest)
- After the Phase 1 MVP, fewer than ~3 external organisations trialing it → stop investing in the worker-node path.
- No second maintainer or sponsor by month 6 → same.
- K0/K1/K5 rejected upstream *and* we're unwilling to carry a kernel fork → limits stay best-effort. Reposition as "jails for trusted workloads + bhyve for everything else".
- **Fallback that still delivers value:** FreeBSD as the **hypervisor + ZFS storage layer** under Linux Kubernetes (bhyve-hosted Linux nodes, democratic-csi on FreeBSD/ZFS).

## 10. Repos (PlatformNerds org)

**Strategy: fork first, upstream later.** Workflow, branch conventions and sync are in [FORKS.md](FORKS.md).
The fork branches (`bsdk8s-*`) are the source of truth for code. `patches/` is an exported snapshot so CI can build without credentials.

| Repo | Purpose | Status |
|---|---|---|
| `BSDKubernetes` | Plan, research, patch series, build scripts, CI | **created** |
| `kubernetes` (private fork) | kubelet / kube-proxy pf / kubeadm, branch `bsdk8s-v1.36.1` | **created** ✅ builds |
| `cri-o` (private fork) | CRI-O FreeBSD WIP, branch `bsdk8s-1.37` | **created** ✅ builds |
| `ocijail` (private fork) | OCI runtime on jails; add rctl resources, containerd shim later | **created** |
| `cni-plugins` (private fork) | dfr's FreeBSD CNI plugins, branch `freebsd` | **created** |
| `kubernetes-demo` (private fork) | single-node bootstrap from our forks, branch `bsdk8s` | **created** |
| `bsdk8s-kernel` | FreeBSD src branches for K-series patches (fork of freebsd-src) | create when K0 work starts |
| `kube-proxy-pf` | Out-of-tree pf proxier + service-map experiments | create in Phase 1 |
| `bsd-cni` | FreeBSD CNI plugins (VNET bridge, flannel port, pf portmap/policy) | Phase 1 |
| `csi-proxy-bsd` / `zfs-localpv-bsd` | Host mount helper + ZFS CSI driver | Phase 2 |
| `kata-bhyve` | bhyve vsock, direct boot, Kata FreeBSD shim | Phase 2 |
| `bsd-placement-webhook` | Image-platform-aware RuntimeClass/placement mutating webhook | Phase 2 |
| `kbsd` | Single-binary node distribution | Phase 3 |

These are created on demand, not empty up front, so the org doesn't fill with dead repos.

## 11. Immediate next actions (this week)
1. Get a FreeBSD 15 host (RACCT enabled). Run `PlatformNerds/kubernetes-demo` branch `bsdk8s` → `build-cluster.sh`. Record what breaks as issues in the forks.
2. Once that runs end to end, offer `bsdk8s-v1.36.1`'s cgo-free kubelet + rctl fix to `dfr/kubernetes` (first upstreaming candidate, see FORKS.md).
3. Write the K0 ATF test to confirm or refute the rctl relink bug (`kern_rctl.c:1992`). Submit to Phabricator/GitHub if confirmed.
4. Stand up nightly cross-compile CI against Kubernetes `master` (catches the next `possibleCPUs`-style breakage within a day).
5. Join a FreeBSD worker to a Linux kubeadm cluster. Run the e2e conformance suite. Publish the pass rate.
