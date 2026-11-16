# 15: Linux containers see their limits: a cgroup v2 view of rctl (2026-11-16)

## The problem

rctl enforces pod/container limits, but a Linux program in a container couldn't *see* them. In a pod limited to
256 MiB / 500m there was no `/sys/fs/cgroup` and no `/proc/self/cgroup`, `/proc/meminfo` showed the host's 12 GB,
and `nproc` printed 8. Runtimes size themselves from cgroup files:
- **Temurin 21** chose `MaxHeapSize = 3 GB` and reported "No metrics available for this platform". The heap grows
  past the limit under load and rctl kills the JVM.
- **Go ≥1.25** sets GOMAXPROCS from `cpu.max`. **.NET** and **Node** size their heaps from cgroup limits.

This hits every unmodified Java/.NET/Node/Go service, and it fails under load rather than at startup. That made it
a bigger blocker than abstract Unix sockets (research/14).

## The design: `lincgroupfs` (kernel)

A read-only cgroup v2 hierarchy whose files are computed **for the reading process**, from its jail's rctl rules
and racct usage. It works like a cgroup namespace whose root is the container's cgroup.

| File | Source |
|---|---|
| `memory.max` | lowest `memoryuse` rule of the jail and its ancestors, with a deny or signal action (our pods use `sigkill`); otherwise `max` |
| `memory.current`, `memory.stat` (`anon`) | the jail's racct `memoryuse` |
| `cpu.max` | `pcpu` rule as `<quota> 100000` (pcpu is % of one CPU) |
| `cpu.stat` | `usage_usec` from the jail's `cputime` |
| `pids.max`, `pids.current` | `maxproc` |
| `cgroup.controllers`, `cpuset.cpus.effective`, `cpu.weight`, `memory.swap.max`, … | fixed defaults (`cpu.weight` 100: FreeBSD has no CPU weights) |

Parts:
- **`rctl_get_jail_limit()`:** limits come from this new function over the process's rule links. With
  hierarchical jail rules (the K0 fix), those include the pod jail's rules, so a container sees the effective
  pod-or-container limit.
- **Its own file system:** it is a second pseudofs in the linsysfs module, mounted on `/sys/fs/cgroup`, a directory
  linsysfs now provides. The JDK and .NET decide "cgroup v2" by `statfs("/sys/fs/cgroup").f_type ==
  CGROUP2_SUPER_MAGIC`. My first version put the files inside linsysfs; the JVM found the hierarchy (`Provider:
  cgroupv2`) but still used 3 GB because statfs said sysfs.
- **Linux statfs mapping:** `lincgroupfs` → `CGROUP2_SUPER_MAGIC`.
- **linprocfs:** lists it as `cgroup2` in `mountinfo`/`mounts`, and adds `/proc/<pid>/cgroup` (`0::/`) and
  `/proc/cgroups` (hierarchy 0, i.e. unified).
- **`kern.features.linux_cgroupfs`:** CRI-O mounts it (ro) for Linux images only when the kernel advertises this
  and CRI-O also provides `/sys`.

Kernel: `bsdk8s/linux-cgroupfs-15.1` (built and tested) and `bsdk8s/linux-cgroupfs` (main: cherry-picked cleanly,
not compiled); patch freebsd-src/0009. CRI-O: `9ec045350`, patch cri-o/0012. Both nodes boot `kernel.bsdk8s6`.

## Verified (Linux images, RuntimeClass `linux`, arm64)

| Check | Result |
|---|---|
| `/proc/self/cgroup`, mountinfo, `statfs` | `0::/`; `… /sys/fs/cgroup … - cgroup2 cgroup2 ro`; `cgroup2fs` |
| `memory.max`, `cpu.max` at 256Mi/500m | `268435456`, `50000 100000` |
| Temurin 21 `-XshowSettings:system` | Provider cgroupv2, Memory Limit 256.00M, CPU Quota 50000us, Effective CPU Count 1 |
| Temurin 21 `MaxHeapSize` | **132,120,576 (126 MiB)**, was 3,212,836,864 |
| Go 1.27 static binary, `cpu.max` 1.5 CPUs | NumCPU=8, **GOMAXPROCS=2** (Linux semantics: rounds up) |
| coreutils `nproc` at 500m | 1 (was 8) |
| Two containers in one pod | each sees its own limits (busybox 128Mi/1.5 CPU next to node 256Mi/0.5) |
| In-place resize to 192Mi/2500m | `memory.max` 201326592, `cpu.max` 250000 100000 immediately; new Go process GOMAXPROCS=3 |
| `memory.current` | live (1.7 MB for an idle shell), after racct's first ~1 s sample |
| Write attempt | refused (read-only) |

## Found on the way

**Pods of most Linux images stayed Terminating forever (ocijail, fixed: `aa3e111`, patch ocijail/0013).** This is
the real cause of the lingering pods in research/12.
1. ocijail resolves volume destinations within the container root, following symlinks, both when mounting and again
   when unmounting.
2. CRI-O unmounts the container's root file system when the container stops, *before* `ocijail delete`.
3. Nearly every Linux image has `/var/run → ../run` (Alpine) or `/run` (Debian, Ubuntu). The kubelet mounts the
   service-account token at `/var/run/secrets/…` and the termination log at `/var/run/…`.
4. The second resolution produced `ROOT/var/run/…`, while the mount was on `ROOT/run/…`. `unmount(2)` returned
   EINVAL, which ocijail treats as "already gone", and the mounts leaked.
5. The kubelet could then never unmount the pod's tmpfs, so the pod stayed Terminating.

Reproduced per image: node:22-alpine, temurin and nginx-alpine leaked 2 mounts each; busybox (no `/var/run`) didn't.
podman never hits it, because it deletes before unmounting storage. The old ocijail (before research/12's change)
leaks identically, so this isn't a regression from it. ocijail now records each mount's resolved path when mounting
and unmounts by it. After the fix: 0 mounts left for all four images, pods gone in 1 s. 12 stranded mounts on node 2
were cleaned by hand: only mounts under container roots that were no longer mounted.

~~**Node.js 22 doesn't run under the Linuxulator on arm64.**~~ **Fixed (research/16): three kernel bugs.** Node now
reports `constrainedMemory` 256 MiB and sizes V8 to 259 MiB in a 256 Mi pod. `node --version` dies with SIGBUS on both the old and the
new kernel, so it's unrelated to this work. That means Node's cgroup behaviour isn't verified. Open; next to
investigate.

## Limits

- **`cpu.weight` is always 100.** FreeBSD has no CPU shares, so the e2e resize tests that compare `cpu.weight` with
  requests still fail. Their `cpu.max`/`memory.max` checks now have real data. To be measured in the next
  conformance run.
- **`memory.stat` has only `anon`** (= resident), and there is no `memory.events` (OOM counters). Tools that read
  them get partial data.
- **CPU quota is rounded to whole percent** (pcpu granularity).
- **Only the Linuxulator sees this.** FreeBSD-native programs read limits through `rctl(8)`/`getrlimit` as before.
