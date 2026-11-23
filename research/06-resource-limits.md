# 06: Container resource limits on FreeBSD (implemented and measured)

*2026-11-14. FreeBSD 15.1-RELEASE-p6 arm64 (QEMU/HVF dev VM, 8 vCPU, no swap). Numbers come from real runs, not estimates.*

## Result

Pod `resources.limits` are now enforced on FreeBSD nodes. Before this work they were silently dropped: no rctl rules existed.

```
pod spec                         kubelet (CRI)                       CRI-O (unchanged)            ocijail (fork)                 kernel
limits.memory: 64Mi   ──►  Linux.Resources.memory_limit=67108864  ──►  linux.resources.memory  ──►  memoryuse:sigkill=67108864  ──►  rctl/racct
limits.cpu: 200m      ──►  cpu_quota=20000 cpu_period=100000      ──►  linux.resources.cpu     ──►  pcpu:deny=20
requests.cpu: 100m    ──►  cpu_shares=102                          ──►  linux.resources.cpu     ──►  (ignored, warning)
```

| Layer | Change | Where |
|---|---|---|
| kubelet | `generateFreeBSDContainerResources` fills `config.Linux.Resources` (CRI has no FreeBSD resources message). Honours `--cpu-cfs-quota[-period]`. Unit test. | `PlatformNerds/kubernetes` `bsdk8s-v1.36.1` (`patches/kubernetes/v1.36.1/bsdk8s/0002`) |
| CRI-O | **No change needed.** The generic create path already writes OCI `linux.resources` on FreeBSD. | n/a |
| ocijail | New `rctl.{h,cpp}`. Translates `linux.resources` into rctl rules on the container jail at create, and removes them at delete. Pure translation + unit test (`rctl_test`). | `PlatformNerds/ocijail` `bsdk8s-main` (`patches/ocijail/0002`) |
| ocijail | Bug fix: `config::set` type asserts aborted **every pod container** (parent jail) in non-NDEBUG builds. | `patches/ocijail/0001` |
| demo | Builds ocijail from the fork (against pinned CLI11 2.3.2) and `pkg lock`s it. | `PlatformNerds/kubernetes-demo` `bsdk8s` |

**Verified on a cluster built from scratch by `build-cluster.sh`:**
- Web pods run under `64Mi/200m`.
- A pod allocating 200 MB under `64Mi` is killed (exit **137**).
- A busy-loop pod limited to `500m` uses **50.3%** of one CPU.
- System pods (CoreDNS `170Mi`) are limited too.
- Rules are removed when pods are deleted.

## Measured rctl semantics (why the mapping is what it is)

### Memory: tested by allocating and touching 200 MB in a jail with a 64 MB rule

| Rule | Result | Usable as k8s memory limit? |
|---|---|---|
| `memoryuse:deny=64m` | All 200 MB allocated. **No enforcement** (only triggers reclaim, nothing to reclaim to without swap). | ❌ |
| `swapuse:deny=64m` | `malloc` fails at 37–47 MB. Hard cap, but it charges **writable anonymous mappings at mmap time**. | ❌. A Go server dies at startup: `fatal error: out of memory allocating heap arena map`. The JVM and other reserving allocators behave the same way. |
| **`memoryuse:sigkill=64m`** | Process killed once RSS > 64 MB. A 20 MB run and a Go server (5.4 MB RSS) are unaffected. | ✅ **Chosen.** Closest to `memory.max` + OOM kill. |

Caveats of `memoryuse:sigkill`:
- **Overshoot:** racctd checks once per second, so a fast allocator overshoots before the kill (the test reached ~192 MB under a 64 MB limit). Size `kube-reserved` and eviction thresholds accordingly.
- **No reclaim before kill:** Linux reclaims page cache before OOM-killing. Here RSS above the limit is killed outright.
- **Reason reporting:** the container's reason is `Error` (exit 137), not `OOMKilled`. CRI-O on Linux learns of OOM kills from cgroup events. On FreeBSD it needs an rctl `devctl` rule or a jail OOM knote (kernel **K5**).

### CPU: busy loop in a jail, CPU seconds measured over 20 s after 15 s warm-up

| `pcpu:deny=` | Measured | Notes |
|---|---|---|
| 20 | **40.0%** | 2× overshoot |
| 50 | **50.1%** | accurate |
| 50 (via k8s `500m`) | **50.3%** | accurate |

`kern.racct.rctl.throttle_pct`/`_pct2` at 200 and 400 changed nothing (they apply to the `throttle` action, not `pcpu` deny).

