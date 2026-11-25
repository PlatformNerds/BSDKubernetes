# 27. Stock Linux control plane, FreeBSD worker

*2026-11-25. Debian 13 arm64 control plane (VM `bsdk8s-linux-10`, 192.168.100.10) and FreeBSD 15.1 arm64 worker
`bsdk8s-3` (192.168.100.3, kernel `bsdk8s29`), both on the vde cluster switch.*

This was the plan's original Phase 1 (docs/PLAN.md §8, §11 item 5) and had never been done: every cluster so far had
a FreeBSD control plane. The control plane here is completely stock. It uses containerd 2.4.2, runc 1.5.3 and CNI
plugins 1.10.0 from their upstream releases, and kubeadm, kubelet and kubectl v1.36.1 from pkgs.k8s.io. The only
cluster-side additions are what any CNI install would add: pod routes, a ServiceAccount, and a second kube-proxy
DaemonSet. The FreeBSD worker joined with the documented path (kubernetes-demo `build-cluster.sh` with `JOIN_*`),
built from the public forks.

## Setup

- `scripts/macos-linux-vm.sh create`: Debian 13 genericcloud arm64 under QEMU/HVF, like `macos-freebsd-vm.sh`.
  It has a user-mode NAT NIC (internet, ssh on 2231) and a NIC on the vde switch (`192.168.100.10`,
  `fd00:100::a`), configured by cloud-init.
- `scripts/linux-control-plane.sh` (`NODE_IP=192.168.100.10`) installs the runtime and kubeadm, runs `kubeadm init`,
  then sets up:
  - **Pod network on the Linux node:** `bridge` + `host-local` on the node's podCIDR, without the plugin's
    `ipMasq` (it would SNAT pod-to-pod traffic to the other nodes). One iptables rule masquerades traffic that
    leaves 10.244.0.0/16.
  - **linux-hostgw** (systemd): bsd-hostgw's loop with `ip route replace`. Every node routes the other nodes'
    podCIDRs via their InternalIP, so the two OSes need nothing in common beyond the L2 network.
  - **`kube-system/bsd-hostgw` ServiceAccount** (nodes get/list/watch). The FreeBSD workers get its token as
    `HOSTGW_TOKEN`.
  - **`kube-proxy-freebsd`:** kubeadm's kube-proxy DaemonSet already selects `kubernetes.io/os=linux`. A copy
    with the FreeBSD image (`ghcr.io/platformnerds/kube-proxy:v1.36.1`) runs on `kubernetes.io/os=freebsd`. They
    share kubeadm's ConfigMap: `mode: ""` means iptables on Linux and pf on FreeBSD.
  - It prints the FreeBSD join command.
- CoreDNS is the stock image (`registry.k8s.io/coredns`) on the Linux node. Its Deployment selects
  `kubernetes.io/os=linux`.

## What joining needed

**The FreeBSD kubelet didn't start on join (fixed, kubernetes 0066).** `kubeadm join` downloads the cluster's
KubeletConfiguration, which the Linux control plane generated with Linux paths:
`clientCAFile: /etc/kubernetes/pki/ca.crt`, `staticPodPath: /etc/kubernetes/manifests`, and
`resolvConf: /run/systemd/resolve/resolv.conf`. On FreeBSD, kubeadm keeps its files in `/usr/local/etc/kubernetes`.
The kubelet failed with `open /etc/kubernetes/pki/ca.crt: no such file or directory`, and the join timed out after
4 minutes. Upstream has the same problem for Windows nodes and solves it in kubeadm
(`componentconfigs/kubelet_windows.go`, `Mutate`: drive letters, no resolver). 0066 does the FreeBSD equivalent: it
rewrites `/etc/kubernetes/...` to `constants.KubernetesDir` and drops systemd-resolved's resolver path. A FreeBSD
control plane's config passes through unchanged. Unit test `TestMutatePlatformPathsFreeBSD`, run on node 3.
build-cluster's join now also patches `resolvConf` to the file `kubeadm init` uses (demo 29869df; kubeadm
`patches.directory`).

**The worker's hostPort for the node's own connections (fixed in build-cluster).** The conformance run's one new
failure was `HostPort ... different hostIP and protocol [LinuxOnly]`. From node 3, `127.0.0.1:<hostPort>` (with
source the node IP) timed out. node 3 had the packaged portmap plugin. Nodes 1 and 2 had the fork's (cni-plugins
0001, research/20), but it had been installed by hand, and `build-cluster.sh` never built it. Demo 3875697/80f1141
now builds `plugins/freebsd/meta/portmap` from `PlatformNerds/cni-plugins` `bsdk8s/portmap-host` (vendored) and keeps
the package's binary as `portmap.pkg`. After that, the hostPort answered on 127.0.0.1 and the node IP from node 3,
and from the Linux node, and `--focus=HostPort` passed 2/2. A connection from 127.0.0.1 to 127.0.0.1 still doesn't
translate (the known limit in research/20).

