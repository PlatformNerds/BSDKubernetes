# 03 — Networking: native Kubernetes worker nodes on FreeBSD

Status: research draft, 2026-11-13. Source trees: shallow clones under `~/bsdk8s/upstream/`
(freebsd-src is 16.0-CURRENT, HEAD from 2026-11-13; kubernetes HEAD from 2026-11-14;
cni-plugins HEAD from 2026-11-09). I also cloned `dfr/plugins` (branch `freebsd`) into
`~/bsdk8s/upstream/dfr-cni-plugins`, because that fork is what FreeBSD actually ships.

**Short version:** networking is the most feasible part of this project. FreeBSD already
has the kernel pieces Kubernetes needs: VNET jails as network namespaces, epair (veth),
if_bridge, if_vxlan, if_geneve, if_wg, a VNET-virtualized pf with tables, anchors,
atomic transactions and a netlink API, plus ipfw, CARP and FRR/BIRD/OpenBGPD. Nothing
fundamental is missing. **The ecosystem software is missing.** Every CNI plugin, every
kube-proxy mode and every NetworkPolicy engine in mainstream use links
`vishvananda/netlink` and drives iptables/nftables/IPVS/eBPF. All of that has to be
rewritten against pf and FreeBSD ioctls. Cilium cannot be ported, and pretending it can
wastes time. The one place where a **kernel change is worth it** is O(1)
service dispatch, the equivalent of an nftables verdict map, so kube-proxy can scale past a few
thousand services.

---

## 1. CNI on FreeBSD

### 1.1 Does upstream containernetworking/plugins build on FreeBSD? No.

* Upstream (`~/bsdk8s/upstream/cni-plugins`) has **zero** `freebsd` references. The
  platform split is by `_linux.go` files in `pkg/` (`pkg/ip`: 8 of 10 files are `_linux`;
  `pkg/ns/ns_linux.go`; `pkg/utils/sysctl/sysctl_linux.go`). The plugin `main` packages
  (`plugins/main/bridge/bridge.go`, `plugins/meta/portmap/*`, `plugins/meta/firewall/*`)
  are untagged but import `github.com/vishvananda/netlink`, `coreos/go-iptables` or
  `knftables` directly (37 non-test files). They fail to compile with `GOOS=freebsd`.
* `vishvananda/netlink` only builds on Linux. Its non-Linux stub returns
  `ErrNotImplemented`, so even code that compiles cannot work.
* The parts that *are* portable: `plugins/ipam/host-local` (pure Go, file-based
  allocator), `plugins/ipam/static`, and the `libcni`/`skel`/`types` packages in
  containernetworking/cni.

### 1.2 What FreeBSD actually ships: `net/containernetworking-plugins` (dfr's fork)

