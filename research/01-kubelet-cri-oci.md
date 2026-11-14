# 01: Node agent stack (kubelet, CRI, container runtime, OCI runtime) on FreeBSD

Scope: what it takes to run a real Kubernetes worker on FreeBSD, with jails as the main container primitive, Linux images running under the Linuxulator, and bhyve microVMs as an optional isolation tier. The goal is that kubectl, Helm, controllers and operators work against these nodes without changes.

Sources are the shallow clones listed in `UPSTREAM-PINS.md`, taken 2026-11-13: kubernetes `b4226aca`, containerd `40ea313`, runj `cb4e1b9`, ocijail `d7a2e71`, runtime-spec `f6cd17e`, podman `a6a942f` and freebsd-src `f429c4a6e` (`__FreeBSD_version 1600031`). The local toolchain is Go 1.27.3 on darwin/arm64.

---

## 1. Executive summary

1. **The Kubernetes control plane and node binaries compile for FreeBSD with a 3-line patch.** Cross-compiling for `freebsd/amd64` with `CGO_ENABLED=0`:
   - `kubectl`, `kube-scheduler` and `kube-proxy` build with no changes.
   - `kubelet`, `kube-apiserver` and `kube-controller-manager` each fail on a single error: `pkg/securitycontext/util.go:217: undefined: possibleCPUs`.
   - Adding `pkg/securitycontext/util_freebsd.go`, a copy of `util_darwin.go` that returns `nil`, fixes all three. After that `kubelet` builds for both amd64 and arm64 as a static FreeBSD ELF of about 115 MB.
   - `kubeadm` still fails in `cmd/kubeadm/app/preflight`.
2. **Compiling is not the same as running.** On FreeBSD the kubelet uses the generic `!linux && !windows` stubs. They return errors, and the kubelet exits during startup:
   - `cadvisor.MachineInfo()` returns `errUnsupported`. `cmd/kubelet/app/server.go:864` returns that error, and `NewMainKubelet` makes the same call (`pkg/kubelet/kubelet.go:729`).
   - Even past that, `cadvisor.Start()` and `ContainerManager.Start()` return "unsupported" errors, and the kubelet calls `os.Exit(1)` (`pkg/kubelet/kubelet.go:1913-1933`).
   - `k8s.io/mount-utils` returns `errUnsupported` for every mount operation (`staging/src/k8s.io/mount-utils/mount_unsupported.go:42-90`).
   - The kubelet sends **no `LinuxContainerConfig`** for containers on "other" OSes. `kuberuntime_container_unsupported.go` is a no-op, so `runAsUser`, resources and similar settings never reach the runtime.
3. **Windows is a complete template for this port.** Windows support is about 8.8k lines of `windows`-tagged code in the node packages; the largest pieces are a cAdvisor stand-in (`pkg/kubelet/winstats`), a container manager with no cgroups (`cm/container_manager_windows.go`) and a CRI stats provider. A FreeBSD port needs the same pieces:
   - a `bsdstats` package built on sysctl and RACCT;
   - a container manager based on rctl;
   - a FreeBSD mounter;
   - subpath handling;
   - CRI container config;
   - stats.

   Estimate: **3.5k–5.5k new lines in kubelet and mount-utils**, plus a kube-proxy backend (pf or ipfw, 2k–5k lines, covered in the networking report).
4. **The API server already accepts FreeBSD nodes and pods, except for one field.** `kubernetes.io/os=freebsd` is just a label: the kubelet sets it from `runtime.GOOS`, and both the scheduler and kubelet admission compare against `runtime.GOOS`. The exception is **`pod.spec.os.name`**, which is restricted to `linux`/`windows` (`pkg/apis/core/validation/validation.go:127` and `:9247`). Pods that leave `spec.os` empty work fine.
   - Phase 1 does not need a KEP: use `nodeSelector` with taints and tolerations.
   - Proper `spec.os.name: freebsd` support and a FreeBSD section in the CRI API need **a KEP**. Both are long-lead items.
5. **containerd is the closest runtime, but the hard parts are still missing on FreeBSD.**
   - What exists: `containerd` with the **CRI plugin** compiles for FreeBSD unchanged, because `cmd/containerd/builtins/cri.go` is tagged only `!no_cri`. The ZFS snapshotter is built in (`builtins_freebsd.go`). `runtime_platforms` lets a RuntimeClass handler pull `linux/amd64` images, which is the hook for Linuxulator pods.
   - What is missing: on FreeBSD `pkg/netns` is a stub (`netns_other.go`: "not implemented on unix"), so **every pod that does not use hostNetwork fails in RunPodSandbox**. Sandbox and container spec options are empty stubs. Port-forward and sandbox stats return `ErrNotImplemented`. containerd never fills in the runtime-spec `freebsd` section.
6. **The OCI runtimes are experimental.**
   - runtime-spec **v1.3.0 now has an official `freebsd` section** (`config-freebsd.md`), including `jail.parent` for sharing a vnet.
   - **runj** calls itself a "proof-of-concept… not evaluated for its security". It has a containerd shim, but the shim does not implement `Stats`, `Update`, `Pids`, `CloseIO`, `Pause` or `Checkpoint`. runj also does not implement `jail.parent`.
   - **ocijail** (C++, Bazel) is the more complete runtime and is used by Podman. It supports a parent jail through the annotation `org.freebsd.parentJail`, which is how Podman runs pods on FreeBSD. ocijail has no containerd shim, and containerd's `containerd-shim-runc-v2` is Linux-only ("build constraints exclude all Go files").
7. **The pod model maps well onto FreeBSD.** Use a persistent jail with `vnet=new` as the pod sandbox ("pause"), with each container as a child jail that inherits the parent's vnet.
   - The kernel supports this directly: `kern_jail.c:1880-1885` either allocates a new vnet or sets `pr->pr_vnet = ppr->pr_vnet`.
   - Podman already does exactly this (`libpod/container_internal_freebsd.go:175-222`, `libpod/networking_freebsd.go:89-112`).
   - FreeBSD 16 adds jail descriptors (`sys/sys/jaildesc.h`), which are the natural equivalent of a netns handle.
