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

## Upstream candidates (fork-first, then upstream; see docs/FORKS.md)

1. ocijail `jail: fix config::set type validation`: **opened as [dfr/ocijail#39](https://github.com/dfr/ocijail/pull/39)** (2026-11-14).
2. ocijail `rctl` resources: needs agreement on using `linux.resources` on FreeBSD vs a future `freebsd.resources` in runtime-spec. Proposal: accept `linux.resources` now (every engine already emits it), add `freebsd.resources` later.
3. kubelet FreeBSD resources: rides on dfr's kubelet series. Eventually a CRI `FreeBSDContainerResources` message (KEP).

## Next steps

- `OOMKilled` reason: add a `memoryuse:devctl` rule plus CRI-O FreeBSD exit-reason mapping, or kernel K5.
- Pod-level limits on the sandbox (parent) jail: the sum of container limits plus overhead.
- `UpdateContainerResources` (in-place resize) → `ocijail update` → rctl rule swap.
- cpuset (CPU manager static policy) → `cpuset -j`.
- Kernel K2 (accurate CPU limits below 500m) and K3 (CPU requests).