* Port `net/containernetworking-plugins` 0.6_21, maintainer dfr@FreeBSD.org. Its distfile is
  `github.com/dfr/plugins` tag `freebsd-v0.6`, based on upstream ~v1.3.0
  ([freshports](https://www.freshports.org/net/containernetworking-plugins/)). It installs
  bridge, firewall, host-local, loopback, portmap, static and tuning.
* Fork layout (`~/bsdk8s/upstream/dfr-cni-plugins`, last commit 2025-06-18). It is a
  **parallel tree**, `plugins/freebsd/{main/bridge,main/loopback,meta/portmap,meta/firewall,meta/tuning}`,
  plus `pkg/ip/{link,ipmasq,ipforward}_freebsd.go`, `pkg/ipam/ipam_freebsd.go`, and
  `plugins/ipam/host-local/backend/disk/backend_freebsd.go`. It is built by `build_freebsd.sh`.
* Implementation style: it **shells out**. It is not an API binding.
  * `pkg/ip/link_freebsd.go:36`: `ifconfig epair create`, then rename, then
    `ifconfig <b> vnet <jail>` (`:82`) moves the B end into the VNET jail.
  * `plugins/freebsd/main/bridge/bridge.go:273`: `ifconfig bridge create`, `:280`
    `sysctl net.link.bridge.pfil_member=1`, `:353` `ifconfig <br> addm <epair>`.
  * `pkg/ip/ipmasq_freebsd.go:26`: IP masquerade means adding the pod IP to the pf table `cni-nat`
    (`pfctl -t cni-nat -T add`). The pf.conf sample in the port holds the `nat` rule.
  * `plugins/freebsd/meta/portmap/portmap.go:117,168`: generates
    `rdr pass ... -> <ip> port <p>` and loads it with `pfctl -a cni-rdr/<ctrid> -f -`.
    Teardown is `-F nat` (`:190`). The host needs `nat-anchor "cni-rdr/*"` in `/etc/pf.conf`.
  * Loopback: `ifconfig -j <jail> lo0 inet 127.0.0.1` (13.3+), with a fallback to `jexec`.
* Podman/containers-common side:
  * `~/bsdk8s/upstream/containers-common/libnetwork/cni/run_freebsd.go`: loopback setup via
    `ifconfig -j`.
  * `libnetwork/cni/config_freebsd.go`: link deletion via `ifconfig <if> destroy`.
  * `libnetwork/network/interface_freebsd.go`: CNI config is in `/usr/local/etc/cni/net.d/`.
    **Netavark is not used on FreeBSD; podman uses CNI.**
  * `~/bsdk8s/upstream/podman/libpod/networking_freebsd.go:95-112`: creates a
    `vnet-<uuid>` jail with `vnet=new` that acts as the "netns". The CNI `CNI_NETNS` value is a
    **jail name**, not a path. `container_internal_freebsd.go:214-218` sets
    the `org.freebsd.jail.vnet=new` OCI annotation.
* **Takeaway:** a working FreeBSD convention already exists: netns = persistent VNET
  jail, and CNI plugins act on it via `ifconfig -j`/`ifconfig vnet`. Reuse this convention, and with
  it the pf anchor layout `cni-rdr/*` and table `cni-nat`.

### 1.3 Where Kubernetes calls CNI: containerd's CRI plugin

The kubelet does not call CNI; the CRI runtime does. In containerd,
`internal/cri/server/sandbox_run_other.go` (`//go:build !windows && !linux`) and
`pkg/netns/netns_other.go` are **stubs**. FreeBSD needs a `netns_freebsd.go` (create or destroy
a `vnet=new persist` jail and return its name) and a `sandbox_run_freebsd.go` that calls go-cni
with that name. This belongs to the runtime research area, but it blocks all
networking, so it appears in the blocker table.

### 1.4 Proposed FreeBSD CNI plugin set ("bsdcni")

Use dfr's fork as the base and modernize it. Replace the `exec("ifconfig")` calls with a small Go library
(`bsdnet`) that uses ioctls (`SIOCIFCREATE2`, `SIOCSIFVNET`, `SIOCAIFADDR`, `SIOCSDRVSPEC`
for vxlan/bridge) and FreeBSD netlink (routes, addrs, neighbors). Exec'ing ifconfig costs
2–5 forks per pod and makes error handling fragile.

| Plugin | FreeBSD mechanism | Notes |
|---|---|---|
| `loopback` | `lo0` exists in every new VNET; add 127.0.0.1 | already done (dfr) |
| `bridge` | `if_bridge` + `epair`; host side `addm`, pod side `vnet <jail>` | already done (dfr). Set `pfil_member=1` so pf sees pod-to-pod traffic. |
| **`ptp` / routed (recommended default)** | `epair` per pod, **no bridge**. Host side is unnumbered with a `/32` host route to the pod via the epair. The pod's default route uses a link-local or onlink gateway. | Calico-style L3. Avoids if_bridge learning/pfil overhead and simplifies hairpin and policy (one interface per pod, so per-pod pf rules use `on epairNa`). Needs new code (~400 LOC). |
| `host-local` IPAM | upstream, pure Go | dfr adds `backend_freebsd.go` (file locking) |
| `portmap` (hostPort) | pf anchor `cni-rdr/<id>` with `rdr`. On -CURRENT, `match in ... rdr-to` | done (dfr); requires `net.pf.filter_local=1` for localhost hostPorts |
| `bandwidth` | `dummynet` pipes via ipfw (`ipfw pipe`), or ALTQ on pf | new code. dummynet is the better fit (per-pod pipes); ipfw and pf can coexist. |
| `firewall` | pf table `cni-allow` | dfr has a skeleton |
| `tuning` | per-VNET sysctls via `jexec sysctl` / `jail -m` | done (dfr) |
| overlay | see 1.5 | |

**Gotchas:**
* IPv6 inside a fresh VNET: set `ifconfig -j <j> <if> inet6 -ifdisabled`. DAD delays the
  first packet by about 1 s unless `net.inet6.ip6.dad_count=0` is set per VNET. Disable
  `accept_rtadv`.
* epair MAC collisions: the fork sets an explicit MAC (`link_freebsd.go:75`).
* epair has **no TSO/LRO**. `sys/net/if_epair.c` only advertises TXCSUM/RXCSUM and
  VLAN_HWTAGGING (lines 449–526). Pod TCP is therefore segmented at MTU size in the pod's
  stack. That costs CPU at 10G+ but is functionally fine. Mitigation: a jumbo underlay with pod MTU of about 8950.

### 1.5 Multi-node: overlay or routed

| Option | FreeBSD primitive | Assessment |
|---|---|---|
| **Routed / BGP** (Calico-BGP style) | Kernel FIB + FRR (`net/frr10`, 10.7.1) or BIRD (`net/bird2/3`). FRR's zebra speaks FreeBSD routing sockets/netlink. | **Best first choice.** No encapsulation, full MTU, works on L2 or with a BGP-capable ToR. Each node advertises its podCIDR. A small Go agent writes the FRR config from `Node.spec.podCIDR`. |
| **host-gw** (flannel-style) | `route add <podCIDR> <nodeIP>` via netlink `RTM_NEWROUTE` (supported: `sys/netlink/route/rt.c:1227`) | Trivial. Same-L2 only. |
| **VXLAN** | `if_vxlan`, with unicast FDB programming via `SIOCSDRVSPEC` `VXLAN_CMD_FTABLE_ENTRY_ADD` (`sys/net/if_vxlan.h:109`) and ARP via `RTM_NEWNEIGH` (`neigh.c:533`) | Works. This maps 1:1 to flannel's Linux model (FDB entry plus permanent neighbor). Default MTU is `ETHERMTU - hdrlen` (`if_vxlan.c:1653`), so pod MTU is 1450. vxlan advertises TXCSUM/TSO caps (`if_vxlan.c:3076-3094`). |
| **Geneve** | `sys/net/if_geneve.c` (new in -CURRENT) | Interesting for future OVN or Antrea-style work, but too new to depend on. |
| **WireGuard** | `if_wg` in base (`sys/dev/wg/if_wg.c`) | Encrypted overlay. wgctrl-go has a FreeBSD backend, so flannel's wireguard backend is portable in principle. |

### 1.6 Porting existing CNIs

* **flannel: modest port, the best "existing CNI" candidate.**
  `~/bsdk8s/upstream/flannel/pkg/backend/`: the split is `!windows` vs `windows` (30 files tagged
  `//go:build !windows`, 9 tagged `windows`). "!windows" really means Linux. There is a working
  precedent for per-OS backends (`device_windows.go`, `hostgw_windows.go`).
  Linux-only deps: `pkg/backend/vxlan/device.go` (netlink.Vxlan, LinkAdd,
  Neigh with NUD_PERMANENT for FDB and ARP), `hostgw/hostgw.go` and `route_network.go` (netlink
  routes), `ipip`/`ipsec` (xfrm, no FreeBSD netlink equivalent), and `pkg/trafficmngr/{iptables,nftables}`.
  The FreeBSD port needs `device_freebsd.go` (vxlan via ioctl), `route_network_freebsd.go`, and a
  new `trafficmngr/pf` for masquerade/forward rules. Drop ipip/ipsec/udp/tencentvpc.
  **Estimate: about 1.5–3 kLOC, 3–5 weeks.** Subnet leasing via the kube API is portable.
* **Calico (BGP mode): partial.** BIRD and confd run on FreeBSD. **Felix** (the policy/route
  dataplane) is Linux iptables/nftables/eBPF plus Windows HNS, and would need a pf dataplane
  driver. That is a large effort, and Calico's code base is optimized for its Linux model. You could run
  calico-node's BGP part and write your own policy agent, but then you are mostly not using Calico.
* **kube-router: no.** It is tightly bound to ipset, iptables, IPVS and netlink. Its GoBGP library is
  portable but does not program the FIB itself.
* **Cilium: no** (see §4).
* **Antrea/OVN-Kubernetes:** Open vSwitch builds on FreeBSD only with the userspace (netdev)
  datapath, which is slow and unmaintained there. No.

**Recommendation:** write our own "bsdcni" (dfr fork + routed ptp + FRR agent), and port
flannel (vxlan + host-gw) for users who want something familiar.

---

## 2. kube-proxy replacement: a `pf` proxier (and later `kproxy`)

### 2.1 Where it plugs into Kubernetes

* `pkg/proxy/{iptables,ipvs,nftables}/proxier.go` are all `//go:build linux`;
  `winkernel` is Windows-only.
* `cmd/kube-proxy/app/server_other.go` (`//go:build !windows && !linux`) **already compiles
  on FreeBSD** and returns "unsupported". A FreeBSD proxier means adding `server_freebsd.go` and
  `pkg/proxy/pf/`.
* Portable machinery we get for free: `servicechangetracker.go`,
  `endpointschangetracker.go`, `endpointslicecache.go`, `topology.go` (topology hints and
  trafficDistribution), `healthcheck/` (the HealthCheckNodePort server for eTP=Local), `runner/`
  (bounded-frequency sync), `metaproxier` (dual-stack), and
  **`localnodeportproxy/`**: a userspace localhost-NodePort proxy that nftables mode uses
  because nftables has no `route_localnet`. Reuse it as-is.
* `Provider` interface: `pkg/proxy/types.go:28` (EndpointSlice, Service, NodeTopology,
  ServiceCIDR handlers, plus Sync and SyncLoop). The proxier itself can be **out-of-tree** (a
  standalone binary implementing this, like kube-router or Cilium), so no upstream merge is needed.

### 2.2 pf facts that matter (verified in -CURRENT source)

* **Load-balancing pools:** `PF_POOL_{NONE,BITMASK,RANDOM,SRCHASH,ROUNDROBIN}` plus the
  `PF_POOL_STICKYADDR` flag (`sys/netpfil/pf/pf.h:126-132`). **Tables work as pool
  targets for random, source-hash and round-robin** (`sys/netpfil/pf/pf_lb.c:727-900`,
  `pfr_pool_get`). This is the key feature: endpoints live in a table, so **endpoint churn
  is a table replace, not a rule reload**.
* **OpenBSD-style syntax in FreeBSD -CURRENT:** `match/pass ... rdr-to/nat-to/af-to`, with
  outbound `rdr-to` to non-local addresses permitted (`share/man/man5/pf.conf.5:1462-1480`).
  Host-originated traffic (hostNetwork pods, kubelet probes) to a ClusterIP can therefore be
  translated outbound, with no `route-to lo0` trick. On 14.x you only get classic `rdr` (inbound only),
  which needs the lo0 hack or the userspace proxy for host-originated traffic.
* **Atomic updates:** ioctl `DIOCXBEGIN/DIOCXCOMMIT` (`sys/net/pfvar.h:2309-2310`) and
  **netlink** `PFNL_CMD_BEGIN/COMMIT/ROLLBACK` (`sys/netpfil/pf/pf_nl.h:90-92`), plus
  `PFNL_CMD_TABLE_SET_ADDR` (atomic table replace), `PFNL_CMD_KILLSTATES`,
  `PFNL_CMD_GETSTATES` and `PFNL_CMD_ADD_TABLE`. The genl family is registered at
  `pf_nl.c:3663`. `lib/libpfctl` wraps it in C (75 functions). From Go, use cgo against
  libpfctl, or hand-roll genetlink, because `mdlayher/netlink` is Linux-tagged.
* **VNET-virtualized:** `VNET_SYSINIT(vnet_pf_init…)` (`pf_ioctl.c:7916`) and
  `V_pf_main_anchor` (`pfvar.h:2805`). Every pod jail can have its own pf, but see §3.
* **Default limits** (all tunable with `set limit`): states 100k (`pf.h:683`), src-nodes 10k
  (`pf.h:715`, which bounds sticky sessions), anchors 512 (`pf.h:501`), table entries 200k
  (`pfvar.h:1992`).
* **Rule evaluation is linear** over the active ruleset, with skip-steps
  (`pf.c:5721-5736`, `r->skip[...]`) and anchors evaluated recursively (`pf_step_into_anchor`,
  `pf.c:5082`). Only the **first packet** of a flow pays this cost; after that the state table is a
  hash lookup. FreeBSD pf is SMP-scalable (per-row state locks). OpenBSD pf is still mostly
  serialized.

### 2.3 pf proxier design

Ruleset skeleton, loaded once; per-service content is swapped atomically per anchor:

```
# /etc/pf.conf (static part, owned by the node agent)
table <kube-cluster-cidrs> persist { 10.244.0.0/16, 10.96.0.0/12 }
table <kube-nodeport-local-ips> persist            # node IPs eligible for NodePorts
anchor "kube-svc/*"                                 # translation (match ... rdr-to)
anchor "kube-masq"                                  # SNAT for hairpin/eTP=Cluster
anchor "kube-np/*"                                  # NetworkPolicy (filter)
pass all keep state                                 # default when no policy selects a pod
```

Per Service port `S` (`ns/name:port/proto`):

```
table <svc-S-all>   { 10.244.1.5, 10.244.2.7, ... }   # ready endpoints, all nodes
table <svc-S-local> { 10.244.1.5 }                     # ready endpoints on THIS node
# ClusterIP (pod- and host-originated; on -CURRENT match works in/out)
match proto tcp to 10.96.0.10 port 53 rdr-to <svc-S-all> port 5353 random \
      tag KUBE_SVC                                     # sessionAffinity=None
match proto tcp to 10.96.4.2 port 80 rdr-to <svc-S-all> port 8080 random sticky-address \
      tag KUBE_SVC                                     # sessionAffinity=ClientIP
# NodePort, externalTrafficPolicy=Cluster
match in on ! epair proto tcp to <kube-nodeport-local-ips> port 30080 \
      rdr-to <svc-S-all> port 8080 random tag KUBE_MASQ
# NodePort/LB, externalTrafficPolicy=Local: local table only, no masquerade
match in proto tcp to 203.0.113.10 port 80 rdr-to <svc-S-local> port 8080 random
# empty endpoint table → reject (k8s semantics: ICMP unreachable/TCP RST)
block return in quick proto tcp to 10.96.4.2 port 80     # emitted only when no endpoints
```

```
# kube-masq anchor
match out proto tcp tagged KUBE_MASQ nat-to (egress_if)            # eTP=Cluster from outside
match out on epair proto tcp from <kube-pod-cidr-local> tagged KUBE_SVC \
      to <kube-pod-cidr-local> nat-to <node-gw-ip>                 # hairpin: pod→own svc→itself
```

Feature mapping:

| k8s feature | pf mechanism | Confidence |
|---|---|---|
| ClusterIP LB | `rdr-to <table> random` (or `round-robin`, `source-hash`) | high, `pf_lb.c:727-900` |
| sessionAffinity ClientIP + timeout | `sticky-address` (random/round-robin only, per man page). The mapping lives in a src-node with timeout `src.track`. The grammar lists `src.track` in the per-rule timeout list (`pf.conf.5:3834`). | medium: per-rule src.track honoring needs a test. Global fallback works. |
| NodePort | `match in to <node-ips> port N rdr-to ...` | high |
| NodePort on 127.0.0.1 | `net.pf.filter_local=1` (dfr port note) or reuse `localnodeportproxy` | high |
| eTP=Local | separate `<svc-S-local>` table, no `nat-to` tag. HealthCheckNodePort from `pkg/proxy/healthcheck` | high |
| iTP=Local | ClusterIP rule points to `<svc-S-local>` | high |
| Hairpin | tag + `nat-to` on pod-side egress when src and dst are both local pods | high (standard pf pattern) |
| LoadBalancer / externalIPs | same rules keyed on the VIP | high |
| Terminating endpoints / ProxyTerminatingEndpoints | computed in the Go controller, which picks the table contents | high |
| Topology hints / trafficDistribution | `pkg/proxy/topology.go`, also table contents | high |
| UDP stale-conntrack cleanup (DNS!) | `PFNL_CMD_KILLSTATES` filtered by rdr address/port, the equivalent of `conntrack -D` | high |
| SCTP | pf handles SCTP states (`pf.conf.5` has sctp.* timeouts) | medium |
| Dual-stack | metaproxier and pf inet6 rules. NAT64 via `af-to` if wanted | high |
| Per-endpoint differing targetPort (named ports) | pf's pool port is per-rule, so emit **one rule per distinct targetPort group** | high, rare case |

**Sync model:** rules change only on Service add or remove. Each Service port gets its own anchor
`kube-svc/<hash>`, replaced atomically with `PFNL_CMD_BEGIN(anchor)` + rules +
`COMMIT`. Endpoint changes become `PFNL_CMD_TABLE_SET_ADDR`, which is atomic and O(endpoints). This is
**structurally better than the iptables proxier**, which rewrites everything.

### 2.4 Scale: 10k services × N endpoints

* **Steady-state (established flows):** an O(1) state-table hash. Fine at any service count.
* **New flows:** pf walks the rules linearly. 10k services × ~1.5 ports ≈ 15k `match`
  rules, plus NodePort/LB rules. Skip-steps don't help: consecutive rules have distinct
  destination addresses, so `PFE_SKIP_DST_ADDR` doesn't chain. *Estimate* (not measured): tens of ns
  per rule, so ~0.3–1 ms of CPU per new connection at 15k rules. That is the same pathology as
  kube-proxy iptables mode at 5k+ services, and it cripples high connection-rate workloads (DNS, short HTTP).
* **Mitigation without kernel changes: a hierarchical anchor trie.** The Service CIDR is
  compact (/12 or /16). Bucket ClusterIPs by their low-order bits: 256 anchors
  `kube-svc/b<xx>`, each reached by one rule `anchor "kube-svc/b17" to 10.96.17.0/24`. Inside each are
  ~60 rules. Cost is ~256 + 60 comparisons, a ~50× reduction. NodePorts get a similar bucket
  by port range. The anchor limit must be raised (`set limit anchors 4096`). This keeps the pf mode
  usable to roughly 10k services. It is ugly but needs zero kernel work.
* **Real fix: a kernel change ("pf verdict maps").** Add a table type whose entries carry a
  value (pool id), so one rule does the dispatch:
  `match to <svcmap:kube> rdr-to svcmap random`. The table is keyed (proto, addr, port) → pool
  (endpoint table + target port + flags). The lookup is a hash or radix (pfr already uses radix).
  It reuses all of pf's NAT/state/reverse-translation logic, so the kernel diff is small:
  * new `PF_ADDR_SVCMAP` address type plus pool indirection in `pf_lb.c` / `pf_match_translation`
  * table-value storage in `pf_table.c`
  * netlink cmds in `pf_nl.c`
  * parse.y and libpfctl support
  
  **Estimate: 2–4 kLOC, 2–3 months for an experienced pf developer.** It is upstreamable: it is
  generically useful and comparable to nftables vmaps. Talk to the pf maintainer
  (Kristof Provost) early.
* **Alternative kernel route: ipfw + an eaction module.** ipfw already has
  **O(1)-ish dispatch**: `IPFW_TABLE_FLOW` tables keyed on (dst-ip, dst-port, proto)
  (`sys/netinet/ip_fw.h:872`) with `skipto tablearg` / `nat tablearg`
  (`sbin/ipfw/ipfw.8:1001,1060`). It also has a **loadable external-action KPI**
  (`sys/netpfil/ipfw/ip_fw_eaction.c`, used by `nptv6`, `nat64lsn`, `tcp-setmss`).
  A `kproxy` eaction module (an IPVS-alike with a Maglev table per service, its own flow table, and reverse
  NAT) is a clean, self-contained kernel module, maybe 3–5 kLOC. But it duplicates
  pf's state engine, and running ipfw and pf together means two firewalls with two conntrack
  tables. The libalias LSNAT (`redirect_port` with multiple targets, `ipfw.8:4870-4874`)
  path is round-robin only, global-locked per instance, and has no affinity or health semantics. **Not
  recommended for services.**
* **Verdict:** ship the **pf proxier with hierarchical anchors** first (v1, ≤10k services).
  Pursue **pf svcmap** as the kernel project (v2). A standalone IPVS clone (`kproxy`) is only
  justified if the pf maintainers reject svcmap.

### 2.5 pf vs ipfw vs OpenBSD pf

| | FreeBSD pf (-CURRENT) | FreeBSD ipfw | OpenBSD pf |
|---|---|---|---|
| DNAT with LB pool from table | yes (random/rr/srchash) | libalias LSNAT only (rr) | yes, plus **least-states** (`openbsd-src/sys/net/pfvar.h:141`) |
| Atomic ruleset swap | anchors + DIOCX / netlink txn | `ipfw set swap`, table swap | anchors + DIOCX |
| O(1) service dispatch | no (linear rules) | **yes** (flow table + tablearg) | no |
| Per-VNET | yes | yes | n/a (rdomains, no jails) |
| SMP | fine-grained | fine-grained | mostly serialized |
| Programmatic API | netlink genl + libpfctl | setsockopt IP_FW3 | ioctl |
| Fit | **primary dataplane** | dummynet (bandwidth), fallback | not a node target |

OpenBSD (`openbsd-src/sys/net/pf.c`, 9.2k lines vs FreeBSD 12.2k) has no jails or VNET and
cannot host pods. DragonFly has an old pf plus ipfw3 and no VNET. NetBSD's **npf** compiles rules to
BPF bytecode run by `bpfjit` (`netbsd-src/sys/net/bpfjit.c`, `sys/net/npf/npf_bpf.c`), which is the
closest BSD idea to an eBPF dataplane, but NetBSD lacks a jail/VNET equivalent. **FreeBSD is
the only viable BSD for this.**

---

## 3. NetworkPolicy (and AdminNetworkPolicy)

### 3.1 Where to enforce it: on the host, not in the pod's own pf

pf is VNET-virtualized, so each pod jail *could* run its own pf. **Don't enforce policy
there.** A pod with `CAP_NET_ADMIN`-equivalent privileges (or a compromised privileged
pod) can `pfctl -F all` inside its own VNET. Policy must sit outside the trust boundary, on the
**host side of the pod's epair** (`epairNa`). This is the same reason Calico puts rules on the
host veth.

With the routed ptp design (§1.4), every pod has exactly one host interface:
* **Egress policy** for pod P = `in on epairPa` (packets leaving the pod enter the host).
* **Ingress policy** for pod P = `out on epairPa`.
* Pod-to-pod on the same node passes through both hooks. With `if_bridge`, `pfil_member=1` gives
  the same per-member hooks (`sys/net/if_bridge.c:465`). With the bridge design, this is the main
  argument for routed ptp instead.
* Service traffic: pf evaluates filter rules **after** translation, so ingress rules see the
  real endpoint pod IP. That matches Kubernetes semantics (policy applies post-DNAT).
* Return traffic: stateful `keep state` (with the default `floating` state policy) admits replies,
  which matches NetworkPolicy's "connection-oriented" semantics.

### 3.2 Compilation scheme

* **Selectors become tables.** For each distinct (namespaceSelector, podSelector) pair, keep a table
  `<np-sel-HASH>` of pod IPs (v4+v6), maintained by an informer-driven controller. ipBlock with
  except becomes a table with negated entries (`!10.0.1.0/24` entries in pf tables are supported).
* **Named ports:** pf has no notion of them. The controller resolves `port: http` per
  *target* pod. Pods using different numbers for the same name produce one rule per (port number,
  pod-set) group. Same approach as Calico/Antrea.
* **Per-pod anchor** `kube-np/<podUID>`, attached with `anchor "kube-np/<uid>" on epairPa`:
  ```
  # pod P selected by an ingress policy → isolated for ingress
  pass out quick on epairPa proto tcp from <np-sel-a1b2> to (epairPa:peer) port 8080
  pass out quick on epairPa proto tcp from <np-ipb-c3d4> to any port 443
  block out quick on epairPa all            # default-deny for isolated direction
  ```
  Interface skip-steps (`PFE_SKIP_IFP`) make the per-pod anchors cheap: rules for other
  interfaces are skipped in one jump. 110 pods × tens of rules is trivial for pf.
* **Updates:** pod IP churn only touches tables (`TABLE_SET_ADDR`). Policy edits re-render the
  affected per-pod anchors in a transaction.
* **AdminNetworkPolicy / BaselineAdminNetworkPolicy** (now consolidated as
  ClusterNetworkPolicy in `policy.networking.k8s.io` v1alpha2): implement tiers as ordered
  anchors: `kube-anp/*` (by priority, `quick` Allow/Deny) → `kube-np/*` → `kube-banp/*` →
  default pass. The ANP **"Pass"** action is a `quick` match that tags the packet (for example
  `KUBE_ANP_PASS`) and skips the remaining ANP anchors via `tagged` guards. That needs care but is
  expressible. Use `egress` FQDN/network-peer features only after a DNS-snooping design exists
  (pf has no FQDN tables. The controller would watch CoreDNS answers, which is hard. Defer.)
* **Conformance target:** the upstream `sig-network` NetworkPolicy e2e suite and the
  `network-policy-api` conformance suite.

### 3.3 Alternative: ipfw per jail

ipfw is also VNET-virtualized, and `table(...)` lookups are fast. But mixing two firewalls
(pf for services, ipfw for policy) doubles state tracking and makes debugging hell. Use ipfw only
for dummynet shaping (bandwidth plugin).

---

## 4. eBPF / Cilium

* **FreeBSD has no eBPF.** `generic-ebpf` (Hayakawa, GSoC 2018 → ~2020) provided a
  VM/JIT and maps behind `/dev/ebpf` but **no kernel hooks**. It has been dormant since about 2020 and is
  not in base ([repo](https://github.com/generic-ebpf/generic-ebpf),
  [BSDCan 2018 paper](https://papers.FreeBSD.org/2018/bsdcan/hayakawa-eBPF_Implementation_for_FreeBSD.files/hayakawa-eBPF_Implementation_for_FreeBSD.pdf),
  [GSoC 2020 XDP hooks](https://wiki.FreeBSD.org/SummerOfCode2020Projects/eBPFXDPHooks)).
  The Q4 2025 status report shows nothing new
  ([report](https://lists.freebsd.org/archives/freebsd-current/2026-February/009928.html)).
  Base has only classic BPF with a JIT (`sys/net/bpf_jitter.c`).
* **Porting Cilium: no.** Cilium's datapath is thousands of lines of BPF C that assume Linux
  hook points (tc, XDP, cgroup/sock_addr, sk_msg), Linux helper ABIs, Linux skb layout and
  BTF. Even Microsoft's eBPF-for-Windows does not run Cilium's datapath. Building a Linux-compatible eBPF subsystem in
  FreeBSD (verifier + JIT + ~100 helpers + hook points) is a multi-year kernel project with
  uncertain upstream reception. **Not on the critical path, and probably never.**
* **netmap/VALE** (`sys/dev/netmap/`): a userspace fast-path switch. It makes sense for **bhyve
  microVM pods** (vale ports / ptnet into the guest) and possibly for a high-performance
  ingress gateway. It bypasses pf (no conntrack, no policy), so it is wrong as the general pod dataplane.
* **netgraph, the out-of-box idea, judged honestly.** netgraph is genuinely programmable at
  runtime from userspace: `ng_eiface` (an interface that can be moved into a VNET instead of epair),
  `ng_bridge`, `ng_bpf` (classic BPF classifier, JIT-able), `ng_nat` (libalias), `ng_tag`,
  `ng_ipfw`, `ng_car` (policing), `ng_ksocket` (tunnels), `ng_one2many` (LB/failover)
  (`sys/netgraph/`). You could build a pod dataplane graph with:
  `eiface(pod) → bpf(classify) → {nat|car|tag} → bridge → ether(uplink)`.
  The limits: there are no maps or keyed state beyond libalias; classic BPF can't do lookups; every service or policy
  change is a graph edit; per-hop function-call overhead applies; and there is no conntrack. Anything
  serious (service LB, policy at scale) would mean writing a **custom `ng_k8s` C node**,
  which is just the kproxy kernel module behind a different plumbing API.
  **Verdict:** a neat demo, not a Cilium analogue. Use `ng_eiface` only if epair proves
  limiting. Don't build the dataplane on netgraph.
* **Observability (a Hubble-like tool) is very achievable without eBPF:**
  * `pflog0` (rule-tagged packet logs, readable by tcpdump),
  * `pflow` (IPFIX/NetFlow v9 export of pf states; `sys/netpfil/pf/pflow.c`, with its own genl
    family),
  * **DTrace SDT probes in pf**: 71 `SDT_PROBE` sites in `pf.c`, e.g.
    `pf:ip:test:done` with the matched `struct pf_krule *` (`pf.c:123-135`), and the
    fbt provider on everything else,
  * `PFNL_CMD_GETSTATES` for a live flow table.
  
  A "bsd-hubble" agent would join pflow/pflog/dtrace events with the pod-IP→pod map and
  export Hubble-compatible flow protobufs. **Effort: weeks, not years.**

---

## 5. Other networking concerns

| Topic | Finding |
|---|---|
| **conntrack** | pf state table: `PFNL_CMD_GETSTATES/KILLSTATES/CLRSTATES`, with tunable timeouts (`tcp.established` etc.) and `set limit states` (default 100k, `pf.h:683`; raise to 1M+ on real nodes). Equivalent to `conntrack -L/-D`. |
| **MTU / offloads** | epair: checksum offload only, no TSO/LRO (`if_epair.c:449-526`). vxlan: −50 B, auto MTU (`if_vxlan.c:1653`). Recommend jumbo underlay. Set MTU on both epair ends (dfr: `link_freebsd.go:57-69`). |
| **IPv6 / dual-stack** | pf, epair, bridge, vxlan, FRR: all dual-stack. Watch DAD and `ifdisabled` in new VNETs. `af-to` gives NAT64 (`pf.conf.5:1420-1430`, `sys/netpfil/pf/inet_nat64.c`). |
| **DNS / CoreDNS** | Pure Go, cross-compiles to freebsd. A port exists (`dns/coredns`, [freshports](https://www.freshports.org/dns/coredns)), and it also runs as a Linux image under Linuxulator. FreeBSD libc honours `options ndots:5` and search lists in `resolv.conf`. The kube-proxy UDP stale-state flush (§2.3) is **mandatory**, or DNS breaks after CoreDNS restarts. |
| **LoadBalancer** | MetalLB **controller** (IP allocation) is portable Go. The MetalLB **speaker** is Linux-bound (AF_PACKET ARP/NDP, memberlist, frr-k8s). Write a small FreeBSD speaker that announces VIPs via **OpenBGPD/FRR/BIRD** (BGP) or **CARP** (L2 failover; CARP has a genl family, `sys/netinet/ip_carp.c:2807`). VIP traffic is handled by the pf proxier like externalIPs. |
| **Ingress** | ingress-nginx is retired upstream (EOL March 2026), so don't target it. **Traefik** (Go, `net/traefik` 3.7.6 in ports) and **HAProxy** (`net/haproxy`, plus the haproxy-ingress controller in Go) run natively. NGINX Gateway Fabric (Go control plane + nginx) is plausible. **Envoy** is effectively unavailable: `www/envoy` is stuck at 1.26.1 and marked BROKEN, needs BoringSSL, and builds with Bazel ([freshports](https://freshports.org/www/envoy/), [PR 247236](https://bugs.freebsd.org/bugzilla/show_bug.cgi?id=247236)). That excludes Contour, Envoy Gateway, Istio, Emissary and Gloo natively. They could run as Linux images in a **bhyve microVM pod**, but not under Linuxulator with any confidence. |
| **Gateway API** | Traefik, HAProxy and NGINX Gateway Fabric implementations exist. The control planes are Go, so they are portable. |
| **FreeBSD netlink (13.2+)** | `sys/netlink/route/`: implements `RTM_{NEW,DEL,GET}LINK`, `{NEW,DEL,GET}ADDR`, `{NEW,DEL,GET}ROUTE`, `{NEW,DEL,GET}NEIGH`, `{NEW,DEL,GET}NEXTHOP` (`iface.c:2067-2100`, `rt.c:1214-1227`, `neigh.c:533-545`, `nexthop.c:1116-1128`). **Not supported:** RTM_*RULE (policy routing; use FIBs/`setfib` instead), qdisc/tclass/tfilter/action (tc), xfrm, nftables, conntrack (`netlink/route/common.h:50-75` comments). Link creation only knows generic cloners and the `vlan` kind (`if_vlan.c:1356`). **No veth/bridge/vxlan kinds.** Generic netlink families: pf, carp, pflow, sysevent, rpc. |
| **Can vishvananda/netlink work?** | It doesn't compile (Linux-tagged). A fork with `freebsd` tags could do route/addr/neigh/link-dump, since the wire format is Linux-compatible (that's the design goal: FRR and BIRD use it). Link-type-specific creation (veth, bridge, vxlan, ipvlan, macvlan) won't work. Recommendation: **don't fork it.** Write `bsdnet` (ioctls + a minimal rtnetlink client) and keep flannel and others behind small per-OS files. |
| **Linuxulator pods** | `sys/compat/linux/linux_netlink.c` translates Linux NETLINK_ROUTE for Linux binaries. `ip addr` in a Linux init container partially works. iptables-based init containers (Istio-init, Linkerd-init) **will fail**, so service meshes need ambient or sidecar-less modes or a pf-aware init. |

---

## 6. Blockers

| # | Blocker | Severity | Solution | Effort |
|---|---|---|---|---|
| 1 | containerd CRI has no FreeBSD netns/sandbox networking (`netns_other.go`, `sandbox_run_other.go` stubs) | **Critical** | netns = `vnet=new persist` jail, following the podman convention; call go-cni with the jail name | 2–4 wks (shared with runtime area) |
| 2 | Upstream CNI plugins don't compile on FreeBSD | **Critical** | Start from dfr's `plugins/freebsd` fork. Add routed ptp. Replace exec with a `bsdnet` lib | 4–6 wks |
| 3 | No kube-proxy mode for FreeBSD (`server_other.go` stub) | **Critical** | Out-of-tree pf proxier using portable trackers, netlink pf txns and tables | 6–10 wks to e2e conformance |
| 4 | pf rule evaluation is linear, so new-flow latency degrades past ~5k services | High (at scale) | v1: hierarchical anchor trie. v2: **pf svcmap kernel feature** (verdict-map analogue) | v1 2 wks; v2 2–3 months kernel |
| 5 | No NetworkPolicy engine | **Critical** (security) | Host-side pf anchors per pod epair, selector tables, named-port resolution; ANP tiers as ordered anchors | 6–8 wks |
| 6 | Multi-node pod networking | **Critical** | Routed + FRR/BIRD agent (first), flannel port (vxlan/host-gw/wireguard) second | 3–5 wks each |
| 7 | No eBPF, so no Cilium/Hubble/eBPF-based tooling | Medium (ecosystem expectation) | Accept it. pf + pflow + pflog + DTrace for observability, "bsd-hubble" exporter | 4–6 wks for observability |
| 8 | MetalLB speaker Linux-only | Medium | **Done (layer 2), research/25:** `components/bsd-lb`, an address-pool controller and an ARP speaker on bpf(4); BGP not done | — |
| 9 | Envoy unavailable, so the Envoy-based ingress/gateway/mesh stack is out | Medium | Traefik/HAProxy/NGINX Gateway Fabric natively; Envoy-based stacks only in bhyve microVM pods | 0 (choice) / port Envoy = months |
| 10 | Service mesh init containers rely on iptables | Medium | pf-based redirect CNI chain plugin (pf `rdr-to 127.0.0.1` in the pod's own VNET; that is the one legitimate in-pod pf use) | 3–4 wks |
| 11 | epair lacks TSO/LRO, so throughput and CPU cost are worse than veth | Low–Med | Jumbo MTU. Later add TSO/LRO passthrough to epair (kernel, small) or switch to `ng_eiface` | kernel: 2–4 wks |
| 12 | No Go netlink library for FreeBSD | Medium | `bsdnet`: ioctl + rtnetlink + pf genl (or cgo libpfctl) | 3–4 wks |
| 13 | UDP stale-state handling for DNS | High (correctness) | `PFNL_CMD_KILLSTATES` on endpoint removal, mirroring `pkg/proxy/conntrack` logic | 1 wk |
| 14 | IPv6 DAD/ifdisabled in new VNETs | Low | Per-VNET sysctls in the CNI | days |

---

## 7. Recommended architecture

```
                         Kubernetes control plane (unchanged, Linux or FreeBSD)
                                         │  watch Services/EndpointSlices/NetworkPolicies/Nodes
 ┌───────────────────────────────────────┼───────────────────────────────────────────────────┐
 │ FreeBSD worker node                   │                                                   │
 │                                       ▼                                                   │
 │  kubelet ─CRI─► containerd(+FreeBSD CRI netns = VNET jail) ─libcni─► bsdcni plugins       │
 │                                                    (ptp|bridge, host-local, portmap,      │
 │                                                     bandwidth→dummynet, loopback, tuning)  │
 │                                                                                           │
 │  bsd-netd (one Go daemon, 3 controllers)                                                  │
 │   ├─ proxier   (k8s pkg/proxy trackers) ──netlink pf txn──┐                               │
 │   ├─ netpol    (NP + ANP/BANP compiler) ──netlink pf txn──┤                               │
 │   ├─ routes    (podCIDR → FRR config / vxlan FDB)         │     frr/bird (BGP to ToR/peers)│
 │   └─ observe   (pflow+pflog+dtrace → Hubble-ish API)      │       ▲                        │
 │                                                           ▼       │ kernel FIB             │
 │  ┌────────────────────────── host VNET (vnet0) ─────────────────────────────────────┐     │
 │  │ pf:  kube-svc/* (rdr-to <tables> random|sticky)  ──► [v2: svcmap O(1) dispatch] │     │
 │  │      kube-masq (hairpin, eTP=Cluster SNAT)                                       │     │
 │  │      kube-anp/* → kube-np/<pod> (on epairNa) → kube-banp/* → pass               │     │
 │  │      state table = conntrack   pflow → IPFIX   pflog0 → flow logs               │     │
 │  │                                                                                  │     │
 │  │  epair1a ──/32 route──┐   epair2a ──/32──┐      vxlan0/wg0 (optional overlay)    │     │
 │  └───────────┼───────────┼──────────────────┼────────────┬────────────────────────── ┘     │
 │       ┌──────┴─────┐ ┌───┴──────────┐  ┌────┴───────┐     │                                │
 │       │ VNET jail  │ │ VNET jail    │  │ bhyve µVM  │     │  em0/ixl0 (underlay, MTU 9000) │
 │       │ pod A      │ │ pod B (Linux │  │ pod C      │     │                                │
 │       │ epair1b    │ │ ulator)      │  │ tap/vale   │     └──── BGP podCIDRs + LB VIPs ───► │
 │       │ lo0        │ │ epair2b      │  │ (Envoy etc)│           (OpenBGPD/FRR or CARP)     │
 │       └────────────┘ └──────────────┘  └────────────┘                                     │
 └───────────────────────────────────────────────────────────────────────────────────────────┘
```

**Phasing:**
1. **MVP (single node):** containerd netns = VNET jail, dfr bridge/host-local/loopback/portmap,
   pf proxier (flat anchors), CoreDNS. Target: Services + DNS conformance.
2. **Multi-node:** routed ptp + FRR agent, and/or the flannel vxlan/host-gw port. NetworkPolicy compiler.
3. **Production:** hierarchical anchors, MetalLB-controller + BGP/CARP speaker, Traefik/HAProxy
   ingress, the pflow/dtrace observability agent, ANP.
4. **Kernel work (upstream with FreeBSD pf maintainers):** pf svcmap (O(1) service dispatch),
   epair TSO/LRO.

---

## Sources

Local (all under `~/bsdk8s/upstream/`):
- `cni-plugins/plugins/main/bridge/bridge.go`, `cni-plugins/pkg/ip/*_linux.go`, `pkg/ns/ns_linux.go`
- `dfr-cni-plugins/plugins/freebsd/**`, `dfr-cni-plugins/pkg/ip/{link,ipmasq,ipforward}_freebsd.go`, `build_freebsd.sh`
- `containers-common/libnetwork/cni/{run,config}_freebsd.go`, `libnetwork/network/interface_freebsd.go`
- `podman/libpod/networking_freebsd.go`, `podman/libpod/container_internal_freebsd.go`
- `containerd/internal/cri/server/sandbox_run_other.go`, `containerd/pkg/netns/netns_other.go`
- `kubernetes/cmd/kube-proxy/app/server_other.go`, `kubernetes/pkg/proxy/{types.go,localnodeportproxy/proxy.go,iptables,ipvs,nftables}/`
- `flannel/pkg/backend/{vxlan,hostgw,wireguard}/`, `flannel/pkg/trafficmngr/`
- `freebsd-src/sys/netpfil/pf/{pf.c,pf.h,pf_lb.c,pf_ioctl.c,pf_nl.c,pf_nl.h,pflow.c,inet_nat64.c}`, `sys/net/pfvar.h`, `lib/libpfctl/`
- `freebsd-src/share/man/man5/pf.conf.5`, `sbin/ipfw/ipfw.8`
- `freebsd-src/sys/netpfil/ipfw/ip_fw_eaction.c`, `sys/netinet/ip_fw.h`
- `freebsd-src/sys/netlink/route/{common.h,iface.c,rt.c,neigh.c,nexthop.c}`, `sys/netinet/ip_carp.c`
- `freebsd-src/sys/net/{if_epair.c,if_vxlan.c,if_vxlan.h,if_bridge.c,if_geneve.c,bpf_jitter.c}`, `sys/dev/wg/`, `sys/dev/netmap/`, `sys/netgraph/`
- `freebsd-src/sys/compat/linux/linux_netlink.c`
- `openbsd-src/sys/net/{pf.c,pfvar.h}`, `netbsd-src/sys/net/{bpfjit.c,npf/npf_bpf.c}`, `DragonFlyBSD/sys/net/{pf,ipfw3}`

Web:
- [FreshPorts net/containernetworking-plugins](https://www.freshports.org/net/containernetworking-plugins/)
- [Port introduction commit (PR 267184)](https://lists.freebsd.org/archives/dev-commits-ports-all/2022-November/044168.html)
- [dfr/plugins freebsd branch](https://github.com/dfr/plugins/tree/freebsd)
- [generic-ebpf](https://github.com/generic-ebpf/generic-ebpf)
- [eBPF for FreeBSD, BSDCan 2018](https://papers.FreeBSD.org/2018/bsdcan/hayakawa-eBPF_Implementation_for_FreeBSD.files/hayakawa-eBPF_Implementation_for_FreeBSD.pdf)
- [GSoC 2020 eBPF XDP hooks](https://wiki.FreeBSD.org/SummerOfCode2020Projects/eBPFXDPHooks)
- [FreeBSD Status Report Q4 2025](https://lists.freebsd.org/archives/freebsd-current/2026-February/009928.html)
- [FreshPorts www/envoy](https://freshports.org/www/envoy/)
- [Bug 247236 envoy BoringSSL](https://bugs.freebsd.org/bugzilla/show_bug.cgi?id=247236)
- [FreshPorts dns/coredns](https://www.freshports.org/dns/coredns)
- [FreshPorts net/frr10](https://www.freshports.org/net/frr10)
- [FreshPorts net/traefik](https://freshports.org/net/traefik/)
- [K8s on FreeBSD with Linux workers + Cilium (prior art; FreeBSD is not the worker)](https://medium.com/@norlin.t/kubernetes-on-freebsd-with-linux-worker-nodes-and-cilium-a87c50daef03)

Not measured, so treat as estimates: per-rule pf cost and the resulting per-connection latency at 15k
rules; LOC and effort figures. To verify on hardware: per-rule `src.track` timeout honouring
with `sticky-address`; whether pf `match ... rdr-to` outbound works for host-originated
ClusterIP traffic on the specific release targeted (the -CURRENT man page says yes; 14.x lacks it).
