# 07: Kubernetes conformance on a FreeBSD node

*2026-11-14. Single node, FreeBSD 15.1-RELEASE-p6 arm64 (QEMU/HVF dev VM, 8 vCPU / 12 GB). Kubernetes v1.36.1 +
`PlatformNerds/kubernetes` `bsdk8s-v1.36.1`, CRI-O `bsdk8s-1.37`, ocijail `bsdk8s-main`, kube-proxy pf mode on the host.
Unmodified upstream `e2e.test` (built from the same tree for freebsd/arm64), `ginkgo --procs=4 --focus='\[Conformance\]'`.
Raw results: [`conformance/`](../conformance/) (gzipped JUnit + failure lists).*

## Result

| Run | Passed | Pass rate | Excluding `[LinuxOnly]` | Duration |
|---|---|---|---|---|
| run 1 | 381 / 446 | 85.4% | 356 / 400 = 89.0% | 1h34m |
| **run 2** | **415 / 446** | **93.0%** | **388 / 400 = 97.0%** | 57m |

Run 2 fixed 34 tests and regressed none.

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

## Remaining failures (run 2: 31)

### Not `[LinuxOnly]`: 12, three work items

| # | Tests | Cause | Fix |
|---|---|---|---|
| 1 | 6 × Pod InPlace Resize | in-place resize not implemented: no CRI `UpdateContainerResources` → ocijail path, kubelet returns nil resources | add `ocijail update` (swap rctl rules; builds on research/06) + kubelet `generateContainerResources` |
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
3. In-place resize (6).
4. Second node (2, plus multi-node coverage that this single-node run can't exercise).