**Why:** `racctd` runs at **1 Hz**. pcpu is an exponential moving average weighted 0.7 to the newest sample (`kern_racct.c:1038–1046`). When it exceeds the limit, the process is suspended until a later pass sees the average below the limit (`:1160–1166`). Enforcement is therefore a 1-second on/off duty cycle with a floor around 33–50%. CPU limits below ~500m overshoot. This is the data behind kernel **K2** (a real per-jail bandwidth controller in ULE).

`cpu.shares` (CPU requests) has **no FreeBSD mechanism**: ULE has no group or proportional scheduling (kernel **K3**).

### Accounting

- `rctl -u jail:<name>` works and **is hierarchical**: a container jail's usage is included in its pod (parent) jail.
- ⚠️ rctl's `jail:` subject is the **jail name**. `rctl -u jail:<jid>` silently creates an empty entry for a jail *named* "<jid>" and reports zero. That looked like an accounting bug in our first run, but it was a footgun.
- rctl rules are keyed by name and **outlive the jail**. ocijail removes them on delete. If a different (older) ocijail deletes the container, they leak (seen once, harmless). Follow-up: clear stale rules for the name at create.

## Other findings along the way

- **Podman on FreeBSD drops `--memory`/`--cpus`.** The OCI bundle has no `linux` section at all (containers/common FreeBSD spec generation). With our ocijail, Podman would get limits for free once it emits `linux.resources`.
- **ocijail + CLI11 ≥ 2.4:** `ocijail --version` fails with "A subcommand is required". Upstream pins 2.3.2 via Bazel, but distro builds against current CLI11 break.
- **containers/common:** logs `getCPUInfo for OS freebsd not implemented` on every call.

## In-place pod resize (implemented 2026-11-15)

`kubectl patch pod --subresource resize` now changes a running FreeBSD container's limits without a restart, on
both nodes. The path: kubelet → CRI `UpdateContainerResources` → CRI-O (unchanged: it already ran
`<runtime> update --resources -`) → **`ocijail update`** → the jail's rctl rules are replaced.

Measured, two-node cluster (`agnhost pause` pod, `resizePolicy: NotRequired`):

| Step | Pod status `resources` | rctl on the container jail | Restarts |
|---|---|---|---|
| created | cpu 100m/200m, mem 64Mi/128Mi | `pcpu:deny=20`, `memoryuse:sigkill=128Mi` | 0 |
| resize up | cpu 300m/600m, mem 96Mi/256Mi | `pcpu:deny=60`, `memoryuse:sigkill=256Mi` | 0 |
| limit 8Mi (usage 46 MB) | unchanged; `PodResizeInProgress` error *"attempting to set container "c" memory limit (8388608) below current usage (46243840)"* | unchanged | 0 |
| resize down | cpu 50m/100m, mem 32Mi/80Mi | `pcpu:deny=10`, `memoryuse:sigkill=80Mi` | 0 |
| cpu limit 100m → 500m under a busy loop | cpu limit 500m | `pcpu:deny=50` | 0 |

The busy loop used 30% of a CPU at 100m (the small-limit overshoot measured above) and 50% at 500m. racct
`cputime` counts whole seconds, so these readings are ±5%.

What it took (each layer refused it before):

1. **ocijail `update --resources`** (`PlatformNerds/ocijail` `update-resources`, patch `ocijail/0010`):
   - The update is merged into the stored `linux.resources` (runc semantics: absent or null fields keep their value).
   - Managed rules are replaced by removing each resource:action first. rctl keeps duplicate rules side by side and
     the lowest wins, so just adding a rule can't raise a limit.
   - A memory limit below the jail's current `memoryuse` is refused, like Linux's EBUSY. This is best effort:
     racctd samples once per second.
2. **kube-apiserver**: the `podresize` admission plugin allowed only Linux nodes. It now allows `freebsd` too
   (patch `bsdk8s/0008`). **This is a control-plane change:** a cluster needs a patched apiserver to resize pods
   on FreeBSD nodes.
3. **kubelet** (patches `bsdk8s/0007`, `0009`):
   - Allocation now allows resize on FreeBSD (container-level only).
   - `cm/helpers_freebsd.go` carries the portable parts of `helpers_linux.go` (`ResourceConfigForPod`, shares and
     quota conversions). The stubs returned nil, and the resize failed with "unable to get resource
     configuration".
   - A FreeBSD pod container manager reports the allocated pod config. That has to be non-nil: a nil value
     **crashed the kubelet**.
   - `generateContainerResources` and `toKubeContainerResources` are no longer stubs, so the kubelet can see the
     resize finish.
