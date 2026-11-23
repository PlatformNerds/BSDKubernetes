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
| run 7 (2 nodes, regression check) | 425 / 446 | 95.3% | 394 / 400 = 98.5% | 52m |
| run 8 (2 nodes, Mac slept mid-run) | 424 / 446 | 95.1% | | |
| run 9 (2 nodes, node 2 froze mid-run) | 420 / 446 | 94.2% | | 1h50m |
| run 10 (2 nodes, regression check) | 425 / 446 | 95.3% | 394 / 400 = 98.5% | 58m |
| run 11 (2 nodes, node 2 froze, 1h suite timeout) | 414 / 426 run | | | 1h |
| run 12 (2 nodes) | 435 / 446 | 97.5% | 394 / 400 = 98.5% | 56m |
| **run 13 (2 nodes)** | **437 / 446** | **98.0%** | **394 / 400 = 98.5%** | 54m |
| run 15 (2 nodes, regression check after block volumes, device nodes, CSI, DRA) | 437 / 446 | 98.0% | 394 / 400 = 98.5% | 53m |

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
2. **`agnhost mounttest` reports filesystem type names on FreeBSD** (FreeBSD's `f_type` numbers are assigned at
   boot). `PlatformNerds/kubernetes` `605db4768ec` used `f_fstypename`; since `dbbe0e5db7d` it names `f_type` through
   `vfs.conflist`, so a nullfs-mounted tmpfs volume reads `tmpfs` as on Linux (research/20).
3. **`--enable-aggregator-routing=true` on kube-apiserver.** The apiserver runs in a host-network jail, and pf on 15.x
   does not redirect host-originated traffic to Service ClusterIPs. So every admission webhook, CRD conversion webhook
   and aggregated API call timed out (21 tests in run 1; the apiserver log had 256× `context deadline exceeded`).
   This resolves those Services to endpoints instead. It's in the demo's kubeadm config.

## Runs 10 to 12: 435/446, and the node freezes explained (2026-11-18)

**Run 10** (kube-proxy traffic policies, `loadBalancerSourceRanges` and reject-without-endpoints from research/19,
kernel `bsdk8s16`): 425/446, with the same 21 failures as run 7. No regressions.

Ten of those 21 were then fixed (research/20). None was a Linux-only test detail:
- **7 EmptyDir tmpfs:** the mounttest image now reports the filesystem behind nullfs.
- **2 fsGroup secrets:** the kubelet never applied `fsGroup` to any volume on FreeBSD.
- **HostPort:** host ports did nothing at all; CRI-O never passed them to the CNI portmap plugin.

**Run 11** was cut short: node 2 froze again 25 minutes in, and the run hit ginkgo's default 1-hour suite timeout
with 20 Serial tests unrun. This time the hung node was debugged live through QEMU's gdb stub. The cause was a lost
wakeup in FreeBSD's rmlock, hit when pf's interface handlers write-lock pf's rules inside the network epoch while
pod churn creates and destroys epairs (research/21, freebsd-src 0032). Run 6's node 1 hang ("logs stop mid
pod-network setup", no panic) and run 9's freeze fit the same pattern.

**Run 12** (`kernel.bsdk8s18` with the rmlock fix; `--timeout=3h`): **435/446 (97.5%)**. No freeze.
- **The 6 non-`[LinuxOnly]` failures are in-place resize.** Resize works (research/06). The tests hard-code
  `spec.os.name: linux`, so their pods run under the `linux` handler. On this cluster the busybox tag is the
  FreeBSD build, and CRI-O refuses it (`... is a freebsd image, but runtime handler "linux" runs linux images`).
  Even with a Linux busybox they would check `cpu.weight`, which FreeBSD can't enforce (research/17).
- **The 5 `[LinuxOnly]` failures:**
  - sample-apiserver (Linux image);
  - sysctls (Linux `kernel.*` names);
  - the privileged dummy-device test (Linux netlink);
  - read-only rootfs (error wording);
  - ImageVolume (not implemented in CRI-O on FreeBSD).

## After run 12: test images and image volumes (2026-11-18)

Three of the five `[LinuxOnly]` failures were worth reopening.

**Read-only rootfs: the busybox image held the wrong BusyBox.** The image is tagged `busybox:1.37.0-1`, but it
contained the FreeBSD package, BusyBox 1.38.0. In 1.38, ash prefixes `sh -c` errors with the line number
(`/bin/sh: line 0: can't create /file: ...`); 1.37 prints `/bin/sh: can't create /file: ...`, which the test
expects. `scripts/busybox-freebsd/build.sh` builds 1.37.0 from the FreeBSD port's recipe at its last 1.37.0 commit,
and the image script takes it as `BUSYBOX=` (it warns when the binary's version doesn't match the tag).
Two lessons:
- Applet sets differ between releases (1.37 lacks `vmstat`, `uuidgen`, `sha384sum`, `ssl_server`), so the image has
  to be rebuilt from the script, not patched with a new binary.
