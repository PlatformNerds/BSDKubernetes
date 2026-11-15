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

## Upstream candidates (fork-first, then upstream; see docs/FORKS.md)

1. ocijail `jail: fix config::set type validation`: **opened as [dfr/ocijail#39](https://github.com/dfr/ocijail/pull/39)** (2026-11-14).
2. ocijail `rctl` resources: **opened as [dfr/ocijail#40](https://github.com/dfr/ocijail/pull/40)** (2026-11-14), adding stale-rule cleanup at create, EPERM tolerance (ocijail inside a jail) and 3 root tests. Open design question for dfr: `linux.resources` on FreeBSD now vs a future `freebsd.resources`.
3. kubelet FreeBSD resources: rides on dfr's kubelet series. Eventually a CRI `FreeBSDContainerResources` message (KEP).

## Next steps

- `OOMKilled` reason: add a `memoryuse:devctl` rule plus CRI-O FreeBSD exit-reason mapping, or kernel K5.
- Pod-level limits on the sandbox (parent) jail: the sum of container limits plus overhead. This would also enable
  pod-level resize, via a real `GetPodCgroupConfig`/`SetPodCgroupConfig` on rctl.
- CPU stats: racct `cputime` has 1-second resolution, so `usageNanoCores` (metrics-server, HPA) is coarse.
- ~~`UpdateContainerResources` (in-place resize) → `ocijail update` → rctl rule swap.~~ Done (above).
- cpuset (CPU manager static policy) → `cpuset -j`.
- Kernel K2 (accurate CPU limits below 500m) and K3 (CPU requests).
