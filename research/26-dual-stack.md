# 26. IPv4/IPv6 dual-stack

*2026-11-23. Two FreeBSD 15.1 arm64 nodes, kernel `bsdk8s29`.*

Until now the cluster was IPv4-only and IPv6 had never been tested: every `[Feature:IPv6DualStack]` test
failed or was skipped, bsd-netpol's IPv6 rules were untested (research/22), and SCTP's dual-stack variant
couldn't run (research/23). The 2-node cluster was converted in place to dual-stack, after a `bectl`
boot environment `pre-dualstack` on each node (rollback: `bectl activate pre-dualstack`, reboot).

## Converting a running cluster

| Piece | Before | After |
|---|---|---|
| Node addresses (vtnet1) | 192.168.100.1, .2 | also `fd00:100::1`, `::2` (`ifconfig_vtnet1_ipv6`), `ipv6_gateway_enable` |
| Service CIDR (apiserver, controller-manager) | 10.96.0.0/12 | `10.96.0.0/12,fd00:10:96::/112`; the default ServiceCIDR object followed the flags |
| Pod CIDR (controller-manager) | 10.244.0.0/16, /24 per node | `10.244.0.0/16,fd00:10:244::/56`, /64 per node |
| kubelet `--node-ip` | v4 | `192.168.100.N,fd00:100::N` |
| CNI (`10-kubetest.conflist`) | one host-local range | a second range (the node's IPv6 /64, gateway `::1`) and a `::/0` route |
| bsd-hostgw (cross-node pod routes) | `spec.podCIDR` via the InternalIP | every `spec.podCIDRs` entry via the node's InternalIP of the same family |
| Service CIDR route on the nodes | `k8ssvc` (v4) | also `k8ssvc6`: `fd00:10:96::/112` via vtnet1 |
| kube-proxy, kubeadm-config | v4 `clusterCIDR`, subnets | both families |

A node's `podCIDRs` can't change once set ("node updates may not change podCIDR except from \"\" to
valid"), so both Node objects were deleted and re-registered with the new kubelet flags (labels restored by
hand: `bsdk8s.io/linux-abi`, and on the control plane `node-role.kubernetes.io/control-plane` and
`node.kubernetes.io/exclude-from-external-load-balancers`). The allocator handed node 1 a new IPv4 range
(10.244.2.0/24 instead of 10.244.0.0/24): its bridge kept the old gateway address and the CNI plugin then
refused every pod ("already has an IP address 10.244.0.1/24 different from 10.244.2.1/24") until that was
removed. Every pod not on the host network was deleted and came back with two addresses.

`build-cluster.sh` in the demo does all this for a new cluster when `NODE_IP6` is set on every node
(kubernetes-demo fork f5025e6; checked offline, not yet used for a fresh build).

The FreeBSD side needed nothing for the basics: the bridge CNI plugin sets `inet6` addresses and `route -6`
routes in the pod's jail and turns on IPv6 forwarding; a pod's global address is usable at once (only its
link-local one stays `tentative` for DAD); pod to pod across nodes, node to pod, and a `RequireDualStack`
Service's IPv6 ClusterIP from a pod and from a node all worked on the first try.

## What broke, and the fixes

- **A node couldn't reach its own NodePort over IPv6** (freebsd-src 0048). The node's connection to
  `[fd00:100::1]:<nodePort>` is redirected on the way out (`pass out on lo0 ... rdr-to <pod>`); the pod's
  reply comes back with the node's own address as source, and `ip6_input()`'s source address validation
  (`net.inet6.ip6.source_address_validation`) dropped it. IPv4 had the same problem, fixed by freebsd-src
  0006, which only covered IPv4. With the sysctl off the connection worked, which confirmed the cause; 0048
  marks those IPv6 replies for local delivery too, like 0006. Kernel `bsdk8s29` on both nodes.
- **kube-proxy's two IP families fought over pf** (kubernetes 0065). A dual-stack kube-proxy runs one pf
  proxier per family and they sync at the same time. Loading an anchor takes a ticket for pf's address pool
  buffer, which is system-wide, so concurrent loads invalidated each other: `DIOCADDADDR: Device busy` and
  `DIOCADDRULE: Device busy`, 297 and 369 times on the two nodes, and the affected Service kept its old
  rules (or none), because the failure was only logged. Every pfctl command that changes pf now runs through
  `runPfctl`: one at a time per process, retried while pf reports the device busy (bsd-netpol and the CNI
  portmap plugin also load rules), and a sync with an anchor that still failed returns an error so the
  runner retries it. After deployment: 0 errors.
- **bsd-hostgw** routed only `spec.podCIDR` (the first family); it now routes every pod CIDR through the
  node address of the same family (demo e011dd4).

## Results

| Check | Result |
|---|---|
| `[Feature:IPv6DualStack]` (23 tests, not Disruptive/Flaky) | 21/23 first; the 2 failures were node → own NodePort over IPv6 (fixed above); then **23/23** |
| Conformance run 17 (dual-stack, `bsdk8s29`) | **437/446**, the same 9 failures as runs 13-16 |
| `[Feature:NetworkPolicy]` and SCTP, including SCTP's dual-stack variant | 50/52: every NetworkPolicy test, the dual-stack SCTP test; the 2 failures below |
| sig-network beyond the conformance set (not Conformance, Feature, Serial, Slow, Disruptive, Flaky, LinuxOnly; 68 tests) | **67/68**, the same as on the IPv4 cluster: the one failure is the CLOSE_WAIT test, which reads Linux conntrack (research/07) |
| All `[NodeConformance]` (not Serial, Slow, Disruptive, Flaky; 235 tests) | **231/235**, the same as on the IPv4 cluster: the 4 `[LinuxOnly]` tests (two sysctl, two `ip link add ... type dummy`, research/07) |

## Limit: SCTP Services and multi-homed pods

`Granular Checks: Services ... pod-Service: sctp` and `endpoint-Service: sctp` passed on the IPv4 cluster
(research/23) and fail on the dual-stack one. A capture shows why: the handshake through the ClusterIP
completes, but a dual-stack pod has more than one address, so its INIT ACK lists them (SCTP multi-homing),
and the client takes the pod's real address as a second path. Its first HEARTBEAT goes to
`<pod IP>:<Service port>`, which no rdr translates; the pod has no association on that port and answers
ABORT, and the client drops the whole association. On the IPv4-only cluster a pod had one address and listed
none. pf follows multi-homed associations for filtering (`pf_multihome_scan`), but not through a translation.
Kubernetes doesn't support this either: its documentation says NAT of multi-homed SCTP associations "requires
special logic in the corresponding kernel modules", which no kube-proxy backend has. Fixing it here would mean
pf rewriting or dropping the address parameters of INIT and INIT ACK chunks it translates. Not done.

**IPv6 LoadBalancer addresses:** bsd-lb now assigns and announces them (NDP), research/25.

**IPv6 NetworkPolicy, checked by hand** (the upstream tests use each pod's first address, IPv4 here): a
server on node 2, a client labelled `role=ok` on node 1 and another on node 2, each trying the server's IPv4 and
IPv6 address. No policy: all open. Deny-all ingress: all blocked. Ingress from `role=ok` on port 8080: `ok` open on
both families, the other blocked on both. Ingress from `ipBlock: fd00:10:244:1::/64` (node 1's IPv6 pod range):
`ok` open over IPv6 only, the other blocked. bsd-netpol logged no errors.

**IPv6 egress to the outside.** The nodes had no IPv6 uplink: once a node forwards IPv6, FreeBSD ignores router
advertisements unless `net.inet6.ip6.rfc6204w3=1`, so vtnet0 never got QEMU's `fec0::/64` address and default route
(fixed: `ifconfig_vtnet0_ipv6="inet6 accept_rtadv"`, `rtsold_enable`, the sysctl). Pods then need NAT66, the IPv6 twin
of the IPv4 pod egress rule: `nat on vtnet0 inet6 from fd00:10:244::/56 to ! fd00:10:244::/56 -> (vtnet0)` (pf
leaves link-local addresses out of `(vtnet0)`). Pods on both nodes then reached `www.freebsd.org` over IPv6, 9/9,
translated to the node's `fec0::` address. The node itself still can't: source address selection prefers its ULA
(`fd00:100::1`) over the deprecated site-local `fec0::`, and QEMU only forwards its own prefix (on a network with a
global IPv6 uplink address that one would win). `build-cluster.sh` adds the NAT66 rule and the sysctl with
`NODE_IP6` (demo 485dfef). Pods resolve external names as before (CoreDNS forwards to the IPv4 resolver).

Not checked: IPv6-only clusters.
