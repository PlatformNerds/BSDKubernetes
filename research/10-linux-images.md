# 10: Linux container images on FreeBSD nodes, via the Linuxulator (2026-11-16)

**Result:** unmodified upstream Linux images run as Kubernetes pods on FreeBSD nodes, in jails, under the Linux ABI
(`linux64.ko`). Their ELF binaries run directly on the FreeBSD kernel: no VM, no Linux kernel. Pods opt in with
`runtimeClassName: linux`.

| Image (unmodified) | Result |
|---|---|
| `docker.io/library/nginx:1.27-alpine` (musl, 8 workers) | serves through NodePort (both node IPs), ClusterIP and Service DNS from a FreeBSD pod |
| `registry.k8s.io/metrics-server/metrics-server:v0.7.2` (Go) | APIService `v1beta1.metrics.k8s.io` Available; **`kubectl top nodes/pods` works** with metrics from the FreeBSD kubelets |
| `debian:12-slim` (glibc), `alpine:3.20` (podman) | shells, `uname` = Linux 5.15, `/proc` |

## How

**CRI-O** (`PlatformNerds/cri-o` `bsdk8s/freebsd-linux-images`; patches `cri-o/0008-0009`):
- **Runtime handler.** A handler with `default_annotations = { "io.bsdk8s.image-os" = "linux" }` pulls its pods'
  images for Linux. The CRI only passes the handler with pulls behind the alpha `RuntimeClassInImageCriApi` gate,
  but the kubelet creates the sandbox first, so CRI-O finds it by pod UID.
- **Mounts.** Containers of Linux images get `linprocfs` at `/proc`, `linsysfs` at `/sys` and a tmpfs at
  `/dev/shm`, as podman gives them. `/dev/shm` must also be unhidden in the jail's devfs: without that,
  `create_directory: Operation not supported`.

**Kernel: `EPOLLEXCLUSIVE`** (`PlatformNerds/freebsd-src` `bsdk8s/linux-epollexclusive`, `-15.1`; patch
`freebsd-src/0007`):
- **The bug.** `epoll_ctl()` rejected the flag ("epoll_ctl unsupported flags: 0x10000001"), so every nginx worker
  exited at startup. That's the gap research/04 predicted.
- **The fix.** It's accepted with `EPOLL_CTL_ADD` and dropped: every waiter is woken, which is correct but slower.
  Like Linux, it's rejected with `EPOLL_CTL_MOD` or `EPOLLONESHOT`.
- **A bug in my first version.** The macro, written `1u<<28` in the header's unparenthesized style, turned
  `~LINUX_EPOLLEXCLUSIVE` into `(~1u)<<28`. That cleared `EPOLLIN` too, so the listening socket was registered
  *disabled*: `procstat kqueue` showed status `D`, and requests timed out. Found with `ktrace` (no worker ever
  called `accept4`) plus `procstat kqueue`.

**Node** (demo `build-cluster.sh`, `LINUX_IMAGES=yes` by default):
- `kld_list` gets `linux64 linprocfs linsysfs fdescfs`.
- `kern.elf64.fallback_brand=3`: static Linux binaries with no ABI note (static Go, static musl) can't be branded any
  other way. FreeBSD binaries are always branded, so they're unaffected. Re-tested 2026-11-17 with the arm64 brand
  fixes (research/16): dynamic glibc and musl binaries no longer need it; a static Go binary and `busybox:1.36-musl`
  still fail without it (`No such file or directory` / `Exec format error`).
- The CRI-O `linux` handler, the RuntimeClass `linux` (scheduling onto `bsdk8s.io/linux-abi=true`), and the node
  label.

Both nodes boot `kernel.bsdk8s3` = 15.1 + allow.pf + pf sticky + rctl hierarchy + pf self-hairpin + EPOLLEXCLUSIVE.

## Known gaps (honest)

- **Scheduling.** Upstream manifests pin `nodeSelector: kubernetes.io/os: linux`, which FreeBSD nodes never match.
  metrics-server needed that one field patched to `runtimeClassName: linux`. **Solved by the `bsd-placement`
  webhook (research/11):** cert-manager now installs unmodified.