4. **CRI-O stats** (patch `cri-o/0002`): container memory stats now report `memoryuse` as `usageBytes`. Without
   it, every memory decrease was refused with "missing container memory usage". Pod stats now report `memoryuse`
   instead of `vmemoryuse` (virtual size).

**Not done:**
- **Pod-level resize** stays unsupported, as do pod-level limits; that's the next item below.
- **Upstream e2e:** the `[sig-node] Pod InPlace Resize` conformance tests verify by reading
  `/sys/fs/cgroup/...` inside the container, so they still fail on FreeBSD. Windows skips them for the same
  reason. Making them count would need a FreeBSD verification path in the e2e framework (e.g. `rctl` from the
  node), an upstream test change.

## Node memory accounting and eviction (fixed and verified 2026-11-15)

The kubelet never evicted on FreeBSD, because it couldn't see memory pressure. Its node working set came from
racct `memoryuse` for jail 0, which is process RSS only and leaves out:
- the ZFS ARC;
- tmpfs (emptyDir `medium: Memory`, projected service-account tokens);
- other wired kernel memory.

On an idle 12 GB node it reported 790 MB used when 2.2 GB was. This is the prime suspect for the node-1 hang in
conformance run 6's first attempt (research/07).

The fix (`PlatformNerds/kubernetes` `23c18881738` + `e917379c18d`, patches `bsdk8s/0011-0012`): working set = (physmem
− free) − inactive pages not held by tmpfs − ARC above its minimum. It took three tests on node 2, each a BestEffort
pod filling a tmpfs emptyDir, behind a kill switch that deletes the pod if free memory stays under 400 MB:

| Test | Result | Log |
|---|---|---|
| 1: inactive counted as reclaimable | **Failed.** tmpfs pages moved to the inactive queue, and "available" *rose* to 5 GB while 326 MB were free. Kill switch deleted the pod. | `conformance/eviction-test1-inactive-bug.log` |
| 2: subtract tmpfs, read via `getfsstat(MNT_NOWAIT)` | **Failed.** Those are statistics cached at mount time, which tmpfs never refreshes (`df`: "stats possibly stale"), so tmpfs read as 0. | `conformance/eviction-test2-stale-statfs.log` |
| 3: `statfs` each tmpfs mount | **Evicted.** "available" tracked free memory within a few MB. At 448 MB available (threshold 500Mi) the kubelet logged `EvictionThresholdMet` and evicted the pod. Free never went below 421 MB. | `conformance/eviction-test3-evicted.log` |

The demo also reserves headroom: `systemReserved` 1Gi, `evictionHard` 500Mi, ARC capped at 25% of RAM.

**tmpfs charged to its pod, and the ZFS cache counted as used (2026-11-16)** (`cri-o/0007`, `bsdk8s/0017`):

| Test | Result | Log |
|---|---|---|
| 4: `hog` (tmpfs fill) vs `bystander` (300 MB RSS) | **Failed.** CRI-O now charged `hog` correctly (6.9 GB vs 303 MB), but no eviction: the ARC stayed at 2.5 GB with its target unchanged while free fell to 290 MB, and "ARC above its minimum" was counted as available. Kill switch. | `conformance/eviction-test4-arc-not-reclaimed.log` |
| 5: same, ARC counted as used | **Right victim.** "available" tracked free within ~2 MB. At 487 MB available `hog` was evicted; `bystander` kept running; free back to 7 GB. | `conformance/eviction-test5-right-victim.log` |

Details:
- **CRI-O charging rule:** CRI-O adds a pod's kubelet volume mounts that are tmpfs mount points to its sandbox
  working set, each file system once. Host tmpfs via hostPath isn't charged.
- **Tradeoff:** counting the ARC as used means evictions start earlier on hosts with a large cache. Hence the demo
  caps the ARC at 25% of RAM.
- **What test 4 suggests about the run-6 hang:** a node full of tmpfs, with an ARC that doesn't shrink and
  "available" overstated, is a plausible mechanism.


## Pod-level limits (implemented and verified 2026-11-16)

A pod is a jail (the pause/infra container's), and its containers are child jails, so pod limits are rctl rules
on the pod jail. That needed one kernel fix and four small changes.

**Kernel: rctl didn't enforce jail rules on child jails (plan item K0)** (`PlatformNerds/freebsd-src`
`bsdk8s/rctl-hier-relink`, `-15.1`; patches `freebsd-src/0003-0004`). Accounting was already hierarchical: the pod
jail's `memoryuse` equals its containers' sum. Enforcement had two bugs:

1. `rctl_proc_ucred_changed()` relinked only the process's own jail's rules, so every process entering a child
   jail (`jail_attach`) dropped the parent's rules. `rctl_rule_add()` already walked up the hierarchy.
2. `rctl_proc_rule_to_racct()` compared a jail rule against the usage of the process's own jail instead of the
   rule's jail.

Plus `kern.features.rctl_jail_hierarchy`, so userland can tell fixed kernels apart.

Measured on stock 15.1 vs the patched kernel, parent jail with two child jails:

| Test | Stock 15.1 | Patched |
|---|---|---|
| `maxproc:deny=<usage>` on parent, fork in a child | fork succeeds | `sh: Cannot fork: Resource temporarily unavailable` |
| `memoryuse:sigkill=<usage+200 MB>` on parent, 300 MB in a child | runs | killed (100 MB is fine) |
| `rctl -l process:<pid>` in a child | no parent rule | parent rules listed |

**Why the kernel check matters:** without the fix, a pod-jail rule counts the containers' usage but applies only
to the infra container. So `memoryuse:sigkill` would kill the *infra container* when the containers use too much.
Hence the gating:
- **CRI-O** applies pod limits only when `kern.features.rctl_jail_hierarchy=1` (`cri-o/0003`, `63a76d5d0`).
- **The kubelet** rejects pods with pod-level resources on other kernels (`PodLevelResourcesNotSupported: pod-level
  resources need a kernel that enforces jail rctl rules on child jails`) instead of running them unlimited.

**Kubernetes side** (`bsdk8s/0013-0014`):
- The kubelet sends the pod's limits with the sandbox, computed like the Linux pod cgroup's
  (`cm.ResourceConfigForPod`): memory only if every container has a limit, likewise CPU, overhead included, pod-level
  `spec.resources` honoured.
- CRI-O puts them on the infra container's `linux.resources`; ocijail turns those into rctl rules on the pod jail.

**Verified on both nodes** (both now boot `kernel.bsdk8s` = 15.1 + allow.pf + pf sticky fix + rctl hierarchy):

| Pod | Pod jail rules | Result |
|---|---|---|
| 2 containers × 128Mi / 200m+300m | `memoryuse:sigkill=256Mi`, `pcpu:deny=50` (containers keep their own) | derived from the container sum |
| `spec.resources.limits: {memory: 200Mi, cpu: 500m}`, containers without limits | `memoryuse:sigkill=200Mi`, `pcpu:deny=50`; containers none | `a` alone 300 MB → **killed**; `a` 40 MB + `b` 40 MB at once → both ok; `a` 80 MB + `b` 80 MB at once (~247 MB > 200) → **`a` killed**, `b` ok; no restarts |
| same pod on node 1 *before* its kernel upgrade | none | **rejected** at admission with the kernel-feature reason |

**Kernel test:** an ATF test (`tests/sys/kern/rctl_jail_hierarchy.sh`, patch `freebsd-src/0005`) covers the
feature flag, maxproc and memoryuse on a parent jail with a child. It passes 3/3 on the fixed kernel and 0/3 on
the previous one (`kernel.allowpf`), so it catches the bug.

### Pod-level in-place resize (implemented and verified 2026-11-16)

`kubectl patch pod --subresource resize` on `spec.resources`, and container resizes that change the pod's summed
limit, now update the pod jail (`bsdk8s/0015-0016`, `cri-o/0004-0006`):
- **kubelet:**
  - Pod-level resize is allowed with `kern.features.rctl_jail_hierarchy`.
  - The pod container manager reports the limits *applied* to the pod jail: recorded when the sandbox is created
    and on each accepted update, or reported as unlimited when unknown, so the kubelet re-sends them.
- **CRI-O:** `UpdatePodSandboxResources` runs `ocijail update` on the infra container (the pod jail).

| Test (node 2) | Pod jail before → after | Result |
|---|---|---|
| `spec.resources` 200Mi/500m → 400Mi/1 | `memoryuse:sigkill` 200Mi → 400Mi, `pcpu:deny` 50 → 100 | 300 MB in a container: killed before, runs after |
| container `a` 128Mi → 384Mi (pod limit = sum) | 256Mi → 512Mi | 300 MB in `a` runs |
| `spec.resources` memory 400Mi → 250Mi (usage 85 MB) | 400Mi → 250Mi | applied |
| `spec.resources` memory → 20Mi | unchanged | refused: "attempting to set pod memory limit (20971520) below current usage (89939968)" |

