# 02 — Resource isolation & accounting: Linux cgroups v2/namespaces vs FreeBSD (and other BSDs)

Status: research draft, 2026-11-13. Sources are shallow clones under `~/bsdk8s/upstream/`:
freebsd-src @ `f429c4a6` (2026-11-13), kubernetes @ `b4226aca` (2026-11-14, ~v1.37 dev), runc, runtime-spec,
runj, ocijail, DragonFlyBSD, netbsd-src, openbsd-src. All paths below are relative to those clones.
Claims marked **[verify]** come from reading code statically and should be checked on a running system.

---

## 0. Executive summary

1. **Kubelet needs a cgroup tree, not just limits.** It builds `kubepods → {burstable,besteffort} → pod<uid> → container`
   and writes `cpu.weight`, `cpu.max`, `memory.max`, `memory.min/low/high` (MemoryQoS is **Beta, on by default** in this
   tree), `pids.max`, `hugetlb.*`, and `cpuset.cpus/mems` at every level. It also reads PSI (`KubeletPSI` is **GA and
   locked on** in this tree), `memory.stat`, and `cpu.stat`. The tree is managed in two places. Kubelet owns the
   node, QoS and pod levels (`pkg/kubelet/cm`). The CRI runtime owns the container level (`LinuxContainerResources`).
2. **On GOOS=freebsd, kubelet does none of this today.** Kubelet builds the stub files
   `container_manager_unsupported.go` and `kuberuntime_container_unsupported.go`, and the stub
   `generateContainerResources` returns `nil`, so **no resource limits reach the runtime at all**. The OCI runtime
   spec's FreeBSD section (`runtime-spec/config-freebsd.md`) has **no resources object**, and neither runj nor
   ocijail implements rctl (`runj/docs/spec-coverage.md:76-79`).
3. **FreeBSD RACCT/RCTL is a real accounting engine with weak enforcement.**
   - Counting and `deny` work well for countable things: maxproc, nthr, openfiles, SysV IPC, memlock, and
     `swapuse`, which is actually an *anonymous-memory commit charge*.
   - **CPU limiting is a 1 Hz, whole-process, EWMA-based stop/start hack.** It is unusable for `100m`-style
     limits without severe latency stalls.
   - **`memoryuse` is not a limit.** A once-per-second daemon deactivates pages of each process
     (`vm_swapout.c:374-412`), which is closer to a lazy `memory.high` than to `memory.max`.
   - There are **no CPU weights**, **no memory protection (min/low)**, **no per-jail OOM**, **no PSI**, no page-cache
     charging, and the **ZFS ARC is completely unaccounted**.
4. **The jail hierarchy can be the cgroup tree.** Jails already nest, racct usage already rolls up to ancestor jails
   (`kern_racct.c:389,679`), rule matching at add time already walks `pr_parent` (`kern_rctl.c:1381`), and the OCI
   FreeBSD spec already has `jail.parent`. So `kubepods.burstable.pod<uid>.<ctr>` as nested persistent jails is a
   viable shape **without a new kernel object**. I found a probable bug: `rctl_proc_ucred_changed()` re-links only
   the *immediate* jail's rules, not ancestors' (`kern_rctl.c:1992-2090`). A process that attaches into a child jail
   after the parent's rule exists would escape the parent limit **[verify]**.
5. **Namespaces map acceptably except user namespaces and mounts.**
   - Network (VNET), UTS, IPC (`sysv*=new`, POSIX shm prefixed by jail path) and process visibility map well.
   - Mounts are *global* (no mount namespace), so mount propagation needs emulation.
   - PIDs are not virtualized (no PID 1 inside the jail).
   - There is **no user-namespace equivalent and no idmapped mounts**.
   - There is **no syscall filter (seccomp)** for native binaries.
   - Linux capabilities have no 1:1 mapping, but the MAC framework's `mpo_priv_check` over 248 `priv(9)` codes
     makes a capability-bounding policy module very feasible.
6. **Verdict.** FreeBSD is still the only BSD worth building on. DragonFly, NetBSD and OpenBSD are each worse for
   this goal; see §4. To get *conformant* Kubernetes resource behaviour you need roughly 5 substantial kernel
   projects: a CPU bandwidth controller, CPU weights, page-ownership memory accounting with reclaim, per-jail OOM,
   and PSI. On top of that come about 6 small ones. Memory is the hard one: real memcg-equivalent semantics
   (page-cache charging, min/low protection, targeted reclaim) is an engineer-year-class VM project and the most
   likely thing to be rejected upstream. **The pragmatic architecture is hybrid.** Run jails with
   "Kubernetes-grade-enough" limits for cooperative workloads. Offer a **bhyve microVM RuntimeClass** for anything
   needing hard memory/CPU isolation, Linux-exact semantics, or `hostUsers:false`. Inside the guest, Linux cgroups
   are real and the VM boundary gives FreeBSD a hard memory cap (bhyve wired memory) for free.

---

## 1. What kubelet actually needs from cgroups (evidence)

### 1.1 Who writes what

| Layer | Owner | Code |
|---|---|---|
| Node allocatable root `kubepods` (+ optional `system-reserved`, `kube-reserved`) | kubelet | `pkg/kubelet/cm/node_container_manager_linux.go:49-124` (`enforceExistingCgroup`) |
| QoS level `kubepods/burstable`, `kubepods/besteffort` (Guaranteed pods sit directly under `kubepods`) | kubelet | `pkg/kubelet/cm/qos_container_manager_linux.go:95-171` |
| Pod level `pod<uid>` | kubelet | `pkg/kubelet/cm/pod_container_manager_linux.go`, `helpers_linux.go:169 ResourceConfigForPod` |
| Container level | **CRI runtime** (containerd/CRI-O → runc) from `LinuxContainerResources` | `staging/src/k8s.io/cri-api/pkg/apis/runtime/v1/api.proto:1073-1093` |
| Resize of pod sandbox | kubelet → CRI `UpdatePodSandboxResources` | `kuberuntime_container_unsupported.go:42` (stub on BSD) |

The abstract resource set kubelet writes is `cm.ResourceConfig` (`pkg/kubelet/cm/types.go:27-44`): `Memory`,
`CPUSet`, `CPUShares`, `CPUQuota`, `CPUPeriod`, `HugePageLimit`, `PidsLimit`, plus `Unified map[string]string`
for raw cgroup v2 keys. `cgroup_manager_linux.go:326-380` converts it to libcontainer resources, and the vendored
`github.com/opencontainers/cgroups/fs2/*.go` writes the files.

### 1.2 Controller by controller

