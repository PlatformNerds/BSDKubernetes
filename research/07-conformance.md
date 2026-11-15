# 07: Kubernetes conformance on a FreeBSD node

*2026-11-14. Single node, FreeBSD 15.1-RELEASE-p6 arm64 (QEMU/HVF dev VM, 8 vCPU / 12 GB). Kubernetes v1.36.1 +
`PlatformNerds/kubernetes` `bsdk8s-v1.36.1`, CRI-O `bsdk8s-1.37`, ocijail `bsdk8s-main`, kube-proxy pf mode on the host.
Unmodified upstream `e2e.test` (built from the same tree for freebsd/arm64), `ginkgo --procs=4 --focus='\[Conformance\]'`.
Raw results: [`conformance/`](../conformance/) (gzipped JUnit + failure lists).*

## Result

| Run | Passed | Pass rate | Excluding `[LinuxOnly]` | Duration |
|---|---|---|---|---|
| run 1 | 381 / 446 | 85.4% | 356 / 400 = 89.0% | 1h34m |
| run 2 | 415 / 446 | 93.0% | 388 / 400 = 97.0% | 57m |
| run 3 | 423 / 446 | 94.8% | 392 / 400 = 98.0% | 48m |
| run 4 (2 nodes) | 422 / 446 | 94.6% | 393 / 400 = 98.2% | 52m |
| run 5 (2 nodes, 35-min clock skew) | 420 / 446 | 94.2% | 389 / 400 = 97.3% | 71m |
| **run 6 (2 nodes)** | **425 / 446** | **95.3%** | **394 / 400 = 98.5%** | 50m |

Run 2 fixed 34 tests and regressed none. Run 3 (after the kube-proxy session-affinity fix and ocijail file mounts below devfs) fixed 8 more, again with no regressions. The only remaining non-`[LinuxOnly]` failures are in-place pod resize (6) and the two-node requirement (2).

**Why report "excluding `[LinuxOnly]`":** upstream tags tests that check Linux-specific behaviour (`/proc`, sysctls,
Linux error-message wording, bind-mount fs types, ...) as `[LinuxOnly]`. Windows nodes skip them in their conformance
runs. The equivalent number for FreeBSD is the second column. Both are shown so nothing is hidden.

**Baseline before this work:** 0 tests that start a pod could pass. CRI-O on FreeBSD refuses every upstream test image
(`choosing an image from manifest list: no match for platform freebsd/arm64`).

## What made it work

1. **FreeBSD test images under upstream names** (`scripts/build-e2e-images-freebsd.sh`): agnhost, kitten, nautilus,
   busybox, nonroot, nonewprivs, glibc-dns-testing, nginx (×2), pause. They are built on the official FreeBSD `runtime`
   OCI base with FreeBSD packages, using the same approach Windows uses. Pitfalls we hit:
   - Tags must come from `test/utils/image/manifest.go`. `test/images/*/VERSION` are the *next* versions.
   - The image needs `ldconfig` hints rebuilt; there's no boot-time `ldconfig` in a container (nginx → libpcre2).
   - `pkg -r <root>` skips post-install scripts (the nginx docroot symlink).
   - Fidelity matters. busybox `nc` rejects `nc -v -t -w 2`, which broke 11 Service tests. Use FreeBSD's
     OpenBSD-derived `/usr/bin/nc`, as upstream uses `netcat-openbsd`.
   - Absolute symlinks in the image resolve against the *build host* with `test -e`; use `-L`.