- Build with the ports framework's CFLAGS and `BB_GLOBAL_CONST=''`. With plain `-O2`, 1.37.0's `awk` crashed on
  every program on arm64, which broke the PreStop and "container that exits" conformance tests in a sig-node rerun
  (95/106). With the flags, both pass again.

**sample-apiserver: build the images.** The aggregator test deploys `sample-apiserver:1.29.2` and
`registry.k8s.io/etcd:3.6.8-0`, which have no FreeBSD variants. FreeBSD builds from the same sources
(`k8s.io/sample-apiserver` v0.29.2, the tag that image was built from according to the history of
`test/images/sample-apiserver`; etcd v3.6.8) pass it unchanged in 34 s (`SAMPLE_APISERVER=`/`ETCD=` in
`scripts/build-e2e-images-freebsd.sh`).

**ImageVolume: implemented, but the test checks for Alpine.** CRI-O had no FreeBSD image-volume code (a literal
`TODO`). Patch `cri-o/0019` mounts the image read-only from containers/storage and gives the jail a read-only nullfs
mount of it (`ro,noexec,nosuid`), with `subPath` resolved inside the image. The test also uses `pullPolicy: Always`,
which goes to registry.k8s.io, where kitten has no FreeBSD image. So the nodes now use a registry mirror for
`registry.k8s.io/e2e-test-images`: `docker-registry` on node 1, filled by `scripts/e2e-image-mirror.sh` with indexes
holding our `freebsd/arm64` build and upstream's `linux/arm64` image. FreeBSD pods get the FreeBSD build, and
`linux`-handler pods still get upstream's. With both, the test's pod pulls, starts and reads `data.json` from the
volume. It then fails on `cat /volume/etc/os-release` expecting `Alpine Linux`: the volume is the FreeBSD variant,
the platform a FreeBSD node selects. That check is Linux-specific, so the test stays `[LinuxOnly]`-red.

Still red, for reasons that hold: sysctls (Linux `kernel.*` names) and the privileged dummy-device test (busybox
`ip`, Linux rtnetlink code the FreeBSD port leaves out).

**Run 13** (all of the above, `kernel.bsdk8s18`): **437/446 (98.0%)**, no regressions, no freeze. The 9 failures:
the 6 in-place resize tests (research/17) and 3 `[LinuxOnly]` (ImageVolume's Alpine check, sysctls, dummy device).
Excluding `[LinuxOnly]` the count is unchanged at 394/400; the two new passes are both `[LinuxOnly]` tests.

**Run 14** (2026-11-20, `kernel.bsdk8s23`, after the hostPath-submount, socket-mount, NFS, kubelet-teardown and
pf-state-option changes): **437/446**, the same 9 failures as run 13. No regressions.

**Run 15** (2026-11-22, `kernel.bsdk8s26`, after block volumes, `allow.mknod` and device nodes, the CSI mount
proxy, DRA and device-plugin allocation): **437/446**, the same 9 failures as runs 13 and 14. No regressions
(`conformance/run15-*`).

**Run 16** (2026-11-23, `kernel.bsdk8s27`, after file-system growth, the orphaned-volume cleanup retry, the
kube-proxy anchor monitor, `SupplementalGroupsPolicy` user conversion, CRI-O log reopening and microsecond CPU
accounting): **437/446**, the same 9 failures as runs 13 to 15. No regressions (`conformance/run16-*`).

**`[NodeConformance]` beyond the conformance set** (94 tests: NodeConformance, not Conformance, not
Serial/Slow/Disruptive): 87/94, then **91/94**. The 4 probe-while-terminating tests (2 for regular containers, 2 for
sidecars) failed with `CreateContainerError: /bin/bash: No such file or directory`. They run `/bin/bash`, which
Alpine's bash package provides, while FreeBSD's bash lives in `/usr/local/bin`. The agnhost image (and kitten and
nautilus, built on it) now links `/bin/bash`, and all 4 pass. The 3 left:
- the memory-backed EmptyDir `df` test (research/20, `MNT_IGNORE`). **Fixed** 2026-11-19: busybox's `df` applet,
  ported to `getmntinfo(3)`;
- sysctls with slashes (`[LinuxOnly]`, Linux `kernel.*` names);
- PrivilegedPod (`[LinuxOnly]`, `ip link add ... type dummy`; FreeBSD has no dummy interface type).

**sig-network beyond the conformance set** (56 tests: not Conformance, Feature, Serial, Slow, Disruptive, Flaky or
LinuxOnly): 52/56, then **54/56**.
- **Fragmented UDP to a Service was dropped (fixed).** "should function for service endpoints using hostNetwork" sends
  a 2000-byte UDP request through a ClusterIP, which must be fragmented at MTU 1500. pf doesn't reassemble by default
  (`set reassemble no`), and only a datagram's first fragment carries ports. kube-proxy's rdr translated that first
  fragment, but the others never reached the endpoint. Measured by hand from a pod on node 1, the request failed via
  the ClusterIP to pod and host-network endpoints alike, while direct requests to the same endpoints worked. Both
  nodes' pf.conf now set `set reassemble yes`, as does the demo's `build-cluster.sh`. Afterwards all four paths
  echo, and the test passes. This affects any UDP Service with large datagrams, for example DNS with EDNS.
- **"should support configurable pod resolv.conf" (fixed):** the test runs `/coredns` from agnhost, which upstream
  bundles (CoreDNS 1.6.2). The FreeBSD agnhost images now include the cluster's CoreDNS 1.14.2 build (`COREDNS=` in
  the image script). 1.6.2 can't be built for freebsd/arm64: its 2019 dependencies predate Go's port. The test's
  Corefile (`hosts`, `log`) behaves the same.
