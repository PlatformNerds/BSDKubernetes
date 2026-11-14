# 05 — Storage, images, security model, control plane and ecosystem

Status: research draft, 2026-11-13. Sources are the shallow clones under `~/bsdk8s/upstream/` (pins in
`UPSTREAM-PINS.md`): freebsd-src @ `f429c4a6e` (16.0-CURRENT), kubernetes @ `b4226aca` (~v1.37 dev),
containerd @ `40ea313`, containers-storage @ `83cf574` (frozen Aug 2025, moved to containers/container-libs),
podman, k3s @ `c55a7df`, runtime-spec @ `f6cd17e`, ocijail, runj. Paths are relative to those clones.
**[verify]** marks a claim from static reading that should be checked on a running system.
**[built]** marks a claim checked by cross-compiling on the research host (`GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0`, Go 1.27.3).

---

## 0. Executive summary (the honest version)

1. **Storage for images is the easy part.** containerd on FreeBSD already ships the **ZFS snapshotter** as a builtin
   (`containerd/cmd/containerd/builtins/builtins_freebsd.go` imports `github.com/containerd/zfs/v2/plugin`), and its
   **native** snapshotter uses `nullfs` (`plugins/snapshots/native/native_freebsd.go`). containers/storage has a
   working FreeBSD ZFS driver (`drivers/zfs/zfs.go` is `//go:build linux || freebsd`; `drivers/driver_freebsd.go`
   sets priority `zfs, vfs`). Forget overlayfs and forget unionfs for layers: ZFS clones are better than overlayfs
   on the axes Kubernetes cares about (O(1) container rootfs, real per-container quotas, snapshots).
2. **Kubelet's volume layer is not the easy part.** On GOOS=freebsd, `k8s.io/mount-utils` compiles to
   `mount_unsupported.go`, which returns `errUnsupported` for *every* mount. Because secret, configMap and projected
   volumes are wrapped in an `emptyDir{medium: Memory}` (tmpfs mount), **even a service-account token mount fails
   today**. `subpath`, `hostutil`, `SetVolumeOwnership` (fsGroup), `emptyDir` medium detection and `cadvisor` are all
   stubs too. All of these are small, well-scoped ports, and the Windows variants are good templates.
3. **The security model maps decently but not 1:1.** Jails cover host namespaces, privileged and most "capabilities"
   coarsely (per-jail `allow.*`), and readOnlyRootFilesystem is trivial (ZFS `readonly=on` or nullfs `ro`). There is
   **no seccomp, no user namespaces, and no per-process capability drop** for jail root. Two kernel modules would
   close most of the gap: **`mac_privfilter`**, which turns Linux capability sets into FreeBSD `priv(9)` deny masks
   through the existing `mpo_priv_check` MAC hook, and **`mac_syscallfilter`/Linuxulator seccomp**, which needs a
   *new* hook in `sys/kern/subr_syscall.c`.
4. **The API and kubelet assume only two OSes.** `pod.spec.os.name` is validated against `{linux, windows}`
   (`pkg/apis/core/validation/validation.go:127`). Kubelet **rejects** any pod whose `spec.os.name != runtime.GOOS`
   (`pkg/kubelet/lifecycle/predicate.go`, with a TODO about LCOW). It also forces the node label
   `kubernetes.io/os=freebsd`, and nearly every Helm chart pins `nodeSelector: kubernetes.io/os: linux`. Running
   Linux images on FreeBSD nodes "with the ecosystem unchanged" is therefore **exactly the LCOW problem Kubernetes
   never solved for Windows**. That is the biggest *ecosystem* blocker, and it is political, not technical.
5. **The control plane already runs on FreeBSD.** The ports tree has `net/kube-apiserver`, `net/kube-controller-manager`
   and `net/kube-scheduler` at **1.37.2** (updated 2026-11-07), plus `devel/etcd`, `dns/coredns` 1.15.0,
   `sysutils/kubectl`, `sysutils/helm` 3.21, `sysutils/node_exporter` and `sysutils/fluent-bit` 5.2. **[built]** All of
   kubelet, kube-apiserver, controller-manager, scheduler, kube-proxy and kubectl cross-compile for FreeBSD from
   upstream HEAD after a **3-line stub** for `possibleCPUs()` (`pkg/securitycontext/util.go:217`); the port carries
   the same patch. Compiling is not working, though: kubelet *starts* and then dies on the stub container manager.
6. **Recommendation:** Phase 1 should be **Linux control plane + FreeBSD workers**, modelled exactly on the Windows-node
   playbook (taints, `kubernetes.io/os=freebsd`, RuntimeClass, a csi-proxy-style host helper). Phase 2 is a FreeBSD
   control plane, which is cheap because it already works. A k3s-style single binary ("kbsd") is attractive, but it
   should be a **new thin distribution**, not a k3s port, because k3s pulls in rootlesskit, opencontainers/cgroups,
   CNI netns and others.

---

## 1. Image layers and snapshotters

### 1.1 Options compared

| Option | Exists on FreeBSD? | Evidence | Verdict |
|---|---|---|---|
| overlayfs | No | not in `freebsd-src/sys/fs/` (has `nullfs unionfs tmpfs tarfs p9fs fuse ...`) | n/a |
| **containerd ZFS snapshotter** | **Yes, builtin** | `containerd/cmd/containerd/builtins/builtins_freebsd.go`; vendored `containerd/vendor/github.com/containerd/zfs/v2/zfs.go` is `//go:build linux \|\| freebsd` | **Default choice** |
| containers/storage `zfs` driver | Yes | `containers-storage/drivers/zfs/zfs.go` (`linux \|\| freebsd`), `zfs_freebsd.go` (unmount uses `MNT_FORCE` since FreeBSD has no `MNT_DETACH`), `drivers/register/register_zfs.go` | Works (Podman default on FreeBSD); only relevant if CRI-O were the CRI |
| containerd `native` snapshotter | Yes | `plugins/snapshots/native/native_freebsd.go`: `mountType = "nullfs"` | Fallback for non-ZFS (UFS) nodes. Full copy per layer, slow and space-hungry |
| containers/storage `vfs` | Yes | `driver_freebsd.go` priority `zfs, vfs` | Same as native: tests only |
| unionfs (per-layer stacking) | Exists, still risky | see 1.2 | **Do not use for layers** |
| tarfs (read-only tar mount) | Yes, jail-mountable | `sys/fs/tarfs/tarfs_vfsops.c:1239` `VFCF_JAIL \| VFCF_READONLY`; zstd only (`tarfs_io.c` `ZSTDIO`) | Interesting for lazy, zstd OCI layers (see 1.4) |
| nullfs | Yes, jail-mountable, file mounts | `sys/fs/nullfs/null_vfsops.c:509` `VFCF_LOOPBACK \| VFCF_JAIL \| VFCF_FILEMOUNT` | Use for **all bind mounts** (volumes, `/etc/hosts`, `resolv.conf`, termination-log, subPath) |

How containerd mounts on FreeBSD: `core/mount/mount_freebsd.go` **shells out to `mount(8)`**, with a retry loop for
`ECHILD`. The comment says "golang.org/x/sys/unix do not define a Mount function for FreeBSD". That costs a fork+exec
per mount, which is fine for MVP and worth replacing with `nmount(2)` later.