2. **`agnhost mounttest` reports `f_fstypename` on FreeBSD** (FreeBSD's `f_type` numbers are assigned at boot).
   `PlatformNerds/kubernetes` `605db4768ec`.
3. **`--enable-aggregator-routing=true` on kube-apiserver.** The apiserver runs in a host-network jail, and pf on 15.x
   does not redirect host-originated traffic to Service ClusterIPs. So every admission webhook, CRD conversion webhook
   and aggregated API call timed out (21 tests in run 1; the apiserver log had 256× `context deadline exceeded`).
   This resolves those Services to endpoints instead. It's in the demo's kubeadm config.

## Run 6: 425/446, 98.5% excluding `[LinuxOnly]` (2026-11-15)

The first clean two-node run with every fix since run 4.
- **Every non-`[LinuxOnly]` failure left is one of the 6 in-place resize tests.** In-place resize itself is
  implemented (research/06); the tests verify by reading `/sys/fs/cgroup` inside the container.
- **The other 15 failures are all `[LinuxOnly]`**, the same set as before: 7 EmptyDir tmpfs, 2 fsGroup secrets,
  plus HostPort, ImageVolume, sample-apiserver, read-only rootfs wording, sysctls, and the privileged dummy-device
  test.

**Before run 6 completed, one attempt hung node 1** about 45 minutes in:
- The kubelet stopped heartbeating and sshd stopped sending its banner, while the API server kept answering.
- Neither the console nor syslog had a panic or OOM message; logs stop mid pod-network setup.
- The VM was hard-reset.

The suspected cause was memory exhaustion that the kubelet couldn't see. Its FreeBSD node working set was racct
process RSS, which misses ARC, tmpfs and wired memory: 790 MB reported vs 2.2 GB in use on an idle node. So
memory eviction could never fire. That was fixed (`PlatformNerds/kubernetes` `23c18881738`), along with headroom
in the demo: `systemReserved` 1Gi, `evictionHard` 500Mi, ARC capped at 25%, and a kubelet restart on exit.

**Honest status:** run 6 logged node 1's memory every minute (`conformance/run6-node1-memory.log`). Free memory
never dropped below 6.3 GB and the node never reported memory pressure. So run 6 neither reproduced the hang nor
exercised eviction, and **the hang's root cause is unconfirmed**. Next time it happens, get the kernel state before
resetting: a QEMU monitor socket or a serial console with a login, then `procstat -kk -a` and `vmstat -z`.

## Run 5: invalidated by clock skew (2026-11-15)

This was the first run after no_new_privs, NodePort SNAT, UDP state cleanup, in-place resize and host→ClusterIP.
- **Fixed since run 4 (3):** all three triaged run-4 failures: `allowPrivilegeEscalation: false`, NodePort session
  affinity across nodes, and the garbage-collector test (a flake, as suspected).
- **New (5):** OIDC discovery, Namespace pod removal, Deployment rollover, and two Job tests. All are
  time-dependent, and the cause was **clock skew, not code**:
  - The VMs ran no NTP, and node 1's clock was 35 minutes behind node 2's. The OIDC validator on node 2 rejected a
    token node 1 had just issued as expired.
  - With ntpd enabled (now in the VM script, docs/MAC-DEV-VM.md), the five tests plus one more matching the same
    filter pass in isolation: 6/6 in 63 s, where run 5 hit 15-minute timeouts.
- Run 6 repeats the full suite on synced clocks.

## Run 4: two FreeBSD nodes (2026-11-14)

The first multi-node run: control plane `bsdk8s` (192.168.100.1) + worker `bsdk8s-2` (192.168.100.2), host-gw routing
(`bsd-hostgw`), kube-proxy pf on both nodes as a DaemonSet pod (allow.pf kernel on both).

- **Fixed (2):** the two tests that require two nodes (DaemonSet rollback, "at least two untainted nodes").
- **New failures (3), each triaged:**
  1. **`allowPrivilegeEscalation: false` was not enforced (security gap). Fixed.** On node 2 the setuid `nnp` helper
     reached `Effective uid: 0`. Run 3's "pass" on node 1 was accidental, and there were two bugs:
     - **ocijail ignored OCI `process.noNewPrivileges`.** It now calls `procctl(PROC_NO_NEW_PRIVS_CTL)` (FreeBSD ≥ 14)
       after dropping to the container user, just before exec; on older releases it refuses instead of silently
       ignoring the flag (`PlatformNerds/ocijail` `no-new-privs`, in `bsdk8s-main`; patch `ocijail/0009`). Tests: a
       setuid-root `id` run as nobody reports euid 0 without the flag and 65534 with it.
     - **Setuid bits are lost when unpacking image layers on FreeBSD.** containers/storage `pkg/archive/archive_bsd.go`
       passed `uint32(os.FileMode)` to `fchmodat(2)`, but Go keeps setuid/setgid/sticky in high bits
       (`os.ModeSetuid` is `1<<23`), so the kernel never saw them. Node 1 built the image with `buildah commit`
       (buggy vendored storage), so its `nnp` was not setuid at all; node 2 got the same image via `podman load`,
       which kept the bit. Already fixed upstream (container-libs `8f9b4818cc`, June 2026), but CRI-O 1.37 and the
       FreeBSD `buildah` 1.43.2 package predate it: **every image CRI-O pulls on FreeBSD loses its setuid binaries**.
       Backported into `PlatformNerds/cri-o` `bsdk8s-1.37` (patch `cri-o/0001`); the e2e image build re-loads
       `nonewprivs` through podman and checks the bit.
     Verified on both nodes with pods pinned per node: `false` → euid 1000, `true` and unset → euid 0 (Kubernetes
     semantics); the 3 `AllowPrivilegeEscalation` e2e tests pass.
  2. **NodePort across nodes. Fixed.** `192.168.100.1:30756` was unreachable from a pod on node 2 when the endpoint
     was on node 2. Confirmed asymmetric routing: node 1 rdr's to the endpoint on node 2, which replies straight to
     the client without passing node 1, so the rdr is never reversed. The same broke a host or external client
     whose return path skips the node. Affinity pins a client to the broken path; round-robin mostly hid it. Fix
     (`PlatformNerds/kubernetes` `4f72bafa2a2`, patch `bsdk8s/0005`): NodePort rdr rules `tag KUBE_NODEPORT`, and
     tagged traffic to **remote** endpoints is `nat`ed to the node IP. ClusterIP traffic keeps its source;
     `externalTrafficPolicy: Local` (no remote endpoints) still preserves the client address. Verified with a
     client × node matrix (pods on both nodes, both hosts → both NodeIPs: all connect). The backend sees the node IP
     via the remote node and the pod IP via the local one. Also fixed: the IPv6 proxier was given the IPv4 node IP.
     - **Found while testing: stale UDP states. Fixed** (`b2cb87563f3`, patch `bsdk8s/0006`). A UDP client with a
       fixed source port stayed pinned by its pf state to a deleted endpoint (or old target port) indefinitely.
       iptables mode deletes conntrack entries; the pf proxier now lists states after each sync and kills (`pfctl
       -k key`) the UDP rdr states whose endpoint is no longer current. Its rules for a Service whose endpoints all
       went away were also never removed; they are flushed now. Fixes 2 (non-conformance) `Conntrack` tests.
     - **hostNetwork clients → ClusterIPs: fixed** (research/09): outbound `rdr-to` filter rules in each Service
       anchor, Service CIDR routed via the node-IP interface; found and fixed a pf sticky-address kernel bug on the way.
  3. **Garbage collector:** the API call timed out (`context deadline exceeded`) under load. Likely flaky; re-run to
     confirm.

## Remaining failures (run 2: 31)

### Not `[LinuxOnly]`: 12, three work items

| # | Tests | Cause | Fix |
|---|---|---|---|
| 1 | 6 × Pod InPlace Resize | ~~in-place resize not implemented~~ **Implemented** (research/06 §In-place pod resize, verified on both nodes). The tests still fail: they check the result by reading `/sys/fs/cgroup` in the container. Windows skips them. | needs a FreeBSD verification path in the e2e test (upstream) |
| 2 | ~~4 × Container Runtime "termination message"~~ **fixed** | test sets `terminationMessagePath: /dev/termination-custom-log`. The jail's `/dev` is devfs, which can't hold a regular file, so the bind mount failed (`No such file or directory`). Linux runtimes use a tmpfs `/dev`. | **ocijail**: mount such files under `/.ocijail-devfiles` and symlink them into the devfs, unhiding the symlink because CRI-O's devfs ruleset 4 hides everything else (`PlatformNerds/ocijail` `devfs-file-mounts`, in `bsdk8s-main`). All 5 termination-message tests pass. |
| 3 | 2 (DaemonSet rollback, "at least two untainted nodes") | single-node cluster | second FreeBSD node |

### `[LinuxOnly]`: 19

| Tests | Cause | Notes |
|---|---|---|
| 7 × EmptyDir tmpfs | volume *is* tmpfs on the host, but it's bind-mounted (nullfs) into the jail, and `statfs` inside reports `nullfs` | FreeBSD semantics. Could mount tmpfs directly in the jail. |
| ~~4 × Service session affinity~~ | `Affinity should hold but didn't`: the pf proxier never emitted `sticky-address` | **Fixed** (`PlatformNerds/kubernetes` `648a05cca2b`): all 4 now pass, plus both affinity-timeout tests |
| 2 × Secrets / projected with fsGroup + defaultMode as non-root | fsGroup ownership/permissions on FreeBSD | investigate |
| 1 × HostPort same port different hostIP/protocol | CNI portmap (pf) | investigate |
| 1 × Aggregator sample-apiserver | Linux-only sample image | needs a FreeBSD sample-apiserver image |
| 1 × read-only rootfs | FreeBSD's busybox prints `/bin/sh: line 0: can't create`, Alpine's prints `/bin/sh: can't create` | cosmetic, build option |
| 1 × Sysctls | Linux `kernel.*` sysctls | n/a on FreeBSD |
| 1 × Security context privileged | test creates a Linux dummy net device | n/a on FreeBSD |
| 1 × ImageVolume | image volumes not implemented in CRI-O FreeBSD | later |

## Next

1. ~~Session affinity in kube-proxy pf mode~~: fixed, 4 tests now pass (→ 419/446 expected on the next full run).
2. ~~Termination message under `/dev`~~: fixed (5/5 pass).
3. In-place resize: implemented (research/06). The 6 tests stay red until the e2e checks have a FreeBSD path.
4. Second node (2, plus multi-node coverage that this single-node run can't exercise).