| Need | cgroup v2 file | Where | Semantics kubelet relies on |
|---|---|---|---|
| CPU requests → weight | `cpu.weight` | `helpers_linux.go:133 MilliCPUToShares` (2..262144), converted by `opencontainers/cgroups/utils.go:425 ConvertCPUSharesToCgroupV2Value` (1..10000) | **Proportional share under contention, work-conserving when idle**, hierarchical: besteffort gets `MinShares=2` (`qos_container_manager_linux.go:258`), burstable gets sum of requests. |
| CPU limits | `cpu.max` (quota/period), `cpu.max.burst` | `helpers_linux.go:93-127` (`QuotaPeriod=100000`µs, `MinQuotaPeriod=1000`), `fs2/cpu.go:40-70` | CFS bandwidth: hard ceiling enforced per 100 ms period, per cgroup, aggregated over all threads/CPUs. |
| CPU idle class | `cpu.idle` | `fs2/cpu.go:25` | SCHED_IDLE-like group (used by some runtimes). |
| Memory limit | `memory.max` | `fs2/memory.go:64` | Hard limit including **page cache**. Exceeding it triggers reclaim *inside the cgroup*, then the memcg OOM killer inside the cgroup, and the container is reported as `OOMKilled`. |
| Memory throttle | `memory.high` | `helpers_linux.go:41-85 ApplyPodLevelMemoryHigh` (KEP-2570 formula) | Soft ceiling: reclaim and allocation throttling, no kill. |
| Memory protection | `memory.min` (Guaranteed), `memory.low` (Burstable) | `qos_container_manager_linux.go:355-430` | Reclaim protection from global memory pressure. **MemoryQoS is Beta and default-on at 1.37** (`pkg/features/kube_features.go:1752-1755`). |
| Swap | `memory.swap.max` | `fs2/memory.go:55`, NodeSwap / `GetContainerSwapBehavior` | LimitedSwap behaviour. |
| PIDs | `pids.max` | `cgroup_manager_linux.go:339`, `fs2/pids.go:34` | Counts **tasks (threads)**, not processes. Pod-level `--pod-max-pids`. |
| Huge pages | `hugetlb.<size>.max` | `cgroup_manager_linux.go:363-385`, `qos_container_manager_linux.go:198` | Per page-size limits for `hugepages-2Mi/1Gi` resources. |
| CPU manager (static) | `cpuset.cpus` | `cm/internal_container_lifecycle_linux.go:35`, `cpumanager/cpu_manager_others.go:39` | Exclusive cores for Guaranteed integer-CPU pods. The shared pool is shrunk on *all other* containers via CRI `UpdateContainerResources`. |
| Memory manager (NUMA) | `cpuset.mems` | `cm/internal_container_lifecycle_linux.go:46` | Pin memory to NUMA nodes (topology manager alignment). |
| OOM ordering | `/proc/<pid>/oom_score_adj` (CRI `oom_score_adj` field 5) | `pkg/kubelet/qos/policy.go:31-108` | kubelet/kube-proxy −999, Guaranteed −997, BestEffort 1000, Burstable `1000 − 1000·req/capacity`. |
| OOM detection | memcg events via cAdvisor OOM watcher | `pkg/kubelet/kubelet.go:1556` | Report `OOMKilled`, emit node events. |
| Eviction | signals in `pkg/kubelet/eviction/api/types.go:30-56` | `eviction_manager.go` | `memory.available` (node, root-cgroup working set), `allocatableMemory.available`, `nodefs.*`, `imagefs.*`, `containerfs.*`, `pid.available`. A cgroup v1 eventfd threshold notifier exists, but on v2 kubelet disables it (`threshold_notifier_linux.go:51-55`) and polls. |
| PSI | `cpu.pressure`, `memory.pressure`, `io.pressure` | `opencontainers/cgroups/fs2/psi.go`; CRI `PsiStats` (`api.proto:2259`); `KubeletPSI` GA+locked (`kube_features.go:1694-1697`) | Exposed in Summary API / metrics. Used by people for eviction/autoscaling heuristics. |
| Stats | `cpu.stat`, `memory.stat`, `io.stat`, `pids.current` | cAdvisor (`pkg/kubelet/cadvisor/cadvisor_linux.go`), or CRI stats (`PodAndContainerStatsFromCRI`, Beta default-off at 1.37 `kube_features.go:1827-1830`) | Working set = usage − inactive_file. Usage in core-nanoseconds. |
| IO | `io.max`, `io.weight` | `fs2/io.go` | **Not used by kubelet**. There is no Kubernetes API for IO limits, only via runtime annotations. |

### 1.3 What happens on FreeBSD today (critical)

- `pkg/kubelet/cm/container_manager_unsupported.go` has `//go:build !linux && !windows`. It is a stub container
  manager: no QoS cgroups, no node allocatable enforcement, no CPU/memory manager.
- `pkg/kubelet/kuberuntime/kuberuntime_container_unsupported.go:37` `generateContainerResources` returns `nil`.
  **Requests and limits are silently dropped.**
- `pkg/kubelet/cadvisor/cadvisor_unsupported.go`: no node or container stats. The eviction manager is blind.
- The Windows port is the precedent to follow. It has its own `container_manager_windows.go`, its own CRI message
  `WindowsContainerResources` (`api.proto:1309-1319`: `cpu_shares`, `cpu_count`, `cpu_maximum`,
  `memory_limit_in_bytes`, `rootfs_size_in_bytes`, `affinity_cpus`), and `eviction/defaults_windows.go`. A FreeBSD
  port needs the same: `*_freebsd.go` files plus either (a) reuse of `LinuxContainerResources` as the wire format, or
  (b) an upstream CRI proto addition `FreeBSDContainerResources`. **Recommendation: (a) first, which needs no CRI API
  change.** The runtime translates the Linux fields into jail and rctl settings. Then push (b) upstream once the
  semantics are proven.

---

## 2. FreeBSD primitives, in detail

### 2.1 RACCT/RCTL inventory

- **Resources:** `sys/sys/racct.h:52-77` defines 25: cputime, datasize, stacksize, coredumpsize, memoryuse (RSS),
  memorylocked, maxproc, openfiles, vmemoryuse, pseudoterminals, swapuse, nthr, msgq*, nsem*, nshm, shmsize,
  wallclock, pcpu, readbps, writebps, readiops, writeiops.
- **Subjects:** `sys/sys/rctl.h:86-89`: process, user, loginclass, jail. There is no "group of arbitrary processes"
  subject; jail is the only container.
- **Actions:** `rctl.h:95-131`: any signal, `deny`, `log`, `devctl`, `throttle`.
  - `throttle` is accepted **only for decaying resources** (the IO ones) and is **explicitly rejected for pcpu**
    (`kern_rctl.c:1300-1308`, `EOPNOTSUPP`).
  - pcpu "limits" are expressed as `deny`, which racctd turns into stop/start throttling.
