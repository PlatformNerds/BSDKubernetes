# 00 — Strategy, prior art, and contrarian analysis

Status: research draft, 2026-11-13. Role: "keep us honest". Upstream snapshot: see `UPSTREAM-PINS.md`
(kubernetes @ `b4226aca`, 2026-11-14, ~v1.37 dev). Everything marked **[measured]** was run locally in this session.
Everything marked **[verify]** comes from secondary sources and should be re-checked before anyone relies on it.

---

## 0. TL;DR (read this if nothing else)

1. **We are not starting from zero. Someone has already built most of this.** Doug Rabson (dfr, author of the
   FreeBSD Podman/Buildah port and of `ocijail`) keeps a **working, FreeBSD-native, single-node kubeadm cluster**:
   [dfr/kubernetes-demo](https://github.com/dfr/kubernetes-demo), last pushed 2026-06-21, targeting
   **Kubernetes v1.36.1**. His stack is:
   - **CRI-O**. Its FreeBSD pod-sandbox and container support is **merged upstream**
     ([cri-o#7472](https://github.com/cri-o/cri-o/pull/7472), [#7727](https://github.com/cri-o/cri-o/pull/7727)),
     plus a 12-commit WIP branch.
   - **ocijail** as the runtime.
   - A **patched kubelet** (branch `freebsd-kubelet-v1.36.1`: 12 commits, 36 files, **+2,218/−23**).
   - A **patched kubeadm** (+595/−33).
   - A **kube-proxy with a new `pf` proxier** (+1,018/−2).
   - Control-plane and etcd images for FreeBSD at `ghcr.io/dfr`.
   - CNI `bridge` plus pf NAT, with ZFS for container storage.

   **The single most important strategic decision is whether we build on and with dfr's work or around it.
   Recommendation: build on it, and recruit him.**
2. **The code-level porting problem is smaller than people assume.** **[measured]** At kubernetes tip,
   `GOOS=freebsd go build ./cmd/kubelet` fails on exactly one undefined symbol (`possibleCPUs` in
   `pkg/securitycontext/util.go`). With a 2-line overlay stub, the following all cross-compile for FreeBSD:
   kubelet, kube-proxy, kube-apiserver, kube-scheduler, kube-controller-manager, and kubectl. kubeadm fails on 2
   preflight symbols. The hard part is not compiling. It is *semantics*:
   - resources (see `02-resource-isolation.md`)
   - stats and eviction
   - volumes and mount propagation
   - networking (see `03-networking.md`)
   - Linux images (see `04-linux-compat-bhyve.md`)
3. **The upstream path is structurally capped at "Tier 3, community-supported", for now.**
   - SIG Release's platform policy (rewritten 2026-03-26) says: *"Platforms where Go itself treats the
     architecture as a secondary port … are restricted to Tier 3."*
   - **freebsd/amd64 and freebsd/arm64 are Go secondary ports.** Go's first-class ports are only linux, darwin and
     windows on {386, amd64, arm, arm64}.
   - Tier 3 means *no official builds, no project CI, and bugs may be closed without action*.

   Both previous FreeBSD PRs to kubernetes/kubernetes were closed by maintainers with "write a KEP / talk to
   SIG-Release first". **Plan for a long-lived, well-maintained downstream patch set, not an upstream merge.**
   Upstream only the small, generic "make the non-Linux Unix build not rot" cleanups.
4. **"The standard k8s ecosystem works unchanged" is true for the control plane and API clients. It is false for
   workloads and node agents.** The ecosystem pins itself to Linux:
   - kubeadm's own CoreDNS and kube-proxy manifests use `nodeSelector: kubernetes.io/os: linux`. dfr had to
     template that.
   - `spec.os.name` validation only accepts `linux|windows` (`pkg/apis/core/validation/validation.go:127`).
   - The kubelet rejects any pod whose `spec.os.name` ≠ `runtime.GOOS`
     (`pkg/kubelet/lifecycle/predicate.go:334-339`).
   - 60 of 466 conformance tests are `[LinuxOnly]`.
   - Most images are `linux/*`-only.

   Promise "unchanged kubectl/Helm/GitOps/controllers against a mixed cluster", not "every Helm chart runs on
   FreeBSD nodes".
5. **Recommended MVP is the Windows playbook:**
   - Linux control plane, using stock kubeadm/k3s/EKS-style anything.
   - **FreeBSD worker nodes** running CRI-O + ocijail + a patched kubelet, labelled `kubernetes.io/os=freebsd`
     and tainted.
   - FreeBSD-native OCI images first.
   - Then a Linuxulator RuntimeClass, and a bhyve-microVM RuntimeClass for "real Linux".

   This delivers value in about 1–3 months because dfr's code already exists. Full plan in §5.

---

## 1. Prior art: every known attempt, and what happened to it

### 1.1 Runtime and image layer (the foundation is real now)

| Effort | Who / when | State (Nov 2026) | Notes / why it stalled (if it did) |
|---|---|---|---|
| **runj**: OCI runtime on jails, with a containerd shim `wtf.sbk.runj.v1` | Samuel Karp (AWS → Google), 2021– | **v0.2.0 (Nov 2025)** adopts OCI runtime-spec 1.3 FreeBSD section; now uses `jail(2)` directly. Repo pushed 2026-09; ★673 | Author says it was a personal learning project and **he lacks time to grow and maintain it; he is recruiting maintainers** ([blog](https://samuel.karp.dev/blog/2025/11/runj-adopts-oci-130/)). Experimental, "not evaluated for security" ([freshports](https://www.freshports.org/sysutils/runj/)). Networking only got Docker-style support in 2022 ([blog](https://samuel.karp.dev/blog/2022/12/docker-style-networking-for-freebsd-jails-with-runj/)). |
| **containerd on FreeBSD** | Karp, dfr, others | **[measured]** `containerd` builds for FreeBSD, **including the CRI plugin**. The CRI plugin compiles via `*_other.go` stubs, so there is no FreeBSD-specific sandbox or jail logic in `internal/cri`. FreeBSD builtins are only `diff/walking` + `zfs`. | Rabson (2024): "containerd had been ported… but lacked comprehensive networking support" ([FF blog](https://freebsdfoundation.org/blog/2024-freebsd-developer-summit-oci-containers-on-freebsd/)). The nerdctl networking PR stalled. "Somewhat flaky" per [lobste.rs](https://lobste.rs/s/lfhj2v/freebsd_jails_containers). |
| **ocijail** + **Podman/Buildah** FreeBSD port | Doug Rabson, 2022– | ocijail **v0.6.1 (2026-09-01)**; `podman-suite` in ports; Handbook chapter ([Handbook ch.18](https://docs.freebsd.org/en/books/handbook/containers/)) | Still called "experimental", root-only, and needs FreeBSD 14.3+ ([guide](https://oneuptime.com/blog/post/2026-03-18-use-podman-freebsd/markdown)). The Foundation's "production-ready by mid-2025" target ([FF blog 2024](https://freebsdfoundation.org/blog/advancing-cloud-native-containers-on-freebsd-podman-testing-highlights/)) slipped. Podman upstream CI could not test FreeBSD for lack of a trusted base image. |
| **OCI runtime-spec v1.3: FreeBSD is an official platform** | Rabson-led, Nov 2025 | Shipped ([FF announcement](https://freebsdfoundation.org/?p=28383)) | Blocked for months by a late requirement for reference implementations (both ocijail *and* runj) ([WG notes](https://hackmd.io/hq_NOVL4RZS7xYYMqfJ6-A)). **Note: the FreeBSD config object has no `resources` section** (see 02). |
| **Official FreeBSD OCI images** | Dave Cottlehuber (dch) | In releases since 14.2. In 14.3+ / 15.x as amd64/arm64 `freebsd-runtime`, `-static`, `-dynamic`, etc.; on Docker Hub and GHCR ([dch post](https://people.freebsd.org/~dch/posts/2024-12-04-freebsd-containers/)) | Image names changed over time. Catalog is tiny compared to Docker Hub. |
| **daemonless.io**: 60+ FreeBSD-native app images with s6 | M. Johnson, dtxdf, 2026 | Active ([Q1-2026 status report](https://www.freebsd.org/status/report-2026-01-2026-03/)) | Homelab-oriented (*arr, Jellyfin, Immich). The only meaningful third-party FreeBSD OCI image catalog. |
| **Cloud Native Buildpacks** FreeBSD binaries | R. Gogolok | `pack` + lifecycle ship FreeBSD binaries upstream (Q1-2026 report) | Useful for "build FreeBSD-native images from source without Dockerfiles". |
| **Dockerbox**: Docker daemon in a Debian bhyve VM | Chun-Cheng Yeh, FF-sponsored, 2026 | Active (Q2/Q3-2026 reports; [repo](https://github.com/leafoliage/freebsd-dockerbox)) | Proof that the Foundation is *also* funding the "Linux in a VM" escape hatch. |

### 1.2 Kubernetes-on-FreeBSD attempts

| Effort | When | What it achieved | What happened |
|---|---|---|---|
| FreeBSD ports `net/kube-apiserver`, `kube-controller-manager`, `kube-scheduler`, `sysutils/kubectl`, `kops`, `minikube` | ~2018– | Control-plane binaries packaged | The pkg-message literally says *"Currently Kubelet hasn't be ported to FreeBSD"* ([cgit](https://cgit-dev.freebsd.org/ports/tree/net/kube-apiserver/pkg-message)). |
| k8s PR [#50211](https://github.com/kubernetes/kubernetes/pull/50211) (2017), [#97270](https://github.com/kubernetes/kubernetes/pull/97270) (2020, `GetBootTime` for FreeBSD) | 2017–2020 | Tiny portability fixes **merged** | Shows that small, generic, build-portability PRs *do* get merged. |
| **k8s PR [#115870](https://github.com/kubernetes/kubernetes/pull/115870) "[WIP] add basic FreeBSD support on Kubelet"** (nerzhul) | Feb–Oct 2023 | Kubelet started on FreeBSD against containerd 1.7 + runj and ran **static pods, `hostNetwork` only**. +1,567/−484 lines in 18 files. | Antonio Ojea (SIG Network): *"Adding support for a new OS … should be discussed and properly documented in a KEP so it can be feature gated."* Dims closed it: *"needs rebase and a discussion with release and other folks"*, linking the SIG-Release platforms guide. Root causes: no KEP, no SIG sponsor, no CI, and runj misbehaving under kubelet. |
| **k8s PR [#120003](https://github.com/kubernetes/kubernetes/pull/120003) "allow freebsd as a host os"** (dfr) | Aug–Oct 2023 | A 4-line build-script change | Even this was labelled `kind/feature`, held for a changelog, re-routed to SIG Architecture + SIG Release, then **closed by dims with the same platforms-guide link**. **Lesson: the word "FreeBSD" in a PR triggers the platform-governance process, regardless of diff size.** |
| **cri-o [#6492](https://github.com/cri-o/cri-o/issues/6492) "Port to FreeBSD"** (dfr) | 2023–2025 | `crio` builds on non-Linux (#7052, #7153, #7214). **FreeBSD pod sandbox (#7472) and container support (#7727) merged**, the latter on 2025-02-21 after ~1 year in review. | Still open: Makefile support [#10005](https://github.com/cri-o/cri-o/pull/10005) (open since 2026-06), plus the WIP branch `dfr/cri-o:freebsd-wip-1.37.0` (12 commits: stats, devices, RunAsUser, service script). The CI request [#9005](https://github.com/cri-o/cri-o/issues/9005) (vmactions/cross-platform-action) was closed without a FreeBSD job. **CRI-O maintainers accepted FreeBSD code. That is the most receptive upstream we have.** |
| **dfr/kubernetes branches** `freebsd-kubelet`, `freebsd-proxy`, `freebsd-kubeadm` (v1.31.0-alpha → **v1.36.1**) + [dfr/kubernetes-demo](https://github.com/dfr/kubernetes-demo) | 2023 → Jun 2026 | **Working single-node FreeBSD-native kubeadm cluster.** It needs RACCT enabled, ocijail ≥ 0.6, and pf/if_bridge/nullfs/fdescfs. Small multi-node is possible by L2-bridging nodes manually. | Self-described as "very demo-ware" in the [WG notes](https://hackmd.io/hq_NOVL4RZS7xYYMqfJ6-A), and *"patches … not reviewed, endorsed or supported by the Kubernetes project"*. The 2024 WG notes name the biggest unknown as **persistent volumes / Linux bind-mount propagation semantics**. Single maintainer. |
| Tony Norlin, "Planternetes" / [tnorlin/kubernetes](https://github.com/tnorlin/kubernetes/releases) | 2021 → **v1.37.0 (2026-08-27)** | Keeps releasing FreeBSD, OpenBSD and illumos builds. **FreeBSD assets contain only control-plane binaries + kubectl, no kubelet.** He runs a v1.29 control plane in VNET jails ([post](https://kubernaut-eu.pages.dev/posts/welcome-mandala-kubernetes-v129-on-freebsd-controlplane)) and the data plane on **Linux** workers with Cilium ([post](https://kubernaut-eu.pages.dev/posts/kubernetes-on-freebsd-with-linux-worker-nodes-and-cilium)). | Proves the *control plane* on FreeBSD is easy and boring. His choice of Linux workers is itself a data point. |
| [rikatz/vk-jails](https://github.com/rikatz/vk-jails): virtual-kubelet provider for jails | Dec 2020 – Jan 2021 | README: "Do not use, WIP!! It allocates IPs, but the IP stack does not work" | **Abandoned after 5 weeks.** By Ricardo Katz, a SIG-Network/ingress-nginx maintainer, which suggests even k8s insiders found the VK route a dead end without networking. |
| Calico ported to FreeBSD VNET jails (Sakaguchi et al., Future Univ. Hakodate / SAKURA internet) | [EuroBSDCon 2025](https://events.eurobsdcon.org/2025/talk/UQX99C/) → [AsiaBSDCon 2026 paper](https://2026.asiabsdcon.org/media/entry/question_uploads/final-paper_3tMo1OJ.pdf) | iptables→ipfw, netlink→routing sockets, wired through runj | Research prototype; not upstreamed **[verify]**. See 03-networking. |
| [ndenev/freebsd-csi](https://github.com/ndenev/freebsd-csi): CSI driver, ZFS zvols over CTL iSCSI/NVMe-oF | 2026, pushed 2026-10 | Controller + `ctld-agent` | FreeBSD as a *storage backend* for (Linux) k8s. Low-risk value today. |
| [f3s blog series](https://www2.buetow.org/gemfeed/2024-11-17-f3s-kubernetes-with-freebsd-part-1.html) (P. Buetow) | 2024–2025 | k3s on FreeBSD hosts, **inside Rocky Linux bhyve VMs** | The practitioner's default answer: FreeBSD as the hypervisor and storage, Linux as the k8s node. |
| openSUSE Hack Week 23: ["k3s and FreeBSD"](https://hackweek.opensuse.org/23/projects/k3s-and-freebsd) | 2023 | No visible outcome | k3s itself has no FreeBSD issue beyond an install-script question (k3s-io/k3s#8513). |
| EuroBSDCon 2026: ["Running BSD workloads in Linux environments as OCI containers"](https://events.eurobsdcon.org/2026/talk/JHXWDC/) (C. Mainas, urunc) | Sep 2026 | FreeBSD/NetBSD/Rumprun workloads as OCI artifacts on **Linux** k8s via the urunc sandbox runtime | The *inverse* strategy: BSD as a workload on Linux k8s. Relevant to §5 option (h). |

### 1.3 Non-Kubernetes orchestrators on jails (relevant prior art)

- **Nomad + pot**:
  - The [nomad-pot-driver](https://github.com/bsdpot/nomad-pot-driver) is listed as an official community driver
    ([HashiCorp docs](https://developer.hashicorp.com/nomad/plugins/drivers/community/pot)). Port v0.10.0 is in
    2026Q4 ports. It was presented at [FOSDEM 2020](https://papers.freebsd.org/2020/fosdem/pizzamig-orchestrating_jails_with_nomad_and_pot).
  - **Both pot and the driver were last pushed 2025-05-09.** Effectively in maintenance.
  - **Nomad has been under the BUSL since Aug 2023**, and HashiCorp is now IBM. A virtual-kubelet *Nomad provider*
    exists, so "k8s API → VK → Nomad → pot → jail" is technically composable today **[verify the provider still
    builds]**.
  - Lessons: it proved that *scheduler-on-jails* works in production (trivago, Subito). It also shows the
    ecosystem-pull problem: nobody followed, because the tooling, Helm charts, operators and CNCF landscape all
    target k8s.
- **AppJail / Director / Overlord** (dtxdf):
  - Shell + C, GitOps-oriented, multi-host "tree chaining", OCI-compatible since 2026
    ([status report](https://www.freebsd.org/status/report-2025-07-2025-09/)).
  - Active and FreeBSD-idiomatic, but a niche, single-author project.
- **Sylve** (AlchemillaHQ, [GitHub](https://github.com/AlchemillaHQ/Sylve)):
  - A Proxmox-like web UI for bhyve + jails + ZFS, written in Go/Svelte. FreeBSD 15+. HA via ZFS replication,
    live-ish migration.
  - **The FreeBSD Foundation expanded its grant in Q3-2026.** The roadmap includes "Native OCI container management
    through Podman".
  - Sylve is where FreeBSD-Foundation money for "datacenter UX" is going. It is a potential *partner*: a k8s node
    agent could appear as a Sylve-managed role.
- **Bastille / iocage / CBSD / pot**: mature single-host jail managers. Bastille is the most active (★1.1k, pushed
  2026-11). None of them is a cluster scheduler.
- **FreeBSD Foundation "Cloud Native Containers" working group**:
  - Run by Alice Sowerby (FF TPM), biweekly from 2025-09-22.
  - Goal: *"make FreeBSD a top-tier choice for running cloud native containerised workloads"*
    ([list](https://lists.freebsd.org/archives/freebsd-enterprisewg/2025-November/000131.html)).
  - **The Nov 3, 2025 call was announced as the final call of the earlier WG** ([notes](https://hackmd.io/hq_NOVL4RZS7xYYMqfJ6-A)).
  - In the Q1–Q3 2026 status reports, the "containers" entries are daemonless, Buildpacks, Dockerbox and Sylve.
  - **No kubelet, CRI or Kubernetes entry in any 2025–2026 quarterly report.**

### 1.4 Why the stalled efforts stalled (pattern)

1. **Single volunteer maintainers.** Karp, Rabson, Norlin, Pizzamiglio, Katz and dtxdf were each essentially alone.
   Every effort's velocity equals one person's spare time.
2. **Upstream governance, not code, blocked the k8s PRs.** No KEP, no SIG sponsor, no CI, no demand data.
3. **Networking was always the last mile.** It held up runj (until 2022), nerdctl (stalled), vk-jails (abandoned)
   and #115870 (`hostNetwork` only). Calico had to be research-ported.
4. **Storage semantics.** Linux mount propagation and bind-mount behaviour are assumed by kubelet volume code and
   every CSI node plugin.
5. **No pull from users.** The flagship FreeBSD shops run containers on Linux:
   - Netflix runs Open Connect on FreeBSD, but its container platform Titus runs on Linux **[verify]**.
   - iXsystems **moved TrueNAS to Linux** in 2024–2025 ([The Register](https://www.theregister.com/2024/03/18/truenas_abandons_freebsd/)),
     citing better app infrastructure "thanks to the integration of Docker".

---

## 2. Precedents for non-Linux Kubernetes nodes

### 2.1 Windows (the only successful precedent)

- **Timeline:**
  - SIG-Windows formed **March 2016**.
  - Alpha in **v1.5 (Dec 2016)**, beta in **v1.9 (Dec 2017)**, **stable in v1.14 (Mar 2019)**
    ([k8s blog](https://kubernetes.io/blog/2019/04/01/kubernetes-v1.14-delivers-production-level-support-for-windows-nodes-and-windows-containers)).
  - **About 3 years to GA, with paid engineers from Microsoft, Docker, VMware, Apprenda and Cloudbase.** About
    7 years to arrive at HostProcess containers (GA 1.26) and a separate conformance story.
- **KEP trail:** [KEP-116 Windows node support](https://www.kubernetes.dev/resources/keps/116/) (GA criteria),
  [KEP-995 kubeadm for Windows](https://www.kubernetes.dev/resources/keps/995/),
  [KEP-2578 Windows conformance / Operational Readiness](https://www.k8s.dev/resources/keps/2578/), and
  [KEP-2802 `spec.os`](https://www.kubernetes.dev/resources/keps/2802/) (alpha 1.23 → GA 1.25; gate removed 1.27).
- **What SIG-Windows had to do:**
  - Build-tag the kubelet (78 files with `//go:build windows` in pkg/cmd today **[measured]**, versus 11 mentioning
    freebsd).
  - Write `container_manager_windows.go` and a `winkernel` kube-proxy backend.
  - Get containerd and hcsshim CRI support.
  - Create `test/e2e/windows/*` (21 test files).
  - Tag Linux-only e2e tests `[LinuxOnly]` (178 occurrences in test/e2e; **60 of 466** conformance tests).
  - Run CI on Azure (capz) as release-*informing* jobs.
  - Get `spec.os` into the core API so Pod Security Admission can skip Linux-only checks.
- **Topology:** the control plane was *always* Linux. SIG-Windows explicitly scoped itself to worker nodes.
  **Copy this.**

### 2.2 KEP-2802 `spec.os` and what it means for FreeBSD

- `OSName` is a free-form string type, but **validation hard-codes `validOS = {linux, windows}`**
  (`pkg/apis/core/validation/validation.go:127`). An unmodified API server **rejects `spec.os.name: freebsd`**.
- The kubelet **rejects pods whose `spec.os.name` ≠ `runtime.GOOS`** (`predicate.go:331-339`). A FreeBSD kubelet
  therefore refuses `os: linux` pods, **including Linux images you intended to run under the Linuxulator**.
- The node label `kubernetes.io/os` is set from `goruntime.GOOS`, so it becomes `freebsd` automatically
  (`kubelet_node_status.go:317`). Scheduling with `nodeSelector: {kubernetes.io/os: freebsd}` works **today on an
  unmodified control plane**. This is the key enabler for the "stock Linux control plane + FreeBSD workers" MVP.
- Getting `freebsd` into `validOS`, and PSA semantics for FreeBSD, needs a KEP touching SIG Node, SIG Auth (PSA) and
  API review. That is a phase-3+ item, and not required for the MVP.

### 2.3 illumos / SmartOS Triton

- Joyent did not port kubelet. It built **sdc-docker**, a *re-implementation of the Docker Remote API* on top of
  its own control plane. Each Docker container became a zone, either native or LX-branded Linux emulation
  ([Cantrill 2015](https://bcantrill.dtrace.org/2015/03/24/triton-docker-and-the-best-of-all-worlds/)).
  Technically excellent. **Commercially, it lost to Kubernetes.** Joyent went to Samsung (2016), and Triton is now
  maintained by MNX.
- **Lesson:** an *API shim* over a foreign primitive works technically, but the target API moved (Docker →
  Kubernetes) faster than the shim could follow. Building an API shim over k8s is the same bet, on a much larger and
  faster-moving API.
- Norlin keeps building illumos kubelets ([tnorlin releases](https://github.com/tnorlin/kubernetes/releases)
  `v1.37.0-illumos`) with no upstream traction. That is the community-supported model in practice.

### 2.4 Virtual Kubelet providers (ACI, Fargate, ECI, Nomad, Liqo, InterLink)

- VK ([repo](https://github.com/virtual-kubelet/virtual-kubelet), v1.14.1 Oct 2026, active) masquerades as a
  kubelet. Providers implement pod CRUD, logs, exec and status.
- **Conformance implication:** VK nodes are **not** conformant nodes:
  - no kube-proxy or Service datapath on the node
  - DaemonSets are typically excluded via taints
  - no CSI node plugin
  - no hostPath/hostNetwork semantics
  - partial probes, downward API and exec

  ACI's own docs list gaps such as no downward-API podIP, no host aliases and limited exec args
  ([azure-aci](https://pkg.go.dev/github.com/virtual-kubelet/azure-aci@v1.6.1)). The cluster remains conformant
  *because* the conformance suite runs on the real Linux nodes, and VK nodes are tainted out.
- For FreeBSD, VK is a good **demo / bridge** but a **ceiling of roughly 40–50% ecosystem compatibility**. It also
  does nothing for "native kubelet semantics on FreeBSD".

---

## 3. Upstream politics: what acceptance would actually require

### 3.1 The written rules (as of 2026-03-26)

From [sig-release/release-engineering/reference/platforms.md](https://github.com/kubernetes/sig-release/blob/master/release-engineering/reference/platforms.md)
and [community/contributors/guide/platforms.md](https://github.com/kubernetes/community/blob/master/contributors/guide/platforms.md):

- **Tier 1** requirements:
  - official binaries and images
  - unit, integration and e2e jobs on the **release-blocking** dashboard
  - 2+ maintainers
  - **Go first-class support**
  - docs on kubernetes.io
- **Tier 2** requirements:
  - official binaries
  - e2e on informing dashboards
  - 2+ maintainers
  - **Go first-class support**
  - a SIG Release subproject with OWNERS
  - a promotion plan referencing a KEP
  - *"Adding a platform to the release builds without any CI is not permitted."*
- **Tier 3:**
  - no official builds and no project CI
  - a documented community build
  - external artifacts may be linked with a disclaimer
  - *"Bug reports … may be closed without action."*
- **Hard gate:** *"Platforms where Go itself treats the architecture as a secondary port … are restricted to
  Tier 3."* The text says "architecture", but the intent covers GOOS/GOARCH pairs.
  - **[measured]** `go tool dist list -json` with Go 1.27.3 shows freebsd/amd64 and freebsd/arm64 as
    `FirstClass: false`.
  - Per the [Go porting policy](https://go.dev/wiki/PortingPolicy), first-class status needs an accepted Go
    proposal and a Go-team commitment. FreeBSD is not on any roadmap for that.
- **Demand evidence** must be *"concrete data … Usage of third-party builds alone does not constitute demand."*
  There is also an explicit **"Right to Reject"** clause.
- The platforms guide warns: *"It is not enough for builds to work as it gets bit-rotted quickly."*
  **[measured] Proof: at tip the kubelet no longer compiles for FreeBSD**, because a new Linux/Darwin/Windows-only
  `possibleCPUs()` lacks a fallback. dfr carries the first commit "Add stub for possibleCPUs on FreeBSD" in every
  branch.

### 3.2 The KEP path (if we ever go for more than Tier 3)

1. A **SIG-Node-sponsored KEP**, "FreeBSD node support", modelled on KEP-116. Scope: worker nodes only. Gate:
   `FreeBSDNodeSupport`.
2. A companion **SIG-Release platform request**, opened as an issue in kubernetes/sig-release.
3. **API review** to add `freebsd` to `validOS`, with **SIG-Auth** handling PSA profile semantics for FreeBSD (what
   do `capabilities`, `seccompProfile` and `allowPrivilegeEscalation` mean in a jail?).
4. **SIG-Testing**: prow jobs. Prow runs on Linux k8s clusters, so FreeBSD e2e needs VMs.
   - The realistic route is a periodic job that provisions FreeBSD EC2 or GCE instances with kubetest2 (there are
     official FreeBSD AMIs).
   - Or a lab donated through the CNCF.
   - GitHub Actions via [vmactions/freebsd-vm](https://github.com/vmactions/freebsd-vm) (v1.6.0, Nov 2026;
     15.0/15.1 on x86_64/aarch64/riscv64) is fine for **our** build and unit CI, but is not prow.
   - Cirrus CI, the old FreeBSD CI workhorse, **shut down ~June 1, 2026**
     ([QEMU patch](https://patchew.org/QEMU/20260602162457.828969-1-stefanha@redhat.com/)), so free FreeBSD CI is
     scarcer.
5. Conformance: a FreeBSD equivalent of KEP-2578, i.e. a defined test subset (non-`[LinuxOnly]`) plus
   FreeBSD-specific node e2e.

### 3.3 Realistic maintenance burden (fork vs upstream)

- **Downstream patch set today** (dfr, v1.36.1):
  - kubelet ~2.2k LoC
  - kube-proxy pf proxier ~1k LoC
  - kubeadm ~0.6k LoC
  - CRI-O WIP ~12 commits
- **After real resource management, stats, eviction, CSI and networking work, expect ~8–15k LoC downstream**
  **[estimate]**.
- **Rebase cadence:** 3 k8s minors per year, each with ~3 patch-release trains maintained. A competent owner needs
  about **0.5 FTE just to rebase, re-test and chase kubelet internal refactors**. Recent examples are the
  `cm`/cgroup-v2 assumptions, PSI GA, MemoryQoS, DRA, user namespaces, in-place pod resize, and the possibleCPUs
  break.
- **The full program** (kubelet + CRI-O + ocijail/runj + CNI + kube-proxy + CSI + images + CI + docs) is realistically
  **2–4 FTE sustained** to be credible to anyone outside a homelab. For comparison, SIG-Windows had double-digit
  corporate engineers.
- **Upstream strategy that is actually achievable:**
  - (i) Merge *generic* build-hygiene PRs: add `_other.go` fallbacks, and avoid `linux||darwin||windows`
    enumerations in shared code. Frame them as "non-Linux Unix build portability", do **not** title them FreeBSD,
    and cite darwin.
  - (ii) Upstream to **CRI-O** (receptive), **ocijail**, **runj**, **CNI plugins** and **mount-utils**. The FreeBSD
    mount-utils changes are self-contained; note that #143108, "mount-utils: decode kernel octal escapes", which
    touched FreeBSD, merged Oct 2026.
  - (iii) Publish Tier-3-style "documented community build" artifacts ourselves.
  - (iv) Collect *real* demand data before ever opening a KEP.

---

## 4. Honest critique

### 4.1 Five biggest reasons this could fail

1. **No market pull + structural Tier-3 cap = permanent fork with a bus factor of 1–3.**
   - Go's secondary-port status alone blocks Tier 1/2.
   - The people who know this stack (Rabson, Karp) are volunteers who say publicly that they lack time.
   - If we become the maintainers, we inherit 3 releases per year of kubelet churn forever.
   - The FreeBSD Foundation's 2026 money is going to Sylve, Dockerbox and bhyve, not to k8s.
2. **"Ecosystem works unchanged" is mostly false at the workload layer.**
   - Images: the overwhelming majority are `linux/amd64|arm64` only.
   - DaemonSets and agents (CNI, CSI node plugins, log shippers, node-exporter, security agents) are `os: linux`
     pinned and read `/proc`, `/sys` and cgroupfs.
   - Operators that assume Linux nodes.
   - Cilium and other eBPF CNIs cannot run.

   Users will hit "chart X doesn't schedule" or "agent Y crashes" on day 1, and judge the whole project by it.
3. **The Linuxulator is a compatibility cliff, not a slope.**
   - No cgroups/namespaces visible inside, no seccomp, partial inotify/epoll edge cases, no io_uring and no eBPF.
     Busy-but-subtle syscalls fail at runtime, not at deploy time.
   - Debugging that across the CRI → ocijail → jail → Linuxulator stack is very expensive support.
   - The only *honest* "runs any Linux image" answer is a VM (bhyve), which erodes the "native" thesis.
4. **Resource semantics don't match** (see 02):
   - RCTL CPU limiting is 1 Hz stop/start.
   - `memoryuse` isn't a hard limit, there is no per-jail OOM or PSI, and the ARC is unaccounted.
   - QoS classes, eviction and requests/limits are core k8s contracts. Faking them produces clusters that look
     healthy but behave pathologically under pressure. Fixing this needs **kernel work**, which is acceptable per
     the brief but is the long pole and needs src committers.
5. **Moving target + CI scarcity + security perception:**
   - kubelet internals assume cgroup v2 more every release.
   - No free first-party FreeBSD CI on prow; Cirrus is gone.
   - Jails are root-only in the Podman/ocijail world today.
   - PSA and admission tooling (Kyverno/Gatekeeper policies) speak Linux security contexts, so a security review
     will ask "what does `restricted` mean here?" and we need an answer before anyone runs untrusted pods.

### 4.2 Five strongest arguments FOR

1. **Jails + VNET are a mature, simple, auditable isolation primitive.**
   - 25 years old, one coherent kernel object (versus namespaces + cgroups + seccomp + LSM glued together).
   - Hierarchical jails map naturally to *pod = parent jail, containers = child jails* sharing the VNET.
   - The OCI runtime-spec now formally defines FreeBSD (v1.3).
2. **ZFS as a first-class node primitive.** Image layers become snapshots and clones; emptyDir and PVs are
   datasets with quotas, compression and checksums; node upgrades get boot environments; workloads can migrate with
   `zfs send/recv` (Sylve already does replication-based migration). Licensing is clean (no CDDL-on-Linux
   ambiguity), and the containerd `zfs` snapshotter is already built in on FreeBSD.
3. **Network stack and pf:**
   - Proven high-throughput stack: kTLS, the Netflix CDN at hundreds of Gbps per box, netmap, netgraph, CARP and
     VNET.
   - pf is a stable, readable policy language. A pf proxier (dfr has one) is conceptually cleaner than iptables
     chains, and pf anchors map nicely to per-Service and per-NetworkPolicy rules.
   - Network appliances (pfSense/OPNsense lineage, Juniper heritage) are a natural edge-k8s audience.
4. **bhyve in the same OS.** One node can run native jails *and* per-pod microVMs (a Kata-style RuntimeClass) for
   real Linux. That is a better story than Linux nodes, which need Kata/QEMU/Firecracker stacks bolted on.
5. **Permissive licensing + a single coherent base system.** Appliance and embedded vendors (NetApp ONTAP, Juniper,
   Sony, Netgate, storage OEMs) build on FreeBSD *because* of the BSD license. They increasingly want
   "Kubernetes-managed" edge devices without shipping a GPL userland and a Linux kernel. A FreeBSD node agent lets
   them join fleets managed by standard k8s and GitOps tooling.

### 4.3 Who is the user? (be specific)

| Segment | Want | Size / willingness | Fit |
|---|---|---|---|
| **FreeBSD-shop SREs** (hosting providers, CDNs, the existing jail fleets of ISPs and SaaS) who are being told "the platform team standardised on k8s" | Keep FreeBSD hosts but be manageable by the corporate k8s/GitOps/observability stack | Small but real, and the most motivated | **Primary** for the MVP (FreeBSD workers in a Linux-controlled cluster) |
| **Appliance / embedded vendors on FreeBSD** | Fleet management via k8s APIs and edge k8s | Few companies, deep pockets, could fund maintainers | **Primary sponsor target** |
| Storage-centric (ZFS) users | FreeBSD as CSI backend / storage node | Medium | Already served by freebsd-csi-style projects; low effort |
| Homelab / enthusiasts | "k8s on my FreeBSD box" | Largest by headcount, zero money | Drives visibility (daemonless, Sylve users) |
| Enterprises wanting "better isolation than Linux containers" | Mostly served by gVisor/Kata/Firecracker on Linux | Large but captured | Weak. Do not pitch to them first. |

---

## 5. Out-of-box strategies, ranked

Ecosystem-compat % here means: *of the Kubernetes API and tooling surface a typical platform team uses
(kubectl/Helm/GitOps/controllers/Services/ConfigMaps/Secrets/probes/logs/exec/volumes/DaemonSets), the share
that works unchanged when targeting these nodes*. It is a judgement estimate, not a measurement. We should replace it
with a real Sonobuoy pass rate in Phase 1.

| # | Strategy | Effort | Ecosystem compat | Risk | Verdict |
|---|---|---|---|---|---|
| **(c)** | **Linux control plane + FreeBSD worker nodes first** (a topology choice, used together with (a)) | Lowest incremental: the control plane is stock | Control plane 100%; FreeBSD node workloads ~60–75% | Low | **Do this. It is the Windows playbook.** |
| **(a)** | **Port the real kubelet with `freebsd` build tags (Windows-style)**, building on dfr's branches; use CRI-O + ocijail (and later containerd + runj) | Medium: the base exists (~3.8k LoC across kubelet, proxy and kubeadm). High for real resources, stats and volumes (kernel work) | **Highest achievable:** 70–85% for FreeBSD-native images; probes, logs, exec, Services, ConfigMaps and emptyDir all work like a real node; DaemonSets work if pinned to freebsd | Medium: fork maintenance, rctl semantics | **Core of the plan.** |
| **(g)** | **"jailcri"**, a purpose-built CRI in Rust/Go | High: re-implements image pull, sandbox lifecycle, streaming, stats and checkpointing | Same ceiling as (a) | High: duplicates CRI-O, which is *already upstream* for FreeBSD; a new single-maintainer component | **Don't, unless CRI-O's containers/* dependency stack proves untenable.** If anything, invest in containerd's CRI `_other.go` → `_freebsd.go` path as the second runtime. |
| **(e)** | **Linux VM per node** (bhyve/Sylve-hosted Linux k8s nodes) | Lowest: works today (f3s does it) | ~100% | Low | **Ship as the immediate fallback and as the "real Linux" story.** It delivers no native-FreeBSD value, so treat it as a tactic, not the product. Better variant: a **per-pod bhyve microVM RuntimeClass** (Kata-like) under the native FreeBSD kubelet, which gives Linux images full compatibility. |
| **(f)** | **"kbsd"**, a k3s-like single-binary FreeBSD distro: kubelet + CRI-O/containerd + pf proxy + CNI + ZFS CSI, with an rc.d + pkg + Sylve integration | Medium, but only *after* (a) is stable | Same as (a) | Medium: packaging churn | **Phase 3 adoption vehicle.** k3s itself is Linux-glued (embedded containerd + flannel + iptables), so fork its *shape*, not its code. |
| **(b)** | **virtual-kubelet provider driving jails** (via ocijail/Podman API or AppJail) | Low to medium | ~40–50%: no node Services datapath, no DaemonSets, partial probes/exec/downward API, not conformant | Low technically, **high strategically** (dead end; vk-jails was abandoned) | **Only as a 2-week demo or bridge.** Not on the critical path. |
| **(d)** | **Nomad-like scheduler + k8s API shim** | Very high: re-implements API semantics; Triton/sdc-docker is the cautionary tale | 20–40% | Very high, plus **Nomad is BUSL-licensed** | **Reject.** |
| **(h)** *(new)* | **FreeBSD as a guest workload on Linux k8s** (urunc/Kata running FreeBSD microVMs; FreeBSD OCI images as VM rootfs) | Low to medium | 100% control plane; FreeBSD apps run as VMs | Low | Not "FreeBSD nodes", but cheap and complementary. Watch the urunc work; possibly a small side quest. |
| **(i)** *(new)* | **Lie about the OS**: a FreeBSD kubelet that reports `kubernetes.io/os=linux` and runs everything through the Linuxulator | Low to start | Looks like 100%, really much less | **Very high:** silent breakage of DaemonSets/agents that assume Linux kernel interfaces | **Reject.** Keep "freebsd" honest. Offer Linux compatibility via an explicit RuntimeClass + a mutating webhook, not by lying. |
| **(j)** *(new)* | **FreeBSD as storage/network infra for Linux k8s** (ZFS CSI over iSCSI/NVMe-oF, pf/CARP load balancers as a `LoadBalancer` provider, BGP) | Low | N/A (Linux nodes) | Low | **Cheap early wins that build credibility and users.** freebsd-csi already exists. |

### 5.1 Recommended phased path

**Phase 0: reproduce and stabilize (weeks 0–4)**
- Reproduce `dfr/kubernetes-demo` on FreeBSD 15.x (amd64 and arm64). Rebase dfr's branches to the current k8s
  minor.
- **Contact dfr (and Sam Karp, Alice Sowerby/FF) before writing code.** Propose co-maintenance and offer
  CI/hardware. This de-risks bus factor more than any technical choice.
- Stand up **our own CI**:
  - vmactions/freebsd-vm (or self-hosted FreeBSD runners) cross-building kubelet, kube-proxy and kubeadm for
    freebsd/amd64+arm64 **nightly against k8s master**, to catch bit-rot like `possibleCPUs` within 24h.
  - Plus a nightly single-node kubeadm bring-up + smoke test on a real FreeBSD VM.
- Fix the trivial upstream gaps generically and send them upstream: the `possibleCPUs` fallback and the kubeadm
  preflight `_other` stubs.

**Phase 1: MVP, "FreeBSD workers in a normal cluster" (months 1–3)**
- Stock Linux control plane (kubeadm/k3s). FreeBSD workers run patched kubelet + CRI-O + ocijail + the pf
  kube-proxy + CNI (bridge/host-local first, then VXLAN overlay or flannel-equivalent / BGP for multi-node; see
  03-networking).
- Node carries `kubernetes.io/os=freebsd` (automatic) plus a taint `os=freebsd:NoSchedule` so Linux-pinned
  DaemonSets never land there by accident.
- Workloads: FreeBSD-native images only (official `freebsd-runtime`, daemonless, Buildpacks-built Go/Rust apps).
- **Deliverables:**
  - a Sonobuoy/e2e run with a published pass rate on the non-`[LinuxOnly]` conformance subset
  - a demo where Argo CD/Flux deploys a FreeBSD-native app to FreeBSD nodes and a Linux app to Linux nodes from
    the same repo
  - metrics-server, `kubectl logs/exec/port-forward` and Services/Ingress all working

  **This is the earliest real value, and most of the code exists.**

**Phase 2: correctness (months 3–9), overlaps with 02/03/04 research**
- An rctl-backed `container_manager_freebsd`: requests/limits, QoS, eviction signals, and a Summary API that is
  honest about what it can't do.
- Kernel work where needed: per-jail memory hard limit / OOM, CPU bandwidth, PSI-like pressure.
- CSI on ZFS for local and remote PVs. Solve mount-propagation semantics with nullfs.
- **Two Linux RuntimeClasses:**
  - `linuxulator`: fast, best-effort, with a documented syscall compatibility matrix
  - `bhyve`: Kata-style per-pod microVM, full Linux compatibility

  Plus a mutating webhook that maps `os: linux` image platforms to the right RuntimeClass on FreeBSD nodes, and
  handles the `spec.os` kubelet-admission conflict.
- Second runtime path via containerd + runj, if a maintainer appears.

**Phase 3: adoption (months 9–18)**
- "kbsd": a pkg + rc.d single-install node bundle, a Sylve integration ("join this host to a k8s cluster"), and a
  BSD-licensed, appliance-friendly build.
- Optional FreeBSD control plane (already works; Norlin/dfr) for all-FreeBSD clusters.
- Publish Tier-3-compliant documented builds. Gather demand data (downloads, named production users).

**Phase 4: upstream (18 months+, only if Phase-3 demand is real)**
- A SIG-Node KEP for FreeBSD node support (worker-only, KEP-116-shaped).
- API review for `spec.os.name: freebsd`.
- A SIG-Release Tier-3 → Tier-2 conversation. This is blocked by the Go first-class rule unless SIG Release
  re-interprets "architecture" or Go changes FreeBSD's tier, so realistically aim for **Tier 3 with an official
  docs mention**.

**Kill criteria (be honest):**
- At the end of Phase 1, fewer than ~3 external organisations are willing to trial it.
- Or no second maintainer, or no sponsor, has emerged by month 6.

If either holds, fall back to (e) + (j): FreeBSD as hypervisor and storage under Linux k8s. That still delivers
value with a tiny maintenance budget.

---

## 6. Which BSD?

| | **FreeBSD** | DragonFly BSD | NetBSD | OpenBSD |
|---|---|---|---|---|
| Container primitive | **Jails + VNET**, hierarchical, mature; **OCI runtime-spec v1.3 platform**; ocijail and runj exist | Jails (old FreeBSD-4-era lineage, no VNET-equivalent maturity) | **None in tree.** "Cells for NetBSD" (ex-"Jails for NetBSD", kauth/secmodel) is out-of-tree and experimental, and **explicitly "not a container platform"** ([tech-kern 2026-03](https://mail-index.netbsd.org/tech-kern/2026/03/09/msg030883.html)); on the roadmap only | **None** (chroot + pledge/unveil, which is process sandboxing, not containers) |
| Resource control | RACCT/RCTL (weak; see 02) | Minimal | None per-container | login.conf rlimits only |
| Hypervisor | **bhyve** (+ libvirt, CPUID control work in 2026) | NVMM (from NetBSD), active commits 2026 | NVMM + Xen; MICROVM kernel config / smolBSD (2025) | vmm(4)/vmd (modest) |
| Firewall | **pf** (FreeBSD fork) + ipfw + netgraph | pf (old import) + ipfw | **npf** (+ ipf) | **pf** (reference implementation) |
| Filesystem | **OpenZFS** (first-class, 2.4) + UFS | HAMMER2 | FFS, ZFS (older port), LFS | FFS only |
| Linux binary compat | **Linuxulator** (actively maintained, amd64/arm64) | Old/limited | compat_linux (older ABI coverage) | **Removed** (mid-2010s) **[verify]** |
| Go support **[measured, Go 1.27.3]** | freebsd/{386,amd64,arm,arm64} **secondary** (riscv64 absent from the 1.27 list **[verify]**) | dragonfly/amd64 secondary (single arch) | netbsd/{386,amd64,arm,arm64} secondary | openbsd/{386,amd64,arm,arm64,ppc64,riscv64} secondary; **since OpenBSD 7.5 `syscall(2)` is removed and Go must go through libc stubs** ([go#63900](https://golang.org/issue/63900)); `golang.org/x/sys/unix` users doing raw syscalls broke |
| Container ecosystem | Podman/Buildah/CRI-O (upstream)/containerd/runj, official OCI images, daemonless, Sylve | None | None | None |
| Community / resources | Largest BSD; FreeBSD Foundation funds containers, bhyve and Sylve; corporate users | Very small (6.4.2 May 2025, 6.6 still pending in 2026) | Small, research-y | Small, security-focused, hostile to "kitchen-sink" features |
| **Verdict** | **The only viable target.** | No | No (maybe in 3+ years if Cells lands) | No (philosophically and technically) |

The other BSDs only matter as idea sources:
- OpenBSD: pf syntax, pledge/unveil-style per-container hardening.
- NetBSD: rump kernels, smolBSD microVMs for fast-booting sandbox VMs.

---

## 7. Specific facts other workstreams should know

- **For kubelet porting:** don't start from the stubs. Diff against `dfr/kubernetes:freebsd-kubelet-v1.36.1`, which
  already has:
  - `container_manager_freebsd.go` (305 LoC)
  - `cadvisor_freebsd.go` (326)
  - `cri_stats_provider_freebsd.go` (153)
  - `summary_sys_containers_freebsd.go`
  - `subpath_freebsd.go` (589)
  - `hostutil_freebsd.go`
  - `mount_freebsd.go` in mount-utils (220)
  - emptyDir, fsquota and atomic_writer support
  - a FreeBSD-specific `TerminationMessagePathDefault`
  - a default kubelet root change

  The `02-resource-isolation.md` statement "kubelet builds the stub files" is true of upstream, **not** of dfr's
  branch.
- **For networking:**
  - kube-proxy: `dfr/kubernetes:freebsd-proxy-v1.36.1` adds `pkg/proxy/pf/proxier.go` (720 LoC) and a
    `server_freebsd.go`.
  - CNI: the demo uses `bridge` with pf `nat`/`rdr` anchors (`cni-rdr/*`, table `<cni-nat>`) and sysctls
    `net.pf.filter_local=1` and `net.link.bridge.pfil_member=1`.
  - CRI-O on FreeBSD currently **disables hostPort mapping** (WIP commit).
- **For kubeadm/bootstrap:** `freebsd-kubeadm-v1.36.1` adds an rc.d `initsystem_freebsd.go`, FreeBSD preflight
  checks, `constants_freebsd.go` (paths under `/usr/local/etc/kubernetes`), and a FreeBSD system-validator. It also
  **templatizes the CoreDNS and kube-proxy `nodeSelector` OS**, and **comments out CoreDNS `capabilities`**, because
  there are no Linux capabilities in jails. That last change is a PSA/securityContext semantics gap to design
  properly.
- **For runtime:** CRI-O FreeBSD support is upstream, but `make` support is still an open PR (#10005), and stats,
  devices and RunAsUser are on `dfr/cri-o:freebsd-wip-1.37.0` (379 commits behind main at time of check).
  containerd's CRI plugin compiles on FreeBSD but has no FreeBSD-specific sandbox code.

---

## 8. Sources (primary first)

- k8s PRs/issues: [#115870](https://github.com/kubernetes/kubernetes/pull/115870), [#120003](https://github.com/kubernetes/kubernetes/pull/120003), [#97270](https://github.com/kubernetes/kubernetes/pull/97270), [#50211](https://github.com/kubernetes/kubernetes/pull/50211)
- SIG Release platform tiers: [platforms.md](https://github.com/kubernetes/sig-release/blob/master/release-engineering/reference/platforms.md); [community platforms guide](https://github.com/kubernetes/community/blob/master/contributors/guide/platforms.md)
- Go: [Porting policy](https://go.dev/wiki/PortingPolicy), [FreeBSD wiki](https://go.dev/wiki/FreeBSD), [OpenBSD libc syscalls #63900](https://golang.org/issue/63900)
- dfr: [kubernetes-demo](https://github.com/dfr/kubernetes-demo), [dfr/kubernetes branches](https://github.com/dfr/kubernetes/branches), [dfr/cri-o](https://github.com/dfr/cri-o), [ocijail](https://github.com/dfr/ocijail), [Mastodon: "FreeBSD native k8s node"](https://mastodon.world/@dfr/110938553831383781), [OCI Containers for FreeBSD](https://medium.com/@dfr/oci-containers-for-freebsd-512a6df2bc85)
- CRI-O: [#6492](https://github.com/cri-o/cri-o/issues/6492), [#7472](https://github.com/cri-o/cri-o/pull/7472), [#7727](https://github.com/cri-o/cri-o/pull/7727), [#9005](https://github.com/cri-o/cri-o/issues/9005), [#10005](https://github.com/cri-o/cri-o/pull/10005)
- runj: [repo](https://github.com/samuelkarp/runj), [OCI 1.3 post](https://samuel.karp.dev/blog/2025/11/runj-adopts-oci-130/), [2021 intro](https://samuel.karp.dev/blog/2021/03/runj-a-new-oci-runtime-for-freebsd-jails/)
- FreeBSD Foundation: [OCI 1.3 announcement](https://freebsdfoundation.org/?p=28383), [2024 DevSummit](https://freebsdfoundation.org/blog/2024-freebsd-developer-summit-oci-containers-on-freebsd/), [Podman testing 2024](https://freebsdfoundation.org/blog/advancing-cloud-native-containers-on-freebsd-podman-testing-highlights/), [WG notes (HackMD)](https://hackmd.io/hq_NOVL4RZS7xYYMqfJ6-A), [WG list post](https://lists.freebsd.org/archives/freebsd-enterprisewg/2025-November/000131.html)
- FreeBSD status reports: [2025Q3](https://www.freebsd.org/status/report-2025-07-2025-09/), [2026Q1](https://www.freebsd.org/status/report-2026-01-2026-03/), [2026Q2](https://www.freebsd.org/status/report-2026-04-2026-06/), [2026Q3](https://www.freebsd.org/status/report-2026-07-2026-09/)
- Handbook: [OCI Containers](https://docs.freebsd.org/en/books/handbook/containers/)
- Norlin: [releases](https://github.com/tnorlin/kubernetes/releases), [FreeBSD control plane](https://kubernaut-eu.pages.dev/posts/welcome-mandala-kubernetes-v129-on-freebsd-controlplane), [forum thread](https://forums.freebsd.org/threads/kubernetes-planternetes-on-freebsd.90030/latest)
- Others: [rikatz/vk-jails](https://github.com/rikatz/vk-jails), [ndenev/freebsd-csi](https://github.com/ndenev/freebsd-csi), [Calico on jails talk](https://events.eurobsdcon.org/2025/talk/UQX99C/), [urunc BSD talk](https://events.eurobsdcon.org/2026/talk/JHXWDC/), [f3s](https://www2.buetow.org/gemfeed/2024-11-17-f3s-kubernetes-with-freebsd-part-1.html), [Sylve](https://github.com/AlchemillaHQ/Sylve), [nomad-pot-driver](https://github.com/bsdpot/nomad-pot-driver), [virtual-kubelet](https://github.com/virtual-kubelet/virtual-kubelet), [vmactions/freebsd-vm](https://github.com/vmactions/freebsd-vm), [Triton/sdc-docker](https://bcantrill.dtrace.org/2015/03/24/triton-docker-and-the-best-of-all-worlds/), [TrueNAS leaves FreeBSD](https://www.theregister.com/2024/03/18/truenas_abandons_freebsd/), [lobste.rs jails discussion](https://lobste.rs/s/lfhj2v/freebsd_jails_containers), [antenore jail orchestration 2026](https://antenore.simbiosi.org/freebsd-jail-orchestration/)
- Windows precedent: [KEP-116](https://www.kubernetes.dev/resources/keps/116/), [KEP-2578](https://www.k8s.dev/resources/keps/2578/), [KEP-2802](https://www.kubernetes.dev/resources/keps/2802/), [v1.14 GA blog](https://kubernetes.io/blog/2019/04/01/kubernetes-v1.14-delivers-production-level-support-for-windows-nodes-and-windows-containers)
- NetBSD Cells: [tech-kern](https://mail-index.netbsd.org/tech-kern/2026/03/09/msg030883.html); [NetBSD virtualization roadmap](https://nxr.netbsd.org/xref/src/doc/roadmaps/virtualization)

### Appendix A: local measurements (reproducible)

```sh
# kubernetes @ b4226aca, Go 1.27.3, host darwin/arm64
cd ~/bsdk8s/upstream/kubernetes
GOOS=freebsd GOARCH=amd64 go build -o /dev/null ./cmd/kubelet
#   pkg/securitycontext/util.go:217:23: undefined: possibleCPUs     <- the only error
# overlay: pkg/securitycontext/util_freebsd.go = `func possibleCPUs() []int { return nil }`
GOOS=freebsd go build -overlay overlay.json ./cmd/{kubelet,kube-proxy,kube-apiserver,kube-scheduler,kube-controller-manager,kubectl}  # all OK
GOOS=freebsd go build -overlay overlay.json ./cmd/kubeadm
#   preflight: MemCheck missing Check(); undefined addExecChecks
grep -rl 'go:build windows' --include='*.go' pkg cmd | wc -l     # 78
grep -rl 'go:build.*freebsd' --include='*.go' pkg cmd | wc -l    # 8 (11 incl. staging)
grep -c '^- testname' test/conformance/testdata/conformance.yaml  # 466 ; [LinuxOnly] in 60
cd ../containerd && GOOS=freebsd go build ./cmd/containerd        # OK, CRI plugin included via *_other.go stubs
```
