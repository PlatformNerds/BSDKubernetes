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
- `kern.elf64.fallback_brand=3`: musl binaries carry no Linux ABI note. FreeBSD binaries are always branded, so
  they're unaffected.
- The CRI-O `linux` handler, the RuntimeClass `linux` (scheduling onto `bsdk8s.io/linux-abi=true`), and the node
  label.

Both nodes boot `kernel.bsdk8s3` = 15.1 + allow.pf + pf sticky + rctl hierarchy + pf self-hairpin + EPOLLEXCLUSIVE.

## Known gaps (honest)

- **Scheduling.** Upstream manifests pin `nodeSelector: kubernetes.io/os: linux`, which FreeBSD nodes never match.
  metrics-server needed that one field patched to `runtimeClassName: linux`. A mutating webhook (plan:
  `bsd-placement`) should do it automatically. This is the biggest blocker for "install any chart".
- **Linuxulator coverage:**
  - `io_setup()` (Linux AIO) returns ENOSYS. nginx logs `[emerg]` and continues.
  - `/proc/net/tcp` is missing, so `netstat` inside Linux containers is empty.
  - metrics-server's certificate-file watch fails: `fsnotify: read: resource temporarily unavailable` (inotify
    under epoll). It's non-fatal, but cert hot-reload is broken.
  - research/04 also lists: unshare/setns, io_uring, signalfd, pidfd, bpf, keyctl, landlock.
- **Images with FreeBSD twins.** Our FreeBSD test images use upstream names (e.g. `agnhost:2.63.0`). A Linux pod
  asking for the same tag gets the local FreeBSD image.
- **Not tested yet:** controllers with admission webhooks (cert-manager), images that need `unshare`, or Java.
- **bhyve/Kata** for images the Linuxulator can't run still needs bare metal: no nested virtualization in the Mac
  VM.
