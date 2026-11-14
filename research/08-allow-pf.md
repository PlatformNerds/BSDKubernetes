# 08: `allow.pf`, kube-proxy as a normal pod (kernel item K14)

*2026-11-14. Implemented, booted and verified on FreeBSD 15.1-RELEASE-p6 arm64.*

## Problem

A Kubernetes `hostNetwork` pod's jails share the host network stack (`vnet=inherit`). FreeBSD grants
`PRIV_NETINET_PF` inside a jail **only to VNET jails** (`prison_priv_check()` in `kern_jail.c`), and pf's netlink
commands require it (`.cmd_priv = PRIV_NETINET_PF`). So kube-proxy (pf mode), which kubeadm deploys as a privileged
host-network DaemonSet, failed every rule load:

```
pfctl: DIOCBEGINADDRS: Operation not permitted
```

The workaround was to run kube-proxy as a host rc service (docs/MAC-DEV-VM.md §4 #4). On Linux the same pod gets
`CAP_NET_ADMIN` and manages iptables/nftables.

Side finding: pf's legacy `/dev/pf` ioctl path has **no** privilege check, so the same jail could still create anchors
through ioctls. pf's two interfaces enforce privilege inconsistently.

## Change (three layers)

| Layer | Change | Where |
|---|---|---|
| Kernel | New jail parameter **`allow.pf`**, modelled on `allow.routing`: a non-VNET jail with `allow.pf` gets `PRIV_NETINET_PF`. VNET jails unchanged. `jail(8)` documented. ~28 lines. | `PlatformNerds/freebsd-src` `bsdk8s/allow-pf` (main), `bsdk8s/allow-pf-15.1` (backport, tested) |
| ocijail | accept the `org.freebsd.jail.allow.pf` annotation | `PlatformNerds/ocijail` `bsdk8s-main` |
| CRI-O | for **privileged + hostNetwork** pods, set `allow.pf` on the sandbox jail **and** on privileged containers (a child jail cannot exceed its parent). Only when the kernel advertises `security.jail.param.allow.pf`, since `jail_set` rejects unknown parameters. | `PlatformNerds/cri-o` `bsdk8s-1.37` (`e543b1881`) |

The rule "privileged + hostNetwork ⇒ may manage host pf" mirrors Linux, where the same pod gets `CAP_NET_ADMIN`.

## Verification

Kernel (plain jails, the exact rule load that failed for kube-proxy):

| Jail | Result |
|---|---|
| non-VNET, no `allow.pf` | `DIOCBEGINADDRS: Operation not permitted`, 0 rules (unchanged, still refused) |
| non-VNET, `allow.pf` | rule loaded |
| VNET | rule loaded (unchanged) |
| child with `allow.pf` under a parent without it | `jail_set: Operation not permitted` |

Kubernetes: the stock kubeadm kube-proxy DaemonSet (privileged, hostNetwork, unmodified) runs as a pod.
- Both jails show `allow.pf=true`.
- kube-proxy logs 0 errors and its pf rules are present.
- Services, DNS and ClientIP session affinity (8/8 to one backend) work.
- The host kube-proxy service is disabled.

## How to build and boot it (15.1, pkgbase systems)

`make installkernel` refuses to overwrite a kernel installed from packages. Install it under another name and
boot it once first:

```sh
make -j8 buildkernel KERNCONF=GENERIC                        # ~2 min on 8 vCPU arm64
make installkernel KERNCONF=GENERIC INSTKERNNAME=kernel.allowpf
nextboot -k kernel.allowpf && shutdown -r now               # one-shot; falls back on the next reboot
# when happy: echo 'kernel="kernel.allowpf"' >> /boot/loader.conf   (done in the dev VM)
```

## Upstream

A small, self-contained change with a clear use case and a clear precedent (`allow.routing`). The FreeBSD submission
should also mention the ioctl/netlink privilege inconsistency. Not submitted yet.

## Notes

- A one-off `curl` segfault (signal 11) was seen once in the agnhost image while kube-proxy pods were being replaced.
  It was not reproducible (100/100 afterwards). Watch for recurrence.
- Cross-compiled (`CGO_ENABLED=0`) CRI-O **compiles but does not work** on FreeBSD: containers/storage falls back to
  `mounter_unsupported.go` and panics "Not implemented" on the first mount. Build CRI-O natively with cgo
  (`gmake bin/crio`), as the demo does.
