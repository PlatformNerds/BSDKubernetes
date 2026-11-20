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
| volume e2e (`HostPath\|Subpath\|EmptyDir\|Projected\|ConfigMap\|Secrets\|Downward API`, 79 tests) | | 78 pass; the failure is a CSI mock driver test (no FreeBSD CSI sidecar images) |

Unit test: `server/submounts_freebsd_test.go` (6 cases).

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

The other 14 `HostPathType` failures are block and character devices. The test creates them with `mknod(2)`
on the node, and FreeBSD has device nodes only in devfs. It has no block devices at all; disks are character
devices. These can't pass.

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
| In-tree `nfs` + `nfs3` drivers and `DynamicProvisioner External`, skipping the `nfs` (v4) Dynamic PV and Generic Ephemeral patterns | **30/35**. The 5 failures check Linux's `mount(8)` output inside the container: 4 ephemeral tests grep for `rw,`/`ro,`, and "provision storage with mount options" looks for `vers=3` in the mount's options. FreeBSD's `mount` prints `(nullfs, local)` and never `rw`. ocijail also flags container mounts `MNT_IGNORE` so that the host's `mount` and `df` stay readable, which hides them from `mount` in the container, and the NFS options belong to the host's mount, outside the jail. |
| `nfs` (v4) Dynamic PV / Generic Ephemeral | not run: unfs3 has no NFSv4 |
