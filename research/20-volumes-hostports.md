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
