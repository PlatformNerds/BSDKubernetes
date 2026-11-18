# 04 — Running Linux container images on FreeBSD: Linuxulator vs bhyve

Status: research note, 2026-11-13. Source baseline: `freebsd-src` shallow clone at `f429c4a6ee50` (2026-11-13), `sys/conf/newvers.sh` reports `REVISION="16.0" BRANCH="CURRENT"`, so this is **16-CURRENT (main)**. Released branches are 15.x (15.0 shipped 2025-12-02). Kata Containers clone is current `main`.

All paths are relative to `~/bsdk8s/upstream/<repo>/`.

---

## TL;DR (the honest version)

1. **The Linuxulator is good enough for "boring" stateless images, but it is not a container kernel.** It presents itself as Linux 5.15 (`sys/compat/linux/linux_mib.h:47-48`). It has epoll, eventfd, timerfd, inotify (via the native `vfs_inotify.c` added in 15.0), memfd, statx, close_range, membarrier, clone3, and netlink. It has **no namespaces, no seccomp, no cgroups view, no io_uring, no Linux AIO, no signalfd, no pidfd, no userfaultfd, no bpf, no keyctl, no landlock**. It also silently ignores `CLONE_NEW*` and `CLONE_PIDFD` flags. Every new glibc, JVM, .NET or Go release can surface a new syscall. You would be on the same ABI treadmill that illumos LX and WSL1 ran on, and both lost.
2. **bhyve today is a solid general-purpose hypervisor. It is not a microVM.** There is no virtio-vsock, no virtio-fs, no balloon, no CPU, memory or PCI hotplug, and no direct Linux kernel boot in mainline. Snapshot support is experimental, amd64-only and off by default. Kata's agent talks **only** over vsock (the hybrid-vsock flavour for Firecracker/CLH). The missing vsock device is therefore the single critical blocker for a Kata-style runtime.
3. **The gap is smaller than it looks.** Kata already has a "Firecracker profile" that needs none of the missing features except vsock: static sandbox sizing, no shared FS (block-device rootfs), and hybrid vsock over a Unix socket. bhyve needs about **1.5–2.5 kLOC of userspace device emulation** (virtio-vsock backed by Unix sockets, plus a Linux boot-protocol loader). The FreeBSD host needs **no** `AF_VSOCK` support for this.
4. **Recommendation: make bhyve ("kata-bhyve") the default for Linux images, and offer `linux-jail` as an opt-in, allow-listed fast path.** For an MVP that ships before kata-bhyve exists, use a **per-node long-lived Linux VM ("hybrid node")** for Linux pods. This is honest, works today, and is what WSL2, Docker Desktop and Colima concluded.

---

## 1. Linuxulator gap analysis (amd64, 16-CURRENT)

### How "unimplemented" looks in the tree

- `UNIMPL` entries in `sys/amd64/linux/syscalls.master` are legacy only: uselib, create_module, get_thread_area, epoll_ctl_old, vserver, tuxcall and so on. The range 335–423 is `UNIMPL nosys` (unused numbers on x86_64).
- The real gaps are **`STD` entries that resolve to `DUMMY()` stubs**. The macro in `sys/compat/linux/linux_util.h:49-61` logs `"syscall %s not implemented"` once per pid, fires an SDT probe `linuxulator:dummy:<name>:not_implemented`, and returns **ENOSYS**. The stubs live in:
  - `sys/compat/linux/linux_dummy.c` (machine-independent)
  - `sys/x86/linux/linux_dummy_x86.c` (contains `DUMMY(signalfd)`)
  - `sys/amd64/linux/linux_dummy_machdep.c` (`io_setup`/`io_submit`/`io_getevents`/`io_cancel`/`io_destroy`, `readahead`, `modify_ldt`, `restart_syscall`, …)

The SDT probe is useful. A runtime can watch `dtrace -n 'linuxulator:dummy::not_implemented'` for each pod and **detect that an image needs a missing syscall**, then reschedule it to the bhyve RuntimeClass. See §5.

### Container-relevant syscall status

