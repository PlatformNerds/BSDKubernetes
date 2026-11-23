# 20. Volume ownership, host ports, filesystem types and PID stats

*2026-11-18. Two FreeBSD 15.1 arm64 nodes, kernel `bsdk8s17`.*

Conformance run 10 (after the kube-proxy work of research/19 and the `bsdk8s16` kernel) passed 425/446 with
exactly the same 21 failures as run 7: no regressions. Ten of those failures were reopened, plus related
non-conformance tests. All ten turned out to be real gaps, not Linux-only test details.

## `fsGroup` was never applied (kubernetes 018cdd93c86, patch 0028)

Repro: a pod with `runAsUser: 1000`, `fsGroup: 1001` and a secret volume with `defaultMode: 0440`. The process had
group 1001, but the files were owned by group 0, so `cat` failed with `Permission denied`.

The kubelet's `SetVolumeOwnership` (chown to the fsGroup, setgid directories, group read/write) was built for Linux
only. Elsewhere `NewVolumeOwnership` returned a no-op, so **no volume on FreeBSD (secret, configMap, projected,
downward API, emptyDir, PV) ever got the pod's fsGroup.** The implementation only uses POSIX calls. Renaming the
file to a neutral suffix builds it for FreeBSD: a `_linux.go` name restricts a file to Linux whatever its build
tags. Its unit tests pass on FreeBSD.

## Host ports did nothing (CRI-O e964b0341, cni-plugins ef9ebba)

A pod with `hostPort` got no pf rules at all, and every connection was refused:

- CRI-O maps host ports itself (iptables or nftables) and gives the CNI plugins none. It has neither on FreeBSD,
  so `disable_hostport_mapping` defaults to true there, and then nothing mapped them. When it doesn't map them,
  CRI-O now passes the port mappings to the CNI plugins (patch cri-o 0018). The node's CNI configuration already
  chains dfr's FreeBSD `portmap` plugin, which writes `rdr` rules.
- `rdr` applies only to packets arriving on an interface, so the node's own connections (127.0.0.1 or the node
  IP, e.g. from a hostNetwork pod) still failed. The plugin now adds `pass out quick on lo0 ... rdr-to` per
  mapping, as kube-proxy does for NodePorts (research/09). It also lower-cases the protocol and flushes the
  filter rules on DEL (patch cni-plugins 0001; installed as `/usr/local/libexec/cni/portmap`, the package's
  binary kept as `portmap.pkg`).

Verified on node 2 with three pods on host port 54399 (127.0.0.1/TCP, node IP/TCP, node IP/UDP). Each is
reached from node 2 itself and from node 1, and each answers with its own hostname. Not working: a connection
from source 127.0.0.1 to 127.0.0.1. pf applies `nat-to` but not `rdr-to` on the outbound lo0 rule, and the
packet's second pass through lo0 creates a second state, so the reply is only half un-translated. Linux needs
`route_localnet` for this case too. The e2e test connects with the node IP as source.

## tmpfs volumes reported as `nullfs`

Volumes are nullfs-mounted into the container's jail. `f_fstypename` names that layer (`nullfs`). Linux's
`statfs` on a bind mount reports the filesystem behind it. nullfs already passes the lower filesystem's type
number (`f_type`) through:

- **agnhost mounttest** (the e2e image, kubernetes dbbe0e5db7d, patch 0030) maps `f_type` to its name through
  `vfs.conflist`. A Memory emptyDir now reads `tmpfs`. The binary was swapped into
  `registry.k8s.io/e2e-test-images/agnhost:2.55` (and the 2.63.0 tag) on both nodes; the old image is tagged
  `2.55-prev-fstype`.
- **Linux programs** (freebsd-src patch 0031, branches `bsdk8s/linux-statfs-nullfs[-15.1]`, kernel `bsdk8s17`,
  `linux64.ko` only): the Linuxulator translated the type by name, so every volume of a Linux pod reported type 0,
  and so did `/proc` (`linprocfs` wasn't in the table). In a Linux pod (static Go `statfs` probe, RuntimeClass
  `linux`):

  | Path | Before | After (Linux reports) |
  |---|---|---|
  | `/proc` | 0 | `0x9fa0` proc |
  | emptyDir, medium Memory | 0 | `0x01021994` tmpfs |
  | emptyDir, disk | 0 | `0x2fc12fc1` zfs |
  | service-account token | 0 | tmpfs |
  | `/`, `/sys/fs/cgroup` | zfs, cgroup2 | unchanged |

**emptyDir tmpfs mode** (kubernetes 826259c9a7a, patch 0031). A Linux tmpfs root defaults to mode 1777, while
FreeBSD's takes the mount point's mode (0777). The kubelet creates a missing subPath directory with the volume
root's mode, so the directory lacked the sticky bit Linux gives it. The kubelet now mounts emptyDir tmpfs with
`mode=1777` on FreeBSD.

## PID stats (kubernetes 84d432e5ab0, patch 0029)

The node summary had no `rlimit` stats on FreeBSD, so the `pid.available` eviction signal and the `PIDPressure`
condition could never fire. FreeBSD limits processes rather than threads. The kubelet now reports
`min(kern.maxproc, kern.pid_max)` and the number of processes in `kern.proc.proc`. Node 2 reports
`"maxpid":17636,"curproc":45`, which matched `ps` at the time.

## Results

Focused e2e (EmptyDir, Secrets, Projected, ConfigMap, Downward API volume and HostPort tests; CSI, local-PV,
Serial, Slow and Feature tests skipped): **92/93**. That covers the 7 EmptyDir tmpfs, 2 fsGroup-secret and
HostPort conformance tests, and 4 non-conformance fsGroup emptyDir tests.

The one left, "pod should support memory backed volumes of specified size" (NodeConformance, not Conformance),
runs `df | grep <volume>`. The size limit works (`df -k -a` in the pod shows 10240 KiB), but ocijail mounts
volumes with `MNT_IGNORE` (dfr, so a node's `df` isn't flooded by every pod's mounts), which hides them from a
plain `df` in the jail too. FreeBSD's `df` also counts 512-byte blocks. The jail and the host share one mount
table, so this one stays a documented difference.

**Update (2026-11-19): passes.** The test runs upstream's busybox image, whose `df` is busybox's applet. Our FreeBSD
busybox build had left the applet out: it reads Linux's mount table (`/proc/mounts`), so the port disables it. With
`scripts/busybox-freebsd/local/` it now walks `getmntinfo(3)` and counts 1K blocks, like upstream's. It lists
`MNT_IGNORE` mounts too, as Linux lists bind mounts. In a jail, a nullfs mount whose source is a host path the jail
can't see is named `nullfs`, as Linux names a bind mount by its filesystem. Otherwise busybox wraps the line after the
long device name, and `awk '{print $2}'` reads "Used" instead of the size. In the pod, `df` now shows
`nullfs 10240 4 10236 0% /usr/share/volumeshare`. FreeBSD's own `df` and `MNT_IGNORE` are unchanged.

## Also fixed along the way

Node 2's clock was 15 minutes behind node 1: its ntpd lost its pool servers to a DNS outage (probably while the
host Mac slept). It now also syncs from node 1 (`server 192.168.100.1 iburst maxpoll 6`), so the nodes stay in
step without the internet.

