# FreeBSD dev VM on Apple Silicon

A FreeBSD 15.1 **arm64** VM under QEMU + Apple's hypervisor (HVF) runs near-native and is enough for almost all of
Phase 1: jails, VNET, pf, ZFS, RACCT/rctl, kubelet, CRI-O, ocijail, CNI, kube-proxy (pf) and custom kernels.

**Not possible here:** bhyve (needs nested virtualization, which QEMU/HVF doesn't provide).
**Not possible either:** Apple `container`. It boots a Linux kernel + Linux init per container, so FreeBSD images can't run.
It is, however, a good reference design for our `kata-bhyve` microVM runtime.

## 1. Create the VM

```sh
scripts/macos-freebsd-vm.sh create     # brew qemu, FreeBSD-15.1 BASIC-CLOUDINIT-zfs image (sha256-checked), cloud-init seed
scripts/macos-freebsd-vm.sh ssh        # user bsdk8s, passwordless sudo, key in ~/bsdk8s/vm/id_ed25519
scripts/macos-freebsd-vm.sh stop|start|console
```
Defaults: 8 vCPU, 12 GB RAM, 80 GB disk, ssh on 127.0.0.1:2222, API server forwarded to 127.0.0.1:6443.
cloud-init adds `kern.racct.enable=1` to loader.conf, so **reboot once** after create (`ssh 'sudo shutdown -r now'`).

> Gotcha found while building this: FreeBSD's nuageinit crashes ("attempt to concatenate a table value") if a
> `runcmd` item contains an unquoted `": "`, because YAML turns it into a mapping. It then silently skips the rest
> of the run stage. The script quotes every item.

## 2. arm64 control-plane images

dfr's published images (`ghcr.io/dfr/*`, `quay.io/dougrabson/pause`) are **FreeBSD/amd64 only**. For arm64:

```sh
# on the Mac: cross-compile (CGO_ENABLED=0) from the fork branch + upstream etcd v3.6.5 / coredns v1.14.2
GOOS=freebsd GOARCH=arm64 ...   # see scripts/build-freebsd-k8s.sh
# in the VM (as root): build images on the official FreeBSD "static" OCI base (sha256-checked)
sudo scripts/build-images-freebsd.sh <bindir-with-binaries-and-pause.c> ghcr.io/platformnerds
```
Images land in local containers storage (shared with CRI-O), so kubeadm finds them without a registry.

## 3. Bring up the cluster

```sh
# in the VM, with sources from the PlatformNerds forks (public: no credentials needed)
cd kubernetes-demo   # branch bsdk8s
BSDK8S_GIT_BASE=<git base> ./build-cluster.sh vtnet0
```
On arm64 the script uses `IMAGE_REPO=ghcr.io/platformnerds`, sets CRI-O's `pause_image` via a drop-in, and installs the
arm64 crictl.

## 4. First end-to-end run: results (2026-11-14, FreeBSD 15.1-RELEASE-p6 arm64, 8 vCPU / 12 GB)

**Works:**
- `kubeadm init` with the control plane as jails: etcd, kube-apiserver, kube-controller-manager and kube-scheduler run as ocijail containers on ZFS clones.
- The node is `Ready`, reported as `cri-o://1.37.0` on arm64.
- Pod networking: VNET jail pods on bridge + epair get `10.244.0.x`, and pod→pod HTTP works.
- **Services through pf:** ClusterIP round-robins across 2 replicas (`rdr … round-robin` in `cni-rdr/kube-svc-*` anchors).
- CoreDNS: `web.default.svc.cluster.local` resolves, and `http://web/` works from a pod.
- `kubectl exec` / `logs`.

**Bugs found and fixed (in `PlatformNerds/kubernetes-demo` `bsdk8s` and `scripts/build-images-freebsd.sh`):**

| # | Symptom | Root cause | Fix |
|---|---|---|---|
| 1 | Node `NotReady`; kubeadm "unable to restart the kubelet service" | rc script tracked the kubelet child. After SIGTERM it's a zombie `(kubelet)`: `kill -0` says gone but rc.subr's `ps` match still finds it, so `restart` → "already running?" | rc script tracks the daemon(8) supervisor (`-P`), `procname=/usr/sbin/daemon` |
| 2 | Sandboxes fail: `/pause: No such file or directory` | CRI-O's `pause_command` is `/pause`; our image had it in `/usr/local/bin` | install pause at `/pause` |
| 3 | pfctl in kube-proxy pod: `abort trap` | Static base image has no runtime linker (`/libexec/ld-elf.so.1`) or `/etc/protocols`, `/etc/services`, `/etc/netconfig` | copy them into the kube-proxy image |
| 4 | **No Service works**: `pfctl: DIOCBEGINADDRS: Operation not permitted` | The kube-proxy DaemonSet pod is a hostNetwork jail (`vnet=inherit`). FreeBSD grants `PRIV_NETINET_PF` only to VNET jails (`kern_jail.c` `prison_priv_check`), and pf's **netlink** commands require it (`.cmd_priv = PRIV_NETINET_PF`). The legacy `/dev/pf` ioctl path has no such check, so anchors got created but address pools didn't: an inconsistency in pf's privilege model. | Run kube-proxy as a **host rc service** (`files/kube-proxy`). **Since fixed** with kernel K14 `allow.pf` ([research/08](../research/08-allow-pf.md)): kube-proxy runs as the normal DaemonSet; the demo falls back to the host service only on kernels without `allow.pf`. |
| 5 | `kubectl exec/logs`: `tls: internal error` | `serverTLSBootstrap: true` + kubelet restart files new serving CSRs that nobody approves | approve pending CSRs at the end of the script |
| 6 | cloud-init stage skipped | nuageinit crashes on unquoted `": "` in `runcmd` | quote items (see §1) |

**Confirmed gaps (as predicted in research/02 and research/03):**
- ~~**Resource limits are silently dropped.**~~ **Fixed 2026-11-14:** limits now become rctl rules (see [research/06](../research/06-resource-limits.md)).
- ~~**Per-container accounting is missing.**~~ This was a measurement error: rctl's `jail:` subject is the jail *name*, and `rctl -u jail:<jid>` silently reports an empty entry. Accounting by name works and is hierarchical.
- **node→ClusterIP times out.** pf `rdr` doesn't apply to host-originated traffic on 15.x (works on 16-CURRENT).
- containers/common logs `getCPUInfo for OS freebsd not implemented` on every buildah/podman call: harmless, easy upstream fix.

## Clock skew between nodes (found in conformance run 5)

The VMs ran no NTP. One guest's clock fell 35 minutes behind (it stopped advancing while the Mac slept), and the
other was right after a reboot. With 35 minutes of skew, conformance run 5 lost tests that passed before:
- **ServiceAccount OIDC discovery:** the token looked expired on the other node.
- **Job:** pods never completed.
- **Namespace deletion and Deployment rollover:** timed out.

The VM script now enables `ntpd` with `ntpd_sync_on_start`. On existing VMs:
`sudo sysrc ntpd_enable=YES ntpd_sync_on_start=YES && sudo service ntpd start`. Check with `date -u` on every
node before an e2e run.

**That was not enough (run 8).** ntpd exits when the offset exceeds its panic threshold (1000 s): `Clock offset
exceeds panic threshold. Set system clock by hand.` That is exactly what happens after the Mac sleeps for more than
~17 minutes. On battery it idle-sleeps after a few minutes and keeps doing short maintenance sleeps. Afterwards both
guests were 35–43 minutes behind. Node 1's ntpd had exited; node 2's was "running free". Run 8's only new failure,
`Deployment should support rollover`, came from that. The controller-manager judged `minReadySeconds` against a
Ready time the other node's kubelet stamped minutes in the future. With clocks synced, the test passed 3/3.

The VM script now also writes `tinker panic 0 stepout 30` (never exit; step large offsets after 30 s) and
`maxpoll 6` on the pool lines (poll at least every 64 s, so a wake is noticed quickly) into `/etc/ntp.conf`. On
existing VMs:

```sh
sudo sed -i '' '/^pool /s/iburst$/iburst maxpoll 6/' /etc/ntp.conf
echo 'tinker panic 0 stepout 30' | cat - /etc/ntp.conf | sudo tee /etc/ntp.conf.new >/dev/null && sudo mv /etc/ntp.conf.new /etc/ntp.conf
sudo service ntpd restart
```

For long runs (conformance), also keep the Mac awake: `caffeinate -i -s -w <pid of the run>`, or plug in
power and disable sleep.

## QEMU user networking fakes connectivity

slirp (`vtnet0`) completes TCP connects to *any* address itself and relays DNS out to the Mac. So `nc -z` to a
ClusterIP "works" even when nothing redirects it (research/09). Verify Service paths with real responses (an HTTP
status code, a resolved name), or with a capture on the interface.

## Debugging a hung VM (monitor + serial console)

`ssh` can fail while the guest still runs: in conformance run 6's first attempt, sshd stopped sending its banner and
the kubelet stopped heartbeating, while the API server kept answering. QEMU user networking accepts the TCP
connection itself, so an open port proves nothing. The VM script now gives each VM two sockets in its VM dir:

- `monitor.sock`, the QEMU monitor: `scripts/macos-freebsd-vm.sh monitor 'info status'`. To capture a hung guest's
  memory before resetting it: `monitor 'dump-guest-memory -z hang.elf'`. `stop` falls back to `system_powerdown`
  and then `quit` when ssh is dead.
- `console.sock`, the serial console (still logged to `console.log`): `scripts/macos-freebsd-vm.sh serial`. Log in
  as `bsdk8s` with the password in `~/bsdk8s/vm/serial-password`. It's local-only: the socket is on the Mac and
  sshd stays key-only. Set it on a new VM with `pw usermod bsdk8s -h 0`.

On a hang, before resetting: `procstat -kk -a > /tmp/stacks`, `vmstat -z`, `top -b -o res`, `sysctl vm.stats`, all
from the serial console, plus a guest memory dump from the monitor.