8. **Recommendation: port the real kubelet; do not use virtual-kubelet.** A virtual-kubelet provider looks cheaper. In practice it would make us rewrite volumes, probes, the pod lifecycle, the stats and summary API, eviction and device plugins, and it would never pass conformance. The real kubelet compiles today. The work is concentrated in three areas:
   - a `freebsd` platform layer in kubelet and mount-utils, following the Windows pattern;
   - containerd CRI FreeBSD support (netns as a vnet jail, spec options, stats);
   - a production-quality shim and OCI runtime, either by hardening the runj shim or by writing a Go shim for ocijail.

   Use virtual-kubelet only as a throwaway demo or for the bhyve microVM tier, if that tier never needs kubelet semantics.

---

## 2. Cross-compile results (question 2)

Command, run from the kubernetes repo:
`GOTOOLCHAIN=auto GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/<x>`

| Binary | Unmodified tree | Notes |
|---|---|---|
| kubectl | **PASS** (6 s) | |
| kube-scheduler | **PASS** (8 s) | |
| kube-proxy | **PASS** (2 s) | Runtime: every platform hook returns `"freebsd/amd64is unsupported"` (`cmd/kube-proxy/app/server_other.go:38-60`, `init_other.go:27`). It starts and then fails. |
| kubelet | **FAIL**: 1 error | `pkg/securitycontext/util.go:217:23: undefined: possibleCPUs` |
| kube-apiserver | **FAIL**: 1 error | same error |
| kube-controller-manager | **FAIL**: 1 error | same error |

**After adding `pkg/securitycontext/util_freebsd.go`** (`func possibleCPUs() []int { return nil }`, copied from `util_darwin.go`):

| Binary | Result |
|---|---|
| kubelet freebsd/amd64 | **PASS**: `ELF 64-bit LSB executable, x86-64, (FreeBSD), statically linked`, 120 MB |
| kubelet freebsd/arm64 | **PASS**: 113 MB |
| kube-apiserver, kube-controller-manager | **PASS** (built with `-gcflags=-e`, no further errors) |
| kubeadm | **FAIL**: `cmd/kubeadm/app/preflight/checks.go:995: MemCheck does not implement Checker (missing method Check)` and `:1155: undefined: addExecChecks` |

`possibleCPUs` is Linux-specific: it parses `/sys/devices/system/cpu/possible`. A proper FreeBSD version would read `kern.smp.maxcpus` or use `cpuset_getaffinity`. The stub is an upstreamable, one-file PR.

Related builds from the same session, all cross-compiled with `CGO_ENABLED=0`:

| Component | Result |
|---|---|
| containerd `./cmd/containerd` (CRI plugin included) | **PASS** |
| containerd `./cmd/ctr` | **PASS** |
| containerd `./cmd/containerd-shim-runc-v2` | **N/A**: Linux-only package |
| runj `./cmd/...` (runj, `containerd-shim-runj-v1`, runj-entrypoint) | **PASS** |
| cri-tools `./cmd/crictl` | **PASS** |
| k3s `./cmd/k3s` | **FAIL**: `pkg/cli/cmds/agent.go:168:16: undefined: DefaultSnapshotter` (first error only; k3s is deeply Linux-specific) |

