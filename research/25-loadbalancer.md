# 25. LoadBalancer Services on bare metal (bsd-lb)

*2026-11-23. Two FreeBSD 15.1 arm64 nodes, kernel `bsdk8s28`.*

A `type: LoadBalancer` Service stayed `<pending>` forever: nothing assigned it an address. Clouds do this
with a cloud-controller-manager; bare-metal Linux clusters use MetalLB, whose controller is portable Go
but whose layer 2 speaker is Linux-only (AF_PACKET, research/03 gap 8). The dataplane side was already
done: the pf kube-proxy has carried LoadBalancer ingress IPs since research/19, with the same rules as
NodePorts (traffic policy, short-circuit for the node and pods, masquerading to remote endpoints, source
ranges). What was missing was the address and its announcement. `components/bsd-lb` does both.

## Design

One static binary and image, two modes:

- **controller** (Deployment, 1 replica, `Recreate`): for each Service with `type: LoadBalancer` and no
  `loadBalancerClass` (or `bsdk8s.io/bsd-lb`), adds the `service.kubernetes.io/load-balancer-cleanup`
  finalizer, then writes one IPv4 address from `-pool` into `status.loadBalancer.ingress` with
  `ipMode: VIP`. It keeps an address the Service already has, honours the deprecated
  `spec.loadBalancerIP` if that is a free pool address, else takes the lowest free one. One worker, so two
  Services can't get the same address. When the Service is deleted or its type changes, it clears the
  status and drops the finalizer. Services of another class are left alone.
- **speaker** (DaemonSet, host network, `NET_RAW` only): CRI-O gives a container with `NET_RAW`
  `/dev/bpf` (CRI-O 0020). The speaker opens bpf(4) on the interface holding the node's InternalIP, filters
  ARP requests in the kernel (classic BPF), and answers those for the addresses this node announces. A
  node that takes an address over sends three gratuitous ARPs. The addresses are not configured on any
  interface: pf's `rdr` rules take the traffic that arrives for them, as for an external IP.

Which node announces an address: every speaker computes the same answer from the same API objects. Among
the nodes that are Ready, not labelled `node.kubernetes.io/exclude-from-external-load-balancers`, and whose
speaker Lease (`kube-system/bsd-lb-speaker-<node>`, renewed every 10 s, valid 30 s) is current, the one with
the lowest SHA-256 of `node#address` wins. With `externalTrafficPolicy: Local` only nodes with a ready
endpoint qualify, and with none, nobody answers. The hash spreads addresses over nodes, the way MetalLB's
layer 2 mode does; the Lease replaces MetalLB's memberlist, so a node whose speaker died stops answering
within 30 s although the node is still Ready. The speaker records a `nodeAssigned` event on the Service
("announcing 192.168.100.200 from node bsdk8s-2").