- **Still failing, Linux mechanisms in the tests:**
  - "should set TCP CLOSE_WAIT timeout" runs `distroless-iptables` to read Linux conntrack. What it checks was
    really missing, though (kubernetes 0039, 2026-11-19). The pf proxier ignored kube-proxy's conntrack
    settings, and pf's timeout once one side has sent a FIN (`tcp.closing`) is 15 minutes, against
    kube-proxy's 1-hour CLOSE_WAIT. So a half-closed connection through a Service lost its translation after
    15 idle minutes. kube-proxy now sets pf's global options from the conntrack settings with `pfctl -m -O`,
    which leaves rules alone: `limit states` (maxPerCore × CPUs), `tcp.established`, `tcp.closing`,
    `udp.single`, `udp.multiple`. Checked: a half-closed connection to a ClusterIP has a pf state expiring in
    59:56. The state limit went from 100000 to 262144 (8 CPUs), and pfctl scaled the adaptive-timeout
    thresholds with it.
  - "should not be vulnerable to the invalid conntrack state bug": its server
    (`regression-issue-74839`, now built for FreeBSD) watches the client's TCP segments through a raw `ip:tcp`
    socket. Linux copies TCP to raw sockets; FreeBSD never does, so the server never sends its out-of-window
    packet. Porting it to BPF would be needed to test pf's window checking this way. **Fixed** 2026-11-19: on
    FreeBSD the server reads the segments from bpf(4) (kubernetes patch 0036), and CRI-O gives `/dev/bpf` to
    containers with `CAP_NET_RAW`, which the test pod asks for (CRI-O patch 0020). In a verbose run, the server
    saw 30 handshakes and injected 30 out-of-window packets, and the client never sent an RST: pf drops packets
    outside the state's window, so they never reach the client.

**All `[NodeConformance]`** (2026-11-19, kernel `bsdk8s20`, 235 tests, not Serial/Slow/Disruptive/Flaky): **231/235**.
The 4 left are `[LinuxOnly]`: the two sysctl tests (Linux `kernel.*` names) and the two `ip link add ... type dummy`
tests (FreeBSD has neither `ip` nor a dummy interface type).

**SCTP** (`[Feature:SCTPConnectivity]` and the other SCTP tests, research/23): none could run, because agnhost's
SCTP library was Linux-only. Now **10/10**, after porting the library and teaching pf (`net.pf.sctp_rdr_ports`,
kernel `bsdk8s20`) to translate SCTP Service ports.

## SIG sweep beyond the conformance set (2026-11-19, kernel `bsdk8s21`)

Not Conformance, Serial, Slow, Disruptive, Flaky, Feature or LinuxOnly:

| SIG | Result |
|---|---|
| sig-cli | 50/59, then 58/59, **59/59** on 2026-11-22 (kernel `bsdk8s26`) |
| sig-auth | 7/7 |
| sig-instrumentation | 4/6, then **6/6** |
| sig-api-machinery | 63/63 (62/62 on 2026-11-22) |
| sig-scheduling | none in this set |
| sig-apps | 48/48 |
| sig-network | 67/68 (the known CLOSE_WAIT test, which reads Linux conntrack); same on 2026-11-22 |
| sig-node | 27/28 without the pod-level resize tests (decided, research/17) and DRA; **28/29** on 2026-11-22, the one failure `Mount propagation` (FreeBSD has no shared subtrees, research/20) |
| `[DRA]` (not Serial, Slow, Disruptive, Alpha; includes the Beta `DRAExtendedResource`, `DRADeviceTaints`, `DRAPartitionableDevices`, `DRAPrioritizedList` tests) | before: every test needing a kubelet plugin failed (no DRA manager in the FreeBSD kubelet); then 88/92, the 4 failures using the `resource.k8s.io` v1beta1/v1beta2 APIs, which the API server didn't serve (only `v1`, the 1.36 default). With `--runtime-config=resource.k8s.io/v1beta1=true,resource.k8s.io/v1beta2=true` (demo `build-cluster.sh` 00620ac): **92/93**, all 92 earlier tests passing. The 93rd, `DeviceTaintRule evicts pod` (`[Feature:OffByDefault]`), only runs once v1beta2 is served; it waits for kube-controller-manager to evict the pod, which needs the `DRADeviceTaintRules` feature gate this cluster leaves off (control plane, not FreeBSD; the kubelet side prepared and unprepared the claim) |
| sig-scheduling Serial | 14/14 |
| Serial tests of sig-node, apps, network, api-machinery, cli | 17/19, then 18/19. On 2026-11-22 (kernel `bsdk8s26`; `--procs=1 --focus='\[sig-(cli\|api-machinery\|apps\|network\|node)\].*\[Serial\]' --skip='Disruptive\|Flaky\|Feature:\|LinuxOnly\|InPlace Resize\|DRA\|Slow'`, a wider set): **37/37** |
| HostPathType + in-tree hostPath/hostPathSymlink/emptydir drivers | 41/57, then **43/57** (socket mounts, research/20); the other 14 are block/character devices, which need `mknod` (since: all pass, `allow.mknod` and ocijail's device node copies, HostPathType 35/35, research/20) |
| volumes (`HostPath\|Subpath\|EmptyDir\|Projected\|ConfigMap\|Secrets\|Downward API`) | 78/79 |
| `[Feature:HPA]` (not Slow, 2026-11-23) | 0/23 (the `resource-consumer` image has no FreeBSD build), then 12/23 with one: its `consume-cpu` couldn't measure itself on FreeBSD and burned about 600m whatever was asked (kubernetes 0062). Then 21/23: container CPU time came from rctl in whole seconds, so metrics moved in 67m steps and the HPA didn't scale down. Fixed with freebsd-src 0046 (`cputime_us`, kernel `bsdk8s27`) and CRI-O 0034: **23/23** (research/06). Also run with the cross-SIG features: NodeAuthorizer, KubeletSecurity, BootstrapTokens, PodPriority, ContainerRestartRules, Networking-IPv4/DNS all pass; StorageVersionAPI needs the alpha `internal.apiserver.k8s.io` API, which this control plane doesn't serve |
| sig-node `[Feature:*]` tests of features on by default in 1.36 (2026-11-23; `--focus='\[sig-node\].*Feature:' --skip='Alpha\|OffByDefault\|Flaky\|Disruptive\|Serial\|Slow\|GPU\|Windows\|NodeSwap\|DRA\|InPlace\|Resize'`, 38 selected, 34 run) | first 19/34. **22/22 of the tests that can run here**: EnvFiles, RuntimeHandler, PodLevelResources, HostAccess, NodeProblemDetector, the examples, and SupplementalGroupsPolicy. The 3 SupplementalGroupsPolicy tests had failed because the FreeBSD kubelet dropped the container user CRI-O reports (a stub `toKubeContainerUser`), although the nodes advertise the feature; fixed, kubernetes 0061. Not counted: 5 UserNamespacesSupport (`hostUsers: false`), a FreeBSD limit, since jails have no user ID mapping; 6 NodeLogQuery, whose test requires a Linux node (on FreeBSD the kubelet's log query has no native logger and falls back to `/var/log/<service>.log`, untested here because `enableSystemLogQuery` is off as upstream defaults it); 1 KubeletCredentialProviders, which needs the CI's test credential plugin and authenticated registry |
| `[Disruptive]` outside sig-storage 2026-11-23 (13 selected without cloud providers, reboots and node resizes) | **8/8** of the tests that run: the 2 NoExecuteTaintManager `[Conformance]` tests, kubelet, kube-proxy and apiserver restarts, kube-proxy killed, the logging soak, and the rules-recreate test. That last one failed at first (it runs `iptables-save`); on FreeBSD it now empties kube-proxy's pf anchors, which exposed a real gap: the pf proxier never restored removed rules (fixed, kubernetes 0057-0060, research/19). 5 skip themselves: 4 need the gce or aws provider (both etcd failure tests, the controller-manager and scheduler restarts), and 1 needs a default StorageClass, which this cluster doesn't have |
| storage `[Disruptive]` 2026-11-23 (`--procs=1 --focus='\[sig-storage\].*\[Disruptive\]' --skip='Flaky\|Feature:\|gce\|pd.csi\|aws\|azure\|vsphere\|cinder\|iSCSI\|iscsi\|Ceph\|RBD\|GlusterFS\|Alpha\|nfs\|NFS\|NonGracefulNodeShutdown'`, `[LinuxOnly]` included; never run before) | first 6/13 before the run was stopped. Fixed: the e2e kubelet and mount helpers for FreeBSD (kubernetes 0055), the kubelet's cleanup of volumes it can't rebuild after a restart (0054), `GetMountRefs` (0056), and the csi-hostpath driver listing a block target twice (csi-driver-host-path 0003); research/20. **34/34** |
| sig-storage sweep 2026-11-22 (kernel `bsdk8s26`; `--focus='\[sig-storage\]' --skip='Serial\|Slow\|Disruptive\|Flaky\|Feature:\|gcepd\|aws\|azure\|vsphere\|cinder\|iSCSI\|iscsi\|Ceph\|ceph\|RBD\|rbd\|GlusterFS\|Alpha'`, `[LinuxOnly]` included; 1443 selected, 416 run, the rest skip themselves) | **391/416**. 19 are the in-tree `nfs` driver's Dynamic PV and Generic Ephemeral patterns: they mount `vers=4.0`, and the FreeBSD nfs-provisioner (unfs3) serves NFSv3 only (research/20; the earlier NFS row left them out). 5 are CSI Mock fsGroup policy tests marked `[LinuxOnly]` that compare the group *name* with `root`: FreeBSD names gid 0 `wheel` (`Expected fsGroup of root, got wheel`), and the gid is right. 1 was a real gap: `PersistentVolumes-expansion loopback local block volume` ran Linux `dd oflag=append`/`losetup -c`, and the kubelet had no FreeBSD file system resizer at all. Fixed (kubernetes 0051-0052, research/20), and that test now passes. Two more fixes followed from the cleanup: leaked CSI volume directories (kubernetes 0053) and kubelet and CRI-O log rotation (CRI-O 0033) |
| `[Slow]` rerun 2026-11-22 (kernel `bsdk8s26`; `--focus='\[Slow\]' --skip='Serial\|Disruptive\|Flaky\|Dynamic PV\|Generic Ephemeral\|[Rr]esize\|DRA\|CSI\|csi-\|gcepd\|aws\|azure\|vsphere\|cinder\|Alpha\|Feature:'`, `[LinuxOnly]` included) | **161/191**; all 30 failures were subPath "outside the volume" tests, a real container escape, fixed by kubernetes 0050 (research/20); every subPath test then passes (156/156). Rerun with the fixed kubelet: **191/191** (80 minutes; ginkgo selects 551 specs by name, 360 skip themselves at runtime as driver/pattern combinations that don't apply, so 191 is the whole set). Use `--timeout=5h`: a slower attempt hit ginkgo's default one-hour suite timeout at 179 specs, with no failures |
| `[Slow]` (not Serial; without Dynamic PV, Generic Ephemeral, resize, DRA, CSI and cloud drivers), kernel `bsdk8s23` | **55/93**; every failure is a known limit: 14 HostPathType block/character devices (`mknod`) (since: all 14 pass, `allow.mknod`, freebsd-src 0043, and ocijail 0019-0020, research/20), 21 local PV (set up through the hostexec pod's `nsenter` into the host mount namespace; a jail can't leave its jail, and the pod mounts `/` read-only) (since fixed: kubernetes 0041-0043, local volume sweep 97/145 with every non-block test passing, research/20), 3 needed the Linux-only `nfs-provisioner` pod (since built for FreeBSD, see the NFS row). All 15 outside sig-storage pass |
| NFS (`NFS\|nfs`), kernel `bsdk8s23` | 0 before (no NFS server pod). Now `PersistentVolumes NFS` **12/12**, and the in-tree `nfs`/`nfs3` drivers plus `DynamicProvisioner External` **30/35** with a FreeBSD nfs-provisioner (unfs3, NFSv3 only, so the `nfs` v4 dynamic patterns aren't run). The 5 failures read Linux's `mount(8)` output in the container (research/20) |

- **`kubectl port-forward` crashed CRI-O (fixed).**
  - The FreeBSD runtime had no `PortForwardContainer`. The generic one called itself until the goroutine stack
    overflowed (`fatal error: stack overflow`, in `/var/log/messages` via daemon(8), not in `crio.log`).
  - The node then went NotReady until crio was restarted. No conformance test port-forwards, so no run had
    caught it.
  - CRI-O 0021: crio re-executes itself as `crio-portforward`, which attaches to the pod's infra jail, connects to
    `localhost:port` and passes the socket back over a socketpair.
  - CRI-O 0022: hostNetwork pods have no jail (dropped infra container), so crio dials `localhost` itself. That
    covers kube-scheduler and kube-controller-manager, where the MetricsGrabber tests read metrics.
- **A pod's containers could not see each other's sockets (fixed).**
  - Every port-forward test pod has a readiness probe `netstat -na | grep LISTEN | grep -v 8080 | grep 80`.
    It runs in one container and looks for the other container's listener.
  - agnhost lacked `netstat`; it now gets FreeBSD's, whose `tcp4 ... *.80 ... LISTEN` matches the same way.
  - Each container is a sibling jail under the pod jail. Socket visibility follows `prison_check()`: own jail and
    children, not siblings. So `netstat` saw only its own container's sockets.
  - On Linux a pod is one network namespace, and every container sees all its sockets.
  - freebsd-src 0034 adds `security.bsd.see_vnet_sockets`, off by default. When set, jails that share a vnet other
    than the host's see each other's sockets.
  - Checked: one pod's containers see each other's listeners, but not another pod's or the host's. Both nodes
    boot `kernel.bsdk8s21` with it set.
- **sig-node:**
  - The 31 `PLR Pod InPlace Resize` tests that ran (pod-level resources, beta) fail like the six container resize tests
    (research/17). Their pods set `spec.os.name: linux`, so they run under the Linuxulator. There the FreeBSD busybox
    tag is refused, and they check `cpu.weight`, which FreeBSD doesn't enforce. Each waits five minutes, so they
    also push a full sig-node run past its one-hour limit.
- **Dynamic Resource Allocation (fixed).**
  - The `[DRA]` kubelet tests failed with "no handler registered for plugin type: DRAPlugin". The FreeBSD
    container manager never built the kubelet's DRA manager, so a DRA driver couldn't register, and pods with
    ResourceClaims never started.
  - Kubernetes 0049 wires it in as on Linux: the DRA manager, its plugin registration handler, prepare/unprepare,
    claim CDI devices passed to the runtime, resource health updates. It also replaces the fake topology manager
    with the real one under the `none` policy. The fake one's admission never called the device manager's
    `Allocate`, so a pod asking for a device-plugin resource got no devices. Checked: the Serial test
    "supports extended resources together with ResourceClaim" passes with a FreeBSD build of
    `sample-device-plugin` (build-e2e-images-freebsd.sh `SAMPLE_DEVICE_PLUGIN`), and a pod requesting
    `example.com/resource` gets the plugin's `/tmp/Dev-1` mount.
  - CRI-O 0032 injects CDI devices on FreeBSD (the stub ignored them). It applies the CDI spec's environment,
    mounts and hooks. Each device node must be a device on the host's devfs; it is unhidden in the container's
    devfs like the CRI `devices` field (CRI-O 0028-0031). Linux-only edits (network devices, Intel RDT) are refused.
  - The e2e test driver runs inside e2e.test and reaches the node through a hostpathplugin pod in proxy mode,
    which writes the CDI files with `base64 -d`. Our FreeBSD hostpathplugin image now carries FreeBSD's
    `base64` (bintrans(1)); without it every prepare failed.
  - 88/92; the 4 failures need `--runtime-config=resource.k8s.io/v1beta1=true,resource.k8s.io/v1beta2=true`,
    which upstream sets only in its DRA jobs. Nothing FreeBSD-specific there. With that flag set (now in the
    demo's `build-cluster.sh`): 92/93. The extra test, `DeviceTaintRule evicts pod`, is `[Feature:OffByDefault]`
    and needs the `DRADeviceTaintRules` gate in kube-controller-manager, which this cluster leaves off.
  - NodeConformance afterwards (not Serial/Disruptive/Flaky/LinuxOnly, and also not Slow/Alpha/OffByDefault,
    hence 172 rather than 182 tests): 172/172.
  - `Mount propagation` can't pass: FreeBSD has no shared subtrees (research/20).
- **Host paths didn't show the file systems below them (fixed).** No test checks this; it came up while looking at
  mount propagation. With the default ZFS layout, hostPath `/var` showed an empty `/var/log`. CRI-O 0023 mounts the
  file systems below a host path too, like Linux's recursive bind, except those of the node stack (research/20).
- **Volumes:** the one failure is a CSI mock driver test that `Secrets` matched. Its CSI sidecar images aren't built
  for FreeBSD.
- **Serial:**
  - `RuntimeClass ... scheduling with taints` needs a runtime handler named `test-handler` on the nodes, as
    upstream CI configures. `build-cluster.sh` now adds one with `E2E_NODE_CONFIG=yes` (demo fork). After that,
    all 8 RuntimeClass tests pass, including the `[Feature:RuntimeHandler]` ones.
  - `DNS configMap ... Change stubDomain` works: the stub domain answers `1.1.1.1`, `2.2.2.2` and `3.3.3.3`
    within 3 seconds. The test's last check expects `abc.acme.local` to have no answer once the ConfigMap is
    restored, but this network's resolver answers every name (below).
- **NodeConformance on `kernel.bsdk8s22`, 250 tests including the Serial ones: 246 pass.** The 4 failures are
  the known `[LinuxOnly]` ones (sysctls ×2, privileged ×2). The full run hit its one-hour limit four tests short,
  so the Serial `when running a container with a new image` group was run separately: 4/4. Two things were
  missing for it, both test setup:
  - The tests run an HTTP registry on the node's `localhost:5000`. containerd pulls from localhost over HTTP;
    CRI-O only does for registries marked insecure. `E2E_NODE_CONFIG=yes` now adds that.
  - The registry is `agnhost fake-registry-server`, which serves `/var/registry`. Upstream's agnhost image bakes
    `pause:3.10.2` in as `pause:testing`; ours had nothing there (404). `build-e2e-images-freebsd.sh` now bakes in
    the FreeBSD pause image the same way (`fake_registry_pause`). The images on both nodes were updated in place
    (originals `*-prev-registry`), and the mirror was refreshed. "should be able to pull image" uses
    `PullAlways`, which replaced the patched tag with the mirror's old copy until the mirror had the new one.
- **NodeConformance on `kernel.bsdk8s24` (2026-11-20), not Serial/Disruptive/Flaky/LinuxOnly: 182/182.** This run
  checks for regressions from the container mount-table work (freebsd-src 0038-0042, CRI-O 0025: containers of
  FreeBSD images now get linprocfs on `/proc`; busybox `mount`). Nothing regressed.
- **NodeConformance on `kernel.bsdk8s25` (2026-11-21), same skip set: 182/182.** Checks for regressions from
  `allow.mknod` (freebsd-src 0043, which also refuses `mknod` in devfs for every jail), CRI-O 0026-0027, ocijail
  0018 and the cluster default `default_capabilities = ["MKNOD"]`. Nothing regressed (3369 s).
- **NodeConformance on `kernel.bsdk8s26` (2026-11-21), same skip set: 182/182.** Checks for regressions from
  nullfs refusing to open devices (freebsd-src 0045), the `/dev/mem` fix (0044) and CRI-O 0028-0029 (host paths on
  devfs become devfs rules or mounts). Nothing regressed (3376 s).
- **NFS (research/20):** NFS server pods needed the kernel (freebsd-src 0036, `nfsd` in an inherited vnet), CRI-O
  (0024, `allow.nfsd` for privileged pods) and a FreeBSD `volume/nfs` image. The PV tests also found two Linux
  leftovers: BusyBox's grep took no `\b` (now linked with `libregex`), and the PV recycler pod used a Linux image (now
  `freebsd-runtime`, set in `build-cluster.sh`).
- **Not FreeBSD:** "should handle in-cluster config" expects `kubectl --server=invalid` to fail name resolution.
  This network's DNS answers for `invalid` (`192.168.160.1`, from the Mac too), so the request reaches an HTML page.

## Run 9: 420/446, no regressions, but node 2 froze once (2026-11-17)

This run checked everything since run 8:
- `kernel.bsdk8s9` on both nodes: lincgroupfs (research/15), the arm64 SME SIGILL fix, mremap EFAULT, and the arm64
  musl brand / glibc interpreter fix (research/16);
- CRI-O mounting lincgroupfs into Linux containers, and the image-OS fallback;
- ocijail sysctls and recorded mount points;
- kubelet's FreeBSD safe sysctls.

Linux workloads (Envoy Gateway, cert-manager, metrics-server, nginx:alpine, node:22-alpine) were left running on
node 2 during the run.

**Failures compared with run 8** (`conformance/run9-failures.json`):
- **21 are the known ones from run 7.**
- **Run 8's rollover failure is gone**, as expected: it was clock skew.
- **5 are new, and all are Serial tests that ran while node 2 was down.** Each one either waited 18 minutes for
  "all nodes ready" in cleanup or never saw a schedulable second node. One of them is the trigger itself, `Garbage
  collector should keep the rc around until all its pods are deleted`.

Corrected for the outage, run 9 matches run 7: **425/446, no regressions.**

### Node 2 froze during the garbage-collector test (solved: research/21)

The cause was found later: an rmlock lost wakeup in the kernel, fixed by freebsd-src 0032 (research/21). The
notes below are from before that.

What happened:
- **02:16:4x UTC:** the GC test started about 100 nginx pods; node 2 created 78 vnet jails in a few seconds.
- **02:16:54:** syslog went silent.
- **02:17:42:** the node controller marked node 2 unreachable.
- **Until 02:25:** the kubelet stayed alive and kept restarting Linux pods whose probes failed. So process
  creation still worked, but the 192.168.100.0/24 network was dead in both directions ("Host is down" from node 1).
- **03:11:** no new process could start. A root login on the serial console was accepted, but the shell never
  printed a prompt.
- **Afterwards:** `kern.lastpid` had wrapped (about 26k at 02:25, 3017 at 03:11), so something forked heavily while
  wedged.
- **Kernel output:** no panic and no `maxproc` message, so the cause isn't visible in the logs.

**Recovery:** a QEMU `system_reset`. Then CRI-O restore spent about 4 minutes timing out on each stale container's
exit file before the kubelet could connect.

**Fixed: slow recovery after a reboot.** Same test each time: hard reset of node 2 with 14 live containers, timed
until the node is Ready.

| CRI-O build | Reset to Ready | Exit-file timeouts |
|---|---|---|
| `9ec045350` (before) | about 2 min | 25 |
| `c49dd0035` (after) | 26 s | 0 |

The two causes and their fixes (`patches/cri-o/0013`–`0014`):
- **Upstream CRI-O `88e8f422b` (March 2026), not FreeBSD-specific.** It waits about 5 s with backoff for a container's
  exit file whenever the runtime's `state` command fails. After a reboot that is every container, and no exit file
  will ever appear. CRI-O now detects the reboot the same way `crio wipe` does (the version file is gone) and skips
  that wait during restore.
- **FreeBSD:** `/var/run` lives on the root filesystem. `cleanvar` removes only plain files, so per-container
  directories and links built up: 140 entries in `/var/run/crio` on a node with 14 containers. The CRI-O rc.d script
  now mounts `/var/run/containers/storage`, `/var/run/crio` and `/var/run/ocijail` as tmpfs, so a reboot empties
  them the way `/run` is emptied on Linux. It only does this at boot or on an empty directory, so a live node never
  hides its running containers' state.

**What did not reproduce it:**
- 16 cycles of 100 nginx pods created and deleted at once on node 2;
- 12 cycles of 80 pods, with all the Linux workloads moved onto node 2 and continuous HTTP load on the Node.js pod
  (the musl `FUTEX_REQUEUE` path);
- 6 more cycles that took `kern.lastpid` through the 99999 wraparound.

**What did not cause it:** CRI-O's "Pid for infra container not found" errors. Node 1 logs them routinely during
pod bursts (217 in one hour of run 8).

**Instrumented for next time** (both nodes):
- `debug.kdb.alt_break_to_debugger=1` (persisted in `sysctl.conf`), so `CR ~ ^B` on the serial console drops into
  DDB for `ps`, `alltrace` and `show lockedvnods`;
- a 15-second state line on the console (pids, jails, interfaces, free pages, sockets, files, mbufs), which survives
  a network and disk wedge because QEMU writes `console.log` on the host.

Not yet re-armed after reboots: the logger lives in `/tmp`.

## Run 8: 424/446, the one new failure was clock skew (2026-11-16)

This run checked:
- `kernel.bsdk8s4` (the Linuxulator per-filter epoll registration);
- ocijail releasing stopped containers' jails and volumes (research/12);
- bsd-placement with 2 replicas and the reconciler, left running during the run.

The webhook rewrote no e2e pods: none pin `kubernetes.io/os=linux`.

**21 of 22 failures are identical to run 7** (`conformance/run8-failures.json`). The new one, `[sig-apps] Deployment
should support rollover`, was **not** a regression:
1. 16:50–16:58 UTC: the Mac went into idle sleep on battery, freezing both VMs.
2. Afterwards both guests were 35–43 minutes behind. Node 1's ntpd had exited: `Clock offset exceeds panic threshold.
   Set system clock by hand.` Node 2's was "running free". The nodes were also minutes apart from each other.
3. The test needs `minReadySeconds`. The controller-manager on node 1 compared it with a Ready time that node 2's
   kubelet stamped minutes in the future, so the new pod never became available.
4. Run alone with skewed clocks, it failed again. After fixing ntpd and resyncing, it passed **3/3** in 16–21 s.

Fix (docs/MAC-DEV-VM.md, VM script): `tinker panic 0 stepout 30` and `maxpoll 6` in `ntp.conf`; use `caffeinate`
for long runs. Corrected for that, run 8 matches run 7: **425/446, no regressions** from the kernel or ocijail
changes.

## Run 7: regression check, same 21 failures as run 6 (2026-11-16)

This run checked everything since run 6:
- both nodes on `kernel.bsdk8s` (rctl hierarchy fix);
- pod-level limits and in-place pod-level resize;
- the node memory accounting fixes;
- the CRI-O boot-relative PID start time.

The failure set is **identical** to run 6's (`conformance/run7-failures.json`). Node 1 never dropped below 5.2 GB
free and never reported memory pressure.

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