Caveat: the FreeBSD ports tree already packages `net/kube-apiserver` and friends. The FreshPorts entry still says "Kubelet hasn't been ported to FreeBSD" ([freshports](https://freshports.org/net/kube-apiserver)). The **build** blocker for the kubelet is now trivial. The **runtime** blockers are the real work.

---

## 3. How the kubelet is split by build tags (question 1)

Build constraints tallied over `pkg/kubelet`, `cmd/kubelet`, `pkg/volume`, `pkg/proxy`, `cmd/kube-proxy` and `staging/src/k8s.io/mount-utils`. The counts exclude tests and only include files with a `//go:build` header; files selected by filename suffix alone add a few more.

| Constraint | Files |
|---|---|
| `linux` | 68 files, **18,411 lines** (about 9k of these are kube-proxy iptables/ipvs/nftables) |
| `windows` | 44 files, **8,783 lines** |
| `!linux` | 24 |
| `!linux && !windows` (the "others" stubs that FreeBSD gets today) | 19 |
| explicitly mention freebsd | 3: `pkg/kubelet/util/boottime_util_freebsd.go` (uses `CLOCK_UPTIME`), `util_unix.go`, and `util_unsupported.go` (which excludes freebsd) |

The kubelet already has a little FreeBSD-specific code: `boottime_util_freebsd.go`, plus `pkg/util/flock` and `pkg/util/filesystem/util_unix.go`. Someone has touched this before.

### 3.1 Per-subsystem map: what FreeBSD gets today and what it needs

| Subsystem | Linux impl | Windows impl | What FreeBSD gets today | FreeBSD port needs | Est. LOC |
|---|---|---|---|---|---|
| **cAdvisor shim** `pkg/kubelet/cadvisor` | `cadvisor_linux.go` (215) uses `github.com/google/cadvisor/lib v0.60.6` | `cadvisor_windows.go` (84) wraps `pkg/kubelet/winstats` (~1,380) | `cadvisor_unsupported.go`: everything returns `errUnsupported`, so **the kubelet exits at startup** | `cadvisor_freebsd.go` plus a new `pkg/kubelet/bsdstats` package. MachineInfo from `hw.ncpu`, `hw.physmem`, `kern.hostuuid` and NUMA from `vm.ndomains`. Node CPU and memory from `kern.cp_times` and `vm.stats.*`. Root and image fs info from `statfs` and ZFS properties. Per-jail usage from RACCT (`rctl -u jail:<name>`, `racct_get_usage`). | 1,200–1,800 |
| **Container manager** `pkg/kubelet/cm` | cgroup v1/v2, QoS, node allocatable, cpu/mem/topology managers (~3k lines) | `container_manager_windows.go` (413): no cgroups, but sets up cpu/memory/topology managers and device plugins | `container_manager_unsupported.go`: `Start()` returns an error, so the kubelet calls `os.Exit(1)` (`kubelet.go:1930-1933`) | `container_manager_freebsd.go` modelled on the Windows one, with `CgroupsPerQOS=false` forced. Optionally enforce node allocatable as an rctl rule on a "kubepods" parent jail. Device plugin manager unchanged. cpuset through `cpuset(1)` per jail. | 400–800 |
| **kuberuntime** | `kuberuntime_container_linux.go` (669) builds `LinuxContainerConfig` (resources, security context, user) | `_windows.go` | `kuberuntime_container_unsupported.go`: **no platform config sent**, so `runAsUser`, `runAsGroup`, readOnlyRootFilesystem, CPU and memory limits are all dropped | `kuberuntime_container_freebsd.go`. Practical option: emit the CRI `LinuxContainerConfig` (resources, security context) and let the FreeBSD runtime translate it to jail params and rctl. Clean option: a CRI `FreeBSDContainerConfig` (needs a KEP). | 300–600 |
| **Stats** `pkg/kubelet/stats` | `cri_stats_provider_linux.go` (153) | `cri_stats_provider_windows.go` | `cri_stats_provider_others.go`: every `addCRIPod*Stats` is a no-op, so pod stats are empty and the metrics-server, HPA and eviction all see nothing | `cri_stats_provider_freebsd.go`, roughly the Linux file. Also needs CRI `ListPodSandboxStats` implemented in containerd (see §5). | 150–300 |
| **Eviction** | `defaults_linux.go`, `threshold_notifier_linux.go` (memcg notifications) | `defaults_windows.go` | polling-only defaults (`defaults_others.go`, `threshold_notifier_unsupported.go`) | Polling is acceptable. Defaults need tuning (no `memory.available` from cgroups, so use `vm.stats.vm.v_free_count` and inactive pages). | 50–150 |
| **mount-utils** `staging/src/k8s.io/mount-utils` | `mount_linux.go` (923), `resizefs_linux.go` | `mount_windows.go` | `mount_unsupported.go`: **Mount, Unmount, List, IsLikelyNotMountPoint and IsMountPoint all return errors**. Volume setup and teardown break, including the emptyDir/configMap/secret mount-point checks. | `mount_freebsd.go`: List through `getfsstat(2)` (`unix.Getfsstat`). Mount through `nmount(2)`, or `mount(8)` as containerd's `core/mount/mount_freebsd.go` does. `unmount(2)`. Bind mounts become `nullfs`. tmpfs exists. `resizefs` for UFS/ZFS can come later. | 400–700 |
| **Volumes** `pkg/volume/util/{subpath,hostutil,fsquota}`, `emptydir` | `subpath_linux.go` (614) uses `openat` and `/proc/self/fd` safety; fsquota uses XFS/ext4 project quotas | Windows variants | `subpath_unsupported.go` errors, so **any `subPath` mount fails**. `hostutil_unsupported` errors. No fsquota. emptyDir `medium: Memory` is unsupported. | `subpath_freebsd.go` (openat with `O_RESOLVE_BENEATH`, which FreeBSD has, plus nullfs). `hostutil_freebsd.go`. emptyDir Memory as tmpfs. fsquota could use ZFS dataset quotas, but leave it out at first. | 500–900 |
| **Node shutdown** | systemd inhibit locks | Windows service preshutdown | no-op (`nodeshutdown_manager_others.go`) | Optional: hook rc.d shutdown or `devd` events. Can be dropped at first. | 0–300 |
| **Node status / labels** | | | `kubelet_node_status_others.go`: generic; `GoRuntime` setter writes `OperatingSystem=freebsd` | Nothing. Labels become `kubernetes.io/os=freebsd`, `kubernetes.io/arch=amd64` automatically. | 0 |
| **Config validation** | | | `validation_others.go` already rejects `singleProcessOOMKill`, `userNamespaces` and `defaultPodSysctls` | Nothing, or a small amount. | ~20 |
| **cmd/kubelet** | `server_linux.go` (inotify lockfile, cgroup v2 check) | | `server_unsupported.go`: `watchForLockfileContention` returns an error, which only matters if `--lock-file` is used | kqueue-based lockfile watch | 50 |
| **kube-proxy** | iptables (1.5k), ipvs (1.8k), nftables (2.0k) | winkernel (in other files) | `server_other.go`: unsupported | A pf (anchors plus tables, rdr) or ipfw (nat/table) proxier. Alternatively drop kube-proxy and use a CNI-integrated service implementation (see the networking report). | 2,000–5,000 |
| **securitycontext** | `util_linux.go` | `util_windows.go` | **missing**, which breaks the build | `util_freebsd.go` | 3–30 |

**Total for the kubelet, mount-utils and volumes, excluding kube-proxy:** about **3.5k–5.5k lines** of new Go plus tests. Windows reached the same functional level with about 8.8k lines across these trees, plus `winstats`. This is a quarter-scale project for one or two engineers who know both codebases. The bigger cost is getting it upstream: SIG Node would need to accept a third OS, and the precedent (Windows) took a KEP, a SIG-Windows-style group and CI jobs.

### 3.2 cAdvisor dependency

The kubelet now depends on `github.com/google/cadvisor/lib v0.60.6` (`go.mod:34`), a split-out library module. The Windows path never imports cAdvisor's Linux collectors; it only uses the `cadvisorapi` model types. FreeBSD should do the same: implement the `cadvisor.Interface` (`pkg/kubelet/cadvisor/types.go`) in-tree from sysctl and RACCT data, and never port cAdvisor itself. With `PodAndContainerStatsFromCRI` set (still Beta and **off by default** in 1.37, `pkg/features/kube_features.go:1827-1830`), per-container stats come from the CRI runtime. cAdvisor is then only needed for machine and node data. Enable that gate on FreeBSD nodes.

---

## 4. OS identity: `kubernetes.io/os=freebsd` and `spec.os` (question 3)

- **Node side.** The kubelet sets `kubernetes.io/os` and `beta.kubernetes.io/os` to `goruntime.GOOS` (`pkg/kubelet/kubelet_node_status.go:317-319` and `:574-579`), and `NodeInfo.OperatingSystem` likewise (`pkg/kubelet/nodestatus/setters.go:429`). On FreeBSD that is `freebsd`, with no code change. Label values are free-form, so the API server accepts them.
- **Kubelet admission.** `pkg/kubelet/lifecycle/predicate.go:314-339` rejects a pod if its `kubernetes.io/os` nodeSelector or its `spec.os.name` differs from `runtime.GOOS`. A pod selecting `kubernetes.io/os: freebsd` is admitted.
- **API validation.** `pkg/apis/core/validation/validation.go:127` has `var validOS = sets.New(core.Linux, core.Windows)`. `validateOS` at `:9238-9250` returns `NotSupported` for anything else. The comment at `:123-126` explicitly anticipates expanding this to "values from runtime-spec platform-specific configuration", and runtime-spec now has `freebsd`. The constants are in `staging/src/k8s.io/api/core/v1/types.go:5012-5013`.
- **Scheduler.** It does not read `spec.os` at all: grepping `pkg/scheduler` finds nothing. OS placement is purely nodeSelector or affinity on the label.
- **PodSecurity admission.** The `restricted` profile exempts only `spec.os.name == windows` from the seccomp and capability checks (`staging/src/k8s.io/pod-security-admission/policy/check_seccompProfile_restricted.go:151`). FreeBSD pods in `restricted` namespaces must therefore *carry* `seccompProfile: RuntimeDefault` and `capabilities.drop: [ALL]`. The FreeBSD runtime must **accept and ignore** them, or map them to jail `allow.*` restrictions. It must not reject them.
- **Ecosystem effect.** Most Helm charts and operators do not set `spec.os` but do set `nodeSelector: kubernetes.io/os: linux`. On a mixed cluster this keeps Linux workloads off FreeBSD nodes, which is good. It also means **DaemonSets from popular charts will not land on FreeBSD nodes** unless they are retargeted. Add a `node.kubernetes.io/os=freebsd:NoSchedule` taint as defence in depth, the same pattern Windows uses.

**Is a KEP needed?**
- Phase 1 (pods without `spec.os`, nodeSelector on the label): **no**.
- Phase 2, adding `OSName FreeBSD` to the API and PSA plus a `FreeBSDContainerConfig` / `FreeBSDPodSandboxConfig` in the CRI (`cri-api .../api.proto` has only `LinuxContainerConfig linux = 15` and `WindowsContainerConfig windows = 16`): **yes**, a SIG Node and SIG Architecture KEP. Without it the API server cannot represent "this pod needs FreeBSD", and Linuxulator pods remain ambiguous.

  Proposed semantics: a Linux image on a FreeBSD node keeps `spec.os=linux` (it is a Linux ABI workload) and selects a `RuntimeClass` whose `scheduling.nodeSelector` targets FreeBSD nodes. That needs **no** API change, because `spec.os` describes the container's OS, not the host's.

---

## 5. containerd on FreeBSD (question 4)

### 5.1 What exists

- **FreeBSD-specific files:**
  - `cmd/containerd/builtins/builtins_freebsd.go` registers the walking differ and the **ZFS snapshotter** (`github.com/containerd/zfs/v2/plugin`).
  - Other files: `core/mount/mount_freebsd.go` (shells out to `mount(8)`), `defaults/defaults_freebsd.go`, `pkg/archive/{tar,link}_freebsd.go`, `pkg/oci/mounts_freebsd.go`, `pkg/shim/shim_freebsd.go`, `plugins/snapshots/native/native_freebsd.go`, `plugins/content/local/store_bsd.go`.
- **The CRI plugin builds on FreeBSD.** `cmd/containerd/builtins/cri.go` is tagged `!no_cri`, with no OS restriction. Cross-build: PASS.
- **Linux images on a FreeBSD host.** Image pull is per-runtime-platform: `internal/cri/config/config.go:270-312` defines `ImagePlatform`, `RuntimePlatforms map[string]ImagePlatform` and `toml:"runtime_platforms"`. That lets `RuntimeClass` handler `linux` pull `linux/amd64` manifests, which is how Linuxulator pods get Linux images. The default platform is `freebsd/amd64`, so without this mapping almost every Docker Hub image fails with "no match for platform".

### 5.2 What is stubbed (`internal/cri`, files tagged `!windows && !linux` or `!linux`)

| File | Behaviour on FreeBSD | Consequence |
|---|---|---|
| `pkg/netns/netns_other.go:33-41` | `NewNetNS` returns `"not implemented on unix"` | **`RunPodSandbox` fails for every non-hostNetwork pod** (`internal/cri/server/sandbox_run.go:240`) |
| `internal/cri/server/podsandbox/sandbox_run_other.go` | Sandbox spec has only annotations. No `/etc/hosts`, `resolv.conf` or hostname files (`setupSandboxFiles` is a no-op). | DNS and hostname inside pods are broken |
| `internal/cri/server/container_create_other.go` | `containerSpecOpts` returns empty | No user, mounts, devices, resources or jail params. **The `freebsd` section of the OCI spec is never populated anywhere in containerd** (grep: no `.FreeBSD` usage outside specs-go). |
| `sandbox_portforward_other.go` | `ErrNotImplemented` | `kubectl port-forward` fails |
| `sandbox_stats_other.go`, `podsandbox/sandbox_stats_other.go`, `stats_collector_other.go`, `list_pod_sandbox_metrics_other.go` | `ErrNotImplemented` | No pod or container metrics |
| `container_update_resources_other.go` | no-op | No in-place resize |
| `nri_other.go`, `nri/nri_api_other.go` | stubs | No NRI plugins |

**Verdict: containerd CRI on FreeBSD is not usable today** beyond hostNetwork pods with trivial specs. The fix stays inside the existing build-tag structure. Add `*_freebsd.go` siblings:

- `pkg/netns/netns_freebsd.go`: create a persistent `vnet=new` jail named after the sandbox ID, "path" = jail name (or a jail descriptor in FreeBSD 16); `Remove` = `jail_remove`. This is Podman's `createNetNS`. About 200 lines.
- `podsandbox/sandbox_run_freebsd.go`: write hosts, resolv.conf and hostname files and nullfs-mount them; set `freebsd.jail.vnet=new` or `parent`. About 300 lines.
- `container_create_freebsd.go`: map CRI `LinuxContainerConfig` / security context to the OCI process user, `freebsd.jail.parent=<sandbox jail>`, `jail.allow.*`, devfs ruleset and mounts (nullfs/tmpfs/devfs), and rctl annotations. About 500–700 lines.
- Stats from RACCT: about 300 lines. Port-forward: either a `LockOSThread` goroutine that calls `jail_attach` (the goroutine then exits so the thread is discarded), or a `jexec <jail> socat` helper. About 150 lines.
- CNI: go-cni and libcni pass `CNI_NETNS=<jail name>`. This needs FreeBSD CNI plugins that interpret it that way. Doug Rabson's patched set is in ports as [`net/containernetworking-plugins`](https://freshports.org/net/containernetworking-plugins), covering bridge, loopback, host-local, firewall and portmap, and containers/common sets up lo0 with `ifconfig -j` (`containers-common/libnetwork/cni/run_freebsd.go`). Upstream `containernetworking/plugins` has **no** freebsd files. Details are in the networking report.

Total containerd work: **about 2k–3k lines**, all upstreamable as FreeBSD build-tagged files. containerd maintainers have previously accepted FreeBSD patches (runj and the ZFS snapshotter).

### 5.3 OCI runtime-spec

[runtime-spec v1.3.0](https://github.com/opencontainers/runtime-spec/blob/main/config-freebsd.md) added `config-freebsd.md` (ChangeLog: "config-freebsd: add the spec for FreeBSD (#1286)"). Go types are in `specs-go/config.go:1011-1081`:

- `FreeBSD{Devices, Jail}`
- `FreeBSDJail{Parent, Host, Ip4, Ip4Addr, Ip6, Ip6Addr, Vnet, Interface, VnetInterfaces, SysVMsg/Sem/Shm, EnforceStatfs, Allow}`
- Sharing modes `disable`, `new`, `inherit`

`parent` is documented as the way "to share namespaces such as vnet with another container", which is exactly the pod model. **The spec has no resources section for FreeBSD**: there is no rctl or racct mapping, and runj's coverage document lists "kernel rctl(8) limits" as unimplemented. A runtime-spec PR for `freebsd.resources` (an rctl rule list) is needed. Until then, use annotations.

### 5.4 runj (Samuel Karp)

- **Status.** The README says it is a "proof-of-concept… has not been evaluated for its security. Do not use runj on a production system." The last commit is 2026-08-17 ("support runtime-spec allow property"), so the project is alive but has low velocity.
- **Coverage** (`docs/spec-coverage.md`, against spec 1.3.0). Implemented: create/start/kill/delete/state, process user/rlimits, mounts, `jail.vnet`, `vnetInterfaces`, ip4/ip6, host, sysv*, enforceStatfs. **Not implemented: `jail.parent` (shared vnet)**, `jail.allow.*` (listed as unimplemented, though the last commit started on it), `freebsd.devices`, `root.readonly`, rctl, and most hooks. It also "ignores configuration it does not implement rather than returning an error".
- **Shim** `containerd-shim-runj-v1` (`containerd/shim.go`). Exec is implemented (`:658`). `Pids`, `Pause`, `Resume`, `Checkpoint`, `CloseIO`, `Update` and `Stats` return `ErrNotImplemented` (`:606-623`, `:741-780`). Missing `CloseIO` breaks `kubectl exec -i`/`kubectl cp` stdin EOF. Missing `Stats` breaks metrics. Missing `Update` breaks resize.
- **Strength.** It is Go and already speaks containerd shim v2. It is the shortest path to a working CRI stack.

### 5.5 ocijail (Doug Rabson)

- **Status.** The README calls it "an experimental OCI runtime intended to be used with buildah and/or podman". It is about 2.8k lines of C++ built with Bazel, and the last commit is 2026-10-16. It is the runtime **actually in use** by FreeBSD Podman users through `sysutils/podman-suite`.
- **Pod support.** It reads the annotation `org.freebsd.parentJail`, creates the container as `<parent>.<id>`, and bumps the parent's `children.max` (`ocijail/create.cpp:233-236`, `:306-307`, `:449-455`). It also supports `org.freebsd.jail.vnet=new|inherit`, `org.freebsd.jail.allow.*` and sysv* sharing (`:225-300`). It uses **annotations, not the new `freebsd.jail.*` spec fields**, so it needs updating to spec 1.3.0. No rctl.
- **Gap.** It has no containerd shim. It is runc-CLI-compatible, but `containerd-shim-runc-v2` is Linux-only (`cmd/containerd-shim-runc-v2`: "build constraints exclude all Go files"). Making the runc shim build on FreeBSD (it depends on cgroups, the Linux reaper, pidfd and similar) is a sizeable port. Writing a small Go shim (`containerd-shim-ocijail-v2`) by forking runj's shim and replacing the runj CLI calls with ocijail is cheaper.

### 5.6 What already works in the ecosystem

Podman on FreeBSD (`libpod/*_freebsd.go`, 20 files) implements pods:

- `createNetNS` builds a `vnet=new`, persistent jail per pod or container (`libpod/networking_freebsd.go:89-112`).
- Containers either get `org.freebsd.jail.vnet=new`, on FreeBSD 13.3 and later where `PostConfigureNetNS` is true, or `org.freebsd.parentJail=<netns jail>` (`container_internal_freebsd.go:205-222`).
- `addNetworkContainer` joins another container's network jail (`:175-188`). That is `--network container:X`, the exact semantics a Kubernetes pod needs.

**containerd CRI on FreeBSD should copy this design wholesale.**

Prior art for multi-node networking: the AsiaBSDCon 2026 paper "Bring Cloud-Native Networking to FreeBSD Jails: Porting Calico from Linux" (Sakaguchi et al., Future University Hakodate and SAKURA internet) replaces iptables with ipfw, netlink with routing sockets, and netns with vnet ([PDF](https://2026.asiabsdcon.org/media/entry/question_uploads/final-paper_3tMo1OJ.pdf)). The same group runs Linux containers in jails with the Linuxulator and Capsicum.

---

## 6. Pod model on FreeBSD (question 5)

```
pod sandbox  = jail "k8s_<podUID>"  persist, vnet=new, path=/ (or empty root), children.max=N
               └── CNI runs against this jail: epair/bridge or vxlan, IPs, routes, lo0 127.0.0.1
container A  = jail "k8s_<podUID>.<ctrA>"  vnet inherit (shares the pod's network stack)
               path=<zfs clone rootfs>, devfs ruleset 4, nullfs volume mounts,
               host=new (hostname=pod name), sysvipc=new|inherit, enforce_statfs=2
container B  = jail "k8s_<podUID>.<ctrB>"  (same vnet, so localhost works between containers)
```

Kernel evidence (`sys/kern/kern_jail.c`):
- `:1880-1885`: `if (pr_flags & PR_VNET) { pr->pr_vnet = vnet_alloc(); } else { pr->pr_vnet = ppr->pr_vnet; }`. A child jail without `vnet` **inherits the parent's vnet**, so containers share the pod's network stack, loopback and IP. This is the netns-sharing guarantee Kubernetes relies on.
- `:1270-1273`: "vnet cannot be changed after creation", so the decision is fixed at create time. That matches CRI, where the sandbox is created first.
- `:1760`, `:1953`, `:2125-2131`: `children.max` is enforced hierarchically. The parent must have `children.max` set to at least the number of containers; ocijail bumps it on demand.
- `:1527-1528`: the child's `path` is resolved with `namei` from the **caller's** context (the host). Child roots can therefore live anywhere on the host filesystem, such as ZFS clones, and do not need to be under the parent's root.
- FreeBSD 16 has **jail descriptors** (`sys/sys/jaildesc.h`; `kern_jail.c:1150-1170` handles `JAIL_GET_DESC`/`JAIL_OWN_DESC`). These are a race-free handle equivalent to an `/proc/<pid>/ns/net` fd. Use them in `netns_freebsd.go` when targeting 15.x or later; check whether they exist in 15.0 or only in 16.

Namespace mapping and gaps:

| k8s / Linux namespace | FreeBSD equivalent | Status |
|---|---|---|
| net (shared in pod) | vnet on the sandbox jail, inherited by children | **works** (kernel plus ocijail/Podman) |
| ipc (shared in pod) | `sysvmsg/sysvsem/sysvshm=new` on the sandbox, `inherit` on children | works (spec fields exist) |
| uts | `host=new`, hostname | works |
| pid (`shareProcessNamespace`) | Processes in child jails are visible from the parent jail, but not between sibling jails | **partial**: the shareProcessNamespace semantics are not available. Drop the feature or put all containers in one jail (which loses per-container rootfs). |
| mount | per-jail root plus nullfs; `enforce_statfs=2` | works, but nullfs mounts are made on the host |
| user (`hostUsers: false`) | none (no uid mapping in jails) | **drop** |
| cgroup (resources) | RACCT/RCTL per jail. In GENERIC but `RACCT_DEFAULT_TO_DISABLED`, so it **needs `kern.racct.enable=1` in loader.conf** (`sys/amd64/conf/GENERIC:92-94`, `kern_racct.c:68-71`) | partial: `memoryuse`, `pcpu`, `maxproc`, `nthr`, `readbps` and others exist (`kern_rctl.c:152-169`). There is **no CPU shares/weight**: `pcpu` is a hard cap, so CPU *requests* cannot be expressed. There is no OOM-kill semantic equivalent to memcg, so OOMKilled reporting would be synthetic. |
| seccomp / capabilities / AppArmor / SELinux | jail `allow.*`, securelevel, MAC (`mac_bsdextended`, `mac_portacl`), Capsicum (not usable for arbitrary images) | **accept and ignore** the Linux fields; map a small subset to `allow.*` |
| sysctls (`securityContext.sysctls`) | a few per-jail sysctls under vnet (`net.inet.*`) | partial: allow a small list |

---

## 7. Blockers (question 6)

| # | Blocker | Severity | Proposed solution | Effort |
|---|---|---|---|---|
| B1 | `possibleCPUs` missing, so kubelet, kube-apiserver and KCM fail to build for freebsd | Low (trivial) | Add `pkg/securitycontext/util_freebsd.go` (read `kern.smp.maxcpus`); upstream PR | 1 day |
| B2 | cAdvisor stub, so the kubelet exits at startup (`server.go:864`, `kubelet.go:729/1913`) | **Critical** | `pkg/kubelet/bsdstats` plus `cadvisor_freebsd.go`, following the winstats pattern (sysctl, RACCT, statfs, ZFS) | 3–5 weeks |
| B3 | ContainerManager stub, so `Start()` errors and the kubelet calls `os.Exit(1)` | **Critical** | `container_manager_freebsd.go` cloned from Windows; `CgroupsPerQOS=false`; optional rctl node-allocatable | 2–3 weeks |
| B4 | mount-utils unsupported, so volume setup and teardown fail | **Critical** | `mount_freebsd.go` (getfsstat, nmount, unmount, nullfs as bind mount) | 2–3 weeks |
| B5 | Kubelet sends no container platform config on "other" OSes, so security context and limits are lost | **High** | `kuberuntime_container_freebsd.go` emitting `LinuxContainerConfig` (pragmatic). KEP for a CRI FreeBSD section later. | 1–2 weeks (+ KEP) |
| B6 | subPath, hostutil and emptyDir(Memory) unsupported | High | `subpath_freebsd.go` (O_RESOLVE_BENEATH plus nullfs), `hostutil_freebsd.go`, tmpfs emptyDir | 2–3 weeks |
| B7 | Pod and container stats empty (`cri_stats_provider_others.go`, containerd stats stubs, runj shim `Stats` unimplemented) | High (HPA, eviction, `kubectl top`) | CRI stats from RACCT in containerd and the shim; enable `PodAndContainerStatsFromCRI` | 2–3 weeks |
| B8 | containerd `netns` stub, so every non-hostNetwork sandbox fails | **Critical** | `pkg/netns/netns_freebsd.go` = persistent vnet jail (Podman design); jail descriptors on 15+/16 | 1–2 weeks |
| B9 | containerd CRI spec options empty, and the `freebsd` OCI section is never populated | **Critical** | `*_freebsd.go` for sandbox and container spec opts: parent jail, hosts/resolv/hostname files, nullfs mounts, devfs ruleset, `allow.*`, rctl annotations | 3–4 weeks |
| B10 | No production containerd shim. runj shim missing `CloseIO`, `Stats`, `Update` and `Pids`. runj has no `jail.parent`. ocijail has no shim; runc shim is Linux-only. | **Critical** | Option A: harden runj and its shim (add parent, rctl, CloseIO, Stats, Update, error on unknown config). Option B: new Go shim `containerd-shim-ocijail-v2` from runj's shim plus ocijail. **Recommend B for the runtime, A's shim code as the base.** Security review either way. | 6–10 weeks |
| B11 | CNI plugins upstream have no FreeBSD support; ports carry patches | High | Upstream dfr's FreeBSD CNI plugin patches; Calico port prior art (AsiaBSDCon 2026). Networking report. | (networking) |
| B12 | kube-proxy has no FreeBSD backend | High | pf- or ipfw-based proxier, or a CNI-integrated service LB | 4–8 weeks (networking) |
| B13 | `kubectl port-forward` not implemented in CRI | Medium | `portForward` via `jail_attach` on a locked OS thread, or a `jexec`+socat helper | 3–5 days |
| B14 | `spec.os.name` only allows linux/windows; CRI API has no FreeBSD section; PSA exempts only Windows | Medium (workaround: nodeSelector, taints, RuntimeClass) | Phase 1: none needed. Phase 2: KEP adding `OSName "freebsd"`, CRI `FreeBSDContainerConfig`, and PSA treatment | KEP: 2–4 releases |
| B15 | Resource model mismatch: no CPU weight, no memcg OOM semantics, RACCT disabled by default | Medium | Require `kern.racct.enable=1`. Map limits to `pcpu`/`memoryuse` rctl deny rules; requests become scheduling-only. Document OOMKilled as best-effort. | 1–2 weeks plus docs |
| B16 | Linux images: default pull platform is `freebsd/*`; Linuxulator coverage gaps (no seccomp/inotify fidelity, some `/proc`/`/sys` gaps) | High for "standard ecosystem works unchanged" | containerd `runtime_platforms` (`RuntimeClass` `linux-compat` gives `linux/amd64`). Linux jails mount `linprocfs`, `linsysfs` and `fdescfs`. Maintain a curated "works under Linuxulator" image list. **Unsupported workloads go to the bhyve microVM RuntimeClass** (Kata-style shim, separate report). | 4–8 weeks plus an ongoing compatibility matrix |
| B17 | Dropped features: user namespaces, shareProcessNamespace, seccomp/AppArmor/SELinux, in-place resize (until B7/B10), swap control, memory QoS, topology manager fidelity, PSI | Low–Medium | **Drop explicitly.** Reject them at kubelet validation (`validation_freebsd.go`) or ignore them with events. Document. | 1 week |
| B18 | Conformance: e2e suites assume Linux images (busybox, agnhost) | Medium | Build FreeBSD variants of agnhost/pause/busybox (as Windows did), or run the conformance suite through Linuxulator RuntimeClass; tag `[LinuxOnly]` tests | 3–6 weeks |
| B19 | kubeadm preflight does not build | Low | Add freebsd preflight stubs (`MemCheck.Check`, `addExecChecks`) | 2–3 days |
| B20 | Upstream acceptance: SIG Node has no FreeBSD owner and no CI | High (strategic) | Keep a fork with all FreeBSD code in `*_freebsd.go` (no edits to shared files) to minimise rebase pain. Seek a SIG Node subproject or working group. Provide FreeBSD CI (Cirrus already runs runj CI). | ongoing |

---

## 8. Real kubelet vs virtual-kubelet

**Virtual-kubelet (VK).** Implement a VK `PodLifecycleHandler` (`CreatePod`, `UpdatePod`, `DeletePod`, `GetPod`, `GetPodStatus`, `GetPods`, plus optional logs, exec, attach, port-forward and stats) in Go, and drive jails directly through libjail/ocijail, ZFS and CNI.

| | Port real kubelet + containerd CRI | Virtual-kubelet provider |
|---|---|---|
| Time to first pod | Long: B2/B3/B4/B8/B9/B10 all have to land (~3–4 months for one or two engineers) | **Short** (~3–6 weeks for a demo) |
| API and ecosystem fidelity | **Full**: probes, volume plugins (configMap, secret, projected, downwardAPI, CSI), eviction, static pods, device plugins, DRA, logs, exec, attach, port-forward, `/stats/summary`, PodResources, graceful termination, init and sidecar containers, restartPolicy semantics | Provider must **reimplement** all of these. VK projects typically do not support probes, volumes, init containers or the summary API faithfully. Every Kubernetes release adds kubelet features (sidecars, in-place resize, pod-level resources, DRA) that must be cloned again. |
| Conformance | Achievable (Windows passes a subset) | Effectively impossible; VK nodes are explicitly non-conformant |
| Operators, Helm, DaemonSets, CSI node plugins | Work (CSI node plugins need FreeBSD builds) | DaemonSets and CSI node plugins usually unsupported; node seen as "virtual" |
| Upstream alignment | Uses the same extension points as Windows; parts upstreamable | Out of tree forever |
| Maintenance | Rebase `*_freebsd.go` files; low churn because the shared code is reused | High: chasing kubelet semantics by hand |
| Good fit for | Jail pods, Linuxulator pods | bhyve microVM "pods" fronted as a separate virtual node pool, or a quick demo |

**Recommendation.** Port the real kubelet. The deciding evidence is that **the kubelet already compiles for FreeBSD**, and that the Windows port shows SIG Node's abstractions (cadvisor.Interface, ContainerManager, mount.Interface, the CRI stats provider) are designed for exactly this substitution. The hard engineering is below the kubelet in containerd CRI, the shim and the OCI runtime, and **VK would not avoid any of it**: a VK provider still needs pod sandboxes, CNI, image pull and unpack, stats, exec and logs. VK only replaces the part that is already free.

Suggested phasing:
1. **M0 (2 weeks):** B1 and B19 upstream PRs. Bring up a kubelet with `--fail-swap-on=false --cgroups-per-qos=false --enforce-node-allocatable=""` plus the minimal `cadvisor_freebsd.go` and `container_manager_freebsd.go` stubs (returning static MachineInfo), so the node registers and is `Ready` with hostNetwork pods only.
2. **M1 (6–8 weeks):** containerd CRI FreeBSD files (B8, B9), an ocijail shim (B10), FreeBSD CNI bridge plus host-local. A pod with two containers shares localhost. `kubectl exec`/`logs` work.
3. **M2 (6–8 weeks):** mount-utils, subPath, stats, rctl limits (B4, B6, B7, B15), and a kube-proxy backend (B12). configMap, secret and emptyDir volumes, `kubectl top`, and HPA all work.
4. **M3:** Linuxulator RuntimeClass (B16), conformance subset (B18), KEP drafting for `spec.os=freebsd` and the CRI FreeBSD section (B14), and a bhyve microVM RuntimeClass via a Kata-like shim.

**Capabilities to drop explicitly** (fail closed with a clear event): user namespaces, `shareProcessNamespace`, seccomp, AppArmor and SELinux profiles (accepted but ignored when PSA requires them), swap, memory QoS, PSI, the CPU and memory managers' NUMA pinning beyond `cpuset`, in-place resize (until the shim supports `Update`), checkpoint/restore, and `hostUsers: false`.

---

## 9. Reproduction notes

```sh
cd ~/bsdk8s/upstream/kubernetes
export GOTOOLCHAIN=auto GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0
for c in kubelet kube-proxy kube-apiserver kube-controller-manager kube-scheduler kubectl; do
  go build -o /dev/null ./cmd/$c && echo "$c OK" || echo "$c FAIL"; done
# fix:
cp pkg/securitycontext/util_darwin.go pkg/securitycontext/util_freebsd.go   # possibleCPUs() returns nil
```
(This was done in a throwaway git worktree, which has since been removed. The upstream clone is unmodified.)

## 10. External references
- runtime-spec FreeBSD section: https://github.com/opencontainers/runtime-spec/blob/main/config-freebsd.md
- runj: https://github.com/samuelkarp/runj (README, `docs/spec-coverage.md`); background: https://samuel.karp.dev/blog/2021/05/running-freebsd-jails-with-containerd-1-5
- ocijail: https://github.com/dfr/ocijail; Doug Rabson, "OCI Containers for FreeBSD": https://medium.com/@dfr/oci-containers-for-freebsd-512a6df2bc85
- FreeBSD Containers wiki: https://wiki.freebsd.org/Containers
- FreeBSD CNI plugins port: https://freshports.org/net/containernetworking-plugins
- kube-apiserver port (says "Kubelet hasn't been ported"): https://freshports.org/net/kube-apiserver
- Earlier containerd-for-kubelet effort: https://bugs.freebsd.org/bugzilla/show_bug.cgi?id=269574
- FreeBSD Foundation, Podman testing (2024): https://freebsdfoundation.org/blog/advancing-cloud-native-containers-on-freebsd-podman-testing-highlights/
- 2024 FreeBSD DevSummit, OCI containers (Rabson upstreaming CRI work): https://forums.freebsd.org/threads/2024-freebsd-developer-summit-oci-containers-on-freebsd.93967
- AsiaBSDCon 2026, Calico on FreeBSD: https://2026.asiabsdcon.org/media/entry/question_uploads/final-paper_3tMo1OJ.pdf
- Note: containers/common and containers/storage in `upstream/` are stale (Aug 2025); current FreeBSD code lives in the containers/container-libs monorepo (`go.podman.io/...`).