- **Linuxulator coverage:**
  - `io_setup()` (Linux AIO) returns ENOSYS. nginx logs `[emerg]` and continues.
  - `/proc/net/tcp` is missing, so `netstat` inside Linux containers is empty.
  - ~~metrics-server's certificate-file watch fails: `fsnotify: read: resource temporarily unavailable`.~~ **Fixed
    (kernel `bsdk8s/linux-epoll-filters`, patch freebsd-src/0008).** Go's netpoller registers every fd for
    `EPOLLIN|EPOLLOUT|EPOLLRDHUP|EPOLLET`. The inotify kqfilter only supports `EVFILT_READ`, so the Linuxulator's
    `epoll_ctl` failed with EINVAL. Worse, it left the read filter registered, so a retry got EEXIST. Go then
    treated the fd as unpollable, and reads returned EAGAIN. timerfd and kqueue fds have the same problem.
    Linux accepts any mask and never reports events the file can't produce. `epoll_ctl` now registers each filter
    separately and skips filters the file rejects. A probe (inotify/timerfd ADD/EEXIST/MOD/DEL, no spurious
    events, Go `os.File.Read` via the netpoller) passes, and metrics-server shows 0 fsnotify errors.
  - `unsupported fcntl cmd 36` = Linux `F_OFD_GETLK`. A DTrace ustack plus `go tool addr2line` traced it to
    `go.etcd.io/etcd/client/pkg/v3/fileutil.init`. That init function is a feature probe that falls back to
    `F_SETLK` on EINVAL, and every client-go binary links it. It's harmless log noise, so OFD locks aren't
    implemented.
  - research/04 also lists: unshare/setns, io_uring, signalfd, pidfd, bpf, keyctl, landlock.
  - ~~Abstract Unix sockets.~~ **Supported (kernel `bsdk8s12`, patch freebsd-src/0017, research/14).**
  - ~~No `/proc/1`, no `/proc/<pid>/fd` for other processes.~~ **Supported (kernel `bsdk8s13`, patch freebsd-src/0018, research/18).**
  - ~~`membarrier(2)` (Node.js, .NET JITs) and `prctl` `PR_SET_VMA`, `PR_GET_NO_NEW_PRIVS`, `PR_GET_CHILD_SUBREAPER`,
    `PR_{G,S}ET_THP_DISABLE` missing on 15.1.~~ **Backported from main (kernel `bsdk8s15`, patches freebsd-src/0021-0028,
    branch `bsdk8s/linux-prctl-membarrier-15.1`):** seven upstream commits with `(cherry picked from commit …)` trailers,
    plus a syscall-table regen for the three-argument `membarrier`. Since then, Go/Envoy's `PR_SET_VMA` and Node's
    `membarrier` log nothing; the only Linuxulator message left on the nodes is `io_uring_setup not implemented`,
    which is Linux's own ENOSYS when io_uring is disabled, so libuv falls back.
- **Images with FreeBSD twins.** Our FreeBSD test images use upstream names (e.g. `agnhost:2.63.0`). A Linux pod
  asking for the same tag gets the local FreeBSD image. CRI-O now refuses this with a clear error (research/17).
- ~~**Commands found only on the image's `PATH`** failed: `command: ["java", "-version"]` on `eclipse-temurin`
  gave `'java' not found in $PATH`.~~ **Fixed (ocijail `e3cd4ae`, patch ocijail/0014).** CRI-O builds the spec with
  runtime-tools, whose env cache is keyed by the whole `KEY=value` string, so the default `PATH` and `TERM` are never
  deduplicated: the spec carries `PATH` twice, CRI-O's default first and the image's second. runc applies
  `process.env` with `setenv` in order, so on Linux the last one wins and the duplicate is invisible. ocijail used
  the first. It now merges duplicate keys the way runc does. Shell-wrapped commands were unaffected, because the
  shell also keeps the last value.
- **Not tested yet:** images that need `unshare`. Java works (research/15). cert-manager (admission webhooks):
  works, see research/11.
- **bhyve/Kata** for images the Linuxulator can't run still needs bare metal: no nested virtualization in the Mac
  VM.
