# 24. CSI on FreeBSD: a mount proxy instead of mount propagation (2026-11-20)

## The problem

A CSI node plugin runs as a privileged pod. To publish a volume, it mounts the volume at a path the
kubelet chose, e.g. `/var/db/kubelet/pods/<uid>/volumes/kubernetes.io~csi/<pv>/mount`. The runtime
then gives that path to the workload container. On Linux the plugin's pod mounts the kubelet's
directories with `mountPropagation: Bidirectional`, so its mounts appear on the host.

FreeBSD has no mount propagation (research/20). The plugin runs in a jail that sees `/var/db/kubelet`
through a nullfs mount. A mount it makes there sits on top of the nullfs vnode, and the host's directory
never sees it. CSI node plugins therefore could not run as pods, and no CSI driver worked.

Windows has the same problem. Kubernetes solves it there with **csi-proxy**, a host service that does
storage operations on behalf of node plugins in containers. This is the FreeBSD equivalent.

## The design

- **`bsdk8s-mount-proxy`** (`components/mount-proxy`): a host daemon (rc.d `bsdk8s_mount_proxy`).
  - It listens on `<kubelet root>/plugins/bsdk8s-mount-proxy.sock`, mode 0600, root only. CSI plugin
    pods already mount the kubelet's `plugins/` directory, so no manifest changes are needed.
  - Requests are JSON, one per connection: `mount`, `unmount` and `list`.
  - It identifies the caller from `LOCAL_PEERCRED`: the peer's pid, then the pid's jail (`ps -o jid`)
    and the jail's root (`jls`).
  - It translates the caller's paths to host paths through that jail's root and its nullfs mounts.
    For example, `/csi-data-dir/<id>` in the plugin's container is `/var/lib/csi-hostpath-data/<id>`
    on the host. `list` translates the other way. A caller can therefore only mount what it can see
    already.
  - Mount targets must be below the kubelet's `pods/` or `plugins/`. Linux bind options become
    nullfs, as in `k8s.io/mount-utils` on FreeBSD.
- **`k8s.io/mount-utils`** (kubernetes 0040): in a jail, when the socket is reachable, `Mount`,
  `Unmount` and `List` go to the proxy. `IsMountPoint` and `UnmountWithForce` go through `List`, so
  they follow. A driver built against this mount-utils needs no FreeBSD code.
- **csi-driver-host-path** (patch 0001): its node server still imported the deprecated
  `k8s.io/utils/mount`, which has no FreeBSD support ("util/mount on this platform is not supported").
  It now imports `k8s.io/mount-utils`, a one-line change that is upstreamable on its own.
- **Images:** the e2e csi-hostpath driver and its 8 sidecars (provisioner, attacher, resizer,
  snapshotter, snapshot-metadata, node-driver-registrar, livenessprobe, health monitor) all build for
  freebsd/arm64 unmodified. `scripts/build-e2e-images-freebsd.sh` (`CSI_BINDIR=`) makes FreeBSD images
  of them at the manifest's tags. The binaries need `-X main.version=<tag>`, as upstream's
  release-tools set it, or the driver refuses `GetPluginInfo` ("Driver is missing version").

The e2e framework rewrites `/var/lib/kubelet/` in the manifests to `--kubelet-root-dir`
(`/var/db/kubelet` here), so the upstream manifests deploy unchanged.

A run, from the proxy's log, for one writer pod then one reader pod:

```
pid 66523 (root /var/db/containers/storage/zfs/graph/42ac...): mount -t nullfs /var/lib/csi-hostpath-data/169d... /var/db/kubelet/pods/fe08.../volumes/kubernetes.io~csi/pvc-31d0.../mount
pid 66523 (...): umount /var/db/kubelet/pods/fe08.../volumes/kubernetes.io~csi/pvc-31d0.../mount
pid 66523 (...): mount -t nullfs /var/lib/csi-hostpath-data/169d... /var/db/kubelet/pods/acc5.../volumes/kubernetes.io~csi/pvc-31d0.../mount
```

## Results

e2e `[Driver: csi-hostpath]`, not Disruptive/Flaky/Feature/LinuxOnly/Serial/Slow: **20/36**.

| | |
|---|---|
| Filesystem volumes: dynamic provisioning, a PVC as data source (clone), expansion (offline, online, with the pod re-created), subPath (4), ReadWriteOncePod, CSINode volume limits, CSI and generic ephemeral volumes (two pods with the same volume, several volumes per pod, expansion) | **20 pass** (snapshots: below) |
| Block volumes (9) | fail at provisioning: the driver makes block devices with `fallocate` + `losetup`. The kubelet's block-volume path (loop devices, device bind mounts) is Linux-only as well. Not attempted. |
| Ephemeral `rw,`/`ro,` checks (7) | read Linux `mount(8)` output in the container, as with NFS (research/20) |

### Snapshots

The snapshot tests need the VolumeSnapshot CRDs and a snapshot-controller in the cluster; e2e doesn't deploy
them. They come from external-snapshotter v8.5.0, the release of the e2e manifest's `csi-snapshotter`: its
`client/config/crd` CRDs, and `deploy/kubernetes/snapshot-controller` with the image set to
`snapshot-controller:v8.5.0`. Upstream's deploy file still names v8.4.0; the image is the v8.5.0 source built
for FreeBSD, so it's tagged as what it is. The image comes from `scripts/build-e2e-images-freebsd.sh`
(`CSI_BINDIR=` with a `snapshot-controller` binary). Both replicas run, one per node.

`[Driver: csi-hostpath]` snapshot tests (not Disruptive/Flaky/LinuxOnly/Serial): **7/8**. Taking a snapshot,
restoring a volume from it, and the `[Feature:VolumeSnapshotDataSource]` provisioning pass. The failure is the
block-volume variant. The driver makes snapshots with `tar`, which the FreeBSD base image has.

## Limits and next steps

- **Block volumes: not pursued.** On Linux, a raw block PV is mapped by bind-mounting the device node onto
  regular files: the kubelet's global map path (a reference it counts through the mount table) and the
  driver's publish target, with a loop device held as a lock. nullfs can't mount a device node, and FreeBSD
  has no block devices at all (disks are character devices). So the e2e block tests, which check `[ -b <path> ]`,
  fail on any FreeBSD node, like the HostPathType block tests (research/20). Supporting raw "block" PVs would
  mean redesigning the kubelet's block mapping (symlinks, `mdconfig` instead of `losetup`), the driver's
  (`truncate` + `mdconfig`) and the runtime's device exposure (unhide the `md` device in the jail's devfs and
  link the requested path to it), for character devices that applications ask for as block devices. Raw
  block PVs are uncommon; filesystem volumes are what CSI drivers are used for.
- **Drivers that format or attach disks** (`SafeFormatAndMount`, iSCSI, NVMe) would send more operations
  through the proxy. Today it does mount, unmount and list.
- **Security:** the socket is root-only and reachable only by pods that mount the kubelet's `plugins/`
  hostPath, which is privileged already. Requests come from root in some jail; the proxy doesn't
  check that the pod is privileged.
