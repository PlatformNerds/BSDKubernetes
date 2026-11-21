# Patch series

Applied on top of upstream `kubernetes/kubernetes` tag **v1.36.1**, in this order (see `scripts/build-freebsd-k8s.sh`):

| Dir | Source | Author | Notes |
|---|---|---|---|
| `kubernetes/v1.36.1/kubelet/` | `dfr/kubernetes` branch `freebsd-kubelet-v1.36.1` | Doug Rabson | FreeBSD cadvisor (racct), container manager, mount-utils, subpath, emptyDir, security context |
| `kubernetes/v1.36.1/proxy/` | `dfr/kubernetes` branch `freebsd-proxy-v1.36.1` | Doug Rabson | kube-proxy `pf` mode |
| `kubernetes/v1.36.1/kubeadm/` | `dfr/kubernetes` branch `freebsd-kubeadm-v1.36.1` | Doug Rabson | kubeadm on FreeBSD |
| `kubernetes/v1.36.1/bsdk8s/` | this project | BSDKubernetes | cgo-free kubelet build (enables cross-compile) + rctl racct parsing fixes, ..., 0022 node identity (`kernelVersion`, `osImage`, `bootID` from `kern.boot_id`, `systemUUID`, `machineID`), 0023 pf proxier honours `externalTrafficPolicy`/`internalTrafficPolicy: Local`, 0024 pf proxier `/healthz`, `healthCheckNodePort`, `externalIPs` and LoadBalancer IPs, 0025 pf proxier `loadBalancerSourceRanges`, 0026 rejects connections to Services without endpoints, 0027 spreads over endpoints whose named targetPort resolves to different ports (research/19), 0028 applies `fsGroup` ownership to volumes, 0029 reports process count and limit (`pid.available`, `PIDPressure`), 0030 agnhost mounttest reports the filesystem behind a nullfs mount, 0031 mounts emptyDir tmpfs with mode 1777 like Linux (research/20), 0032 implements mount-utils `GetMountRefs` (nullfs), so local PersistentVolumes mount for pods with `fsGroup`, 0033 drops a `SafeFormatAndMount.IsMountPoint` stub that always failed and shadowed the working one, 0034 ports the vendored SCTP library agnhost uses (`github.com/ishidawataru/sctp`) to FreeBSD, 0035 makes the pf proxier warn when pf won't translate an SCTP Service's port (research/23), 0036 makes the `regression-issue-74839` test server read TCP through bpf(4) on FreeBSD, 0037 gives NFS mounts `retrycnt=3` (mount_nfs retries an unreachable server forever; Linux gives up after two minutes, research/20), 0038 tears down an NFS volume whose server is gone (mount-table checks instead of stat/statfs, `umount -N` after the unmount timeout, research/20), 0039 applies kube-proxy's conntrack settings to pf's state options (CLOSE_WAIT → `tcp.closing` 1 h instead of pf's 15 min; state limit; research/07), 0040 mount-utils mounts through bsdk8s-mount-proxy from a jail (CSI node plugins, research/24), 0041 e2e only: the storage tests' hostexec runs its commands on a FreeBSD node over SSH instead of `nsenter` (a jail can't leave its jail), and bind-mounted local volumes use nullfs (research/20), 0042 the kubelet tears down a block-mode volume it couldn't map (FreeBSD has no loop devices: `DetachFileDevice` has nothing to detach; the pod stayed Terminating forever), 0043 e2e only: local volume teardown retries the unmount while the kubelet's nullfs mount still holds it (research/20) |

Outside Kubernetes (the branches are the source of truth; these are exports for review):

| Dir | Applies to | Notes |
|---|---|---|
| `ocijail/` | `dfr/ocijail` `main` → our `bsdk8s-main` | jail param fix, rctl resource limits, `allow.pf`, file mounts below devfs, `noNewPrivileges` (0009), duplicate env keys last-wins like runc (0014), unix socket mounts (hostPath to a socket, 0015-0016, research/20), teardown without stat(2) on mount sources (an NFS volume whose server is gone hung the runtime, 0017, research/20) |
| `freebsd-src/` | `releng/15.1` (one `bsdk8s/*-15.1` branch per topic in PlatformNerds/freebsd-src; main-based twins where main lacks the fix) | pf, rctl, jail `allow.pf`, Linuxulator (epoll, cgroup v2 view, mremap, arm64 brands), arm64 SME trap; 0013-0015 are backports from main (`TCP_USER_TIMEOUT`, sockopt copyout, `TCP_INFO`), 0016 fixes the `TCP_INFO`/optlen length on top; 0017 adds Linux-compatible abstract Unix socket addresses (15.1 only: main reworked `unp_connectat()`, port pending); 0018 adds `/proc/<pid>/fd` directories and a per-jail `/proc/1` to linprocfs (research/18); 0019 gives unnamed AF_UNIX addresses Linux's lengths; 0020 makes `sockstat` show abstract addresses as `@name`; 0021-0028 backport main's Linuxulator `membarrier` and prctl additions (`PR_SET_VMA`, `PR_GET_NO_NEW_PRIVS`, `PR_GET_CHILD_SUBREAPER`, THP) with a syscall regen; 0029 logs `FUTEX_REQUEUE` only when rejected; 0030 lets pf send a `block return` RST for a packet whose checksum is left to offload (from a VNET jail over an epair: pods), research/19; 0031 makes Linux `statfs(2)` report the filesystem behind nullfs and linprocfs as proc (research/20); 0032 rmlock wakes all turnstile waiters (lost wakeup with epoch waiters: node freeze under pod churn, research/21); 0033 `net.pf.sctp_rdr_ports` lets rdr change SCTP destination ports, recomputing the CRC32c (SCTP Services with `targetPort` ≠ `port`, NodePorts; research/23); 0034 `security.bsd.see_vnet_sockets` lets jails sharing a vnet (a pod's containers) see each other's sockets, as in one Linux network namespace (research/07); 0035 is a backport of main's 2c68ad49f13d (nullfs mounts a unix socket over a socket), for hostPath volumes that name a socket (research/20); 0036 `security.jail.nfsd_inherited_vnet` lets a jail with `allow.nfsd` run nfsd in a vnet it inherits from its parent jail (a pod's container in the pod's vnet), so an NFS server runs in a privileged pod (research/20); 0037 lets `mount_nfs` take Linux's `vers=4.1` and `nfsvers=` (NFS mount options written for Linux, e.g. in PVs; userland only, research/20); 0038 is a backport of OpenZFS 16858492e6e4 (FreeBSD honours the `relatime` property; default off, the nodes set `relatime=on` on zroot); 0039 makes linprocfs' `/proc/mounts` and `mountinfo` describe a nullfs (bind) mount by the filesystem behind it, with `relatime` and the NFS client's options as Linux shows them, and read the table without `MNT_WAIT` (a dead NFS server no longer hangs it); 0040 keeps the options ZFS records while mounting (they were dropped on a first mount); 0041 swaps a mount's option list under the mount interlock on update; 0042 lets the NFS client accept Linux's access time options (`relatime` ... ; a PV with `mountOptions: [relatime]` failed to mount) (research/20) |
| `cri-o/` | `dfr/cri-o` `freebsd-wip-1.37.0` → our `bsdk8s-1.37` | backport of the containers/storage fix for setuid/setgid bits lost when unpacking layers on FreeBSD, ...; 0018 passes host ports to the CNI plugins when CRI-O doesn't map them (research/20); 0019 image volumes on FreeBSD (read-only nullfs of the mounted image, research/07); 0020 containers with `CAP_NET_RAW` get `/dev/bpf` (research/07); 0021 implements port forwarding (`kubectl port-forward` crashed crio with a stack overflow), 0022 port-forwards to hostNetwork pods; 0023 mounts the file systems below a host path too, like Linux's recursive bind (research/20); 0024 sets `allow.nfsd` for privileged pods with their own network when the kernel has it (NFS server pods, research/20); 0025 gives containers of FreeBSD images Linux's `/proc` (linprocfs), whose mount table describes their volumes like Linux's (research/20) |
| `nfs-provisioner/` | `kubernetes-sigs/nfs-ganesha-server-and-external-provisioner` master `b8617ca` (no fork; patches only) | 0001 updates `x/sys` and `x/net` for freebsd/arm64 (`go.mod`/`go.sum`; run `go mod vendor`), 0002 runs unfs3 instead of NFS Ganesha on FreeBSD (the e2e NFS driver's provisioner pod, research/20) |
| `csi-driver-host-path/` | `kubernetes-csi/csi-driver-host-path` `v1.17.1` (no fork; patches only) | 0001 uses `k8s.io/mount-utils` instead of the deprecated `k8s.io/utils/mount` (build against the fork's mount-utils with a `replace`, research/24) |
| `unfs3/` | `unfs3/unfs3` `unfs3-0.11.0` (no fork; patches only) | 0001 reports the nanoseconds of file times (clients kept stale data after a same-size rewrite within the second, research/20) |
| `cni-plugins/` | `dfr/plugins` `freebsd` → our `bsdk8s/portmap-host` | 0001 FreeBSD portmap: host ports for the host's own connections (`pass out quick on lo0 ... rdr-to`), lower-case protocol, full flush on DEL (research/20) |

All patches are Apache-2.0 like Kubernetes itself. dfr's patches are unreviewed upstream WIP. The intent is to
contribute our fixes back to `dfr/kubernetes`, not to fork his work.

## main-based twins: built and booted (2026-11-17)

The main-based branches were applied together to FreeBSD main `ee9e945e7` (16.0-CURRENT), built on node 1 with its
clang 19 (`kernel-toolchain` without the clang/lld bootstrap), and booted once on a drained node 2 with
`nextboot -k`. 15.1 userland runs on it:

| Branch | Build | On the main kernel |
|---|---|---|
| `bsdk8s/linux-abstract-unix` | ok | `abstest` 19/19, VNET isolation, 1,600-connection churn: no panic |
| `bsdk8s/linux-proc-pid1-fd` | ok | `procfd` 6/6 in a jail (`/proc/1`, readlink, write through `/proc/1/fd/N`) |
| `bsdk8s/linux-unix-unnamed-len` | ok | unnamed-address lengths 2/2/0 |
| `bsdk8s/linux-futex-requeue-msg` | ok | - |
| `bsdk8s/linux-sockopt-copyout-len` | ok | not run (needs a TCP peer pod) |
| `bsdk8s/sockstat-abstract` | changed hunk compiles | main's `sockstat` needs main headers for unrelated `xtcpcb` fields |
| `bsdk8s/pf-local-cksum` (2026-11-18) | ok (`pf.ko`) | not booted; tested on 15.1 only |
| `bsdk8s/linux-statfs-nullfs` (2026-11-18) | ok (`linux64.ko`) | not booted; tested on 15.1 only |
| `bsdk8s/rmlock-wake-all` (2026-11-18) | ok | not booted; tested on 15.1 only |
| `bsdk8s/pf-sctp-rdr-port` (2026-11-19) | ok (`pf.ko`) | not booted; tested on 15.1 only (pods, ATF `sctp:rdr_port_v4`) |
| `bsdk8s/jail-vnet-socket-visibility` (2026-11-19) | ok | not booted; tested on 15.1 only |
| `bsdk8s/jail-nfsd-inherited-vnet` (2026-11-19) | ok (`kern_jail.o`) | not booted; tested on 15.1 only (NFS server pods, hand-made jails) |
| `bsdk8s/mount-nfs-linux-vers` (2026-11-19) | not built (`mount_nfs.c` is identical to 15.1's) | tested on 15.1 only |
| `bsdk8s/linprocfs-mounts-lower` (2026-11-20; main has the relatime backport already) | ok (`linprocfs.ko`, `zfs.ko`, kernel) | not booted; tested on 15.1 only |
| `bsdk8s/nfscl-linux-atime-opts` (2026-11-20) | ok (kernel) | not booted; tested on 15.1 only |

main lacks our other 15.1-only patches (`allow.pf`, the arm64 musl brand, the SME trap fix, the cgroup view), so
the node was drained and the tests ran on the host and in hand-made jails, not in pods.