### 1.2 unionfs after the 2021–2025 rework: still no for layers

- Jason Harmening's locking rework landed in Feb 2024, with further fixes into late 2025 (`VOP_GETLOWVNODE`, Dec 2025),
  per the [FreeBSD Foundation](https://freebsdfoundation.org/?p=16842) and the
  [cgit log](https://cgit.freebsd.org/src/log/sys/fs/unionfs/union_vfsops.c).
- **But** the man page in today's 16-CURRENT tree still opens BUGS with *"THIS FILE SYSTEM TYPE IS NOT YET FULLY
  SUPPORTED (READ: IT DOESN'T WORK) AND USING IT MAY, IN FACT, DESTROY DATA"*
  (`freebsd-src/sbin/mount_unionfs/mount_unionfs.8`, `.Dd October 3, 2016`). Forum posters who say the warning was
  removed are wrong for this tree.
- **Structural blocker:** deleting or renaming a lower-layer file needs `VOP_WHITEOUT` on the upper fs. Only UFS
  (`sys/ufs/ufs/ufs_vnops.c`) and tmpfs (`sys/fs/tmpfs/tmpfs_vnops.c`) implement it. **ZFS does not** (no
  `vop_whiteout` under `sys/contrib/openzfs/module/os/freebsd`). So a unionfs container rootfs cannot have a ZFS upper
  layer. The man page also documents that ACL/MAC extattrs are not copied up, and that `find(1)` creates shadow
  directories.
- An N-layer image would need N stacked unionfs mounts, and lookup cost grows with depth.
- The legitimate niche is a **tmpfs upper over a ZFS read-only image snapshot**, for ephemeral, throwaway rootfs.
  Even there, ZFS clones are just as cheap.

### 1.3 ZFS snapshotter mechanics and performance

- Model: each layer is a dataset, and once committed it becomes a snapshot. A child layer is a `zfs clone` of the
  parent snapshot, and the diff (tar, with OCI whiteout files turned into deletions by the walking differ) is applied
  into the clone. A container rootfs is one more clone, which costs **O(1) regardless of image size**.
- Pull and unpack cost is dominated by gzip decompression and by writing each layer's delta once, the same as overlayfs.
  There is **no published FreeBSD benchmark**. The FreeBSD Foundation's Podman testing only says ZFS was "the most
  efficient and reliable option" versus vfs
  ([Foundation blog, Nov 2024](https://freebsdfoundation.org/blog/advancing-cloud-native-containers-on-freebsd-podman-testing-highlights/)).
  Treat performance as **unmeasured** and benchmark it in Phase 0.
- Known costs [verify]:
  - `containerd/zfs` uses `mistifyio/go-zfs` (`containerd/go.mod:124`), which **execs `zfs(8)` for every operation**:
    tens of ms per call, several calls per layer.
  - There is one dataset per layer plus one per container. Thousands of datasets are fine for ZFS, but `zfs list` and
    boot-time mount scans slow down. Use `canmount=noauto` / `mountpoint=legacy`, as containers/storage does
    (`zfs.go:235,316`).
  - ARC memory competes with pod memory. Kubelet's allocatable must reserve ARC (`vfs.zfs.arc_max`) or eviction
    accounting will be wrong.
- **What ZFS gives you that Linux can't do cheaply:**
  - Per-container `refquota` gives *hard* ephemeral-storage limits (Linux kubelet polls `du`).
  - `readonly=on` gives readOnlyRootFilesystem.
  - `zfs snapshot` makes container checkpoints and image warm-up cheap.
  - `zfs send | recv` can be a fast image distribution path between nodes. This is a "spegel over ZFS" idea:
    replicate unpacked layer snapshots and skip decompression entirely.

### 1.4 Outside-the-box idea: lazy layers with tarfs

FreeBSD has `tarfs` (read-only, jail-mountable, supports zstd frames with seek). OCI allows `+zstd` layers. A
"tarfs snapshotter" could mount each zstd layer blob directly from the content store with no unpack, then stack
them. Stacking needs a union, though, and unionfs is the weak link, so this only works for **single-layer
(squashed) images** or a future, solid unionfs. Park it as Phase 3 research. Do not put it on the critical path.

### 1.5 Image platform: who publishes `os: freebsd`?

- **Official FreeBSD base images exist** from **14.2** onward on Docker Hub (`docker.io/freebsd/freebsd-{static,dynamic,runtime,notoolchain,toolchain}`)
  and GHCR (`ghcr.io/freebsd/...`), for amd64 and aarch64
  ([Handbook ch. 18](https://docs.freebsd.org/en/books/handbook/containers/),
  [Foundation](https://freebsdfoundation.org/freebsd-container-images/)).
- FreeBSD became an officially supported platform in **OCI runtime-spec v1.3 (Nov 2025)**
  ([Foundation](https://freebsdfoundation.org/?p=28383)). See `runtime-spec/config-freebsd.md` and
  `specs-go/config.go:1011-1082` (`FreeBSD{Devices, Jail{Parent, Host, Ip4, Vnet, SysV*, EnforceStatfs, Allow}}`).
- **Almost no application images carry an `os: freebsd` manifest.** Brutal truth: "the ecosystem works unchanged"
  means **Linuxulator for >99% of images**. FreeBSD-native images will be home-built (buildah) for a long time.
- **How containerd picks the platform:** `containerd/vendor/github.com/containerd/platforms/defaults_freebsd.go`
  returns `Ordered(freebsd/GOARCH, linux/GOARCH)`. A FreeBSD containerd **already prefers freebsd and silently falls
  back to linux**, which is convenient but dangerous: you can unknowingly run the Linux variant of a multi-arch image.
  Override per RuntimeClass with CRI `runtime_platforms` (`internal/cri/config/config.go:308-312`,
  `RuntimePlatforms map[string]ImagePlatform{Platform, Snapshotter}`). Map handler `linux` to `linux/amd64` and
  handler `jail` to `freebsd/amd64` so selection is explicit.
- Podman already does this: `podman run --os=linux docker.io/alpine` after `service linux onestart` (Handbook).
- **Pause image:** `registry.k8s.io/pause` has no freebsd variant. A jail with `persist` needs **no pause process at
  all**, so the CRI shim should implement a sandbox without a pause container. containerd's sandbox API (`podsandbox`
  vs. shim-based sandboxers) allows this. Otherwise, publish a 2-line static FreeBSD pause.

---

## 2. Volumes

### 2.1 What kubelet does on GOOS=freebsd today (all stubs)

| Component | File | FreeBSD behaviour | Consequence |
|---|---|---|---|
| mount-utils | `staging/src/k8s.io/mount-utils/mount_unsupported.go` (`!linux && !windows`) | every Mount/Unmount/List returns `errUnsupported` | **All volume plugins that mount fail** |
| emptyDir medium | `pkg/volume/emptydir/empty_dir_unsupported.go` | `GetMountMedium` always returns default; memory resize errors | `medium: Memory` triggers a tmpfs mount, which fails |
| secret/configMap/projected | `pkg/volume/secret/secret.go:54-56`, `projected/projected.go:61-65` wrap `EmptyDir{Medium: Memory}` | tmpfs mount through mount-utils | **SA token projection fails**, so essentially every pod fails |
| atomic writer | `pkg/volume/util/atomic_writer.go` | portable POSIX (symlink + `rename` of `..data`), but `atomic_writer_unsupported.go` makes `lchown` a no-op with a warning | Works once tmpfs works; per-file owner (projected `fsGroup`) ignored |
| fsGroup | `pkg/volume/volume_unsupported.go` (`SetVolumeOwnership` is Linux-only) | no-op | fsGroup silently ignored, so non-root pods can't write volumes |
| subPath | `pkg/volume/util/subpath/subpath_unsupported.go` | `errUnsupported` | subPath mounts fail (common in charts) |
| hostutil | `pkg/volume/util/hostutil/hostutil_unsupported.go` | stubs | hostPath `type:` checks fail |
| cadvisor | `pkg/kubelet/cadvisor/cadvisor_unsupported.go` | `MachineInfo`, `RootFsInfo`, `ImagesFsInfo` return errors | kubelet can't report capacity; eviction is blind |
| container manager | `pkg/kubelet/cm/container_manager_unsupported.go` | `Start()` returns "Container Manager is unsupported in this build" | **kubelet exits at startup**; no device manager |
| kuberuntime | `pkg/kubelet/kuberuntime/kuberuntime_container_unsupported.go` | `applyPlatformSpecificContainerConfig` returns nil; `generateContainerResources` returns nil | **No securityContext and no resources reach CRI** (see §3) |

Atomic writer details: it relies on `os.Symlink`, `os.Rename` (atomic on FreeBSD for symlink replacement) and
relative symlinks `<key> -> ..data/<key>`. Nothing in it is Linux-specific beyond `lchown`. Apps that watch
`..data` with inotify should work: FreeBSD now has **native inotify** (`sys/kern/vfs_inotify.c`), and Linuxulator
maps `inotify_init1` (`sys/compat/linux/linux_file.c`). Whether events propagate through the **nullfs** bind into
the jail is **[verify]**, and it matters for config-reloading apps.

### 2.2 Porting work (all small, all upstreamable in principle)

- `mount_freebsd.go` for mount-utils: `nmount(2)` (or `mount(8)` like containerd), `List()` via `getfsstat(2)`,
  `IsLikelyNotMountPoint` via `st_dev` compare, `MNT_FORCE` instead of `MNT_DETACH`. About 400 LOC plus tests.
- emptyDir:
  - `medium: Memory` maps to `tmpfs -o size=` (sizeLimit is honoured natively by tmpfs).
  - **disk-backed emptyDir maps to a ZFS child dataset with `refquota=sizeLimit`.** That is hard enforcement and
    better than Linux's `du` polling.
- subPath: on FreeBSD the safe path is `openat(O_RESOLVE_BENEATH)`. FreeBSD has had `O_RESOLVE_BENEATH` since 13.0
  [verify flag name in golang.org/x/sys]. Follow it with a nullfs **file** mount (`VFCF_FILEMOUNT`). The Linux
  `/proc/self/fd` bind trick is unnecessary.
- `SetVolumeOwnership`: plain `chown`/`chmod` walk. It is portable, so just widen the build tag to `linux || freebsd`.

### 2.3 Volume types

| Type | FreeBSD mapping | Notes |
|---|---|---|
| emptyDir (disk) | ZFS dataset under pod dataset, `refquota` | per-pod dataset `zroot/k8s/pods/<uid>`, destroyed on pod delete |
| emptyDir (Memory) | tmpfs (`VFCF_JAIL`, `sys/fs/tmpfs/tmpfs_vfsops.c:726`) | counts against pod memory? **No**: tmpfs pages aren't charged to the jail's rctl `memoryuse` the way Linux memcg charges shmem [verify]; cap with `size=` |
| hostPath | nullfs into the jail | `type:` checks need hostutil port; Linux paths like `/var/log/pods`, `/sys`, `/proc` are meaningless or different |
| configMap/secret/projected/downwardAPI | tmpfs + atomic writer, then nullfs into jail | works after mount-utils port |
| image volume (KEP-4639) | snapshotter view (ZFS clone, `readonly=on`) then nullfs | free win with ZFS |
| PVC via CSI | see 2.4 | hard part |
| local PV | ZFS dataset or zvol | **this is the killer app: a native ZFS local-PV provisioner with CSI snapshots and clones** |
| nfs / iscsi in-tree | in-tree plugins are deprecated; migrate to CSI | |

### 2.4 CSI on FreeBSD: the mount-propagation problem and the fix

**The problem.** A CSI node plugin runs as a privileged DaemonSet with `/var/lib/kubelet` mounted
`mountPropagation: Bidirectional`. It performs `NodeStageVolume`/`NodePublishVolume` mounts *inside its container*,
and those mounts must appear on the host so kubelet can hand them to the workload pod. FreeBSD has **no mount
namespaces and no shared/slave subtrees**. CRI has `PROPAGATION_PRIVATE/HOST_TO_CONTAINER/BIDIRECTIONAL`
(`cri-api/.../api.proto:352-359`), and nothing implements them on FreeBSD.

**The subtle truth:** FreeBSD has *one global mount table*, so a jail sees host mounts below its root directly.
Propagation breaks because the runtime **nullfs-mounts** `/var/lib/kubelet` into the jail, and nullfs does not expose
mounts stacked on the lower tree, nor push the jail's mounts back to the host path [verify exact nullfs behaviour].
In addition, jails can only mount `VFCF_JAIL` filesystems: devfs, tmpfs, fdescfs, nullfs, procfs, linprocfs,
linsysfs, tarfs, p9fs, fuse and zfs (`grep VFCF_JAIL sys/`). **NFS, UFS and cd9660 are not in that list**, so an NFS
or iSCSI+UFS CSI node plugin cannot mount from inside a jail at all.

**Options:**
1. **`csi-proxy` model (recommended).** This is precedent from Windows, where the CSI node DaemonSet talks over a
   socket to a host daemon that performs privileged storage ops
   ([kubernetes-csi/csi-proxy](https://github.com/kubernetes-csi/csi-proxy),
   [CSI Windows docs](https://kubernetes-csi.github.io/docs/csi-windows.html)). Build a `csi-proxy-freebsd` (Go, rc.d
   service) that exposes gRPC for `mount/nmount`, `iscsictl`, `zfs`, `newfs`/`growfs`, `mdconfig` and `geli`. Node
   plugins need a small FreeBSD backend. Upstream drivers would need that backend contributed, as they did for Windows.
2. **Host-mounted "Bidirectional" emulation in the runtime.** For a container requesting Bidirectional on
   `/var/lib/kubelet`, *don't* use nullfs. Instead create the jail with `path=/` (or chroot to a tree where
   `/var/lib/kubelet` is the real host directory) and `enforce_statfs=0`, `allow.mount.*`. Mounts then land in the
   global namespace at the right path. This is effectively a "privileged host-process" container, equivalent to
   Windows HostProcess. It is simple, it works for ZFS, tmpfs, nullfs and fuse, and it still fails for NFS and UFS
   because of VFCF_JAIL. A **non-jailed host-process container** (no jail, just chroot/exec on the host) handles
   those too. That is the honest answer for `privileged: true` + Bidirectional.
3. Kernel work: mark NFS client jail-safe (`VFCF_JAIL` on nfs). This has been discussed for years and is not trivial
   (credentials and the kernel RPC client in vnet jails).

**Protocols:**
- **iSCSI initiator** is `iscsictl(8)` + `iscsid(8)` (kernel `iscsi(4)`), and the target side is `ctld(8)`. There is no
  open-iscsi, so every Linux CSI driver's iSCSI code (which shells out to `iscsiadm`) needs a FreeBSD backend.
- **NFS** works via host `mount_nfs`.
- **ZFS** is native. **NVMe-oF** has an initiator and target in FreeBSD 15 (`nvmecontrol`, `nvmft`) [verify release].

**democratic-csi:**
- It treats FreeBSD/TrueNAS as a **storage server** (the `freenas-*` and `zfs-generic-*` drivers SSH into the box and
  run `zfs` + `ctladm`/targetcli). Its **node plugin is Linux-only** (it calls `iscsiadm`, `mount`, `blkid`, and the
  Linux `/sys` and `/dev/disk/by-path`) ([repo](https://github.com/democratic-csi/democratic-csi)). It has no FreeBSD
  node support, and none was found in issues or docs.
- It is Node.js, which runs on FreeBSD, so adding a FreeBSD node backend (iscsictl, `geom`, `/dev/da*`) is plausible
  but is a fork's worth of work.
- **Better MVP:** write a tiny Go **`zfs-localpv-freebsd`** CSI driver. The controller creates datasets/zvols, and the
  node does `zfs set mountpoint` or nullfs bind. It supports snapshots, clones and expansion, uses only ZFS (which is
  jail-mountable via `zfs jail` + `allow.mount.zfs`, the way dch mounts datasets into Podman jails
  ([dch 2025](https://people.freebsd.org/~dch/posts/2025-06-27-oci-zfs/))), and needs no propagation tricks.

### 2.5 Device plugins and devfs

- The device-plugin gRPC API is OS-agnostic, but the **device manager lives in the container manager**, which is the
  unsupported stub. Port `container_manager_freebsd.go` modelled on `container_manager_windows.go` (device manager,
  no cgroups), plus rctl from the resource-isolation report.
- `DeviceSpec{container_path, host_path, permissions}` maps directly onto `runtime-spec` `FreeBSD.Devices[{path, mode}]`
  (`specs-go/config.go:1019-1024`). The runtime turns these into a **per-jail devfs ruleset** (`devfs rule -s N add
  path X unhide mode M`). Podman already does exactly this (`podman/pkg/specgen/generate/config_freebsd.go:97-125`,
  "Find the devfs mount so that we can add rules to expose the device"). ocijail passes `devfs_ruleset`
  (`ocijail/ocijail/jail.cpp:17`).
- GPUs:
  - The NVIDIA FreeBSD driver exposes `/dev/nvidia*`. Native CUDA does not exist.
  - **CUDA under Linuxulator** partly works via community shims [verify per workload].
  - AMD/Intel via drm-kmod with `/dev/dri/*`, with no ROCm.
  - Honest verdict: there is no GPU story for ML workloads. Don't promise it.

---

## 3. Security model

### 3.1 The plumbing gap first

Even with perfect jail mappings, **kubelet on FreeBSD sends nothing**:

- `kuberuntime_container_unsupported.go` returns nil for `applyPlatformSpecificContainerConfig`, so
  `LinuxContainerConfig.SecurityContext` (runAsUser, capabilities, readonlyRootfs, privileged, seccomp,
  supplementalGroups, maskedPaths) is never populated.
- `kuberuntime_sandbox_unsupported.go` returns nil for sandbox resources.
- containerd's CRI on FreeBSD is equally empty: `internal/cri/server/container_create_other.go` returns
  `[]oci.SpecOpts{}`, so even the image's `USER` and the env-derived options from the Linux path are skipped.
- CRI has only `Linux*` and `Windows*` messages (`api.proto:529-1325`).

**Pragmatic decision:** on FreeBSD, kubelet should build the *Linux* messages. That means widening
`kuberuntime_container_linux.go` and `kuberuntime_sandbox_linux.go` to `linux || freebsd` minus the cgroup bits, and
containerd should interpret `LinuxContainerSecurityContext` as "POSIX security context". Adding `FreeBSDContainerConfig`
to CRI is cleaner, but it is a KEP and API-review fight. Do the hack first and propose the KEP with working code.

### 3.2 PSS field mapping

Legend: **OK** = faithful; **≈** = coarser but acceptable; **✗** = no equivalent (reject or ignore with an event).

| Pod field (PSS level) | FreeBSD mapping | Fidelity |
|---|---|---|
| `privileged` (baseline forbids) | jail with `allow.*` all, `enforce_statfs=0`, devfs ruleset 0 (unhidden), `securelevel=-1`; for Bidirectional/storage use a **non-jailed host-process** container | ≈ |
| `hostNetwork` (baseline) | non-vnet jail with `ip4=inherit ip6=inherit` (spec `FreeBSDJail.Ip4/Ip6/Vnet`) | OK |
| `hostPID` (baseline) | **no clean mapping.** A jail only sees its own processes, and only the host (jid 0) sees all. Requires running un-jailed. Treat it as privileged-only | ✗/≈ |
| `hostIPC` (baseline) | `sysvmsg/sysvsem/sysvshm=inherit` (spec `SysVMsg/SysVSem/SysVShm`); POSIX shm is per-jail namespaced since 13 [verify] | OK |
| `hostPorts` (baseline) | port-forward in pf (see networking report) | OK |
| `hostPath` (baseline) | nullfs | OK |
| capabilities add (baseline allows NET_BIND_SERVICE etc.) | coarse per-jail `allow.*` (list below); fine-grained via the proposed `mac_privfilter` | ≈ |
| capabilities `drop: [ALL]` (restricted) | jail root still holds every in-jail privilege. For **non-root** (required by restricted) this is moot, since an unprivileged uid has no privs. For root it needs `mac_privfilter` | ≈ |
| `allowPrivilegeEscalation: false` (restricted) | `procctl(PROC_NO_NEW_PRIVS_CTL)` (`sys/sys/procctl.h:64`), which disables setuid/setgid on exec. Linuxulator already maps `PR_SET_NO_NEW_PRIVS` to it (`sys/compat/linux/linux_misc.c:1957`) | **OK** |
| `runAsUser`/`runAsGroup`/`runAsNonRoot` (restricted) | `setuid`/`setgid` before exec (ocijail/runj already honour `process.user`) | OK |
| `supplementalGroups` (+ `supplementalGroupsPolicy`) | `setgroups` | OK |
| `fsGroup` | chown/chmod walk (port `SetVolumeOwnership`) | OK after port |
| `readOnlyRootFilesystem` | ZFS clone `readonly=on`, or nullfs `ro` remount of the rootfs | OK |
| `procMount: Unmasked` (baseline forbids) | FreeBSD procfs inside a jail shows only jail processes. Linuxulator `linprocfs`/`linsysfs` expose host-wide info (cpu, meminfo, some `/sys`). There is no masking equivalent; implement as "don't mount linsysfs" or mount an empty tmpfs over sensitive paths | ≈ |
| `sysctls` (safe set) | many `net.*` are per-vnet. Map the k8s "safe" set (`net.ipv4.ip_local_port_range`, `ip_unprivileged_port_start`, `tcp_syncookies`, `ping_group_range`, ...) to FreeBSD equivalents (`net.inet.ip.portrange.*`, `net.inet.ip.portrange.reservedhigh` per vnet, ...). Reject the rest | ≈ |
| `seccompProfile` (restricted requires RuntimeDefault/Localhost) | **no seccomp.** See 3.4. MVP: accept `RuntimeDefault` and map it to the jail default (no MAC filter), and **reject `Localhost`** | ✗ (MVP) |
| `appArmorProfile` / `seLinuxOptions` | reject non-empty, except `Unconfined` / `RuntimeDefault`, which become no-ops. Future: map SELinux-type labels onto `mac_mls`/`mac_biba` labels? Not worth it | ✗ |
| `hostUsers: false` (user namespaces) | **none.** Reject. See 3.5 | ✗ |
| `windowsOptions` | reject | n/a |

Jail `allow.*` knobs available today (`freebsd-src/sys/kern/kern_jail.c:5290-5334`): `set_hostname`, `sysvipc`,
`raw_sockets` (≈ CAP_NET_RAW), `chflags` (≈ CAP_LINUX_IMMUTABLE), `quotas`, `socket_af`, `mlock` (≈ CAP_IPC_LOCK),
`reserved_ports` (≈ CAP_NET_BIND_SERVICE; `PRIV_NETINET_RESERVEDPORT` at `kern_jail.c` ~L4747), `read_msgbuf`
(≈ CAP_SYSLOG), `unprivileged_proc_debug`, `unprivileged_parent_tampering`, `suser`, `nfsd`, `extattr`, `adjtime`,
`settime` (≈ CAP_SYS_TIME), `routing` (≈ part of CAP_NET_ADMIN), `setaudit` (≈ CAP_AUDIT_WRITE), and
`allow.mount.{devfs,fdescfs,nullfs,procfs,tmpfs,zfs,linprocfs,linsysfs,...}` (≈ CAP_SYS_ADMIN for mount). ocijail
exposes all of these as `org.freebsd.jail.allow.*` annotations (`ocijail/ocijail/create.cpp:270-292`), and the OCI
spec has `FreeBSDJailAllow` (`specs-go/config.go:1071-1081`). Note that ocijail defaults to `enforce_statfs=1` and
`allow.raw_sockets` **on** (`create.cpp:312-313`). The Kubernetes default must turn `raw_sockets` **off** (containerd
on Linux does drop NET_RAW by default these days).

Also note the default Docker/containerd cap set is about 14 caps. Jail root without `allow.*` is roughly that set
*minus* NET_RAW and *plus* CHOWN/FOWNER/SETUID/KILL (all allowed in-jail). That is a reasonable default.

### 3.3 Proposed kernel module 1: `mac_privfilter` (Linux caps → FreeBSD privs)

- FreeBSD has about 248 fine-grained privileges (`grep -c '^#define	PRIV_' sys/sys/priv.h`), and the MAC framework
  already has **`mpo_priv_check`** (`sys/security/mac/mac_policy.h:454,929`), which can *deny* any privilege.
- Design:
  - Store a per-jail (via OSD, as `mac_do` does with `osd_jail_slot`, `sys/security/mac_do/mac_do.c:80`) or per-cred
    deny bitmap.
  - The runtime translates the CRI capability set into a priv mask, using a static table such as CAP_CHOWN →
    `PRIV_VFS_CHOWN`, CAP_KILL → `PRIV_SIGNAL_*`, CAP_SETUID → `PRIV_CRED_SETUID...`.
  - Set it as a jail parameter (`mac.privfilter.deny=...`).
- Size: about 600 LOC, low risk, and it makes `capabilities.drop` real for root. **Do it if anything kernel-side
  gets done.**

### 3.4 Proposed kernel module 2: syscall filtering ("seccomp")

- There is **no syscall-entry MAC hook.** The only `syscall` in `mac_policy.h` is `mpo_syscall` (the policy's own
  `mac_syscall(2)` multiplexer).
- The natural insertion point is `sys/kern/subr_syscall.c` around L125-135, where Capsicum already rejects
  non-`SYF_CAPENABLED` syscalls. Add a per-prison (or per-proc) bitmap check, `if (pr->pr_sysfilter && !bit_test(...))
  return ENOSYS/EPERM`. That is an O(1) bit test, cheaper than seccomp-BPF.
- **Outside the box:**
  - For **Linuxulator processes** the sysent table is `linux_sysent`, so syscall numbers are *Linux* numbers. Docker's
    and containerd's default seccomp JSON (`names: [...]`, `action: SCMP_ACT_ERRNO`) can be compiled into the
    Linux-ABI bitmap almost verbatim. Argument filters (`clone` flags, `personality`) need more work.
  - The kernel already has a cBPF interpreter (`sys/net/bpf_filter.c`), so a **real `seccomp(SECCOMP_SET_MODE_FILTER)`
    in Linuxulator** is conceivable. Today `linux_seccomp` returns `EINVAL`/`EOPNOTSUPP`
    (`sys/compat/linux/linux_misc.c:2767-2780`), which means Linux apps that sandbox themselves (Chrome, systemd units,
    Firefox, some Go programs via libseccomp) already see "kernel without CONFIG_SECCOMP" and fall back.
  - For native FreeBSD processes, a name-based profile is translated through `syscalls.master`.
- Capsicum is **not** a seccomp substitute for unmodified binaries. `cap_enter()` before exec breaks anything that
  opens paths. It stays an in-app hardening tool.
- MVP: skip. `RuntimeDefault` maps to "jail + no_new_privs + MAC defaults". Document honestly that **restricted PSS on
  FreeBSD is weaker on syscall attack surface than on Linux**. This is the one place a FreeBSD node is objectively less
  hardened. Mitigation: the bhyve microVM RuntimeClass for untrusted pods.

### 3.5 User namespaces, MAC, securelevel

- **No user namespaces.** UID 0 in a jail is host UID 0 on files. Container breakout lands you as root on the host,
  although jail escapes are historically rare and jail root is heavily restricted by `prison_priv_check`
  (`kern_jail.c:4407`, about 144 `case PRIV_` entries). Files written to hostPath/PV by in-pod root are owned by host
  root. `hostUsers: false` must be **rejected**, not silently ignored, because users rely on it for security. Possible
  mitigations:
  - Force a per-pod UID range by policy (an admission webhook that rewrites `runAsUser` into `[100000+N*65536, ...)`):
    poor-man's userns, which breaks images that need root.
  - `mac_bsdextended` (`ugidfw`) rules can match on jail id to restrict what in-jail uid 0 can touch.
- **MAC modules present** (`sys/security/`): `mac_bsdextended` (file-system firewall by uid/gid/jail), `mac_portacl`
  (per-uid port binding; useful for non-root port<1024, though per-vnet `portrange.reservedhigh=0` is simpler),
  `mac_do` (per-jail rules for uid transitions without setuid binaries; useful for "sudo-less" images), `mac_ipacl`
  (per-jail IP address control, which helps non-vnet hostNetwork-ish jails), `mac_seeotheruids`, `mac_priority`,
  `mac_ntpd`, `mac_veriexec` (signed-exec allowlist), `mac_mls`/`mac_biba`/`mac_lomac` (labels), `mac_partition`,
  `mac_grantbylabel`. None is a seccomp or capability system out of the box.
- **securelevel:** per-jail `securelevel` (≥1 makes `schg` files immutable and blocks kmem/raw-disk writes). Set
  `securelevel=3`-ish for non-privileged pods [verify interaction with Linuxulator `mount`/`chflags`].
- **Service account token projection** works unchanged once tmpfs and the atomic writer work. It is pure kubelet plus
  API, nothing OS-specific.
- **Linuxulator surface:** linprocfs/linsysfs/lindebugfs are jail-mountable (`VFCF_JAIL` in
  `sys/compat/lin*/`). **Never mount `lindebugfs` in pods.** Per-jail `linux.osrelease` is settable
  (`sys/compat/linux/linux_mib.c:396-401`), which helps images that version-check the kernel.

---

## 4. Control plane

### 4.1 Component status

| Component | FreeBSD status | Evidence |
|---|---|---|
| etcd | port `devel/etcd` (since Dec 2023); etcd docs list FreeBSD as a platform | [freshports](https://www.freshports.org/devel/etcd/), [etcd docs](https://etcd.io/docs/v3.3/platforms/freebsd/) |
| kube-apiserver | port `net/kube-apiserver` **1.37.2_1** (2026-11-11), packages for 14/15 amd64+aarch64 | [freshports](https://www.freshports.org/net/kube-apiserver/), adds FreeBSD `possibleCPUs()` stub |
| kube-controller-manager / kube-scheduler | ports `net/kube-controller-manager`, `net/kube-scheduler`, updated with apiserver | [freshports](https://www.freshports.org/net/kube-controller-manager), [freshports](https://freshports.org/net/kube-scheduler/) |
| kubectl | `sysutils/kubectl` 1.36.1 | [freshports](https://www.freshports.org/sysutils/kubectl/) |
| helm | `sysutils/helm` 3.21.0 | [freshports](https://www.freshports.org/sysutils/helm) |
| CoreDNS | `dns/coredns` 1.15.0 (binary; *in-cluster* it runs as a Linux image) | [freshports](https://www.freshports.org/dns/coredns/) |
| metrics-server | no port found; pure Go, should build | depends on kubelet `/metrics/resource` (see §5) |
| kubelet | **no port**; compiles, but won't run (stubs, §2.1) | **[built]** |
| kube-proxy | compiles **[built]**; only the iptables/ipvs/nftables backends are Linux, so a pf backend is needed (see networking report) | |

**[built]** Cross-compile of upstream HEAD with `GOOS=freebsd CGO_ENABLED=0`:
- kube-scheduler, kube-proxy and kubectl build cleanly.
- kubelet, kube-apiserver and kube-controller-manager fail only on `pkg/securitycontext/util.go:217: undefined:
  possibleCPUs`. There are `util_{linux,darwin,windows}.go` but no freebsd file. With a 3-line `util_freebsd.go` stub
  (via `go build -overlay`), **all three build**.
- containerd `./cmd/containerd` builds with `-mod=vendor`.
- Log: `/private/tmp/claude-501/-Users-jamengual-orca-projects-local/5687a0f0-273a-4dfa-aa72-3d75e3c7ce88/scratchpad/k8s-freebsd-build.txt`.

**Existing efforts:**
- Tony Norlin ran a FreeBSD (and illumos) control plane in jails with **Linux bhyve workers** + Cilium
  ([write-up](https://kubernaut-eu.pages.dev/posts/kubernetes-on-freebsd-with-linux-worker-nodes-and-cilium),
  [Mandala 1.29](https://medium.com/@norlin.t/welcome-mandala-kubernetes-v1-29-on-freebsd-control-plane-7732fa7a1271)).
  His key lesson is that **admission webhooks fail because the apiserver can't reach ClusterIPs/pod IPs**. Fixes are
  BGP-advertising pod and service CIDRs to the control-plane host, or running `konnectivity` (apiserver-network-proxy,
  pure Go). The same problem exists for any control plane outside the pod network (GKE/EKS solve it the same way).
- The 2024 FreeBSD DevSummit mentioned kubeadm/kube-proxy porting efforts and CRI work
  ([forum](https://forums.freebsd.org/threads/2024-freebsd-developer-summit-oci-containers-on-freebsd.93967)).
  **No upstream Kubernetes FreeBSD support exists**, and none was found in k/k issues.
- "Planternetes" was a one-off binary build of 1.28 for illumos, FreeBSD and OpenBSD
  ([forum](https://forums.freebsd.org/threads/kubernetes-planternetes-on-freebsd.90030/latest)).

### 4.2 k3s: what's Linux-specific

**[built]** `GOOS=freebsd go build .` of k3s HEAD fails as follows:
- `pkg/cgroups` and `pkg/proctitle` have no FreeBSD files.
- After stubbing those (copying the Windows variants) it next fails on:
  - `pkg/cli/cmds` (`DefaultSnapshotter` undefined)
  - `pkg/agent/templates` (`templateFuncs`, containerd config templates)
  - `pkg/agent/syssetup` (`machine.GetTopology`, from cadvisor)
  - `pkg/agent/cri` (`socketPrefix`)
  - `github.com/containernetworking/plugins/pkg/ns` (Linux netns only)
  - `rootless-containers/rootlesskit/v3` (`unix.Mount`, `MS_BIND`, `PR_SET_SECCOMP`)
  - `opencontainers/cgroups` (`Openat2`, `RESOLVE_BENEATH`)
  - the kubelet `possibleCPUs` again (k3s's fork `k3s-io/kubernetes v1.37.2-k3s1`)

Linux-only pieces by design:
- `pkg/agent/flannel/setup_linux.go` (vxlan/host-gw via netlink)
- `pkg/agent/netpol` (kube-router: iptables + ipset)
- `pkg/agent/loadbalancer/utlity_linux.go`
- `pkg/agent/syssetup` (modprobe br_netfilter, sysctls)
- `pkg/rootless`
- `pkg/daemons/agent/agent_linux.go` (170 LOC of cgroup and kubelet flags)
- `pkg/executor/embed/embed_linux.go`
- `pkg/containerd/builtins_linux.go` (overlayfs, fuse-overlayfs, stargz, btrfs)
- the embedded runc, and klipper-lb, which is an iptables DNAT pod (a Linux image).

Portable pieces:
- server/supervisor
- kine (SQLite via `modernc.org/sqlite`, pure Go)
- embedded etcd (`k3s-io/etcd`)
- helm-controller, deploy controller, cert management
- local-path-provisioner. It uses a busybox helper pod, which would run under Linuxulator. Better: replace it with
  ZFS datasets.

There is precedent: k3s already has a **Windows agent** split (`run_windows.go` 46 LOC, `agent_windows.go` 104 LOC,
`builtins_windows.go`, `netpol_windows.go`, `flannel/setup_windows.go`). A FreeBSD agent is roughly the same shape
plus networking.

### 4.3 "kbsd": a k3s-style single binary for FreeBSD

Recommendation: **don't fork k3s. Build a thin distribution that reuses k3s's portable libraries (kine,
helm-controller) where convenient.** Contents:

- **Server:** apiserver, controller-manager, scheduler (upstream, with the `possibleCPUs` patch), kine/SQLite
  or embedded etcd, konnectivity-server (for webhooks when the control plane is not on the pod network), and a
  CoreDNS manifest (Linux image under Linuxulator, or FreeBSD-native CoreDNS image built from `dns/coredns`).
- **Agent:**
  - kubelet with the FreeBSD ports from this report
  - containerd with the CRI and ZFS snapshotter, plus a jail shim (runj/ocijail) and RuntimeClasses `jail`,
    `linux` (Linuxulator) and `bhyve`
  - a pf-based kube-proxy
  - a vnet/bridge CNI
  - `csi-proxy-freebsd`
  - a bundled **ZFS LocalPV CSI** as the default StorageClass, with VolumeSnapshotClass included
  - node-exporter.
- **rc.d integration**, so no systemd is needed: `kbsd_enable`, with flags in `/etc/rc.conf`.
- Packaging: a single port, `sysutils/kbsd`, with `pkg install kbsd && service kbsd start` as the "60-second cluster"
  pitch. This is where FreeBSD can differentiate: **ZFS-native PVs, snapshots and hard ephemeral quotas out of the
  box**, which k3s cannot do.

---

## 5. Observability and node ecosystem

- **cAdvisor is Linux-only** (cgroups, `/proc`, `/sys`). In kubelet it is the `cadvisor_unsupported.go` stub. Write
  `cadvisor_freebsd.go` to provide `MachineInfo` (`hw.ncpu`, `hw.physmem`, `kern.smp.topology`), `RootFsInfo`/
  `ImagesFsInfo` (`statfs` on the ZFS datasets) and `VersionInfo`. Container stats should come from the CRI, with
  feature gate `PodAndContainerStatsFromCRI` (the `cri_stats_provider_others.go` path exists, and the runtime fills
  `ContainerStats` from **rctl/racct** per jail: `cputime`, `memoryuse`, `readbps`, ...). Network stats come from
  per-vnet `if_data`.
- **`/stats/summary` and metrics-server:** metrics-server scrapes `/metrics/resource`. It works as soon as CRI stats
  carry CPU and memory. HPA and `kubectl top` then work unchanged.
- **node-exporter:** FreeBSD is a first-class target (FreeBSD collectors in-tree; port `sysutils/node_exporter`
  1.11.0). Run it as a host rc.d service or as a FreeBSD-native DaemonSet image. The upstream Linux image under
  Linuxulator would report Linux-emulated `/proc` garbage, so don't do that.
- **Logging:** containerd's CRI writes the CRI log format (`<ts> stdout F msg`) to `/var/log/pods/...`. That is
  portable Go, so kubelet log rotation and `kubectl logs` work. **fluent-bit** has a port (`sysutils/fluent-bit` 5.2.0);
  run it as rc.d or as a FreeBSD-image DaemonSet. Vector and promtail are Go and should cross-build [verify].
- **kubelet journal endpoint** (`kubelet_server_journal_others.go`) is unsupported, which is fine.
- **DaemonSets that assume Linux:** kube-proxy, every CNI daemonset (Calico/Cilium/flannel), every CSI node plugin,
  node-local-dns, log shippers, GPU operators, security agents (Falco is eBPF and impossible). **Strategy: copy the
  Windows playbook.**
  - Kubelet labels `kubernetes.io/os=freebsd` automatically (`kubelet_node_status.go:317`, from `goruntime.GOOS`).
    Linux DaemonSets that already set `nodeSelector: kubernetes.io/os: linux` (most charts do) skip FreeBSD nodes for
    free.
  - Add the taint `node.kubernetes.io/os=freebsd:NoSchedule` so untargeted workloads don't land there by accident.
  - Ship FreeBSD-native equivalents as host services first (kube-proxy-pf, CNI, csi-proxy, node-exporter,
    fluent-bit), and as DaemonSets later.
- **The Linux-images-on-FreeBSD tension (the LCOW problem):**
  - Kubelet hard-codes the OS label to GOOS and re-asserts it every sync (`kubelet_node_status.go:574-579`).
    Admission rejects pods whose `nodeSelector kubernetes.io/os` or `spec.os.name` mismatches GOOS
    (`lifecycle/predicate.go`, with a comment saying "Relax this restriction when we start supporting LCOW").
  - So a Linux image under Linuxulator on a FreeBSD node must *not* say `os: linux` anywhere, while most charts say
    exactly that.
  - Options:
    - (a) Users add a toleration and a FreeBSD nodeSelector per workload. This is honest, and it's what Windows
      users do.
    - (b) A **mutating webhook** that rewrites the selector for pods in opted-in namespaces with a `linux`-ABI
      RuntimeClass. RuntimeClass `scheduling.nodeSelector`/`tolerations` already merge at admission.
    - (c) Patch kubelet to accept `spec.os.name=linux` when the RuntimeClass handler is Linuxulator or bhyve. This is
      the LCOW TODO; propose it upstream with a KEP.
    - (d) Lie: make kubelet report `kubernetes.io/os=linux`. This breaks the moment someone runs a real Linux
      DaemonSet with privileged netlink calls.
  - **Choose (b) + (c).** And `spec.os.name: freebsd` itself requires an API change
    (`validation.go:127 validOS = {Linux, Windows}`) plus PSA updates, since `check_seccompProfile_restricted.go:151`
    exempts only Windows.
- **Mixed clusters, Phase 1 = Linux control plane + FreeBSD workers.** Yes, this is the right first milestone.
  Control-plane OS is invisible to users, and CNI controllers, CSI controllers, operators and webhooks keep running
  on Linux nodes. FreeBSD workers only need kubelet, containerd, a pf kube-proxy and a CNI that interoperates
  (VXLAN with flannel's Linux side, or BGP routing). Windows proved this model works at scale. Phase 2 moves the
  control plane to FreeBSD, which is already demonstrated, solving webhooks via konnectivity.

---

## 6. Blockers

| # | Blocker | Severity | Solution | Effort |
|---|---|---|---|---|
| 1 | kubelet container manager is a stub, so kubelet exits (`cm/container_manager_unsupported.go`) | **Critical** | `container_manager_freebsd.go` modelled on Windows; device manager; rctl hooks | M (2–4 wk) |
| 2 | mount-utils unsupported, so **every pod fails** (SA token tmpfs) | **Critical** | `mount_freebsd.go` (nmount/getfsstat) | S–M (1–2 wk) |
| 3 | cadvisor stub: `MachineInfo` errors, so no node capacity | **Critical** | `cadvisor_freebsd.go` (sysctl/statfs) + `PodAndContainerStatsFromCRI` | S (1 wk) |
| 4 | No securityContext or resources sent to CRI on non-Linux (`kuberuntime_*_unsupported.go`); containerd CRI "other" ignores them | **Critical** | build Linux CRI messages on FreeBSD; containerd `container_create_freebsd.go` → OCI `freebsd.jail` | M (3 wk) |
| 5 | `possibleCPUs()` missing, so kubelet/apiserver/kcm don't compile | Low | 3-line `util_freebsd.go` (ports already carry it) | XS |
| 6 | API validates `spec.os.name ∈ {linux, windows}`; kubelet rejects `os: linux` pods on FreeBSD; charts pin `kubernetes.io/os: linux` | **High (ecosystem)** | Phase 1: taints + webhook rewrite; Phase 2: KEP for `freebsd` OSName + LCOW-style relaxation | S code / **XL politics** |
| 7 | No mount propagation, so CSI node plugins can't publish mounts | High | `csi-proxy-freebsd` host daemon + host-process (non-jailed) privileged containers | M (3–4 wk) |
| 8 | NFS/UFS not jail-mountable (`VFCF_JAIL`) | Medium | mount on host via csi-proxy; long-term kernel work | S / L |
| 9 | No FreeBSD CSI node backends (iscsictl, geom, /dev/da*) in any driver | High | ship our own ZFS LocalPV CSI; add iSCSI later | M |
| 10 | subPath, fsGroup, hostutil stubs | Medium | port (openat `O_RESOLVE_BENEATH` + nullfs file mounts; chown walk) | S |
| 11 | No seccomp | Medium (security posture) | MVP: accept RuntimeDefault, reject Localhost; later `mac_syscallfilter` / Linuxulator seccomp | L (kernel) |
| 12 | No per-process capability drop for jail root | Medium | coarse `allow.*` now; `mac_privfilter` via `mpo_priv_check` | M (kernel module) |
| 13 | No user namespaces | Medium | reject `hostUsers:false`; optional UID-range admission policy; bhyve RuntimeClass for untrusted | n/a |
| 14 | `hostPID` has no jail equivalent | Low | privileged/host-process only | S |
| 15 | No freebsd pause image | Low | pauseless sandbox (`persist` jail) or a static FreeBSD pause | XS |
| 16 | Few `os: freebsd` images; containerd silently falls back to linux | Medium | RuntimeClass + `runtime_platforms`; document | S |
| 17 | containerd mounts via `mount(8)` exec; go-zfs execs `zfs(8)` | Low (perf) | nmount + libzfs_core later; benchmark first | M |
| 18 | Linux DaemonSets (kube-proxy, CNI, CSI, log, Falco) | Medium | Windows playbook: taints/selectors + native host services | S each |
| 19 | Admission webhooks unreachable from a FreeBSD control plane off the pod network | Medium (Phase 2) | konnectivity or BGP | S |
| 20 | k3s won't build (rootlesskit, opencontainers/cgroups, CNI ns, ...) | Low (it's a choice) | new thin "kbsd" distribution | L |
| 21 | unionfs still documented as data-destroying; no ZFS whiteouts | Low (avoid it) | ZFS clones | – |
| 22 | GPU / CUDA | Low (scope) | out of scope; devfs-ruleset device plugin only | – |

### 6.1 What to drop for the MVP

**MVP = Linux control plane + FreeBSD worker running FreeBSD-native *and* Linuxulator pods via containerd + ZFS
snapshotter + jail shim, passing a curated subset of conformance.**

- **Keep:** blockers 1–5 (kubelet runs), 10 (subPath/fsGroup, since too many charts need them), 15, 16, and the
  hostNetwork, hostIPC, runAsUser, readOnlyRootFilesystem, allowPrivilegeEscalation and privileged mappings. Storage:
  emptyDir (ZFS + tmpfs), configMap/secret/projected, hostPath, and **one CSI driver: ZFS LocalPV**, which needs no
  propagation because the node plugin runs as a host-process container.
- **Drop:**
  - seccomp Localhost, AppArmor, SELinux and user namespaces: reject with a clear event.
  - hostPID (privileged only).
  - mount propagation in general, NFS/iSCSI CSI and third-party CSI drivers.
  - the unionfs and tarfs snapshotters.
  - GPUs.
  - `spec.os.name: freebsd`: use labels and taints instead.
  - a FreeBSD control plane, k3s parity, and in-cluster DaemonSets for node agents (use rc.d host services).
- **Explicitly document as weaker than Linux:** no syscall filtering, and no userns. Recommend the bhyve RuntimeClass
  for multi-tenant or untrusted workloads.

---

## Sources (web)

- FreeBSD Handbook, OCI Containers: https://docs.freebsd.org/en/books/handbook/containers/
- FreeBSD Foundation, container images: https://freebsdfoundation.org/freebsd-container-images/
- FreeBSD Foundation, OCI runtime-spec 1.3 / FreeBSD: https://freebsdfoundation.org/?p=28383
- FreeBSD Foundation, Podman testing (ZFS vs vfs): https://freebsdfoundation.org/blog/advancing-cloud-native-containers-on-freebsd-podman-testing-highlights/
- FreeBSD Foundation, unionfs work: https://freebsdfoundation.org/?p=16842 ; cgit log https://cgit.freebsd.org/src/log/sys/fs/unionfs/union_vfsops.c
- Forum thread, thin jails + unionfs: https://forums.freebsd.org/threads/seeking-advice-thin-jail-unionfs-running-with-workarounds.92826/latest
- dch, ZFS datasets into OCI jails: https://people.freebsd.org/~dch/posts/2025-06-27-oci-zfs/
- FreshPorts: https://www.freshports.org/net/kube-apiserver/ , https://www.freshports.org/net/kube-controller-manager , https://freshports.org/net/kube-scheduler/ , https://www.freshports.org/sysutils/kubectl/ , https://www.freshports.org/sysutils/helm , https://www.freshports.org/dns/coredns/ , https://www.freshports.org/sysutils/node_exporter/ , https://www.freshports.org/sysutils/fluent-bit/ , https://freshports.org/devel/etcd
- etcd on FreeBSD: https://etcd.io/docs/v3.3/platforms/freebsd/
- Tony Norlin, FreeBSD control plane + Linux workers: https://kubernaut-eu.pages.dev/posts/kubernetes-on-freebsd-with-linux-worker-nodes-and-cilium ; https://medium.com/@norlin.t/welcome-mandala-kubernetes-v1-29-on-freebsd-control-plane-7732fa7a1271
- 2024 DevSummit OCI on FreeBSD: https://forums.freebsd.org/threads/2024-freebsd-developer-summit-oci-containers-on-freebsd.93967
- Planternetes: https://forums.freebsd.org/threads/kubernetes-planternetes-on-freebsd.90030/latest
- CSI on Windows / csi-proxy: https://kubernetes-csi.github.io/docs/csi-windows.html , https://github.com/kubernetes-csi/csi-proxy
- democratic-csi: https://github.com/democratic-csi/democratic-csi
- Samuel Karp, runj + containerd: https://samuel.karp.dev/blog/2021/05/running-freebsd-jails-with-containerd-1-5