**The fork's kubeadm didn't build for Linux, Windows or macOS (fixed, kubernetes 0067).** Linux uses stock kubeadm
here, so this didn't block the cluster, but the kubeadm series has to build everywhere before it goes upstream.
dfr's "WIP: kubeadm on FreeBSD" commit moved the default directories into per-OS files:
`v1beta4/defaults_common.go` declared `package v1beta3`, both API versions declared `DefaultCertificatesDir` twice,
and `KubernetesDir` existed only for Linux and FreeBSD. The same commit also commented out CoreDNS's capabilities
(`drop: ALL`, `add: NET_BIND_SERVICE`) on every OS, and took the addons' `kubernetes.io/os` from `runtime.GOOS`.
Those two changes would have reached Linux control planes as soon as the code compiled. 0067 keeps the
capabilities everywhere except FreeBSD, selects `linux` everywhere except FreeBSD (as upstream does), and fixes the
tests that broke with the templates or hardcoded Linux paths. kubeadm now builds for linux/arm64, linux/amd64,
windows, darwin and freebsd. On Linux, all 55 kubeadm test packages pass. `test/cmd` drives a kubeadm binary
(`KUBEADM_PATH`). On the control-plane VM, its `init` tests failed for stock v1.36.1 kubeadm too: a dry-run `init`
there reuses the existing `/etc/kubernetes/pki/ca.crt` and can't read its root-only key. On a clean Debian 13 VM
(`NODE=11 scripts/macos-linux-vm.sh create`, no Kubernetes installed), both the fork's kubeadm (`85b1bbfb6bd`) and
stock v1.36.1 (dl.k8s.io, sha256-checked) pass **63/63**, all 24 `init` tests included. A dry-run
`kubeadm init` with both binaries also gives the same result: the 1346-line output differs only in the bootstrap
token's expiration (one second apart), and the 34 written files are the same, with the 12 that aren't
certificates (static pod manifests, kubelet config, kubeconfigs) identical apart from certificate data. The changed packages also pass on FreeBSD and macOS. On Linux, the rendered CoreDNS and kube-proxy
manifests equal upstream's. The one remaining Linux difference is a second container removal in `kubeadm reset`,
before unmounting (FreeBSD needs it); on Linux it finds nothing.

## Cross-OS checks (by hand)

Two agnhost pods, one per node. The Linux node pulled the upstream image, and the FreeBSD node used the FreeBSD
build under the same tag.

| Check | Result |
|---|---|
| Pod to pod, both directions | OK |
| DNS: FreeBSD pod → CoreDNS on Linux (ClusterIP 10.96.0.10) | resolves |
| ClusterIP Service backed by the other OS's pod, from each side | OK (Linux iptables and FreeBSD pf proxies) |
| `kubernetes.default` from the FreeBSD pod | reaches the apiserver |
| NodePort to the FreeBSD pod via the Linux node and via the FreeBSD node | OK |

## Conformance

Upstream `e2e.test` and `ginkgo` v1.36.1 (`kubernetes-test-linux-arm64`, unmodified, not the fork's e2e) ran on the
Linux control plane with `--procs=4 --focus='\[Conformance\]' --provider=skeleton`. The control-plane taint was
removed, so test pods landed on both nodes. The run took 43 minutes.

**438/446.**

| Failure | |
|---|---|
| 5 × `Pod InPlace Resize Container` | the known ones (research/07): FreeBSD has no `cpu.weight`, and busybox runs under the upstream tag |
| `Security Context ... privileged ... unprivileged when false [LinuxOnly]` | known, research/07 |
| `ImageVolume ... pull policy of Always [LinuxOnly]` | known, research/07 |
| `HostPort ... different hostIP and protocol [LinuxOnly]` | worker built without the fork's portmap. Fixed above |

So, apart from the fixed HostPort failure, the failures are the FreeBSD-cluster set (run 19: 437/446). These
counts can't be compared exactly: with two OSes, a test passes on whichever node gets its pod. `Sysctls` and
one in-place resize test, which fail on FreeBSD, passed here and probably landed on the Linux node.
A run that pins test pods to the FreeBSD worker (taint the control plane again) would measure the worker
alone, but then the two-node tests would have only one schedulable node.

## Cleanup notes

- `kubeadm reset` on a FreeBSD node leaves kube-proxy's pf anchors (`cni-rdr/kube-svc-*`) behind, as it leaves
  iptables on Linux. node 3 still had its IPv6-only cluster's Service rules. They were flushed by hand
  (`pfctl -a <anchor> -F all`). Its `/etc/pf.conf` still has that cluster's NAT66 line for `fd00:20:244::/56`,
  which is harmless.
- node 3 had been the IPv6-only single-node cluster (research/26). That cluster is gone; `build-cluster.sh`
  with `IPV6_ONLY=yes` rebuilds it.