| Syscall / feature | Status | Evidence | Container impact |
|---|---|---|---|
| `clone` with `CLONE_NEW*` | **Silently ignored.** Flags are not checked or mapped. | `linux_fork.c` `linux_clone()` → `linux_clone_proc()` maps only VM/SIGHAND/FILES/FS/VFORK/PARENT_SETTID | Dangerous. bwrap, Chrome, nested runc and buildah believe they got a namespace and did not. It should fail with EINVAL/EPERM. |
| `clone3` | Implemented. `CLONE_NEWTIME`, `CLONE_INTO_CGROUP` and `set_tid` return ENOSYS. Other `CLONE_NEW*` are accepted because `LINUX_CLONE_LEGACY_FLAGS = 0xffffffff`. | `linux_fork.c:376-420`, `linux_fork.h:61` | glibc ≥2.34 uses clone3 for pthreads: OK. |
| `CLONE_PIDFD` | Defined (`linux_fork.h:35`) and **ignored** | — | Low risk only because `pidfd_open` is ENOSYS, so Go, Python and systemd probe and fall back. |
| `unshare`, `setns` | **ENOSYS** (DUMMY) | `linux_dummy.c:77,93` | Only nested-container tooling (buildah/podman-in-pod, Kaniko, bwrap, Chromium sandbox, systemd `PrivateTmp=`). |
| `pivot_root` | ENOSYS | `linux_dummy.c:60` | Same as above. |
| `seccomp` | Stub. `GET_ACTION_AVAIL` → EOPNOTSUPP, everything else → EINVAL ("like a kernel without CONFIG_SECCOMP") | `linux_misc.c:2767` | Apps tolerate it. **Security:** Kubernetes `RuntimeDefault` seccomp cannot be enforced. PSS "restricted" expects it. |
| `prctl` | SET/GET_PDEATHSIG, DUMPABLE, KEEPCAPS, NAME, SECCOMP (stub), CAPBSET_READ, CHILD_SUBREAPER, NO_NEW_PRIVS, PTRACER, SET_VMA, THP_DISABLE; others log "unsupported prctl option" | `linux_misc.c` (case list) | Mostly fine. tini and dumb-init need SUBREAPER, which is present. |
| `io_uring_*` | **ENOSYS** | `linux_dummy.c:116-118` | Good fallbacks exist: libuv, Postgres 18 `io_method=worker`, Rust tokio-uring is opt-in. |
| Linux native AIO `io_setup/submit/getevents` | **ENOSYS** | `amd64/linux/linux_dummy_machdep.c:54-58` | MySQL/MariaDB InnoDB fall back with a warning. **ScyllaDB/Seastar fail.** Some Java NIO-heavy DBs are affected. |
| inotify | **Implemented**, a thin shim over native `sys/kern/vfs_inotify.c`. Event values are `_Static_assert`-equal to Linux. | `linux_file.c:1995-2060` | Good: config reloaders, Node watchers, nginx-ingress-style sidecars. |
| fanotify | ENOSYS | `linux_dummy.c:88-89` | Only security agents (Falco-like) and AV. |
| `memfd_create` | Implemented | `linux_file.c:1834` | Good (JIT runtimes, Wayland, systemd). `memfd_secret` is ENOSYS. |
| `userfaultfd` | ENOSYS | `linux_dummy.c:105` | CRIU and live-migration tooling only. |
| epoll | Implemented on kqueue (`linux_event.c`). **`EPOLLEXCLUSIVE` is not in `LINUX_EPOLL_EVSUP`, so epoll_ctl returns EINVAL** | `linux_event.h:47-49`, `linux_event.c:180-194` | **Risk for nginx with multiple workers.** nginx sets EPOLLEXCLUSIVE when `worker_processes>1` and `accept_mutex off` (the default since 1.11.3). Needs a test. The fix is trivial: accept and ignore the flag. |
| `epoll_pwait2` | Implemented | `linux_event.c:508` | — |
| `eventfd/eventfd2`, `timerfd_*` | Implemented | `linux_event.c:591,614` | Good (glib, systemd-less apps, Java NIO). |
| `signalfd`, `signalfd4` | **ENOSYS** | `x86/linux/linux_dummy_x86.c:50`, `linux_dummy.c:84` | systemd, some Rust/C daemons, s6? Most runtimes (libuv, Go, JVM) don't use it. |
| `statx` | Implemented | `linux_stats.c:751` | glibc ≥2.28 `stat` path and coreutils: OK. |
| `close_range` | Implemented | `linux_file.c:1890` | Python subprocess, systemd: OK. |
| `pidfd_open/send_signal/getfd` | **ENOSYS** | `linux_dummy.c:115,127,130` | Go 1.23+ os/exec and Python asyncio probe and fall back. systemd ≥253 is degraded. |
| `membarrier` | Implemented on main (maps to native `membarrier(2)`); **15.1 had only a stub**, backported in kernel `bsdk8s15` (research/10) | `linux_misc.c:3296` | Important for .NET and some JITs: OK. |
| `rseq` | ENOSYS (`linux_rseq.c:80`) | — | glibc ≥2.35 disables rseq on ENOSYS: OK. tcmalloc-rseq is slower. |
| `bpf`, `perf_event_open` | ENOSYS | `linux_dummy.c:86,101` | Observability agents (Cilium, Pixie, Parca, Datadog system-probe) fail completely. |
| `landlock_*` | ENOSYS | `linux_dummy.c:136-138` | Apps degrade gracefully. |
| `keyctl/add_key/request_key` | ENOSYS | `linux_dummy.c:72-74` | Kerberos KEYRING ccache, some systemd and ecryptfs paths. |
| `openat2`, `open_tree`, `move_mount`, `fsopen`, `fsmount`, `mount_setattr` | ENOSYS | `linux_dummy.c:120-134` | runc and containerd internals only, not app workloads. |
| `process_vm_readv/writev` | ENOSYS | `linux_dummy.c:95-96` | py-spy, rbspy, debuggers. |
| `splice` | "not really implemented" | `linux_file.c:1876-1879` | HAProxy splice mode and some proxies fall back. `vmsplice`/`tee` are ENOSYS. |
| `futex_waitv` | ENOSYS | `linux_dummy.c:145` | Wine/Proton only. |
| `fchmodat2` (6.6) | ENOSYS | `linux_dummy.c:150` | glibc 2.39+ falls back. |
| `mlock2`, `preadv2`/`pwritev2` | ENOSYS | `linux_dummy.c:107-110` | RocksDB and some databases use pwritev2 with RWF flags and fall back. |
| netlink | Native netlink plus `linux_netlink.c` | — | `ip`/iproute2 partial (2026 PR: `IFF_LOWER_UP` missing). |

### /proc and /sys (`sys/compat/linprocfs/linprocfs.c` 2718 LOC, `sys/compat/linsysfs/linsysfs.c` 648 LOC)