## Local PersistentVolumes in e2e: hostexec over SSH (kubernetes 0041-0043)

*2026-11-20.* The local volume tests (`PersistentVolumes-local`, `In-tree Volumes [Driver: local]`) prepare each
volume on the node through the "hostexec" pod, which enters PID 1's mount namespace with `nsenter` (below). A jail
can't leave its jail, so every one of them failed at setup, before Kubernetes' local volume code ran.

- **0041 (e2e only):** on a `kubernetes.io/os=freebsd` node, hostexec runs its command as root over the e2e
  framework's SSH helper instead (the provider's key: `LOCAL_SSH_KEY`, `KUBE_SSH_USER`, `sudo -n` when that user
  isn't root). The commands then act on the node's own file systems, as `nsenter` does on Linux. Bind-mounted
  volume types use `mount -t nullfs`. The rest of the commands run as written: `mkdir`, `ln -s`,
  `mount -t tmpfs -o size=10m`, `umount`, `rm`, `find | xargs -r -0`.
- **0043 (e2e only):** the tests delete the pod without waiting and unmount the volume 5 ms later. On Linux the
  source of a bind mount can be unmounted while the bind is in use. On FreeBSD the kubelet's nullfs mount of the
  volume holds the file system below it busy (`EBUSY`) until the kubelet unmounts it, a few seconds after the pod
  is gone. On FreeBSD the teardown retries for up to three minutes. `umount -f` would pull the file system out from
  under a pod that may still be running.
- **0042 (kubelet), a real bug the tests exposed:** a pod given a block-mode volume never finished terminating.
  Mapping it fails, as it must (FreeBSD has no block or loop devices). Unmapping it started with
  `DetachFileDevice`, which returned "not supported", and the kubelet retried forever. FreeBSD now has its own
  `volumepathhandler`: nothing can have been attached, so there is nothing to detach (Linux does the same when no
  loop device holds the file). `AttachFileDevice` still fails. Deployed: kubelet `v1.36.1-bsdk8s+1994eece269` on
  both nodes.

Running the tests needs a key both nodes accept (on node 1: `~/.ssh/e2e_ed25519`, authorized for `bsdk8s`):

```
env LOCAL_SSH_KEY=$HOME/.ssh/e2e_ed25519 KUBE_SSH_USER=bsdk8s ./ginkgo ... \
  --focus="PersistentVolumes-local|\[Driver: local\]" --skip="Disruptive|Flaky|Serial|LinuxOnly|Feature" ...
```

**Result: 97/145; every non-block test passes (97/97),** including `[Slow]`, across the dir, dir-link,
dir-bindmounted, dir-link-bindmounted and tmpfs types: fsGroup, two pods at once and one after the other, subPath,
multiVolume, volumeIO, and the block-volmode "should fail to create pod" checks. The 48 failures were the `block`,
`blockfs` and `blockfswith[out]format` types, whose setup runs `losetup` (and `mkfs -t ext4`). Before: 72/145, and
before 0041 none got past setup. **Since then all of them pass: 143/143** (the other 2 skip themselves, as on Linux);
see "Block volumes" below. I had written here that they "can't pass" because FreeBSD has no block devices. That was
wrong: FreeBSD has no *block* devices, but `md(4)` in vnode mode is its loop device, and disks are character
devices that work as Linux's block devices do, given whole-sector I/O.

## Local PersistentVolumes with fsGroup (kubernetes 92f38fe72f8, patch 0032)

*2026-11-19.* A sig-storage run beyond the conformance set (no CSI or cloud drivers; 97 tests) passed 22. Of the 75
failures, 74 are local-volume tests: `PersistentVolumes-local` and `In-tree Volumes [Driver: local]`. They prepare
each volume on the node through a privileged "hostexec" pod that runs
`nsenter --mount=/rootfs/proc/1/ns/mnt -- sh -c '<cmd>'`, with Linux commands such as `mount --bind`, `losetup` and
`mkfs.ext4`. A FreeBSD jail can't enter the host's mount namespace that way, so this test machinery doesn't port.
The 75th failure is the known `df` test above.

So local PVs were checked by hand: a `local` PV on a directory of node 2, a PVC, a writer pod with `fsGroup: 1234`,
then a reader pod. **The writer never started:** `MountVolume.SetUp failed ... mount.GetMountRefs unsupported`.
- **Why:** the local plugin calls `GetMountRefs` before applying `fsGroup`, to see whether another pod already uses
  the volume. The FreeBSD mount-utils returned "unsupported". Without `fsGroup` the mount worked.
- **Fix:** `GetMountRefs` is now implemented. FreeBSD's bind mounts are nullfs mounts whose source
  (`f_mntfromname`) names the directory they show, so the references are the nullfs mounts that share the path's
  source, from `getfsstat(2)`. Unit test: `mount_freebsd_test.go`.
- **Result:** the writer's file is owned by group 1234 on the node, and the reader reads it back.

## Host paths didn't show the file systems below them (CRI-O 153946dd0, patch 0023)

*2026-11-19.* On Linux a hostPath volume is a recursive bind (`rbind`). The container also sees the file systems
mounted below the path when it starts. A FreeBSD nullfs mount shows only the file system the path itself is on.
With the default ZFS layout, `/var/log`, `/tmp`, `/home`, `/usr/src` ... are separate datasets, so:

- a pod with hostPath `/var` saw an **empty** `/var/log` (36 files on the node; a log shipper would read nothing);
- a pod with hostPath `/` (node-exporter style) saw none of `/tmp`, `/home`, `/var/log`, ...

No e2e test caught it. They mount host directories that have nothing mounted below them.

**Fix:** for each directory host path, CRI-O adds a nullfs mount for every file system mounted strictly below it, at
the same place below the destination. Parents go first, and ocijail unmounts them in reverse. Each one is
read-only when the host path is. That is stricter than Linux, where only the top mount is read-only unless
`recursiveReadOnly` is requested.

Two things differ from Linux, both on purpose:

- **Mounts of the node stack are skipped.** FreeBSD has no mount namespaces, and a nullfs mount keeps the file
  system below it busy: unmounting a tmpfs with a nullfs on top fails with `Device busy`. Copying a pod's secret or
  emptyDir tmpfs into another container (a log agent with hostPath `/var`) would therefore stop the kubelet from
  unmounting it when that pod is deleted, which is the old Linux "mount leak". Skipped trees:
  - the kubelet's directory (`/var/db/kubelet`, `/var/lib/kubelet`);
  - container storage (CRI-O's root and runroot);
  - `/var/run/ocijail` and `/var/run/crio`.
  A hostPath of the kubelet's directory therefore shows the pod directories but not the volumes mounted inside them.
- **Pseudo file systems are skipped** (`devfs`, `fdescfs`, `procfs`, `linprocfs`, `linsysfs`, `mqueuefs`). A
  nullfs view of the host's devfs bypasses the jail's devfs ruleset: a container would get the host's device
  nodes, and a read-only mount doesn't stop writes to a device. On Linux the device cgroup restricts that; jails
  have only the ruleset.

Verified on both nodes:

| Check | Before | After |
|---|---|---|
| hostPath `/var`, `ls /host/var/log` | 0 entries | 36 (as on the node) |
| hostPath `/` (read-only): `/var/log`, `/tmp`, `/home` ... | empty | 10 datasets mounted, all `ro`; a write fails with "Read-only file system" |
| ... `/rootfs/dev` | empty | empty (devfs skipped) |
| delete a pod with a memory emptyDir while the hostPath `/` pod runs | | 1.5 s, all 6 of its mounts gone |
| delete the hostPath `/` pod | | its 10 extra mounts gone (301 → 278 mounts) |
| volume e2e (`HostPath\|Subpath\|EmptyDir\|Projected\|ConfigMap\|Secrets\|Downward API`, 79 tests) | | 78 pass; the failure is a CSI mock driver test (no FreeBSD CSI sidecar images then; it passes since, research/24) |

Unit test: `server/submounts_freebsd_test.go` (6 cases).


## The mount table in a container (freebsd-src 0038-0042, CRI-O be684e8d3, busybox `mount`)

Tests check a volume's options from inside the pod the Linux way: `mount | grep /mnt/test | grep rw,`
(or `ro,`), `mount | grep 'on /mnt/test' | awk '{print $6}'` for a PV's `mountOptions`, `grep " /dir "
/proc/mounts` for its type. Twelve storage tests failed on that (7 csi-hostpath, 5 NFS). On FreeBSD:

- A container (a jail) had no `/proc`, and FreeBSD's `mount(8)` showed only `/`: ocijail flags
  container mounts `MNT_IGNORE`, so that the host's `mount` and `df` stay readable. `mount -v` shows
  them as nullfs mounts of host paths, `(nullfs, local)`, never `rw`, with nothing about the volume's
  file system or its NFS options, which belong to the host's mount.
- Linux describes a bind mount by the file system behind it. linprocfs (Linux's `/proc`, which Linux
  images already get) showed the nullfs mount instead.
- "provision storage with mount options" did not fail on the output at all, as this document said
  before: the PV's `mountOptions` are `relatime,vers=3`, and the NFS client refused `relatime`
  ("mount option <relatime> is unknown"), so the pod never started.

Fixes:

| | |
|---|---|
| freebsd-src 0039 | linprocfs' `/proc/mounts` and `/proc/<pid>/mountinfo` look through nullfs (also nested) to the mount below and report its source and type, with `ro`/`noatime` from either mount; they add the options Linux shows that FreeBSD keeps only in the mount's option list: `relatime` and the NFS client's `vers=`, `rsize`/`wsize`, `hard`/`soft`, `proto=`, `timeo`, `retrans`. The table is read without `MNT_WAIT`, so a dead NFS server's mount no longer hangs it. |
| freebsd-src 0038 | backport of OpenZFS 16858492e6e4: FreeBSD 15.1's ZFS listed `relatime=on` but ignored it (strict atime). The backport implements it, with the default off on FreeBSD as upstream chose; **the nodes set `relatime=on` on zroot** (Linux nodes' default). ZFS now also records relatime in the mount's options, as it does atime. |
| freebsd-src 0040 | a bug: ZFS's property callbacks record options in `mnt_opt` while `VFS_MOUNT()` runs, and the kernel then replaces `mnt_opt` with the caller's list, so everything they recorded on a first mount was lost (the root file system showed no `relatime`). They now record in `mnt_optnew` while a mount is in progress. |
| freebsd-src 0041 | updates swap a mount's option list under the mount interlock, which ZFS and linprocfs hold when they touch it. |
| freebsd-src 0042 | the NFS client accepts Linux's access time options (`relatime`, `strictatime`, `lazytime`, `[no]diratime`). The server keeps NFS access times, so they ask nothing of the client, as on Linux. |
| CRI-O be684e8d3 (0025) | containers of FreeBSD images get linprocfs on `/proc` too, unless the pod mounts something there. FreeBSD's tools (`ps`, `procstat`) don't use `/proc`. |
| busybox image | BusyBox's `mount` applet, ported (`scripts/busybox-freebsd/local/patch-util-linux_mount.c`): it lists `/proc/mounts` in Linux's format (`getmntinfo(3)` without `/proc`) and mounts like FreeBSD's `mount(8)` (`mount_<type>` if there is one, else `nmount(2)`; `bind` is nullfs). Upstream's image is busybox, so its `mount` is this applet. |

In a pod now:

```
zroot/ROOT/default on /mnt/test type zfs (rw,relatime)
zroot/ROOT/default on /mnt/ro type zfs (ro,relatime)
tmpfs on /var/run/secrets/kubernetes.io/serviceaccount type tmpfs (ro)
```

Results: NFS drivers 35/35 (NFS section below); csi-hostpath 26/36, all filesystem tests (research/24).
Linux containers see the same table. Nodes boot `/boot/kernel.bsdk8s24`.

## subPath could escape the volume (kubernetes 0050, a security fix)

*2026-11-22, found by the `[Slow]` rerun.*

- **The bug:** the FreeBSD subPath code (`pkg/volume/util/subpath/subpath_freebsd.go`, from the 2023 "WIP:
  subpath mounts" commit in the series we build on) resolved a subPath's symlinks on the host with
  `filepath.EvalSymlinks` and nullfs-mounted the result, without checking that it was still inside the volume.
  A pod that writes a symlink into its volume (`ln -s /etc x`) and mounts `x` as a subPath in another container
  got the host's `/etc` there. Any host path the kubelet can reach was exposed this way. It is CVE-2017-1002101,
  which Linux has prevented since 2018 by opening each path component without following symlinks and
  bind-mounting the resulting file descriptor.
- **How it showed:** the 30 `[LinuxOnly]` subPath tests "should fail if subpath {directory,file,with
  backstepping} is outside the volume" (10 drivers × 3) failed with "subpath container unexpectedly
  terminated": the container started, with the host's `/bin` or `/bin/sh` mounted.
- **The fix:**
  - The resolved path must lie within the resolved volume path, or the container fails with
    `CreateContainerConfigError` ("failed to prepare subPath"), as on Linux.
  - nullfs can't mount a file descriptor, and mount(8) resolves the path again. So the kubelet opens the path
    component by component without following symlinks (`doSafeOpen`). After the mount, it checks that the mount's
    root is the same file: inode number, generation and type. nullfs passes these through from the file below; the
    device differs, so `os.SameFile` can't be used. A component swapped for a symlink between the check and the
    mount is unmounted and refused.
- **Checked:** every subPath e2e test (not Serial/Disruptive/Feature/CSI): **156/156**, including the 30.
  Before the fix, those 30 failed in the `[Slow]` run (research/07).

## Mount propagation: not available

`[sig-node] Mount propagation should propagate mounts within defined scopes` fails, and can't pass on FreeBSD:

- The test drives the node through the hostexec pod (`nsenter`, see above).
- More fundamentally, FreeBSD has no shared subtrees. Checked by hand on node 2: with `/a` nullfs-mounted at `/b`,
  a tmpfs mounted later at `/a/sub` doesn't appear at `/b/sub`, and one mounted at `/b/sub2` doesn't appear at
  `/a/sub2`.
- So `HostToContainer` and `Bidirectional` behave like `None`: a snapshot taken at container start (above), with no
  later propagation either way.
- The kubelet already treats propagation that way on Windows, which has none either (`translateMountPropagation`
  maps every mode to private).
- The practical cost: CSI node plugins rely on `Bidirectional` to make their mounts visible to the kubelet, so a
  CSI node plugin can't run as a pod on FreeBSD. It would have to run on the host.

## hostPath to a unix socket (ocijail dbf2231, freebsd-src 0035)

*2026-11-19.* A hostPath can name a daemon's socket (a container runtime, a CSI or device plugin, a log or
metrics agent). On FreeBSD such a pod never started:

```
container create failed: mounting {"destination":"/mnt/test",...,"source":".../asocket","type":"bind"}: Invalid argument
```

nullfs in 15.1 mounts only directories and regular files. Upstream lifted that in main on 2026-08-28:
`2c68ad49f13d` "nullfs: Allow VSOCK to be mounted on top of another VSOCK" (D59158, reviewed by dfr@), written
for exactly this container use. The mount point must be a socket too.

- **Kernel:** freebsd-src 0035, a backport of `2c68ad49f13d` to 15.1. It is upstream's commit unchanged, on
  branch `bsdk8s/nullfs-socket-mount-15.1`. No main twin is needed. Both nodes boot `kernel.bsdk8s22`.
- **ocijail** (0015-0016): for a socket source it creates a socket node as the mount point and mounts over it.
  It uses `bindat(2)` relative to the directory, since container paths are longer than `sun_path`. The node is
  removed on unmount. On a kernel without the change the error says so. New test `test_socket_mount`: a host
  listener, its socket mounted into a container, `nc -U` there reads the greeting, and the node is gone
  afterwards. It is skipped where `mount_nullfs` can't mount a socket.
- **Userland:** `mount_nullfs(8)` from the same commit, linked statically against the patched `libutil.a` and
  installed on both nodes (old binary kept as `.prev-vsock`). The system `libutil.so` isn't replaced, so
  `mount -t nullfs` still refuses sockets; `mount_nullfs` itself works.

| Check | Before | After |
|---|---|---|
| `HostPathType Socket` e2e | 5/7 (both "should be able to mount" fail) | **7/7** |
| ocijail tests (node 2, `bsdk8s22`) | 31 | 32/32 (`test_socket_mount` runs) |
| `mount_nullfs` of a socket on the node, `nc -U` through it | "must be either a file or directory" | the listener's reply arrives |

The other 14 `HostPathType` failures are block and character devices; see the next section.

## Device nodes: `allow.mknod` (freebsd-src 0043, CRI-O 0026-0027, ocijail 0018)

*2026-11-21.* The `HostPathType` Character and Block Device tests create `achardev` (`c 89 1`) and `ablkdev`
(`b 89 1`) with `mknod` in an ordinary pod, in a hostPath directory, then check how the kubelet classifies them.

**Correction:** this section used to say FreeBSD can't have these nodes outside devfs. It can. As root on the
node, `mknod` makes both character and block nodes on ZFS and tmpfs. They are **inert**: since FreeBSD 5 a device
is reached only through devfs. A node elsewhere is a file with a type and a number. Checked on node 2 with the
number of `/dev/mem`: reading the copy on ZFS returns end of file, and writing to it fails.

What failed was the jail: `mknod` in a pod returned `EPERM`, because a jail never gets `PRIV_VFS_MKNOD_DEV`. On
Linux it is a capability, `CAP_MKNOD`: containerd and Docker include it in the default set, and CRI-O leaves it out.

- **Kernel (0043, branch `bsdk8s/jail-allow-mknod-15.1`, stacked on `allow.pf`):** a new jail parameter,
  `allow.mknod`, grants `PRIV_VFS_MKNOD_DEV`.
  - devfs implements `mknod(2)` by bringing back a node that was removed or that the jail's devfs ruleset hides
    (`DE_WHITEOUT`). With `allow.mknod` alone, that would let a jail unhide devices.
  - So `devfs_mknod()` now refuses every jailed caller, with or without `allow.mknod`. Before this patch a jail
    never got that far.
  - Checked by hand: in a jail with `allow.mknod` and ruleset 4, `mknod /dev/mem` fails and `mem` stays hidden.
- **CRI-O (0026-0027):** a container jail gets `allow.mknod` when the container is privileged or ends up with
  `MKNOD`. "Ends up with" means the defaults, plus what it adds, minus what it drops; one `hasCapability`
  helper now serves both `NET_RAW` and `MKNOD`. The sandbox jail always gets the parameter when the kernel knows it.
  A child jail can't have a permission its parent lacks, which containers will have the capability isn't known
  yet, and the sandbox only runs pause.
- **ocijail (0018):** accepts the `org.freebsd.jail.allow.mknod` annotation.
- **Cluster configuration (demo `build-cluster.sh`, `35-default-capabilities.conf`):** like containerd, containers
  have `MKNOD` by default. On FreeBSD it grants nothing beyond the inert nodes.

| Pod (node 2, `kernel.bsdk8s25`) | `mknod /mnt/c c 89 1`, `mknod /mnt/b b 89 1` | `mknod /dev/mem c 0 15` |
|---|---|---|
| plain, CRI-O's default capabilities | `EPERM` | `EPERM` |
| `capabilities.add: [MKNOD]` | both created | `EPERM` |
| `privileged: true` | both created | `File exists` (a privileged pod sees all of `/dev`) |
| plain, `MKNOD` in the defaults (as configured now) | both created | `EPERM` |

**`HostPathType`: 21/35 → 31/35.** All the "should fail" checks for device nodes now pass: the kubelet
classifies the inert nodes correctly.

**The 4 left** ("should be able to mount character/block device ... when HostPathType is CharDev/BlockDev/Unset")
start a pod that bind-mounts the device node itself into the container. ocijail does that with a nullfs file mount,
and nullfs mounts only directories, regular files and sockets (`Invalid argument`). **They pass since ocijail
0019-0020 (35/35)**; see "Device nodes outside devfs" below.

The real gap was a hostPath to a real device (`/dev/fuse`, a GPU's `/dev/dri`). It is closed now; see the next
section.

## Host devices: hostPath on devfs and CRI `devices` (freebsd-src 0044-0045, CRI-O 0028-0029)

*2026-11-21.* Working on the gap above found a hole first. A hostPath volume is a nullfs mount, and a hostPath
`/dev` gave a container a nullfs view of the **host's** devfs: every host device, whatever the container's devfs
ruleset hides. It looked harmless, because `read` and `write` fail (`Device not configured`): the file gets
vnops, and devfs's `dead_read`/`dead_write` run. But `truss` showed `open` succeeding. `devfs_open` runs the
driver's `d_open` (`/dev/klog` answered `Device busy`: the driver was reached), and `vn_ioctl` passes `VCHR` on
through `null_bypass` to `devfs_ioctl`, which calls the driver's `d_ioctl`. An unprivileged pod with hostPath
`/dev` could, for example, issue `DIOCGDELETE` on a host disk. Jails may also read `/dev/mem` once they can see it
(`PRIV_KMEM_READ` is allowed in `prison_priv_check`). On Linux the device cgroup stops such a container from
opening any device it wasn't given.

**Kernel (0045, `kernel.bsdk8s26`):** `null_open` refuses a `VCHR` vnode with `ENXIO`, the error `read` already
returned. Device nodes outside devfs have no driver, so only devfs devices are affected, and those couldn't be read
or written through nullfs anyway. FIFOs and sockets through nullfs still work (checked).

**A kernel panic on the way (0044).** Checking what host root can do through such a view, `dd if=/dev/mem bs=512
count=1` *on the host* panicked node 2: `pmap_map_io_transient: TODO: Map out of DMAP data`. arm64 (and riscv)
`memrw()` hands an offset outside the direct map to `uiomove_fromphys()`, whose `pmap_map_io_transient()` panics
for such addresses. The direct map covers all RAM, so anything outside it is a hole or device memory: the access
now fails with `EFAULT`. Main has the same code. (Pods never reached this: `/dev/mem` is hidden from them, and
through nullfs the open fails.)

**CRI-O (0028-0029):** host paths on devfs no longer become nullfs mounts. They do what the device cgroup does on
Linux, with devfs:

| Request | Becomes |
|---|---|
| a device, or a directory of devices, at its own path below `/dev` (hostPath `/dev/fuse` at `/dev/fuse`) | devfs rules that unhide it (and its parent directories; a directory's entries three levels deep) in the container's devfs |
| hostPath `/dev` at `/dev` | nothing: the container's own devfs |
| hostPath `/dev` elsewhere (`/host/dev`) | a second devfs mount with the ruleset and rules of the container's `/dev` (`ruleset=0` for privileged containers, which see every device, as on Linux) |
| a device at another path (`/dev/klog` at `/dev/kmsg`) | an error: devfs can't show a device under another name |
| CRI `devices` (device plugins) | the same unhide rules; privileged containers see everything already |

Pods on node 2:

| Pod | Result |
|---|---|
| hostPath `/dev/full` at `/dev/full` | `/dev/full` shown and readable; `/dev/mem` still hidden |
| hostPath `/dev` at `/host/dev` | `/host/dev` lists what `/dev` lists (`fd null random ... zero`); `/host/dev/zero` readable |
| hostPath `/dev/full` at `/dev/myfull` | `CreateContainerError`: "/dev/full is a device on the host's devfs; on FreeBSD a container can only get it at /dev/full, not /dev/myfull" |
| hostPath `/dev/input` at `/dev/input` | `event0` ... `event2` shown |
| privileged, hostPath `/dev` at `/host/dev` | every host device |
| hostPath `/dev` at `/dev` | the usual `/dev` |

Limits:
- Rules are applied when the container starts. A device that appears later (a driver loaded afterwards) stays
  hidden until the container restarts. Linux creates a container's device nodes at start too.
- `additional_devices` in crio.conf and the `io.kubernetes.cri-o.Devices` annotation were accepted and ignored on
  FreeBSD (a stub). CRI-O 0030 parses them with the Linux syntax and unhides them the same way; the access mode
  (`rwm`) is accepted, but devfs has no per-container modes, so the node's own mode applies.
- Volume e2e (`HostPath|Subpath|EmptyDir|Projected|ConfigMap|Secrets|Downward API`, 118 tests) on bsdk8s26 with
  this CRI-O: 113 pass; the 5 failures are the CSI mock token test and the 4 below, as before. (Since 2026-11-22 all pass:
  the mock driver reaches the mount proxy, research/24, and the 4 are copies, below.)
- The 4 HostPathType tests above still failed here: their device nodes are inert nodes on a ZFS or tmpfs, not
  devfs, so they were nullfs file mounts, which nullfs refuses. Fixed by ocijail 0019-0020 (next section).

## Device nodes outside devfs: copied, not mounted (ocijail 0019-0020)

*2026-11-21.* A hostPath to a device node made with `mknod` on a disk file system or tmpfs (the HostPathType
CharDevice/BlockDevice tests) can't be a nullfs mount, and the kernel can't simply allow one: in `struct vnode`,
`v_rdev` (the device of a `VCHR` vnode) shares a union with `v_mountedhere`. Mounting over a device node would put a
`struct mount` pointer where generic code expects a `cdev`; `vn_fsync_buf()`, for one, reads
`vp->v_rdev->si_mountpt` for every `VCHR` vnode. So no kernel change.

Such a node is inert: since FreeBSD 5, a device is reached only through devfs. ocijail now gives the container a
node of the same type, number, mode and owner (`mknod` in the container's root, then `chmod`/`lchown`), the way it
already copies regular files when the kernel has no file mounts. `delete` removes it, with the directories made for
it, and puts back what the image had at that path. A device **in devfs** is refused ("a devfs device can't be
bind-mounted; expose it with a devfs rule"): its copy would be inert, not the device. CRI-O never sends those as
mounts anyway (previous section).

Differences from a Linux bind mount, all about the node's metadata: it isn't the same inode, so a `chmod`, `chown`
or removal on one side doesn't show on the other, and the mount's `ro` option has no effect on it.

| Check | Before | After |
|---|---|---|
| `HostPathType` e2e | 31/35 | **35/35** |
| ocijail tests (node 1) | 32 | 33/33 (`test_device_node_mount`: char and block copies with mode and owner, the image's file restored, a devfs source refused) |

## Block volumes and formatted local disks (kubernetes 0044-0046, CRI-O 0031)

*2026-11-21.* Kubernetes' block volumes (`volumeMode: Block`, `volumeDevices`) and local PVs of a disk (formatted by
the kubelet) work on FreeBSD now. FreeBSD has no block devices: disks are character devices in devfs, read and
written in whole sectors, with no buffer cache. `md(4)` in vnode mode (`mdconfig -a -t vnode -f file`) is the loop
device. What changed, layer by layer:

| Layer | Linux | FreeBSD |
|---|---|---|
| What is a disk (`hostutil.GetFileType`) | a block device | a character device that is a GEOM provider (disk, partition, label, `md`, zvol), read from `kern.geom.conftxt` without opening it. Local PVs of a disk, block volumes and HostPathType `BlockDevice` take FreeBSD disks |
| Per-pod references below the volume's global map path | bind mounts of the device node, counted through the mount table | symbolic links to the device (nullfs can't mount a device node), counted as links that lead to a device; `FindGlobalMapPathUUIDFromPod` (after a kubelet restart) follows them |
| Lock against removal | a loop device attached to the global map path | none: `md(4)` can't stack on a device, and an open device is busy anyway |
| Format and check (`SafeFormatAndMount`) | `blkid`, `mkfs.<type>`, `fsck -a` | the same `blkid` and `mkfs.ext4` (e2fsprogs, now installed on the nodes and by `build-cluster.sh`), `e2fsck -p` or `fsck_ufs -p` (FreeBSD's `fsck` picks the checker from fstab, not the disk), `newfs -U` for ufs; ext2/3/4 mount as `ext2fs`, which handles all three. Before, the kubelet mounted without formatting |
| The device in the container (CRI `devices`, at the pod's `devicePath`, e.g. `/mnt/volume1`) | a device node made there | the device unhidden in the container's devfs at its own name, and `devicePath` a symbolic link to it in the container's root file system (CRI-O makes it once the root file system is mounted; a path below `/dev` stays an error, the devfs would hide the link) |

The e2e tests needed FreeBSD paths too (e2e only): `mdconfig` for `losetup` (and `mdconfig -d` retried while the
kubelet's mount still holds the disk: `losetup -d` detaches on release), `mkfs.ext4` + `mount -t ext2fs` for the
`blockfs` type, `test -c` instead of `test -b`, `ext2fs` as the file system name of an ext4 volume, md's 512-byte
sectors where Linux runs `blockdev --getss`, and no `oflag=nocache` (FreeBSD's `dd` has none, and a raw disk has no
cache). Their block reads and writes now use whole sectors (`dd bs=512 conv=sync`, then `head -c`), which a Linux
block device accepts the same way; writing 18 bytes with `echo > /dev/md0` fails on a raw disk (`Invalid argument`).
Applications that use raw block volumes (databases) already do sector-aligned I/O.

**Result: local volumes 143/143** (`PersistentVolumes-local|[Driver: local]`, not Disruptive/Flaky/Serial/LinuxOnly/
Feature, 2 more skip themselves: no fsGroup on block devices), from 97/145. The run left no `md` disks or mounts.

Limits:
- `test -b` is false for a FreeBSD disk: a program that insists on `S_ISBLK` won't take it. `lstat(2)` on the
  `devicePath` shows a link; `stat(2)`, `open(2)`, `read(2)`, `write(2)` and `ioctl(2)` reach the disk.
- I/O must be whole sectors (no buffer cache for disks).
- CSI block volumes work too, with csi-driver-host-path 0002 (`mdconfig` in the driver, publish as a link; csi-hostpath
  36/36, research/24). kubernetes 0047 (e2e only) makes the ephemeral block check expect a character device.

## Growing file systems (kubernetes 0051-0052)

*2026-11-23.* FreeBSD built mount-utils' `resizefs_unsupported.go`, so every expansion the kubelet does itself
failed with `Resize is not supported for this build`. That covers a local PV of a disk, and any in-tree volume the
kubelet formats. CSI drivers grow their own file systems in NodeExpandVolume, so csi-hostpath expansion already worked.

kubernetes 0051 adds `resizefs_freebsd.go`:
- **UFS:** grown with `growfs -y`, which works while the file system is mounted.
- **ext2/3/4:** grown with `resize2fs` (e2fsprogs).
- **NeedResize:** compares the media size from `diskinfo` with the UFS size from `dumpfs` (`size` fragments × `fsize`).
  It skips read-only mounts.

Checked by hand:
- A local PV of `/dev/md77` with `fsType: ufs`, formatted by the kubelet, grew online from 63 MB to 126 MB.
- The kubelet logged `MountVolume.NodeExpandVolume succeeded`.
- The pod then wrote 90 MB, and its earlier file was intact. `fsck_ufs` was clean afterwards.

Limit: **ext file systems grow only while unmounted.** FreeBSD's ext2fs has no online resize, so `resize2fs` on a
mounted ext4 volume fails opening the device (`Operation not permitted`). The kubelet reports that error. Use UFS
(`fsType: ufs`) for volumes that must grow online.

The e2e `PersistentVolumes-expansion loopback local block volume` test (kubernetes 0052, e2e only):
- It ran the Linux `dd oflag=append` and `losetup -c`. That loopback file system is mounted on the directory that
  holds its backing file, so `loopDir/file` names a file *inside* the mounted file system. Upstream's `dd` appends to
  that inner file, and the test passes without growing anything.
- On FreeBSD it now grows the `md(4)` device itself (`mdconfig -r`, from its `diskinfo` size). A vnode md may be
  larger than its backing file, which grows as the new blocks are written.
- Its AfterEach unmounted the volume while the test pod still ran. On FreeBSD the container's nullfs mount keeps it
  busy (`Device busy` for three minutes), so the pod is now deleted first.
- The test passes. Its PV is the mounted directory, for which the kubelet's NodeExpand is a no-op, so it doesn't
  exercise `growfs`: the hand check above does.

Along the way: mount-utils' `TestSafeFormatAndMount` expected the Linux `fsck -a` and `mkfs.xfs -f`. On FreeBSD it
panicked and stopped the package's tests; the FreeBSD commands are now mapped, and every mount-utils test passes on
FreeBSD.

## CSI volume directories leaked (kubernetes 0053)

*2026-11-23.* Every CSI pod volume on FreeBSD left its directory behind:
`pods/<uid>/volumes/kubernetes.io~csi/<pv>/vol_data.json`, which kept the pod directory too. The nodes had 378
and 372 of them.

- **Why:** mount-utils' `IsLikelyNotMountPoint` returned `(false, ENOENT)` for a missing path on FreeBSD, where Linux
  returns `(true, ENOENT)`. After NodeUnpublishVolume removes the target directory, the CSI plugin's `isDirMounted`
  accepts `ENOENT` and uses the boolean, so `removeMountDir` took the volume for mounted and silently kept its files.
  TearDown still reported success.
- **How it showed up:** after a kubelet restart, the kubelet rebuilds these volumes from disk. Once the test's CSI
  driver was gone, it retried TearDown every two minutes, for good (`driver name ... not found in the list of
  registered CSI drivers`: 144,000 log lines on one node).
- **Fix:** kubernetes 0053 returns `true` with the error, as Linux does, and a unit test covers it.
- **Cleanup:** the stale directories (nothing mounted, pod gone) were removed with the kubelet stopped. After
  csi-hostpath tests on the new kubelet, no CSI volume directory remains, and the kubelet's orphan cleanup took the
  pod directories from 391 to 20 and from 359 to 3.
- **Regression check:** the change affects every plugin's teardown. On the new kubelet: local volumes 143/143 (as
  before), csi-hostpath 36/36, and HostPath, EmptyDir and subPath all pass (120/120 without `[Slow]`). No md disks,
  mounts or volume directories were left.

## Kubelet restarts with volumes mounted: the `[Disruptive]` storage tests (kubernetes 0054-0056, csi-driver-host-path 0003)

*2026-11-23.* No storage `[Disruptive]` test had run on FreeBSD before. They stop and restart the kubelet while a pod
uses a volume, and some delete the pod while the kubelet is down. They then check that the kubelet still serves the
volume, or that it cleans the volume up.

**The test helpers (kubernetes 0055, e2e only):**
- They ran `systemctl stop|start kubelet` and `systemctl show kubelet` over SSH, read the host's
  `/proc/self/mountinfo`, and hard-coded `/var/lib/kubelet`.
- On a FreeBSD node they now use `service kubelet stop|start|status` and `mount(8)`, and the framework's
  `--kubelet-root-dir` (the right path on any node).
- `findMountPoints`, used for the "global mount points leaked" check, only parsed its output when the command
  failed. So it always returned nothing, and that check couldn't fail on Linux either; it now parses on success.
  FreeBSD has no `mountpoint(1)`, so there it reads `mount -p`.

What the tests then found:

1. **A volume stayed mounted after the kubelet restarted (kubernetes 0054).** All 7 "should unmount if pod is
   force deleted while kubelet is down" subPath tests failed.
   - A volume the kubelet can't rebuild from disk after a restart gets one cleanup: one UnmountVolume, then it's
     forgotten.
   - That cleanup runs about a second after the kubelet starts, before it has killed the deleted pod's containers.
   - On FreeBSD the container's nullfs mount of the subPath keeps the kubelet's subPath mount busy, so the unmount
     failed with `Device busy`. On Linux the container's mount namespace holds copies, so the unmount goes through.
   - The volume then stayed mounted until the next kubelet restart.
   - Fix: the kubelet keeps such a volume while its directory exists and tries again every 10 s, for at most 5
     minutes. A unit test covers it.
2. **A disk-backed local volume couldn't be rebuilt (kubernetes 0056).**
   - FreeBSD's `GetMountRefs` gave, for a pod's nullfs mount, the other nullfs mounts of the same source, but not
     the source itself. Linux lists every mount of the same directory, including the kubelet's global mount of the
     disk.
   - The local plugin's `ConstructVolumeSpec` uses that global mount to find the device. Without it, rebuilding
     the volume failed (`local volume source is nil or local volume path is empty`).
   - The fallback cleanup unmounts only the pod's mount, so the disk stayed mounted under
     `plugins/kubernetes.io/local-volume/mounts` for good (found as a leftover `md` disk on node 2).
   - Fix: the source is now included when it is itself a mount point.
3. **A csi-hostpath block volume could never be deleted (csi-driver-host-path 0003).** This was a bug in the test
   driver port, not the kubelet.
   - Patch 0002 publishes a block volume as a symbolic link. Upstream's "already published" check only recognises a
     mount, so the kubelet's republish after its restart added the target to the volume's `Published` list a second
     time.
   - NodeUnpublishVolume removes one entry, so the volume stayed "still published": NodeUnstageVolume and
     DeleteVolume failed for good, and the PV was never deleted.
   - Fix: the driver doesn't list a target twice.
   - Building the driver: it must be built against the fork's mount-utils (the `replace` in its `go.mod`), and with
     `-X main.version=...`. Without the `replace`, every publish fails with `util/mount on this platform is not
     supported`; without the version, every call fails with `Driver is missing version`.

**Result:** storage `[Disruptive]` (`--focus='\[sig-storage\].*\[Disruptive\]'`, `[LinuxOnly]` included,
without cloud drivers, NFS and `NonGracefulNodeShutdown`; 161 selected, 34 apply): **34/34**. The 32 others passed
in the full run, and the 3 csi-hostpath block tests on the fixed driver. Each run left no mounts, `md` disks or PVs
behind. With the rebuilt driver, the regular csi-hostpath tests still pass 36/36.

## Node logs are rotated (CRI-O 0033)

*2026-11-23.* Nothing rotated `/var/log/kubelet.log` or `/var/log/crio.log`. After eight days they had reached 535
and 577 MB (kubelet) and 105 and 162 MB (CRI-O).

- **kubelet:** its rc script already runs it under `daemon(8) -o ... -H`, which reopens the file on SIGHUP.
- **CRI-O:** it opened its `--log` file once. CRI-O 0033 also reopens the file on SIGHUP; SIGHUP still reloads the
  configuration too.
- **Rotation:** `/usr/local/etc/newsyslog.conf.d/kubernetes.conf` (demo `build-cluster.sh` 2e08622) rotates both at
  100 MB and keeps 7 bzip2 copies. It signals the `daemon(8)` supervisor for the kubelet and crio itself.
- Both nodes rotated without a restart, and the pods kept running.

## NFS: client, server pods, recycler (freebsd-src 0036, CRI-O a90bf43d4)

*2026-11-19.* Kubelet's NFS client already worked: `mount_nfs` from the host, then nullfs into the container.
The e2e NFS tests run their NFS *server* in a privileged pod (`volume/nfs`), which never started on FreeBSD:

- **Kernel:** `allow.nfsd` lets `mountd` and `nfsd` run in a jail, but only in a jail with its own vnet. A pod's
  containers are child jails that *inherit* the pod's vnet, so even a privileged container couldn't serve NFS.
  freebsd-src 0036 adds `security.jail.nfsd_inherited_vnet` (default 0). When it is set, a jail with
  `allow.nfsd` may also run the daemons in a vnet inherited from a jail ancestor. NFS server state is per vnet,
  so the host's server stays isolated. A jail on the host's network stack never qualifies. The other
  requirements are unchanged: the jail's root must be a mount point, and `enforce_statfs` must not be 0. A
  main-based twin (`bsdk8s/jail-nfsd-inherited-vnet`) compiles. Both nodes boot `kernel.bsdk8s23`.
- **CRI-O** (0024): privileged pods with their own network get `allow.nfsd` when the kernel has it. Linux has
  the same rule: kernel nfsd in a container needs a privileged pod.
- **Node:** `build-cluster.sh` loads `nfsd` and sets the sysctl.
- **Image:** `volume/nfs:1.6.0` (`scripts/build-e2e-images-freebsd.sh`) runs `rpcbind`, `mountd` and `nfsd -t`.
  It is TCP only, because RPC in a jail refuses UDP. It exports its arguments, with `V4:` on the first one.
- **Checked by hand:** a pod on one node mounts the server pod on the other over NFSv3 and v4.

Found along the way:

- **BusyBox grep had no `\b`.** The PV tests run `id -G | grep -E '\b777\b'`. Our BusyBox used libc's POSIX
  `regcomp`, which says "trailing backslash". It now links FreeBSD's `libregex` (what `grep(1)` uses) statically.
- **The PV recycler image was Linux.** The `Recycle` reclaim policy runs a pod with
  `registry.k8s.io/build-image/debian-base`. `build-cluster.sh` now passes
  `--pv-recycler-pod-template-filepath-{nfs,hostpath}` to the controller-manager. The template is upstream's
  default with `docker.io/freebsd/freebsd-runtime:<release>`, which runs the same scrub command.
- **A dead NFS server hangs its clients.** Both bugs above made a test delete its NFS server while a client pod
  still had the volume mounted. The mounts are hard, as on Linux, so kubelet, CRI-O and podman threads
  then blocked in `statfs(2)`; the node kept running, but those pods never terminated. Linux has the same
  problem: kubelet's in-tree NFS teardown doesn't force the unmount there either. Recovery:
  `umount -N <mount point>` cancels the outstanding RPCs and dismounts. Separately, a manifest list left behind
  by an interrupted image-mirror run had broken CRI-O's `ListImages`. The mirror script now always removes it.

- **Linux NFS mount options (freebsd-src 0037).** The dynamic tests put `vers=4.0` / `vers=3` in the StorageClass,
  and Linux PVs commonly say `vers=4.1` or `nfsvers=4.1`. FreeBSD's `mount_nfs` rejected `vers=4.1` ("illegal vers
  value") and passed `nfsvers` on to `nmount(2)`, which refused it, so such a PV couldn't be mounted. 0037 accepts
  `nfsvers` as a synonym and `4.N` as `vers=4,minorversion=N`. Checked: `vers=4.0/4.1/4.2` and `nfsvers=4.1` give
  that minor version (`nfsstat -m`), `nfsvers=3` gives v3, and a pod with a PV with `mountOptions: [nfsvers=4.1]`
  reads and writes over NFSv4.1. The binary is installed on both nodes (`/sbin/mount_nfs.prev-vers` kept).

### Dynamic provisioning: a FreeBSD nfs-provisioner (2026-11-19)

The in-tree NFS e2e driver starts `registry.k8s.io/sig-storage/nfs-provisioner` in every test. That
is `kubernetes-sigs/nfs-ganesha-server-and-external-provisioner`, which serves NFS with NFS Ganesha.
Ganesha has no FreeBSD port. The pod isn't privileged (`DAC_READ_SEARCH` only), so the kernel nfsd can't
stand in either. The FreeBSD build (`patches/nfs-provisioner`, image from
`scripts/build-e2e-images-freebsd.sh` with `NFS_PROVISIONER=` and `UNFSD=`) runs **unfs3**, a user-space
NFSv3 server. It writes unfs3 export lines and reloads `unfsd` with SIGHUP. NFSv3 only, so the `nfs`
driver's dynamic tests, which mount `vers=4.0`, can't use it. The `nfs3` driver's can.

Getting it to pass turned up four more problems:

- **unfs3 sent whole-second file times** (`patches/unfs3` 0001). A pod rewrote a 64-byte file within the
  second another pod had cached it. The size and mtime matched, so the second pod kept reading the old
  data (the `multiVolume` "concurrently access" tests). unfsd now sends the nanoseconds (`st_mtim`).
- **mount_nfs retries forever** (kubernetes 0037). For a server that doesn't answer, FreeBSD's
  `mount_nfs` retries every 60 s indefinitely, so the kubelet's mount never returned. The volume
  operation stayed pending and the pod could neither start nor be deleted; one such mount was still
  retrying after 3 h 41 min. Linux's `mount.nfs` gives up after two minutes in the foreground, so the
  kubelet's FreeBSD mounter now adds `retrycnt=3` unless the PV sets a retry count.
- **ocijail stat'ed volume sources during teardown** (ocijail 0017). Deleting a namespace that holds
  an NFS server pod and its clients removes both at once. ocijail's `state`/`delete` then called
  `stat(2)` on the (dead) NFS volume source, which blocks on a hard mount, and took CRI-O down with it
  on that node. It now looks at a source only where the answer matters.
- **The kubelet stat'ed the dead volume too** (kubernetes 0038). Tearing the volume down,
  `PathExists` called `stat(2)` and `IsMountPoint` called `statfs(2)` on the mount point. Both block on a
  hard mount whose server is gone, and so would `umount(8)`, which looks the path up. On FreeBSD both
  checks now read the mount table (`getfsstat(2)` without `MNT_WAIT`). The mounter also implements
  `UnmountWithForce`, so the NFS plugin takes upstream's `CleanupMountWithForce` path: after the
  plugin's one-minute timeout, `umount -N` cancels the RPCs and dismounts without a path lookup.
  Deleting a namespace with an NFS server pod and two client pods now finishes on its own in 97 s;
  before, it never finished. (Upstream's Linux code has the same `stat(2)`; not checked here.)

| e2e (`NFS\|nfs`, without Disruptive/Flaky/Feature/LinuxOnly/Serial) | Result |
|---|---|
| `PersistentVolumes NFS` (PV/PVC pairs, multiple PVs, Recycle) | **12/12** |
| In-tree `nfs` + `nfs3` drivers and `DynamicProvisioner External`, skipping the `nfs` (v4) Dynamic PV and Generic Ephemeral patterns | **35/35** (2026-11-20; 30/35 before "The mount table in a container" above) |
| `nfs` (v4) Dynamic PV / Generic Ephemeral | not run: unfs3 has no NFSv4 |