Deploy (pool: free addresses on the nodes' L2 network):

```
components/bsd-lb/build.sh            # on a node: localhost/bsd-lb:dev
sed -e "s|\${IMAGE}|localhost/bsd-lb:dev|" -e "s|\${POOL}|192.168.100.200-192.168.100.250|" \
  components/bsd-lb/deploy/bsd-lb.yaml | kubectl apply -f -
```

Unit tests: pool parsing and allocation, the election (deterministic, Local policy, spread), ARP request
parsing, reply and gratuitous frames, and which Services are handled. The first run caught an IPv6 pool
prefix panicking before the IPv4-only check.

## Checked by hand

- A Deployment of 2 nginx pods, `kubectl expose --type=LoadBalancer`: within a second
  `{"ingress":[{"ip":"192.168.100.200","ipMode":"VIP"}]}`, the finalizer, and the event "announcing
  192.168.100.200 from node bsdk8s-2".
- ARP: node 1 pinged the address (ICMP isn't redirected, so the kernel has to resolve it) and its ARP table
  then held `192.168.100.200 at 52:54:00:bd:01:02`, node 2's vtnet1 address.
- HTTP to the address from node 1's host and from a pod on node 1: 200.
- `type: ClusterIP` afterwards: status empty, finalizer gone.

## Session affinity for the node's own connections (freebsd-src 0047, kubernetes 0064)

The 4 LoadBalancer session-affinity tests failed ("Affinity should hold but didn't"): their client is the
e2e binary on node 1, whose connections pf redirects on the way out (`pass out ... rdr-to`), and the pf
proxier never put `sticky-address` on those rules. Stock 15.x pf records the client's own address for
outbound rdr-to source tracking and would then send every later connection of that client to itself. The
bsdk8s kernels have carried the fix since the first one (freebsd-src 0002), but kube-proxy couldn't tell.

- **freebsd-src 0047:** pf advertises the fix as `kern.features.pf_rdr_out_sticky`.
- **kubernetes 0064:** the pf proxier reads it once at start and, where it is set, adds `sticky-address` to
  the host-side redirects (ClusterIP, externalIPs, LoadBalancer IPs, the node's own NodePorts) of Services
  with `sessionAffinity: ClientIP`. On a stock kernel nothing changes.

Both nodes boot `kernel.bsdk8s28`. All 19 affinity tests (`--focus='[Aa]ffinity'`, `[LinuxOnly]` included):
**17/19**, every LoadBalancer, NodePort and ClusterIP affinity test passing. The 2 failures are
`[Feature:IPv6DualStack]` "Secondary IP Family" tests, and this cluster is single-stack IPv4.
Regression on `bsdk8s28` with the new kube-proxy: sig-network `[Conformance]` **47/47**.

## IPv6 (2026-11-23, on the dual-stack cluster, research/26)

The pool takes IPv6 ranges and prefixes too (a prefix leaves out its Subnet-Router anycast address), and the
controller gives a Service one address per IP family it lists, in its order (`ipFamilyPolicy: RequireDualStack`
gets both); a family the pool doesn't cover is reported in an `AllocationFailed` event and the other one is still
served. The speaker announces addresses in any subnet of the interface: IPv4 by ARP as before, IPv6 by NDP. The
kernel bpf filter also passes ICMPv6 Neighbor Solicitations; the speaker answers those for its addresses with a
solicited Neighbor Advertisement (target link-layer option, ICMPv6 checksum over the pseudo-header), joins each
address's solicited-node multicast group so the interface passes the solicitations up at all, and sends three
unsolicited advertisements when it takes an address over (the counterpart of gratuitous ARP). Unit tests cover the
IPv6 pool, per-family allocation, and the NDP frames, including a checksum that verifies.

Checked: a `RequireDualStack` LoadBalancer Service got `192.168.100.200` and `fd00:100::201`, both announced from
node 2; node 1 pinged the IPv6 address and its neighbor cache then held `fd00:100::201 52:54:00:bd:01:02 vtnet1 R`
(node 2's interface, reachable); HTTP to both addresses from node 1's host and from a pod on node 1 succeeded.

## e2e

`--focus=LoadBalancer`, not Disruptive/Flaky/Serial, `[Slow]` and `[LinuxOnly]` included (18 tests). Before
bsd-lb no test could get an address. With bsd-lb: 11/18, then **15/18** with session affinity for host
clients (the 4 affinity tests rerun above). The 3 left assume the e2e binary runs outside the cluster, as in
upstream CI; here it runs on node 1:

| Test | Why it fails here |
|---|---|
| `ExternalTrafficPolicy: Local` should work for type=LoadBalancer | Checks the client address seen by the pod is *not* in the node subnet (/16). The client is node 1 (192.168.100.1), and the pod saw exactly that: the source was preserved, but it is a node address. |
| should only allow access from service loadbalancer source ranges | The allowed ranges are the accept pod's IP and its node's IP. That node is node 1, which is also where the "outside the cluster" client runs, so that client is allowed. |
| should be able to change the type and ports of a UDP service | With the backend scaled to 0 the test wants the UDP port refused. pf rejects the node's own packet on output, so `write(2)` returns EPERM, which the test counts as reachable. Linux does the same for a sender on the node (a REJECT in OUTPUT makes `sendmsg` fail with EPERM); a sender outside gets the ICMP port unreachable and passes. |


On the dual-stack cluster (2026-11-24, with IPv6 support in bsd-lb): **14/18**, the 3 above plus one that passed on
the IPv4 cluster, `ExternalTrafficPolicy: Local should only target nodes with endpoints`. It checks a Service's
`healthCheckNodePort` on every InternalIP of every node, of both families (upstream ff7c781e251, 2025), but the
Service is IPv4-only, and since upstream 0659a346eac (2026, in 1.36) kube-proxy's IPv4 proxier binds its health
checks with `tcp4` only, to stop the two families colliding on dual-stack: nothing listens on the node's IPv6
address, `connection refused`. The pf proxier uses the same upstream health server, so Linux behaves the same; a
mismatch between the test and kube-proxy upstream, seen only on dual-stack clusters with a LoadBalancer.

**From a client outside the cluster** (2026-11-24): a third VM (`bsdk8s-3`, not a node, on the same vde switch,
docs/MAC-DEV-VM.md) ran the e2e binary with an address outside the node subnet (`10.99.0.3/24`, on-link routes both
ways). The UDP type and ports test, the source ranges test and `ExternalTrafficPolicy: Local` "should work for
type=LoadBalancer" (source address preserved: the pod saw `10.99.0.3`) all **pass**: those three failures came from
the e2e binary running on node 1. A first try also needed `kubectl` on the client (the tests exec into pods with it)
and dropping kubeadm's `node.kubernetes.io/exclude-from-external-load-balancers` label from the control plane:
here it runs workloads, and with the label bsd-lb correctly never announced from it, so a Service whose only
endpoint was there (`ExternalTrafficPolicy: Local`) had no node to announce it (in upstream CI the control plane is
tainted, so the test never puts an endpoint there). "should only target nodes with endpoints" then got past that
and fails only at the IPv6 health check above. So all 18 LoadBalancer tests pass except that one upstream mismatch.

The control-plane node carries kubeadm's `node.kubernetes.io/exclude-from-external-load-balancers` label, so
only node 2 ever announced: as intended.

Not done: a client outside both nodes (QEMU's 192.168.100.0/24 has only the two VMs), BGP.
Delivery of traffic that arrives from outside for an address node 2 announces was verified in research/19
(an external IP and a LoadBalancer IP routed by hand to a node, nginx logging the real client).
