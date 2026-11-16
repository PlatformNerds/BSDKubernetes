# 13: ingress-nginx unmodified: capabilities, low ports, digest-pinned images (2026-11-16)

**Result [verified]:** ingress-nginx `controller-v1.12.0` installs from its unmodified bare-metal release manifest
(sha256 `765ed0a8…`):
- **Admission-cert Jobs:** both run to completion.
- **Controller:** Running and Ready. Its nginx runs as uid 101 and serves ports 80/443. Leader election works.
- **Traffic:** an Ingress for `demo.bsdk8s.test` routes to Linux nginx backends. Via the controller's NodePort on
  both nodes, 6/6 requests return 200, and an unknown host gets 404.
- **Admission webhook:** ingress-nginx's own `validate.nginx.ingress.kubernetes.io` rejects an Ingress with a
  disallowed snippet annotation.

bsd-placement placed all its pods: they pin `kubernetes.io/os: linux`. Three things had to be fixed first, each
the next layer of the same failure.

## 1. Digest-pinned images under the Linux RuntimeClass (CRI-O)

The manifest references `controller:v1.12.0@sha256:<index digest>`. The pull already chose the linux/arm64
instance (research/10). Resolving the *name* afterwards in local storage, for ImageStatus and CreateContainer,
picks the index instance for the host OS, and failed:

    choosing image instance: no image found in image index for architecture "arm64", variant "", OS "freebsd"

Tag-only references never hit this. CRI-O now resolves names through `imageStatusByName` (all 4 call sites). On
exactly that error, it retries with the platform's fallback OSes: `linux` on FreeBSD, none elsewhere.

## 2. Capabilities (CRI-O)

`securityContext.capabilities.add: [NET_BIND_SERVICE]` failed in two layers:
1. `unknown capability "CAP_NET_BIND_SERVICE" to add`: the FreeBSD capability list was empty.
2. `CAP_NET_BIND_SERVICE is not supported on the current host`: the runtime-tools generator checks against the
   running kernel.

FreeBSD has no capabilities, and jails don't enforce them. On FreeBSD, CRI-O now validates the names, so typos
are still errors, and puts none in the spec. **Honest consequence:** `capabilities.add/drop` is accepted and has
**no effect**:
- root in a jail keeps root's jail-restricted privileges;
- other users have none;
- `drop: [ALL]` doesn't take anything away from root.

## 3. Binding ports below 1024 as non-root (ocijail + kubelet + CRI-O config)

On Linux, the controller's uid 101 binds :80 via `NET_BIND_SERVICE`. The FreeBSD equivalent is the reserved port
range, `net.inet.ip.portrange.reservedhigh`. It is `CTLFLAG_VNET`, and the kernel lets a jail that owns its vnet
write it. Every pod has its own vnet jail, so this is per pod:

- **ocijail** applies the sandbox spec's `linux.sysctl` when it creates a new vnet jail. It forks, calls
  `jail_attach` and sets each value with `sysctlbyname`:
  - `net.ipv4.ip_unprivileged_port_start=N` → `reservedhigh=max(N-1,0)`
  - `net.ipv4.ip_local_port_range` → `portrange.first/last`
  - anything else is logged and ignored.
  The translation has unit tests (`sysctl_test`).
- **Node config:** CRI-O `default_sysctls = ["net.ipv4.ip_unprivileged_port_start=0"]`. This is the containerd 2.x
  default (`enable_unprivileged_ports`), and the demo script sets it.
- **kubelet:** `SafeSysctlAllowlist` returned nothing on non-Linux, so every pod sysctl got `SysctlForbidden`. On
  FreeBSD it now allows exactly the two sysctls above. The other Linux "safe" sysctls would be silently ignored,
  so they stay forbidden.

A/B with uid 1000 running `nc -l -p 80` in a Linux busybox pod:

| Pod sysctl | Result |
|---|---|
| default (`ip_unprivileged_port_start=0`) | bound :80 |
| `securityContext.sysctls: ip_unprivileged_port_start=1024` | `bind: Permission denied` |

The host keeps `reservedhigh=1023`.

## Limits

- Capabilities are accepted, not enforced (above). A workload relying on `drop` to restrict root gets no such
  restriction. Run non-root (`runAsNonRoot`) for real reduction.
- In `/proc` (linprocfs), `ip_unprivileged_port_start` doesn't exist, so tools that read it see nothing.
- The digest fallback reports the local Linux instance even to a pod of the FreeBSD runtime. It then runs under
  the default handler, still via the Linuxulator, but without the Linux mounts.
