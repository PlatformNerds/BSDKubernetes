# Patch series

Applied on top of upstream `kubernetes/kubernetes` tag **v1.36.1**, in this order (see `scripts/build-freebsd-k8s.sh`):

| Dir | Source | Author | Notes |
|---|---|---|---|
| `kubernetes/v1.36.1/kubelet/` | `dfr/kubernetes` branch `freebsd-kubelet-v1.36.1` | Doug Rabson | FreeBSD cadvisor (racct), container manager, mount-utils, subpath, emptyDir, security context |
| `kubernetes/v1.36.1/proxy/` | `dfr/kubernetes` branch `freebsd-proxy-v1.36.1` | Doug Rabson | kube-proxy `pf` mode |
| `kubernetes/v1.36.1/kubeadm/` | `dfr/kubernetes` branch `freebsd-kubeadm-v1.36.1` | Doug Rabson | kubeadm on FreeBSD |
| `kubernetes/v1.36.1/bsdk8s/` | this project | BSDKubernetes | cgo-free kubelet build (enables cross-compile) + rctl racct parsing fixes, ..., 0022 node identity (`kernelVersion`, `osImage`, `bootID` from `kern.boot_id`, `systemUUID`, `machineID`), 0023 pf proxier honours `externalTrafficPolicy`/`internalTrafficPolicy: Local` (research/19) |

Outside Kubernetes (the branches are the source of truth; these are exports for review):

| Dir | Applies to | Notes |
|---|---|---|
| `ocijail/` | `dfr/ocijail` `main` → our `bsdk8s-main` | jail param fix, rctl resource limits, `allow.pf`, file mounts below devfs, `noNewPrivileges` (0009), duplicate env keys last-wins like runc (0014) |
| `freebsd-src/` | `releng/15.1` (one `bsdk8s/*-15.1` branch per topic in PlatformNerds/freebsd-src; main-based twins where main lacks the fix) | pf, rctl, jail `allow.pf`, Linuxulator (epoll, cgroup v2 view, mremap, arm64 brands), arm64 SME trap; 0013-0015 are backports from main (`TCP_USER_TIMEOUT`, sockopt copyout, `TCP_INFO`), 0016 fixes the `TCP_INFO`/optlen length on top; 0017 adds Linux-compatible abstract Unix socket addresses (15.1 only: main reworked `unp_connectat()`, port pending); 0018 adds `/proc/<pid>/fd` directories and a per-jail `/proc/1` to linprocfs (research/18); 0019 gives unnamed AF_UNIX addresses Linux's lengths; 0020 makes `sockstat` show abstract addresses as `@name`; 0021-0028 backport main's Linuxulator `membarrier` and prctl additions (`PR_SET_VMA`, `PR_GET_NO_NEW_PRIVS`, `PR_GET_CHILD_SUBREAPER`, THP) with a syscall regen; 0029 logs `FUTEX_REQUEUE` only when rejected |
| `cri-o/` | `dfr/cri-o` `freebsd-wip-1.37.0` → our `bsdk8s-1.37` | backport of the containers/storage fix for setuid/setgid bits lost when unpacking layers on FreeBSD |

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

main lacks our other 15.1-only patches (`allow.pf`, the arm64 musl brand, the SME trap fix, the cgroup view), so
the node was drained and the tests ran on the host and in hand-made jails, not in pods.