No container restarted.

**Two bugs found on the way, both fixed:**
- **Stale pod-jail limit after a container resize.** Until this change, a container resize never updated the pod
  jail's summed limit, so growing a container past the creation-time sum got it killed by the stale pod limit
  (live since pod-level limits shipped earlier the same day).
- **Pod-level update never sent.** The first version recorded the "applied" config in `applySandboxResources`,
  which runs on *every* pod sync from the desired resources, so the kubelet never saw a pod-level change. It's now
  recorded only once the runtime created the sandbox, with a regression test.

**Limits:**
- The applied-config record is in kubelet memory. After a kubelet restart the first resize re-sends the pod's
  limits (idempotent), but its update ordering is conservative.
- Removing a pod-level limit isn't supported, as upstream (#128675).
- **Which process dies.** When the pod limit trips, the process that crossed it is killed (racctd's per-process
  sample), not the largest one, as a Linux OOM kill would pick.

## CPU usage to the microsecond (freebsd-src 0046, CRI-O 0034; 2026-11-23)

Container and pod CPU usage (the CRI's `usageCoreNanoSeconds`, what metrics-server and the horizontal pod
autoscaler use) came from rctl's `cputime`, which is whole seconds.
- metrics-server turns it into a rate over 15 s, so a container's usage moved in steps of 67 millicores. A pod
  using 63m read 0, 67 or 133m.
- The HPA keeps the highest recommendation over its scale-down stabilization window, so one high reading kept
  the extra replicas. Two `[Feature:HPA]` behaviour tests failed: they never scaled down.
- The kernel already accounts CPU time in microseconds (`RACCT_IN_MILLIONS`); `rctl_racct_to_sbuf()` divides
  it down to seconds for display.

freebsd-src 0046 (branches `bsdk8s/rctl-cputime-us[-15.1]`) adds a `cputime_us` entry, the CPU time in
microseconds, to `rctl -u` and `rctl_get_racct(2)`. `cputime` keeps its meaning, and `rctl(8)` prints the new
entry like the others (`-h` humanizes it). CRI-O 0034 uses `cputime_us` when the kernel reports it, and
`cputime` otherwise.
- Running processes' CPU time is updated by racctd once a second, so a reading can be up to a second old.
  Over a 15 s window at 63m that is an error of a few millicores, not 67.
- Measured: a pod burning 63m now reads 64-65m, steadily.
- Both nodes boot `kernel.bsdk8s27`.

The HPA tests also needed a FreeBSD `resource-consumer` image (scripts/build-e2e-images-freebsd.sh,
`RESOURCE_CONSUMER_BINDIR`) and kubernetes 0062. Its `consume-cpu` throttles itself by its own CPU use, read
through a package that has only stubs outside Linux. On FreeBSD it saw no use and spun: asked for 250m it used
about 600m. It now uses `getrusage(2)`, as the Windows version uses `GetProcessTimes`.
- `[Feature:HPA]`, not Slow: 0/23 (no image), then 12/23, then 21/23, now **23/23**.

## Upstream candidates (fork-first, then upstream; see docs/FORKS.md)

1. ocijail `jail: fix config::set type validation`: **opened as [dfr/ocijail#39](https://github.com/dfr/ocijail/pull/39)** (2026-11-14).
2. ocijail `rctl` resources: **opened as [dfr/ocijail#40](https://github.com/dfr/ocijail/pull/40)** (2026-11-14), adding stale-rule cleanup at create, EPERM tolerance (ocijail inside a jail) and 3 root tests. Open design question for dfr: `linux.resources` on FreeBSD now vs a future `freebsd.resources`.
3. kubelet FreeBSD resources: rides on dfr's kubelet series. Eventually a CRI `FreeBSDContainerResources` message (KEP).

## Next steps

- `OOMKilled` reason: add a `memoryuse:devctl` rule plus CRI-O FreeBSD exit-reason mapping, or kernel K5.
- ~~Pod-level limits on the sandbox (parent) jail~~: done (above). Pod-level resize still open.
- ~~CPU stats: racct `cputime` has 1-second resolution, so `usageNanoCores` (metrics-server, HPA) is coarse.~~
  Done 2026-11-23: rctl reports `cputime_us` (below).
- ~~`UpdateContainerResources` (in-place resize) → `ocijail update` → rctl rule swap.~~ Done (above).
- cpuset (CPU manager static policy) → `cpuset -j`.
- Kernel K2 (accurate CPU limits below 500m) and K3 (CPU requests).