- **Flags:** `kern_racct.c:131-176`. The key one is that `RACCT_RSS` (memoryuse) is **not DENIABLE**. It is only
  RECLAIMABLE.
- **Off by default.** GENERIC builds `RACCT` + `RCTL` but also sets `RACCT_DEFAULT_TO_DISABLED`
  (`sys/amd64/conf/GENERIC:92-94`). You need `kern.racct.enable=1` in loader.conf, which costs a reboot.
- **One global lock.** `racct_lock` is a single global `mtx` (`kern_racct.c:81-82`), taken on every racct add/sub.
  With hundreds of pods and thousands of processes this is a scalability concern **[verify with lockstat]**.
- **No in-jail introspection.** Usage is readable via `rctl -u jail:<name>` (`rctl_get_racct`). Inside a jail,
  processes cannot read their own limits, so there is no analogue to reading `/sys/fs/cgroup/memory.max`.

### 2.2 CPU

- **pcpu limit, as implemented** (`kern_racct.c:1116-1173` `racctd`, `1020-1055` `racct_updatepcpu_*`,
  `906-958` `racct_proc_throttle`, `880-899` `ast_racct`):
  - The kernel thread `racctd` wakes **once per second** (`pause("-", hz)`).
  - It recomputes %CPU as an EWMA (`0.7·new + 0.3·old`).
  - If a jail's pcpu rule is exceeded, *every process* in it with own %CPU > `kern.racct.pcpu_threshold` (default 1)
    is marked `p_throttled = -1`.
  - Threads block in `ast_racct` until a later racctd pass finds the jail under the limit.
  - Consequences:
    - Granularity is about 1 s, versus CFS bandwidth's 100 ms period with per-CPU slices.
    - A `cpu: 100m` limit becomes "run flat out for ~1 s, then freeze for several seconds". That is catastrophic
      for tail latency, liveness probes, and Go/JVM GC.
    - It is unit-oriented ("percent of one CPU") and capped at `100·ncpu`.
    - It throttles whole processes, not threads.
    - It does not account interrupt or softclock time charged on behalf of the jail.
- **No proportional share.** `sched_ule.c` (3516 lines) has nice-based timeshare priority, interactivity scoring and
  per-CPU runqueues. It has **no group or fair-share scheduling**: no weights, no hierarchy. Neither has
  `sched_4bsd.c`. `cpu.weight` (the thing every Kubernetes CPU *request* maps to) has **no FreeBSD equivalent at
  all**. Under contention, a BestEffort pod with 500 threads beats a Guaranteed pod with 4 threads.
- **New hook point.** `sys/kern/sched_shim.c` now dispatches scheduler entry points through ifuncs and selects the
  implementation by `kern.sched.name` at boot (`sched_shim.c:67-108`, `sysctl kern.sched.available`). That makes a
  *third, out-of-tree, boot-selectable scheduler* (e.g. a hierarchical fair scheduler) structurally feasible without
  forking ULE.
- **cpuset(2) works well.** Jails get root cpusets (`kern_cpuset.c:1722 cpuset_create_root`). `CPU_WHICH_JAIL` is
  supported for both affinity and domain policy (`kern_cpuset.c` `kern_cpuset_setdomain` at 2470, cases at
  ~2519/2552). Nested jail cpusets must be subsets. This is a good fit for the CPU manager static policy and for
  `cpuset.mems` via `domainset` (`sys/sys/domainset.h:88`). One gap: the static policy also expects isolation from
  *non-pod* work. FreeBSD has no `isolcpus`/`nohz_full`, but you can shrink cpuset 1 (host) via
  `cpuset -l -s 1`.

### 2.3 Memory

- **vmemoryuse (`RACCT_VMEM`).** Deniable at mmap time (`vm_mmap.c:1532-1560 kern_mmap_racct_check`). It is
  useless as `memory.max` because Go, the JVM, V8, and anything with sanitizers reserve huge virtual ranges.
- **memoryuse (`RACCT_RSS`).** Not deniable.
  - Enforcement lives in `vm_daemon` (`sys/vm/vm_swapout.c:290-420`). It runs every `hz` when racct is on, walks
    *all* processes, and for those whose racct says RSS > available calls `vm_swapout_map_deactivate_pages()`.
    It removes at most 25% of RSS per attempt and retries up to 10 times.
  - **Deactivation only moves pages to the inactive queue.** They are freed only if the global page daemon later
    needs memory. A jail can therefore stay far above its "limit" indefinitely on an idle machine, which makes it a
    lazy `memory.high`, not `memory.max`.
  - RSS is `vmspace_resident_count`, summed per process into the jail. **Shared pages (libc, shared mappings,
    fork-COW) are counted once per process**, so jail usage is *over*-estimated.
  - **Unmapped page cache is charged to no one.**
  - **ZFS ARC is charged to no one.** The ARC is the biggest memory consumer on a typical FreeBSD node, and a pod
    streaming a 100 GB file pushes everyone else's pages out without being charged.