- **linprocfs nodes present:** cmdline, cpuinfo, meminfo, loadavg, stat, uptime, version, mounts, filesystems, modules, partitions, swaps. Per pid: cmdline, cwd, environ, exe, maps, mem, **mountinfo**, mounts, root, stat, statm, status, fd, auxv, limits, oom_score_adj, task. sys/kernel: osrelease, pid_max, random/uuid, boot_id, sysvipc limits. sys/fs/inotify/*, mqueue, vm/min_free_kbytes, max_map_count, and net/dev, net/route.
- **linsysfs:** PCI devices, `class/net`, `class/drm`, `devices/system/cpu/{online,possible,present}`, `kernel/debug`.
- **Missing and important: cgroups.** There is no `/proc/self/cgroup`, no cgroup2 mount in mountinfo, and no `/sys/fs/cgroup/{memory.max,cpu.max,…}` (`grep cgroup` in both files returns nothing). Consequences:
  - **JVM** (`-XX:+UseContainerSupport`) and **.NET** (`GCHeapHardLimit` auto) size heaps from **host** RAM and CPU count. On a 256 GB node, a pod limited by RCTL to 1 GB is OOM-killed or swaps, or the JVM refuses to start. Workaround: explicit `-Xmx`/`-XX:MaxRAM`, `DOTNET_GCHeapHardLimit`.
  - **Go 1.25+** (cgroup-aware GOMAXPROCS) and Node `os.availableParallelism` fall back to `sched_getaffinity`, which the Linuxulator implements (`linux_misc.c:2114`) on top of cpuset. **If the kubelet pins the jail with cpuset(1), CPU counts are right. Memory is not.**
- Also missing: `/proc/pressure/*` (PSI), `/proc/sys/net/*` sysctls (ingress controllers and Envoy write some), and `/proc/self/ns/*`.

### Arm64 Linuxulator

`sys/arm64/linux/` exists (273 `linux_` entries in its `syscalls.master`, a single machdep DUMMY `kexec_file_load`). Arm64 inherits the MI stubs above, so it has the same gaps. It is far less exercised than amd64: fewer users, no Linux Steam or Chrome pressure. Treat it as "works for static binaries, unproven for glibc-heavy images".

---

## 2. Practical compatibility: what actually runs

Evidence is anecdotal. There is **no** public compatibility matrix. That is a gap this project should fill (see §5, "compat CI").

| Image class | Linuxulator expectation | Evidence / reasoning |
|---|---|---|
| **Go static binaries** (most CNCF tools, Caddy, Traefik, etcd) | **Works well** | Simple syscall surface. Go probes pidfd and falls back. Caveat: the cgroup memory limit is invisible (GOMEMLIMIT unset). |
| **nginx** | Mostly works (guides show it) | [oneuptime podman guide](https://oneuptime.com/blog/post/2026-03-18-use-podman-freebsd/view), [tumfatig](https://tumfatig.net/2026/exploring-docker-containers-on-freebsd). **Verify the EPOLLEXCLUSIVE path with >1 worker.** |
| **redis** | Works | Same guides (redis:7). |
| **postgres** | Works **only if** the jail has `allow.sysvipc` | Fails with "could not create shared memory segment: Function not implemented" without it ([forum](https://forums.freebsd.org/goto/post?id=725545), [daemonless/postgres](https://github.com/daemonless/postgres)). The kubelet must set `sysvipc=new` per pod jail. |
| **Node.js** apps | Works (uptime-kuma reported) | libuv doesn't need io_uring. Heap sizing ignores cgroup memory limits. |
| **Python** (glibc slim and alpine) | Generally works | asyncio pidfd falls back. musl `stat` uses fstatat: OK. |
| **JVM** (Temurin, Spring Boot) | Starts, but **container ergonomics are wrong** (no cgroup) | Historical SIGSEGVs: [Bug 267121](https://lists.freebsd.org/archives/freebsd-java/2024-September/002877.html). Futex lost-wakeup reports in 2026: Bug 296441. |
| **.NET** | Unknown and risky | No reports found. Needs membarrier (present), cgroup (absent), and `/proc/self/mountinfo` (present). |
| **Alpine/musl** | Works for basics | musl uses fewer new syscalls than glibc. One DNS quirk was reported (in bhyve, not the Linuxulator). |
| **s6-overlay images** (linuxserver.io etc.) | **Broken** | [forum report](https://forums.freebsd.org/goto/post?id=724981). This is a large share of Docker Hub "app" images. |
| **systemd-in-container, Docker-in-Docker, buildah/kaniko, Chrome headless (sandboxed)** | **Broken** | Needs namespaces, cgroups and signalfd. |
| **eBPF/observability DaemonSets** (Cilium, Falco, node-exporter's Linux collectors) | **Broken / meaningless** | bpf and perf ENOSYS. These are also host-OS specific, so they would never apply anyway. |
| ScyllaDB, anything with hard io_uring/AIO dependency | Broken | ENOSYS. |

Community consensus (forum thread ["Linux docker & docker compose in 2025"](https://forums.freebsd.org/threads/linux-docker-docker-compose-in-2025.99100), EuroBSDCon summary relayed there): Linux images "can break at any point in time"; "for production-critical Linux container workloads, use Linux." Podman's FreeBSD port (`podman/pkg/specgen/generate/oci_freebsd.go:70-110`) handles Linux images by remapping `/proc`→linprocfs, `/sys`→linsysfs, `/dev`→devfs ruleset 4 + fdescfs + tmpfs shm. That is the whole "Linux container" story on FreeBSD today. runj only filters incompatible Linux mounts (`runj/containerd/bundle.go:13-80`). ocijail has no Linux-specific code.

---

## 3. bhyve as a microVM runtime

### Device model (`usr.sbin/bhyve/`, 16-CURRENT)

| Capability | Present? | Evidence |
|---|---|---|
| virtio-blk, virtio-net, virtio-scsi, virtio-console (multiport), virtio-rnd, virtio-input | Yes | `pci_virtio_{block,net,scsi,console,rnd,input}.c` |
| virtio-9p (p9fs) | Yes. lib9p, a single `vsc_mtx` around request handling | `pci_virtio_9p.c` (348 LOC), `contrib/lib9p`. FreeBSD guests get a `p9fs` client in `sys/fs/p9fs` + `sys/dev/virtio/p9fs`. |
| NVMe, AHCI, e1000, xHCI, HDA, fbuf/VNC, TPM (swtpm/passthru), PCI passthrough | Yes | — |
| **virtio-fs** | **No** | No device. Guest-side work only: D46296 and a 2026 kernel virtiofs RFC ([list](https://lists.freebsd.org/archives/freebsd-virtualization/2026-May/003649.html)). |
| **virtio-vsock** | **No** (device and host `AF_VSOCK` both absent) | No `grep vsock` hits. `sys/dev/virtio/` has no socket driver. Out-of-tree guest driver WIP; status report lists "VirtIO Socket device for bhyve" as future work ([2024Q3 report](https://www.freebsd.org/status/report-2024-07-2024-09/vsock/)). |
| **memory balloon** | **No device** (only the FreeBSD *guest* driver `sys/dev/virtio/balloon`) | — |
| CPU hotplug | **No** | `qemu_fwcfg.c:258` "We don't support cpu hotplug yet" |
| memory hot-add, PCI hotplug | **No** | — |
| Direct Linux kernel boot (`-kernel/-initrd/-append`) | **No in mainline.** Prototype by Rob Norris (2024) implementing the x86 Linux boot protocol ([list](https://lists.freebsd.org/archives/freebsd-virtualization/2024-May/002112.html), [branch](https://github.com/robn/freebsd-src/tree/bhyve-loader-linux/usr.sbin/bhyve)). Today Linux boots via EDK2 UEFI (`-l bootrom,BHYVE_UEFI.fd`) or `sysutils/grub2-bhyve`. `bhyveload` loads FreeBSD only. | `bhyve.8` examples, `usr.sbin/bhyveload/` |
| fw_cfg + ACPI table loader | Yes | `qemu_fwcfg.c`, `qemu_loader.c`, `basl.c`. A hook for passing kernel/initrd to EDK2 is plausible but **unverified**. |
| Snapshot / restore | **Experimental**: `BHYVE_SNAPSHOT` is in `__DEFAULT_NO_OPTIONS` and `BROKEN` on non-amd64 (`share/mk/src.opts.mk:202,385`) | Device support: blk, net, rnd, ahci, e1000, xhci, fbuf. **Not** 9p, console or scsi. No live migration in mainline. |
| bhyve in jails | Yes: `allow.vmm` (`sys/kern/kern_jail.c:4834`), `allow.vmm_ppt`; non-root `vmm` group (RELNOTES af099eaa5ec) | A "jailer"-equivalent sandbox is available natively. Capsicum is applied in bhyve already. |
| arm64 / riscv | bhyve and vmm on arm64 and riscv landed for 15.0 ([Foundation](https://freebsdfoundation.org/blog/freebsd-15-why-youll-want-it/)). arm64 boots via u-boot `-o bootrom=u-boot.bin` (`bhyve.8:1301`). | No UEFI and no snapshot on arm64. Needs EL2/VHE hardware. |
| Nested virt | No (VMX/SVM not exposed to guests) | No Kata-in-a-cloud-VM unless the cloud is bare-metal. **Big practical limit: most k8s nodes run in cloud VMs.** |

### Boot time and footprint (honest estimates; benchmark before trusting)

| | Firecracker | Cloud Hypervisor | bhyve + EDK2 UEFI (today) | bhyve + direct boot (proposed) |
|---|---|---|---|---|
| VMM to guest init | ~125 ms (vendor claim) | ~100–200 ms | **~1–3 s** (firmware init, PCI enumeration, GRUB/EFI stub) | ~150–300 ms (est.) |
| VMM RSS overhead | <5 MiB | ~10–20 MiB | ~10–30 MiB (est.; heavy device model) | ~10 MiB with minimal profile |
| Device model | virtio-mmio, ~5 devices | virtio-pci, hotplug | full PCI, LPC, ACPI | PCI + ACPI, no LPC/UART |

I found no published bhyve boot benchmarks. Colin Percival's work shows FreeBSD *guests* booting in about 20–25 ms under Firecracker, and he estimates Linux at 75–80 ms in the same environment ([status report](https://www.freebsd.org/status/report-2022-07-2022-09/firecracker/), [The Register](https://www.theregister.com/2023/08/29/freebsd_boots_in_25ms/)). The guest kernel is not the bottleneck. **Firmware is.** bhyve's floor is set by EDK2, so the direct-boot loader is the highest-leverage bhyve change after vsock.

### What Kata needs from a hypervisor

Go runtime interface: `kata-containers/src/runtime/virtcontainers/hypervisor.go:1349-1393`:
`CreateVM, StartVM, StopVM, PauseVM, SaveVM, ResumeVM, AddDevice, HotplugAddDevice, HotplugRemoveDevice, ResolveColdPlugVFIOGuestPciPaths, ResizeMemory, ResizeVCPUs, GetTotalMemoryMB, GetVMConsole, Disconnect, Capabilities, HypervisorConfig, GetThreadIDs, Cleanup, setConfig, GetPids, GetVirtioFsPid, fromGrpc, toGrpc, Check, Save, Load, GenerateSocket, IsRateLimiterBuiltin`.

Rust runtime-rs trait: `src/runtime-rs/crates/hypervisor/src/lib.rs:139-184`. It adds `wait_vm, update_device, get_agent_socket, get_ns_path, get_jailer_root, get_passfd_listener_addr, set_guest_memory_block_size`. Existing backends: qemu, ch, firecracker, dragonball, openvmm, remote.

**Mapping a bhyve backend:**

| Kata need | bhyve today | How to satisfy |
|---|---|---|
| Agent channel | **Kata agent listens on vsock only** (`src/agent/src/config.rs:72` `vsock://-1`). The runtime supports `HybridVsock` (Firecracker/CLH: Unix socket + `CONNECT <port>\n`), `kata_agent.go:420-473`. | **Implement `virtio-vsock` in bhyve with a Unix-socket backend using exactly Firecracker's hybrid-vsock protocol.** No host `AF_VSOCK` needed. Stopgap: boot param `agent.server_addr=unix:///run/agent.sock` (`config.rs:21`) plus a tiny in-guest bridge from a virtio-console port to that socket. |
| Hotplug (block, net, VFIO) | None | Cold-plug everything. Kata's Firecracker profile already does this (`static_sandbox_resource_mgmt`, `configuration-fc.toml.in:350`). |
| ResizeMemory/VCPUs | None | `static_sandbox_resource_mgmt = true`: size the VM from the pod spec at creation. In-place pod resize stays unsupported, which is the same as Firecracker. |
| Shared FS | 9p only (slow, single-mutex) | `shared_fs = "none"` (`katautils/config.go:652`), as with FC. Rootfs is a **ZFS zvol clone per container exposed as virtio-blk**, or guest-pull (image pulled inside the guest, CoCo style). ConfigMaps and Secrets get copied by the agent (Kata already does this for no-shared-fs). |
| Pause/Resume | `bhyvectl --suspend`? Partial | vCPU suspend via `vm_suspend` / checkpoint socket. Low priority. |
| Console | virtio-console/uart → socket | OK |
| Kernel + initrd boot | UEFI only | Direct-boot loader, or EDK2 + EFI stub as a slower fallback. |

**The Kata host side cannot be reused as-is.** `containerd-shim-kata-v2` is Linux-only (netns, tc-mirroring, cgroups, devmapper). The realistic plan reuses the **guest side** (Kata guest kernel config, rootfs with `kata-agent`, the ttrpc protocol in `src/libs/protocols`) and writes a **new FreeBSD shim** (Go, containerd shim v2), or adds a `bhyve` backend to runtime-rs behind a FreeBSD build tag.

---

## 4. Out-of-the-box options and precedent

### illumos LX branded zones / SmartOS / Triton: the closest precedent to `linux-jail`

- LX was removed from Solaris in 2010. Joyent resurrected it in SmartOS in March 2014 ([LX dev docs](https://docs.smartos.org/lx-dev/)). Triton's `sdc-docker` ran Docker images directly in LX zones on bare metal ([Cantrill, 2015](https://bcantrill.dtrace.org/2015/03/24/triton-docker-and-the-best-of-all-worlds/)).
- **What worked:** strong isolation (zones), bare-metal performance, DTrace across the boundary, and an `sdt:lx_brand::brand-lx-unsupported` probe to find gaps. That is the same idea as our Linuxulator SDT probe.
- **What hurt:** a permanent treadmill. The docs say "as changes are made in the Linux kernel and new distributions use them, illumos may not yet support those features; periodically we need to add new LX facilities". The brand has no iptables, exclusive-IP only, and no Linux kernel modules ([lx(7)](https://smartos.org/man/7/lx)). The real killer was **commercial**, not technical. Kubernetes won over Docker Swarm-style APIs. Joyent was sold to Samsung in 2016 and the public cloud shut down in 2019. Triton's Kubernetes story ran k8s **inside VMs**. Tellingly, illumos/SmartOS later **ported bhyve itself** (2018) for full-Linux needs. LX zones survive as a niche (OmniOS, MNX Triton).
- **WSL1 → WSL2 is the other data point.** Microsoft had a syscall translation layer with far more resources than FreeBSD and **abandoned it for a real Linux kernel in a lightweight VM**, citing full syscall compatibility and filesystem performance.
- **Lesson: an ABI-translation layer is a great "80% fast path" and a terrible "100% default". Every serious vendor converged on a VM for the long tail.**

### gVisor-style userspace kernel on FreeBSD

gVisor's Sentry implements the Linux ABI in Go, but its platforms (ptrace, systrap, KVM) and the gofer are all **Linux-host** specific. A FreeBSD port would need a new platform on top of `libvmmapi`/`vmm` (analogous to the KVM platform: Sentry runs in guest ring 0) and a rewrite of the host-side file proxy. Estimate: **≥12 engineer-months**, permanently out-of-tree, and it buys a *second* incomplete Linux ABI. **Not recommended.** The Linuxulator already *is* FreeBSD's "gVisor without the sandbox".

### "Hybrid node": one long-lived Linux VM per FreeBSD node (WSL2 / Docker Desktop / Colima model)

A FreeBSD kubelet with a CRI multiplexer: FreeBSD-image pods go to jails (runj/ocijail), and Linux-image pods are forwarded to **containerd/runc inside one bhyve Linux VM** over virtio-console or TCP.

- **Pros:** 100% Linux compatibility **today** with zero bhyve changes (UEFI boot time is irrelevant because it boots once). Runc seccomp and cgroups work. Bit-for-bit the same behaviour as a Linux node.
- **Cons:**
  - **Networking:** pod IPs live in the VM. The FreeBSD CNI must route a pod-CIDR slice to the VM's tap, which is a doable static route.
  - **Volumes:** hostPath and CSI must cross into the VM. Use 9p (slow) or NFS from the host (fine). Block CSI can map zvols as virtio-blk, but that needs hotplug, which is missing. Pre-provisioned NFS/iSCSI from inside the VM works.
  - **Resources:** the VM size is static with no balloon. The kubelet must report allocatable as host minus VM and split capacity.
  - **Isolation:** Linux pods share one kernel, which is the same as a normal Linux node.
- **Verdict:** this is the **MVP path**. It is the honest way to say "Linux images run on FreeBSD nodes" in v0.1.

### KubeVirt-style VMs via bhyve

Running full VMs as pods with bhyve is very natural and arguably easier than Kata: no agent, UEFI boot is fine, and `allow.vmm` jails exist. libvirt has a bhyve driver ([drvbhyve](https://www.libvirt.org/drvbhyve.html)). It is a good follow-on product but orthogonal to "run Linux container images".

### Other ideas

- **Make the problem smaller:** encourage multi-OS OCI images with `os=freebsd`. containerd and podman already do platform selection, and daemonless.io publishes FreeBSD-native images. Every image that has a FreeBSD variant needs neither Linuxulator nor bhyve.
- **Compatibility oracle:** run each new image digest once in `linux-jail` under CI with the `linuxulator:dummy::not_implemented` DTrace probe plus `linux_msg` log scraping. Publish a signed per-digest verdict, and have the admission webhook pick the RuntimeClass automatically.
- **Read-only cgroup v2 facade** in linsysfs/linprocfs, fed from RCTL. A small piece of work that makes JVM, .NET and Go limits correct (see §5).

---

## 5. Concrete proposals

### 5a. Linuxulator improvements, ranked by container impact

| # | Change | Files | LOC est. | Impact |
|---|---|---|---|---|
| 1 | **Reject `CLONE_NEW*`** in `clone`/`clone3` (EPERM/EINVAL) instead of silently ignoring; same for `CLONE_PIDFD` until pidfd exists | `sys/compat/linux/linux_fork.c` | ~30 | Correctness and security; sandboxing tools fall back properly |
| 2 | **Accept and ignore `EPOLLEXCLUSIVE`** (thundering-herd hint only) | `linux_event.h`, `linux_event.c` | ~10 | nginx and other multi-worker servers |
| 3 | **cgroup v2 read-only facade**: `/proc/self/cgroup` → `0::/`, a cgroup2 line in mountinfo, `/sys/fs/cgroup/{memory.max,memory.current,cpu.max,cpu.weight,pids.max,cgroup.controllers}` derived from the jail's RCTL rules and cpuset | `sys/compat/linprocfs/linprocfs.c`, `sys/compat/linsysfs/linsysfs.c`, RCTL query helpers in `sys/kern/kern_rctl.c` | ~800–1,200 | **Highest app-level impact**: JVM, .NET, Go (GOMEMLIMIT tools), Node and Python worker-count heuristics |
| 4 | **signalfd/signalfd4** on top of kqueue `EVFILT_SIGNAL` + sigprocmask | new `linux_signalfd.c` | ~500 | systemd-style daemons, some Rust/C servers |
| 5 | **pidfd_open / pidfd_send_signal / CLONE_PIDFD** on top of procdesc (`sys/kern/sys_procdesc.c` needs a "procdesc for an existing pid" helper) | `linux_fork.c`, new `linux_pidfd.c`, procdesc | ~600–900 | Go 1.23+, Python 3.9+, systemd ≥253 (they fall back, so this is medium) |
| 6 | **seccomp filter mode** (`SECCOMP_SET_MODE_FILTER`, cBPF via the existing `net/bpf_filter.c` interpreter at Linux syscall entry, actions ERRNO/KILL/ALLOW/LOG) | `linux_misc.c`, the sysvec syscall entry `sys/amd64/linux/linux_sysvec.c`, new `linux_seccomp.c` | ~1,500–2,500 | Lets kubelet enforce `RuntimeDefault`. Chrome and Firefox sandboxes. Big security win |
| 7 | **Linux native AIO** on top of FreeBSD `aio(4)` | `amd64/linux/linux_dummy_machdep.c` → new `linux_aio.c` | ~1,000 | MySQL/MariaDB perf, ScyllaDB |
| 8 | `/proc/pressure/*`, `/proc/sys/net/*` (read and some write via sysctl mapping) | linprocfs | ~400 | Envoy/ingress tuning, autoscalers |
| 9 | `process_vm_readv/writev` | new | ~300 | Profilers |
| — | **Do not do:** namespaces (`unshare`/`setns` mapping to jails), io_uring, bpf, userfaultfd. Each is massive, and VMs cover them. | | | |

Items 1–3 (about 1–1.3 kLOC) move `linux-jail` from "toy" to "usable for allow-listed stateless apps". Items 4–6 (about 3–4 kLOC) make it defensible security-wise.

### 5b. "kata-bhyve" design

**bhyve changes (in order):**

1. **`pci_virtio_vsock.c`** (~1,200–1,800 LOC). virtio-vsock device with a **Unix-socket backend speaking Firecracker's hybrid-vsock protocol**: host connects to `<uds>` and writes `CONNECT <port>\n`; guest-initiated connections go to `<uds>_<port>`. Stream mode first, SEQPACKET later. This alone unblocks Kata's agent. It also benefits LXD, libvirt `qemu-guest-agent`-style tooling and the 2024 vsock WIP author.
2. **Linux direct-boot loader** (~600–1,000 LOC, building on the robn prototype). `-o boot.kernel=… -o boot.initrd=… -o boot.cmdline=…`: x86 boot protocol, e820 from `amd64/e820.c`, ACPI from `basl.c`. On arm64, the u-boot path already exists (Image + DTB). Target is **<300 ms to agent ready**.
3. **Minimal device profile** (`-o profile=micro`; ~200 LOC of config defaults): hostbridge, virtio-blk ×N, virtio-net ×N, virtio-vsock, virtio-rnd, virtio-console. No LPC, no UART, no fbuf. Guest memory backed by `MAP_ANON` with optional preallocate/wired (`-S`).
4. **virtio-fs is optional and later.** Avoid shared FS by design: rootfs via **ZFS zvol clone → virtio-blk**, containing an ext4/erofs image built once per image digest. Alternatively the agent pulls the image in the guest. Improving 9p (multithreaded lib9p workers instead of the global `vsc_mtx`) is a cheaper stepping stone for ConfigMap/volume sharing than virtio-fs plus a FreeBSD virtiofsd. A real virtio-fs device needs a host FUSE-protocol server (port `virtiofsd` (Rust, Linux-centric) or write one on lib9p's backend), ~3–5 kLOC.
5. **Balloon device** (~500 LOC) for over-commit reclaim, later. Hotplug is not required; follow Kata's FC model (static sizing).

**Host-side runtime:** new containerd shim v2 `containerd-shim-bhyve-v1` (Go, ~5–8 kLOC):

- One VNET jail per pod with `allow.vmm`. bhyve runs *inside* it (Firecracker-jailer equivalent), and its tap and the CNI epair are bridged inside the jail.
- The shim speaks the Kata agent ttrpc API (`kata-containers/src/libs/protocols`) over the hybrid-vsock UDS.
- It reuses the Kata guest kernel and agent rootfs unchanged (Linux guest).
- Pod memory and CPU come from the sum of container limits plus overhead (`static_sandbox_resource_mgmt` semantics).

Alternative: a `bhyve` backend in Kata runtime-rs plus a FreeBSD port of its sandbox/network layer. That is upstream-friendlier but a much larger Linux-ism purge.

### 5c. RuntimeClasses

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: jail }          # FreeBSD-native images (os=freebsd)
handler: jail                      # runj/ocijail
---
metadata: { name: linux-jail }    # Linuxulator; opt-in, allow-listed images only
handler: linux-jail
scheduling: { nodeSelector: { bsdk8s.io/linuxulator: "true" } }
overhead: { podFixed: { memory: "0" } }
---
metadata: { name: bhyve }          # Linux images in per-pod bhyve microVM (default for os=linux)
handler: bhyve
overhead: { podFixed: { memory: "64Mi", cpu: "50m" } }
---
metadata: { name: linux-vm }       # MVP: shared per-node Linux VM (hybrid node)
handler: linux-vm
```

A mutating admission webhook picks the default: `os=freebsd` → `jail`; `os=linux` with an allow-listed digest → `linux-jail`; otherwise → `bhyve` (or `linux-vm` until kata-bhyve ships).

---

## 6. Blocker table

| Blocker | Severity | Solution | Effort |
|---|---|---|---|
| No virtio-vsock in bhyve; Kata agent is vsock-only | **Critical** (kata-bhyve) | `pci_virtio_vsock.c` with hybrid-vsock UDS backend; stopgap virtio-console bridge + `agent.server_addr=unix://` | 1.2–1.8 kLOC, ~6–10 wks |
| No direct Linux boot (EDK2 adds ~1–2 s) | High | Upstream the robn loader | 0.6–1 kLOC, ~4–6 wks |
| Kata host shim is Linux-only | High | New FreeBSD containerd shim reusing the Kata guest and protocol | 5–8 kLOC, ~3–4 months |
| No nested virt: bhyve microVMs don't run on cloud VMs | **High (deployment)** | Bare-metal nodes only; on cloud VMs use `linux-jail` or a remote/peer-pods model | Not fixable by us |
| No hotplug, balloon or resize | Medium | Static sandbox sizing (Kata FC model); balloon later | 0.5 kLOC (balloon) |
| No virtio-fs; 9p slow | Medium | Block-device rootfs (zvol clones) + agent copy for configmaps; multithread lib9p | 1–2 kLOC (9p), 3–5 kLOC (virtio-fs) |
| Snapshot experimental, amd64-only | Low | Not needed for v1; later for fast-start templates | — |
| Linuxulator: no cgroup view → wrong JVM/.NET sizing | High (linux-jail) | cgroup v2 facade from RCTL | ~1 kLOC |
| Linuxulator: `CLONE_NEW*` silently ignored | High (security/correctness) | Return error | ~30 LOC |
| Linuxulator: EPOLLEXCLUSIVE → EINVAL | Medium (nginx, needs test) | Accept and ignore | ~10 LOC |
| Linuxulator: no seccomp | High (security posture) | cBPF seccomp filter mode | 1.5–2.5 kLOC |
| Linuxulator: no namespaces, io_uring, bpf, AIO, signalfd, pidfd | Medium–Low | signalfd/pidfd/AIO yes; the rest → bhyve | 2–2.5 kLOC |
| s6-overlay / systemd images broken | Medium (common on Docker Hub) | Route to bhyve | — |
| Postgres needs SysV IPC | Low | kubelet sets `sysvipc=new` per pod jail | trivial |
| ABI treadmill (new glibc/runtimes need new syscalls) | **Permanent** | Compat-oracle CI + auto-fallback to bhyve | ongoing |
| Arm64: Linuxulator lightly tested; bhyve arm64 new, u-boot only | Medium | Treat arm64 as phase 2 | — |

---

## Verdict

- **Default for `os=linux` images: bhyve.** Ship it in two steps:
  - **v0 (now): `linux-vm`**, a hybrid node with one long-lived Linux VM per node running containerd/runc. It needs zero bhyve changes, gives 100% compatibility, and is honest about isolation.
  - **v1: `bhyve` (kata-bhyve)**, per-pod microVMs. This needs vsock, direct boot and a FreeBSD shim, about 4–6 engineer-months total. It is bare-metal only because there is no nested virt.
- **`linux-jail` (Linuxulator) is an opt-in fast path, not the default.** It is great density (no guest kernel, shared page cache, ZFS clones) for an **allow-list** of verified images: Go static binaries, nginx, redis, simple Python/Node. It only becomes defensible after the ~1.3 kLOC fixes (CLONE_NEW* rejection, EPOLLEXCLUSIVE, cgroup facade), and ideally seccomp. Making it the default would repeat Triton's and WSL1's mistake. Users would see random breakage, blame the platform, and leave.
- **Wording "use bhyve when possible" correctly:** use bhyve whenever the image is Linux and the node is bare metal. Use the Linuxulator only when an image is proven to work there **and** density or startup matter, or when the node is itself a cloud VM with no nested virt. That last case is uncomfortable: many real clusters run on cloud VMs, which turns the Linuxulator from "optional" into "the only option" there. Plan the compat-oracle tooling accordingly.

## Sources

- FreeBSD source (local clone): `sys/amd64/linux/syscalls.master`, `sys/compat/linux/{linux_dummy.c,linux_fork.c,linux_fork.h,linux_event.c,linux_event.h,linux_file.c,linux_misc.c,linux_rseq.c,linux_mib.h,linux_util.h}`, `sys/x86/linux/linux_dummy_x86.c`, `sys/amd64/linux/linux_dummy_machdep.c`, `sys/compat/linprocfs/linprocfs.c`, `sys/compat/linsysfs/linsysfs.c`, `usr.sbin/bhyve/*`, `share/mk/src.opts.mk`, `sys/kern/kern_jail.c`, `RELNOTES`
- Kata (local clone): `src/runtime/virtcontainers/hypervisor.go`, `src/runtime-rs/crates/hypervisor/src/lib.rs`, `src/runtime/virtcontainers/kata_agent.go`, `src/agent/src/config.rs`, `src/runtime/pkg/katautils/config.go`, `src/runtime/config/configuration-fc.toml.in`
- podman `pkg/specgen/generate/oci_freebsd.go`; runj `containerd/bundle.go`
- [FreeBSD vsock status report 2024Q3](https://www.freebsd.org/status/report-2024-07-2024-09/vsock/)
- [Direct Linux loading for bhyve (R. Norris, 2024)](https://lists.freebsd.org/archives/freebsd-virtualization/2024-May/002112.html)
- [Kernel virtiofs driver RFC, 2026](https://lists.freebsd.org/archives/freebsd-virtualization/2026-May/003649.html), [D46296](https://reviews.freebsd.org/D46296?id=156286)
- [FreeBSD 15: Why you'll want it (bhyve arm64/riscv)](https://freebsdfoundation.org/blog/freebsd-15-why-youll-want-it/)
- [FreeBSD on Firecracker status report](https://www.freebsd.org/status/report-2022-07-2022-09/firecracker/), [The Register: FreeBSD boots in 25 ms](https://www.theregister.com/2023/08/29/freebsd_boots_in_25ms/)
- [Linux docker & compose in 2025 (forum)](https://forums.freebsd.org/threads/linux-docker-docker-compose-in-2025.99100), [OCI containers on FreeBSD thread](https://forums.freebsd.org/threads/an-introduction-to-oci-containers-on-freebsd.99907/post-731625), [postgres sysvipc report](https://forums.freebsd.org/goto/post?id=725545), [daemonless/postgres](https://github.com/daemonless/postgres), [s6-overlay breakage](https://forums.freebsd.org/goto/post?id=724981)
- [Podman on FreeBSD guide](https://oneuptime.com/blog/post/2026-03-18-use-podman-freebsd/view), [tumfatig: exploring Docker containers on FreeBSD](https://tumfatig.net/2026/exploring-docker-containers-on-freebsd), [hofstede: Podman on FreeBSD](https://blog.hofstede.it/podman-on-freebsd-oci-containers-without-systemd/)
- [Linuxulator Java crash Bug 267121](https://lists.freebsd.org/archives/freebsd-java/2024-September/002877.html), [freebsd-emulation 2026 archives](https://lists.freebsd.org/archives/freebsd-emulation/2026-May/)
- [SmartOS LX dev](https://docs.smartos.org/lx-dev/), [lx(7)](https://smartos.org/man/7/lx), [Cantrill: Triton, Docker and the best of all worlds](https://bcantrill.dtrace.org/2015/03/24/triton-docker-and-the-best-of-all-worlds/)
- [libvirt bhyve driver](https://www.libvirt.org/drvbhyve.html)
