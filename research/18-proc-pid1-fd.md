# 18: `/proc/1` and `/proc/<pid>/fd` for Linux containers (2026-11-17)

*Kernel `kernel.bsdk8s13` (now `bsdk8s14`, which adds research/14's unnamed-address fix) on both nodes. Patch freebsd-src/0018, fork branches `bsdk8s/linux-proc-pid1-fd-15.1`
(tested) and `bsdk8s/linux-proc-pid1-fd` (main; built and booted, see patches/README.md).*

**Result [verified]:** inside a Linux container, `/proc/1` is the container's first process, and
`/proc/<pid>/fd` lists, resolves and opens another process's descriptors as on Linux. `echo x > /proc/1/fd/1`
from `kubectl exec` lands in `kubectl logs`. Envoy Gateway's shutdown hook now works, so Envoy drains gracefully,
and deleting an Envoy pod takes **12 s instead of 361 s**.

## The problem

Found while re-enabling Envoy hot restart (research/14). Envoy Gateway's preStop hook, `envoy-gateway envoy
shutdown`, panicked:

    panic: open /proc/1/fd/1: no such file or directory

It writes its log to PID 1's stdout, a common container convention: on Linux the container's entrypoint is PID 1 in
its PID namespace, and its stdout is the container log. Two things were missing here:

1. **No `/proc/1`.** Jails have no PID namespace. A container sees only its own processes, under host PIDs, so no
   process 1 is visible.
2. **No `/proc/<pid>/fd` for other processes.** linprocfs made `fd` a symlink to `/dev/fd` (fdescfs) for the
   calling process and to `unknown` for any other. Even `/proc/self/fd/N` could not be resolved with `readlink`
   (fdescfs isn't mounted with `linrdlnk` in containers).

The hook therefore never ran. Envoy was never drained; it kept serving until the kubelet killed it at the end of the
360 s grace period, so every Envoy pod deletion, rollout and node drain waited six minutes. Draining node 1 for this
kernel timed out on exactly that.

## The change

- **`/proc/<pid>/fd` is a directory.** This is a new pseudofs node type, `pfstype_fddir`.
  - `readdir` lists the open descriptors.
  - `readlink` gives the path, or `pipe:[ino]`, `socket:[ino]`, or `anon_inode:[eventpoll]` (the Linuxulator's epoll
    is a kqueue).
  - `open` gives the caller the process's open file. Entries are VCHR vnodes with `VV_READLINK`, like fdescfs's, so
    open reaches `VOP_OPEN`. There the file is installed in a spare descriptor and handed to `openatfp()` through
    `td_dupfd`/ENXIO; `dupfdopen()` moves it to the descriptor `open()` returns.
  - That handover only happens for `open(2)` itself, and not in capability mode or with `O_RESOLVE_BENEATH`, where
    `openatfp()` would not take the descriptor; those get EOPNOTSUPP rather than a leaked descriptor.
  - Access requires `p_candebug()`, as Linux requires ptrace access; otherwise EACCES.
- **`/proc/1` in a jail is the jail's oldest process.** A new optional pseudofs hook, `pi_pidmap`, rewrites the pid
  of a process-directory lookup. linprocfs maps 1 to the oldest live process whose credentials are in the caller's
  own jail. Each container is its own jail, so that is its entrypoint. These lookups aren't name-cached, because
  the answer depends on the caller. On the host `/proc/1` is still init.
- **On main**, `fget_remote()` also returns the descriptor's Capsicum rights, and the new descriptor keeps them. On
  15.1 it gets full rights; that needs debug access to the target, which grants more than this already.

## Verified (Alpine pod, `kubectl exec`)

| Check | Before (bsdk8s12) | After (bsdk8s13) |
|---|---|---|
| `/proc/1/cmdline` | No such file | `sleep 3600` (the entrypoint) |
| `ls -l /proc/1/fd` | No such file | `0 -> /dev/null`, `1 -> pipe:[5280]`, `2 -> pipe:[5282]`, modes `lr-x`/`lrwx` |
| `readlink /proc/self/fd/7` (open file) | fails | `/etc/hostname` |
| `echo … > /proc/1/fd/1`, `>> /proc/1/fd/2` | No such file | both lines appear in `kubectl logs` |
| non-root user → root's `/proc/1/fd` | - | Permission denied |
| `/proc/1/fd/99` | - | No such file |
| 2000 opens of `/proc/1/fd/2` | - | no leaked descriptors |
| `exec /proc/self/fd/8` | - | refused (EACCES), no leaked descriptor |
| native FreeBSD procfs | works | works |
| delete the Envoy pod | 361 s, `FailedPreStopHook` panic | **12 s**; shutdown-manager logs "drain sequence completed" |

`cat /proc/1/fd/1` blocks reading the container's stdout pipe. FreeBSD pipes are bidirectional, so the pipe opens for
reading. On Linux that open also reads from the pipe; don't do it.

## Remaining differences (honest)

- **Envoy's own HTTP preStop** (`/shutdown/ready` on the shutdown-manager) still logs `EOF`, because the
  shutdown-manager exits as soon as the drain completes. The drain itself ran. I haven't compared this against
  Linux.
- **Zero-downtime deletes need two replicas.** With one Envoy replica, requests failed from about 3 s after the delete
  (the draining Envoy fails its readiness probe) until the new pod became Ready at 10 s (its probes run every 5 s and
  10 s). That's upstream behavior: before this fix there was no gap only because Envoy was never drained. Envoy
  Gateway's docs recommend `replicas >= 2`.
- **No PID namespace.** `getpid()` in the entrypoint is still the host PID, `/proc` lists host PIDs, and `1` isn't
  listed. Only lookups of `/proc/1` are mapped.
- **`stat()` of an fd entry** describes the entry (a link), not the target file. Linux follows the magic link.
- **`opendir()` of `/proc/<pid>/fd/N`** for a directory descriptor fails with ENOTDIR; `/dev/fd` behaves the same.