- **swapuse (`RACCT_SWAP`) is the hidden gem.** It is charged in `swap_reserve_by_cred()` (`sys/vm/swap_pager.c:266-287`).
  That is the *swap reservation / commit charge* for private writable (anonymous) memory, not swap actually used.
  - `deny` makes mmap/brk/mprotect(RW) or COW fail with ENOMEM at reservation time.
  - It is **the closest thing FreeBSD has to a hard anonymous-memory cap today**, and it is precise: no sampling,
    no double counting of shared COW pages beyond what's reserved.
  - Caveats:
    - The failure mode is ENOMEM, not OOM-kill. Most programs abort, which is fine. Some hang or loop.
    - File-backed and shared memory are not covered (shm objects are charged to the creator's cred).
    - Interaction with `vm.overcommit` must be configured.
  - A rule such as `jail:pod.x:swapuse:devctl=…` plus `…:deny=…` lets the runtime report the event as `OOMKilled`.
- **No memory.min/low/high.** No protection, no soft-limit reclaim targeting a jail's pages. Pages carry no owner
  (`struct vm_page` has no memcg pointer). Only `vm_object` has a `cred` for swap charge.
- **OOM killer is global and size-based.** `vm_pageout_oom()` (`sys/vm/vm_pageout.c:1936-2070`) picks the largest
  process by `vm_pageout_oom_pagecount`, excluding `P_PROTECTED` (set via `procctl(PROC_SPROTECT)`) and system
  processes. It has no per-jail OOM, no `oom_score_adj` weighting, and no event to userland beyond the kill. The
  Linuxulator stores `oom_score_adj` per process (`sys/compat/linprocfs/linprocfs.c:2062-2085`), but **nothing in
  sys/vm reads it**, so it is cosmetic. `PROC_SPROTECT` is a usable stand-in for kubelet/containerd's −999.

### 2.4 PIDs

- `maxproc` (deny) and `nthr` (deny) per jail work. Linux `pids.max` counts threads, so map `pids.max → nthr`, plus
  optionally `maxproc` with the same value.
- The jail param `children.max` limits nested jails, not processes.
- `pid.available` eviction: compare `kern.maxproc` against `kern.proc` count, which is trivial in userland.

### 2.5 IO

- `readbps/writebps/readiops/writeiops` with `throttle` exist. Charged at:
  - `vfs_bio.c:2199,2272,2376`
  - `vfs_cluster.c:251,313`
  - `ufs_bmap.c:138`
  - `vm_fault.c:1977`
  - `kern_physio.c:115`
  - ZFS at DMU level (`sys/contrib/openzfs/module/os/freebsd/zfs/zfs_racct.c`)
- It is logical IO charged to `curproc` with sleep-based throttling (`kern.racct.rctl.throttle_*`). There is no
  per-device key and no weight.
- Kubernetes doesn't need it. It is a nice-to-have for noisy-neighbour control.

### 2.6 Hierarchy: are rctl rules hierarchical?

**Partially, and that is better than commonly assumed:**

- **Accounting rolls up.** Every racct add/sub walks `for (pr = cred->cr_prison; pr; pr = pr->pr_parent)` and
  adjusts each ancestor's `prr_racct` (`kern_racct.c:389-390, 679-680, 854-858`). A parent jail's usage *includes*
  its children's.
- **Rule linking at add time is hierarchical.** `rctl_rule_add` links a jail rule into every process whose prison
  *or any ancestor* matches (`kern_rctl.c:1379-1388`). Enforcement (`rctl_enforce`, `kern_rctl.c:495+`) checks each
  linked rule against the subject's aggregated racct, so a parent limit constrains the whole subtree.
- **Probable bug.** `rctl_proc_ucred_changed()` (`kern_rctl.c:1992-2090`), called on jail_attach/setuid/setloginclass,
  rebuilds the rule list from process rules, uid rules, loginclass rules and **only `newcred->cr_prison`'s rules**,
  not the ancestors'. A process that `jail_attach`es into `kubepods.burstable.podX.ctr` after the `podX` rule was
  added gets *no link* to the `podX` rule, and the pod-level limit is not enforced for it **[verify with a test]**.
  The fix is about 40 LOC and upstreamable.
- **prison_racct is keyed by jail name** (`kern_jail.c:5495-5530`). Same-named jails share accounting, so runtimes
  must use unique hierarchical names.
- Missing: no "limit must be ≤ parent" validation, no delegation, and no per-level rules on resources that can't be
  denied (pcpu, rss).

**Conclusion:** use **nested persistent jails as the cgroup tree**:
`kubepods` → `kubepods.burstable` → `kubepods.burstable.pod<uid>` (the pod sandbox jail, owns VNET + SysV IPC) →
`…pod<uid>.<ctr>` (container jails with `vnet=inherit` and `sysv*=inherit`, own `host.hostname`).
The OCI FreeBSD spec already has `jail.parent` (`runtime-spec/config-freebsd.md`, Jail section). Nesting a VNET
pod jail under non-VNET QoS jails needs checking (`kern_jail.c` PR_VNET handling ~793-801, 1836-1881) **[verify]**.

### 2.7 Things FreeBSD has that Linux cgroups don't, worth exploiting

- **Jail descriptors** (`sys/kern/kern_jaildesc.c`: fd-based jail handles with kqueue knotes, `jaildesc_knote`).
  Usable like pidfd/cgroup fds for race-free lifecycle and for event delivery (e.g. OOM events).
- **Jail metadata** (`sys/kern/kern_jailmeta.c`, `meta`/`env` params up to 4 KiB). Can carry pod UID, namespace and
  labels so host tools (`jls`, `rctl`, dtrace) are Kubernetes-aware.
- **DTrace SDT probes** on every racct op (`kern_racct.c` `SDT_PROBE3(racct, …)`) give a cheap path to PSI-like
  metrics before any kernel work.
- **Boot-selectable scheduler** via `sched_shim`.

---

## 3. Namespaces & securityContext mapping

### 3.1 Namespaces

| Linux ns | FreeBSD | Fidelity | Notes |
|---|---|---|---|
| mnt | jail `path` + nullfs/tmpfs/devfs mounts made by the runtime; `enforce_statfs` hides others | **Partial** | No per-jail mount table. Mounts are global objects in the host namespace. Mount propagation (`HostToContainer`, `Bidirectional`, needed by CSI node plugins) cannot be expressed: nullfs does not propagate sub-mounts. It needs runtime emulation (watch host mounts, re-nullfs) or a kernel "shared nullfs" feature. |
| pid | jail process visibility (jail sees itself + descendants' processes) | **Partial** | No PID virtualization: no PID 1 in the container, and PIDs are host PIDs. `shareProcessNamespace: true` maps to "all containers live in the pod jail itself" (no child jails), which loses per-container rctl. Alternative: per-container accounting via per-process rules. |
| net | VNET (`vnet=new`, child `vnet=inherit`) | **Good** | Pod-shared netns maps naturally to a pod jail. `hostNetwork` maps to a non-VNET jail with `ip4=inherit`. |
| uts | `host.hostname`/`host.domainname` per jail | **Good** | `host=new` per container jail. |
| ipc | `sysvmsg/sysvsem/sysvshm = new/inherit/disable` (`sysv_msg.c:1529-1690`); POSIX shm names prefixed by jail path (`uipc_shm.c:694-703`); mqueuefs per-root (`uipc_mqueue.c:454`) | **Good** | `hostIPC` maps to `inherit` from prison0. |
| user | **none** | **Missing** | Jail root is uid 0 constrained by `prison_priv_check()` (`kern_jail.c:4407`). On-disk ownership is real host uid 0. No idmapped mounts (nullfs has no uid/gid mapping). |
| cgroup | n/a | n/a | Only matters for Linux binaries reading `/proc/self/cgroup` (see shim proposal K10). |
| time | none | Irrelevant | Linuxulator rejects `CLONE_NEWTIME` (`linux_fork.c:411`). |

Linuxulator note: `unshare` and `setns` are `DUMMY` (`sys/compat/linux/linux_dummy.c:77,93`). `CLONE_INTO_CGROUP`
is rejected (`linux_fork.c:415`). Linux userland tools that create namespaces won't work inside a jail. Pods that
run nested runtimes (DinD, BuildKit, kind) are out of scope on jails and should go to the bhyve RuntimeClass.

### 3.2 securityContext field by field

| Field | Verdict | Mechanism / reason |
|---|---|---|
| `runAsUser` / `runAsGroup` / `supplementalGroups` / `supplementalGroupsPolicy` | **Supported** | setuid/setgid inside the jail. Resolve names against the image's `/etc/passwd`. |
| `runAsNonRoot` | **Supported** | Kubelet-side check. |
| `fsGroup` / `fsGroupChangePolicy` | **Supported** | Recursive chown/chmod by the runtime. ZFS dataset per volume makes it cheap. |
| `readOnlyRootFilesystem` | **Supported** | Mount the rootfs nullfs `ro` (or ZFS `readonly=on` clone). |
| `allowPrivilegeEscalation: false` | **Supported** | `procctl(PROC_NO_NEW_PRIVS_CTL)` (`sys/sys/procctl.h:64`). Also `nosuid` mounts. |
| `privileged: true` | **Emulatable** | Jail with `allow.*` maxed, `devfs_ruleset` open, `enforce_statfs=0`, `allow.mount.*`, `allow.vmm`, … It is still not "host root". Document the differences. |
| `capabilities.add/drop` | **Emulatable with K13** | No caps today. Coarse mapping to `allow.*` params: `NET_RAW → allow.raw_sockets`, `SYS_TIME → allow.settime/adjtime`, `CHOWN/FOWNER → default root`, `NET_BIND_SERVICE → allow.reserved_ports` or `mac_portacl`, `SYS_ADMIN → allow.mount.*` (reject partial), `IPC_LOCK → allow.mlock`, `SYS_CHROOT` default. Precise mapping needs a MAC module over `priv(9)` (`mpo_priv_check`, `sys/security/mac/mac_policy.h:454,929`). |
| `seccompProfile: RuntimeDefault` | **Must be rejected or no-op today**. With K12 it becomes "Supported for a syscall-number deny list". | No syscall filter for native ABI. Capsicum (`cap_enter`) is opt-in by the program and kills most unaware software. Pod Security Standard "restricted" *requires* RuntimeDefault or Localhost, so an honest no-op breaks policy semantics. |
| `seccompProfile: Localhost` | **Reject**, or translate the Linux profile for Linuxulator processes with K12 | — |
| `appArmorProfile` / `seLinuxOptions` | **Reject** (or map a tiny subset to `mac_bsdextended`/`mac_mls` labels). | Not worth it. |
| `procMount: Unmasked` | **Supported** | Mount procfs/linprocfs in the jail. The default masked behaviour is FreeBSD's default anyway, since there are no `/proc/kcore`-like leaks. |
| `sysctls` (pod) | **Partial** | VNET-virtualized `net.*` sysctls are per-jail. Map the "safe" Kubernetes set (`net.ipv4.ip_local_port_range` → `net.inet.ip.portrange.*`, `net.ipv4.tcp_syncookies` → `net.inet.tcp.syncookies`, `kernel.shm*` → SysV jail params). Reject `kernel.*`. |
| `hostUsers: false` (user namespaces) | **Reject on jails; Supported via bhyve RuntimeClass** | Root in a jail is already not host root (prison_priv_check, securelevel), but there is no UID remap of file ownership. Claiming support would be a lie for hostPath and volume semantics. |
| `windowsOptions` | Ignore | — |
| `hostPID` | **Privileged-only** | Jail visibility is downward-only, so no jail can see host processes. hostPID therefore means running the container as a non-jailed chroot, which loses all jail isolation. |
| `hostNetwork`, `hostIPC` | **Supported** | `ip4=inherit`/`ip6=inherit`; `sysv*=inherit`. |
| `securelevel` | Extra | Per-jail `securelevel` can harden pods beyond Linux (e.g. immutable flags). |

---

## 4. Other BSDs — quick honest verdicts

- **DragonFly BSD.** Jails exist with a nice capability bitmask (`sys/sys/jail.h:69-92`, `PRISON_CAP_*`), but there
  is **no VNET, no RCTL/RACCT** (only classic `RLIMIT_*`, `sys/sys/resource.h:103-120`), and no jail cpusets.
  Variant symlinks (`kern_varsym.c`) are a cute per-jail path trick. vkernel (`sys/platform/vkernel64`) is an
  interesting "userland kernel as microVM" idea, but it's x86-64-only and lightly maintained. The `usched_dfly`
  scheduler has no group fairness either. LWKT doesn't help here. Tiny community, small Go/containerd porting
  surface. **Verdict: worse than FreeBSD on every axis this project needs. No.**
- **NetBSD.** No jails at all; there is only a chroot-escape warning in `vfs_lookup.c:1092-1129`. `kauth(9)` plus
  `secmodel` (`sys/secmodel/{suser,securelevel,bsd44,extensions,overlay}`) is the cleanest authorization hook
  architecture of the four BSDs. It is a better base than `priv(9)` for writing a capability model, but you'd
  have to build *all* namespace isolation from scratch. Rump kernels (`sys/rump`) are a clever "library OS per pod"
  idea (gVisor-like). In practice, though, a rump kernel offers drivers and stacks, not a Linux or POSIX process
  environment that Kubernetes images expect. **Verdict: no. The isolation substrate doesn't exist.**
- **OpenBSD.** No jails, no VNET-like stack virtualization (rdomains give routing separation, but share one
  socket/PF namespace model), and only `RLIMIT_*`/login.conf. `pledge`/`unveil` are excellent but *self-imposed by
  the program*. `execpromises` lets a parent pre-pledge a child, but images aren't written for it and the promise
  set is coarse. `vmm(4)`/`vmd` is a simpler bhyve: fewer features, no live migration, limited device model.
  **Verdict: no as a node OS. Steal ideas only:** an `unveil`-like path allowlist would be a good model for
  `readOnlyRootFilesystem` hardening, and the pledge promise set is a good model for a FreeBSD
  "RuntimeDefault"-style syscall policy.
- **FreeBSD.** It is the only one with jails + VNET + RCTL + cpusets/domainsets + bhyve + Linuxulator + ZFS + an
  official OCI spec section (runtime-spec 1.3, Nov 2025) + active runtimes (ocijail, runj, podman/buildah ports).
  **Verdict: FreeBSD, and it isn't close.**

---

## 5. Proposed kernel modifications (FreeBSD)

LOC are rough, excluding tests and man pages. "Upstreamable" is my judgement of src-committer appetite.

| # | Proposal | Files touched | LOC | Difficulty | Upstreamable |
|---|---|---|---|---|---|
| **K0** | **Fix hierarchical rule relink**: in `rctl_proc_ucred_changed`, walk `pr_parent` chain like `rctl_rule_add` does. Add ATF test (jail_attach into child after parent rule). | `sys/kern/kern_rctl.c` | ~40–80 | Easy | **Yes** (bug fix) |
| **K1** | **Hierarchical rctl semantics, done right**. (a) Enforce "child limit ≤ effective parent" and report effective limits. (b) Per-level `rctl -u` with exclusive and inclusive usage. (c) Allow rules on jails from a jail's own parent (delegation, so kubelet in a jail can manage its subtree). (d) Replace global `racct_lock` with per-racct locks or atomics for counters. (e) In-jail read-only view of own limits/usage (new `rctl_get_self` or sysctl `security.jail.rctl.*`). | `kern_rctl.c`, `kern_racct.c`, `sys/sys/racct.h`, `usr.bin/rctl` | 800–1500 | Medium (locking rewrite is the risky part) | **Likely** for (a)(b)(e); (d) needs careful review |
| **K2** | **CPU bandwidth controller (cpu.max)**. Per-jail runtime budget per period (default 100 ms, configurable), charged from `sched_clock()`/`sched_switch()` into the jail's racct chain. On exhaustion, mark the jail "throttled". ULE's `tdq_choose`/`sched_add` skips threads of throttled jails (park them on a per-jail deferred list). A per-period callout refills and requeues. Hierarchical: charge each ancestor and throttle if any is out. Replaces the racctd pcpu hack. | `sys/kern/sched_ule.c`, new `sys/kern/kern_cpubw.c`, `kern_racct.c`, `sys/sys/proc.h` (td→jail bw pointer cache), `kern_rctl.c` (new resource `cpuquota` = µs/period, `cpuperiod`) | 1500–2500 | **Hard** (scheduler hot paths, SMP fairness, priority-inversion with throttled lock holders, the same problems Linux CFS bandwidth spent years on) | Possible if made optional (`options SCHED_BW`) and benchmarked neutral when off |
| **K3** | **CPU weights (cpu.weight)**. Option A: hierarchical fair scheduler as a third `sched_shim` implementation (EEVDF/WFQ over a jail tree, per-CPU group entities, load balancing). Option B ("cheap ULE hack"): per-jail virtual-runtime debt that biases ULE timeshare priority (like a dynamic nice) computed every tick. Gives rough proportionality for CPU-bound jails, nothing for latency. | A: new `sys/kern/sched_fair.c` + `sched_shim` registration. B: `sched_ule.c` `sched_priority()`, `kern_racct.c`. | A: 6000–10000. B: 500–900 | A: **Very hard** (multi-year to be production-grade). B: Medium | A: only as long-lived out-of-tree / experimental. B: plausible but reviewers will rightly call it a heuristic. |
| **K4** | **Page-ownership memory accounting + targeted reclaim (memory.max/high/min/low)**. Charge resident pages to an owner racct at allocation (`vm_page_alloc*` from object or fault context). Anonymous pages via `vm_object->cred`'s jail. Vnode pages via the first faulting or reading jail (Linux-style first-touch). Keep a per-jail page count (memory.current). Per-jail reclaim: when over `high`, scan that jail's objects (per-jail object list) and deactivate or launder their pages, plus allocation throttling. When over `max`, synchronous reclaim then OOM (K5). min/low: page daemon skips protected jails' pages in `vm_pageout_scan_*` when the jail is under its protection. | `sys/vm/vm_page.c`, `vm_object.c/h`, `vm_pageout.c`, `vm_fault.c`, `vm_phys.c` (maybe), `kern_racct.c`, `vm_swapout.c` (retire RSS loop) | 3000–6000 | **Very hard**. Page-owner field costs memory (8 B/page ≈ 0.2%). Touches the most performance-critical code in the kernel. | **Unlikely** as a whole. The per-page owner pointer is a hard sell. An object-level approximation (charge per `vm_object` and reclaim per object) is more upstreamable at ~1500 LOC but inaccurate for shared vnode objects. |
| **K4z** | **ZFS ARC per-jail charging**. Charge ARC buffers to the dataset's owning jail (`jailed=on` datasets) and evict per jail. | `sys/contrib/openzfs/module/zfs/arc.c` + FreeBSD glue | 1000+ | Very hard (upstream is OpenZFS, cross-platform) | **No** in the near term. Instead, cap `vfs.zfs.arc_max` and reserve it in kube-reserved. |
| **K5** | **Per-jail OOM + oom_score_adj**. (a) `p_oom_score_adj` in `struct proc` + `procctl(PROC_OOM_ADJ)`. `vm_pageout_oom()` weights by it (Linux formula: `points = rss + swap + adj·totalpages/1000`). Linuxulator `oom_score_adj` writes through. (b) Jail OOM: when a jail hits its memory max (from K4, or today from `swapuse` deny), kill the highest-score process *in that subtree* instead of returning ENOMEM, then post a `NOTE_JAIL_OOM` knote on the jail descriptor and a devctl event. The runtime reports `OOMKilled`. (c) OOM counters per jail (`memory.events` analogue). | `sys/vm/vm_pageout.c`, `sys/kern/kern_procctl.c`, `sys/sys/proc.h`, `kern_jaildesc.c`, `sys/compat/linprocfs/linprocfs.c`, `swap_pager.c` | 500–900 | Medium | **Yes** for (a) and (c). (b) yes if opt-in via jail param `oom=kill`. |
| **K6** | **Pressure stall info (PSI)**. Per-racct "some/full" stall time for cpu (runnable-but-not-running time: timestamp at `sched_add`, accumulate at `sched_switch`), memory (time in `vm_wait*`, page-fault pagein waits, K4 throttling), io (time in `bwait`/`biowait`/ZFS `zio_wait` on behalf of thread). 10s/60s/300s averages computed in racctd. Export via `rctl -u` and `kern.racct.psi.*` sysctl. CRI `PsiStats` filled by the runtime. | `sched_ule.c` (2 hooks), `vm_page.c`/`vm_fault.c`, `vfs_bio.c`, `kern_racct.c`, `racct.h` | 800–1500 | Medium (cheap per-event cost needed; per-CPU accumulation) | **Plausible**. It is generally useful (system-wide PSI too). |
| **K7** | **pids & hugetlb**. pids: none needed (`nthr` + `maxproc`). hugetlb: add `RACCT_LARGEPAGE` charged in `shm_largepage` allocation (`uipc_shm.c` `SHM_LARGEPAGE`) so `hugepages-2Mi/1Gi` resources can be bounded. | `uipc_shm.c`, `racct.h`, `kern_racct.c` | 100–200 | Easy | Yes |
| **K8** | **Mount propagation for jails**. A "shared" nullfs flag that propagates host sub-mounts under the lower dir into the upper (and optionally back). Needed by CSI node plugins and many operators. | `sys/fs/nullfs/*`, `sys/kern/vfs_mount.c` | 800–1500 | Hard (VFS locking) | Maybe |
| **K9** | **UID/GID mapping ("user namespace lite")**. (a) nullfs `-o mapuid=0:100000:65536,mapgid=…` translating ownership on the boundary (idmapped-mount analogue). (b) Optional jail param `uidmap` that translates `cr_uid` for *filesystem ownership only*, leaving priv semantics to jail. (a) alone gives most of `hostUsers:false`'s value for volumes. | `sys/fs/nullfs/null_vnops.c`, `null_vfsops.c`, `kern_jail.c` | (a) 600–1000, (b) 2000+ | (a) Medium, (b) Very hard (all credential checks) | (a) **plausible**, (b) unlikely |
| **K10** | **cgroupfs shim for Linux containers**. In `linsysfs`, synthesize `/sys/fs/cgroup` (cgroup2 layout) for the calling process's jail: `cgroup.controllers`, `cpu.max` (from K2 or pcpu rule), `cpu.weight`, `memory.max`/`memory.current`/`memory.stat`, `pids.max`/`pids.current`, `cpu.stat`, `*.pressure` (from K6), read-only. In `linprocfs`, `/proc/self/cgroup` → `0::/`, `/proc/self/mountinfo` cgroup2 entry. Makes JVM `UseContainerSupport`, Go 1.25+ container-aware GOMAXPROCS (Linux-only logic, which works under Linuxulator once it sees cgroupfs), .NET, Node and Python sizing correct. Writes return EPERM (or optionally map to rctl for nested runtimes). | `sys/compat/linsysfs/linsysfs.c` (648 lines today), `sys/compat/linprocfs/linprocfs.c` | 1000–1800 | Medium (pseudofs plumbing) | **Yes** (self-contained Linuxulator feature) |
| **K10n** | **Native equivalent**: read-only `sysctl security.jail.self.rctl.*` (or K1(e)) plus patches to Go runtime (`runtime/os_freebsd.go` GOMAXPROCS from jail cpuset ∩ cpu quota) and OpenJDK's `os_bsd` container support | userland | 300 kernel + upstream patches | Easy/medium | Go/OpenJDK patches upstreamable |
| **K12** | **Syscall filter**. (a) Linuxulator seccomp: implement `prctl(PR_SET_SECCOMP)`/`seccomp(2)` with cBPF, reusing `sys/net/bpf_filter.c`, run in the Linux sysentvec `syscallenter` path. (b) Native: a per-jail (inherited, ratchet-only) syscall bitmap checked in `subr_syscall.c syscallenter()` before dispatch. That gives a RuntimeDefault-like deny list (`kldload`, `reboot`, `ptrace` of non-children, `kenv` set, …). | (a) `sys/compat/linux/linux_misc.c`, new `linux_seccomp.c`; (b) `sys/kern/subr_syscall.c`, `kern_jail.c` | (a) 1500–3000, (b) 400–800 | (a) Medium-hard, (b) Easy-medium | (a) plausible, (b) contentious, since FreeBSD culture prefers Capsicum, but defensible as a jail param |
| **K13** | **mac_capabilities** policy module: per-credential capability bounding/effective sets (label on ucred). `mpo_priv_check` maps 248 `PRIV_*` codes (`sys/sys/priv.h`) to Linux capability names and denies when the cap is absent. Set by the runtime via `mac_set_proc` before exec. | new `sys/security/mac_capabilities/` | 800–1500 | Medium (the mapping table is the work) | **Yes** as a module |

**Not proposed / drop:**
- Cgroup v1 compatibility.
- Freezer: use `SIGSTOP` to the jail or `jail -r`. Note: kubelet doesn't use the freezer.
- RDMA/misc controllers.
- `io.weight`: no Kubernetes API.
- Per-device `io.max`.

**Minimum viable set for an honest beta:** K0, K1(a,b,e), K5, K7, K10, K13. That is about 4–6 KLOC, roughly
1 person-year with tests. Add the userland translation layer (kubelet `*_freebsd.go` + runtime rctl mapping),
using `swapuse` as the memory hard cap and the existing pcpu as a *documented best-effort* CPU cap.
**For conformance-grade:** add K2, K3-B, K6, K8. **K4 is the moonshot.**

---

## 6. Master mapping table

| Kubernetes feature | Linux mechanism | FreeBSD today | Gap | Proposal / decision |
|---|---|---|---|---|
| QoS / pod hierarchy (`kubepods/burstable/pod`) | cgroup v2 tree | Nested persistent jails; racct rolls up to ancestors | Ancestor rules lost on `jail_attach` (bug); no effective-limit view | **K0 + K1**; kubelet `cgroupManager` for freebsd = "jail tree driver" |
| CPU requests → `cpu.weight` | CFS group weights | **None** (ULE has no group fairness) | Total | **K3-B** short term, **K3-A** long term; until then requests are scheduling-only (admission) |
| CPU limits → `cpu.max` | CFS bandwidth (100 ms) | rctl `pcpu:deny` (1 Hz stop/start EWMA) | Wrong granularity, latency stalls | **K2**; interim: pcpu with warning, or *don't enforce* (`--cpu-cfs-quota=false` equivalent) |
| `cpu.max.burst`, `cpu.idle` | CFS | None | — | Drop (or with K2) |
| Memory limit → `memory.max` | memcg hard limit incl. page cache, OOM-kill in cgroup | `swapuse:deny` = anon commit cap (precise, ENOMEM); `memoryuse` = lazy 1 Hz deactivate | No page-cache charge, ENOMEM not OOMKilled, ARC uncharged | **Interim:** `swapuse` deny + `devctl`, `memoryuse` as high-water. **K5** for OOMKilled semantics; **K4** for real |
| `memory.high` (MemoryQoS) | memcg soft limit/throttle | `memoryuse` deny ≈ lazy high | Not targeted, 1 Hz | Map to `memoryuse`; **K4** |
| `memory.min/low` (MemoryQoS, default-on) | memcg protection | None | Total | Report unsupported (feature gate off on FreeBSD); **K4** |
| Swap (`NodeSwap`, LimitedSwap) | `memory.swap.max` | No per-jail swap-out limit (swapuse is reservation, not usage) | Semantics differ | NoSwap only on jails |
| `pids.max`, `--pod-max-pids` | pids controller (threads) | `nthr` + `maxproc` deny | None material | **Supported** |
| `hugepages-*` | hugetlb controller | Largepage shm exists, not accounted | Accounting | **K7** |
| CPU manager static | `cpuset.cpus` | jail cpuset (`CPU_WHICH_JAIL`), nested subsets | No isolcpus/nohz | **Supported** (shrink host cpuset 1) |
| Memory manager / NUMA | `cpuset.mems` | `cpuset_setdomain(CPU_WHICH_JAIL)` + domainset policies | Topology discovery source (cAdvisor sysfs) | **Supported**; kubelet needs FreeBSD topology reader (`kern.sched.topology_spec`, `vm.ndomains`) |
| Topology manager | sysfs topology | sysctl topology | Porting | Userland work |
| OOM ordering (`oom_score_adj`) | `/proc/pid/oom_score_adj` | `PROC_SPROTECT` only (binary); linprocfs value ignored | Weighted scoring | **K5(a)** |
| OOMKilled reporting | memcg `memory.events` | Nothing (signal is SIGKILL from global OOM, indistinguishable) | Event source | **K5(b,c)**; interim: rctl `devctl` on swapuse |
| Eviction: `memory.available` | root memcg working set | `vm.stats.vm.v_free_count + inactive + laundry` (+ ARC evictable) | Semantics of "available" with ARC | Userland: define as free+inactive+ARC-evictable minus `arc_min` |
| Eviction: `nodefs/imagefs/containerfs` | statfs | statfs/ZFS `zfs get available` | None | **Supported** (ZFS quotas per pod even better) |
| Eviction: `pid.available` | `kernel.pid_max` | `kern.maxproc` | None | **Supported** |
| `ephemeral-storage` limits | du / XFS quota | ZFS dataset `quota`/`refquota` per container | Better than Linux | **Supported (hard)** |
| PSI (`KubeletPSI` GA) | `*.pressure` | None | Total | **K6**; interim: dtrace-derived approximations or report empty |
| Stats (CPU ns, working set, RSS) | `cpu.stat`, `memory.stat` | `rctl -u` (cputime µs, memoryuse, swapuse, …) | Working set definition; no page cache | Runtime implements CRI stats (`PodAndContainerStatsFromCRI`); kubelet cAdvisor-less path |
| IO limits | `io.max` | rctl read/write bps/iops throttle (logical, curproc) | No device key | Optional via annotations; not a Kubernetes API |
| Network ns | netns | VNET | — | **Supported** |
| UTS / IPC | ns | jail host.* / sysv* params / shm prefix | — | **Supported** |
| PID ns, `shareProcessNamespace` | pidns | jail visibility, no PID virtualization | No PID 1; sharing vs per-container rctl conflict | Supported with caveats |
| Mount ns, mountPropagation | mntns + propagation | global mounts + nullfs | No propagation | **K8**, or runtime emulation; CSI node plugins run privileged/non-jailed |
| User ns (`hostUsers:false`) | userns + idmapped mounts | None | Total | **Reject on jails**; bhyve RuntimeClass; **K9(a)** |
| Capabilities | caps | `allow.*` params + `priv(9)` | No per-process sets | **K13** |
| seccomp | seccomp-BPF | None (Capsicum is opt-in) | Total | **K12**; until then reject or declare "Unconfined", which fails PSS restricted |
| AppArmor / SELinux | LSM | MAC framework | Different model | Reject |
| `allowPrivilegeEscalation` | no_new_privs | `PROC_NO_NEW_PRIVS_CTL` | — | **Supported** |
| `readOnlyRootFilesystem` | ro bind | ro nullfs / ZFS readonly | — | **Supported** |
| `privileged` | all caps, all devices | jail with allow.* + open devfs ruleset | Not equal to host | Emulated, documented |
| In-container limit discovery (JVM/Go) | `/sys/fs/cgroup` | Nothing visible in jail | Apps oversize heaps/threads | **K10** (Linux ABI), **K10n** (native) |

---

## 7. Honest verdict

- **What works with zero kernel changes:** pod/container isolation via nested jails + VNET, UTS/IPC, PIDs limits,
  cpusets and NUMA pinning, ephemeral storage (ZFS is *better* than Linux here), `allowPrivilegeEscalation`,
  read-only root, and a precise anonymous-memory cap (`swapuse`). That is enough for a demo and for trusted,
  cooperative, mostly-native-FreeBSD workloads.
- **What is fundamentally missing:**
  - **CPU fairness**: no weights, and a CPU cap that stalls processes for whole seconds.
  - **Memory semantics**: no page-cache or ARC charging, no protection, no targeted reclaim, no per-jail OOM.
  - User namespaces, seccomp, PSI.
- **These aren't polish.** Kubernetes' scheduling model *assumes* `requests` translate into guaranteed share
  under contention, and `limits` into OOMKilled / CFS throttling. Without K2/K3/K5, a FreeBSD node will pass
  "pod runs" conformance but fail "noisy neighbour" reality. It must be labelled honestly, e.g. node taint
  `bsdk8s.io/resource-isolation=best-effort`.
- **The VM is the deepest hole.** The memory work (K4) is where FreeBSD diverges most from Linux. Linux took memcg
  about 10 years to get right. Plan for it to be out-of-tree for a long time, or sidestep it: run any pod with
  hard memory needs in a **bhyve microVM RuntimeClass**. bhyve wires guest memory, so the cap is hard by
  construction, and inside the guest real cgroups give Linux-exact semantics.
- **Recommended plan:**
  1. Ship the jail-tree cgroup driver plus K0, K5, K7, K10 and K13 first. These are tractable and mostly
     upstreamable.
  2. Prototype K2 behind `options SCHED_BW` and measure it.
  3. Choose K3-B over K3-A for year one.
  4. Do K6 opportunistically.
  5. Treat K4 as a research track.
- **Other BSDs:** none is a better base. FreeBSD by a wide margin.

## Sources (web)

- [FreeBSD Containers and Orchestration wiki](https://wiki.freebsd.org/Containers)
- [FreeBSD Foundation, Nov 2025 update (OCI runtime spec 1.3 adds FreeBSD)](https://freebsdfoundation.org/?p=28383)
- [Klara: Red Hat OpenShift vs FreeBSD jails](https://klarasystems.com/articles/red-hats-openshift-vs-freebsd-jails/)
- [Klara: Jails, not containers](https://klarasystems.com/articles/jails-not-containers-freebsd-isolation-done-right/)
- [Kubernetes on FreeBSD with Linux worker nodes (control plane only on FreeBSD)](https://medium.com/@norlin.t/kubernetes-on-freebsd-with-linux-worker-nodes-and-cilium-a87c50daef03)
- [AsiaBSDCon 2026 paper](https://2026.asiabsdcon.org/media/entry/question_uploads/final-paper_3tMo1OJ.pdf)
- Local: `runtime-spec/config-freebsd.md` (no resources section), `runj/docs/spec-coverage.md:76-79` (rctl unimplemented)
