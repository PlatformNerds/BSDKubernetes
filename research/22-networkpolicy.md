# 22. NetworkPolicy with pf (bsd-netpol)

*2026-11-18. Two FreeBSD 15.1 arm64 nodes, kernel `bsdk8s18`.*

Until now a NetworkPolicy was accepted by the API server and silently not enforced: nothing on
the nodes read it. research/03 §3 had the design; this is the implementation,
`components/bsd-netpol`.

## Where the rules go

Pods attach to the CNI bridge through the host side of an epair, and pf filters bridge members
(`net.link.bridge.pfil_member=1`, already set). From the host:

- egress of pod P is a packet `in` from P's address;
- ingress of pod P is a packet `out` to P's address.

Rules match addresses, not interface names, so the agent needs no mapping from pods to epairs.
The node's main ruleset evaluates `anchor "kube-np/*"` before kube-proxy's `anchor "cni-rdr/*"`
(added to `build-cluster.sh` in the demo). kube-proxy's split-port groups use filter rules
(`pass in quick ... rdr-to`) that would otherwise skip egress policy.

## Checked on a hand-written prototype first

Two questions decide whether this design works. Both were tested on node 2 with a server pod,
a client on the same node and a client on node 1, directly and through a ClusterIP:

| Question | Result |
|---|---|
| Does a NAT/rdr state created on the client's epair let the packet skip the server's ingress rules when it leaves on the server's epair? | No. A deny-all on the server blocked all four paths (pod and Service, same and other node). |
| Does a pass with `keep state` on the client's egress let the packet skip the server's ingress? | No. With the client allowed egress but not listed in the server's ingress, the client was blocked; with it listed, allowed. |
| Do replies to an ingress-isolated pod's own connections get back? | Only with a state. Via a ClusterIP the rdr state covered it, but a direct connection to an outside address was dropped until a `pass in ... keep state` was added. |

pf's default rule keeps no state (SSH to the node had none), so traffic no policy covers stays
stateless, as before.

## Rules per local pod

```
# ingress-isolated
pass out quick from (self) to P keep state                     # kubelet probes from the node
pass out quick proto tcp from <np_…> to P port 8080 keep state  # one per allowed (peers, port)
block drop out quick to P
# egress-isolated
pass in quick proto udp from P to <np_…> port 53 keep state
block drop in quick from P
# a direction that is not isolated still gets a state, for replies:
pass out quick to P keep state    |    pass in quick from P keep state
```

- **Peers become const tables inside the anchor**: one per ipBlock (`!except` entries, longest
  prefix wins) and one for the selected pods' addresses.
- **Named ports**: ingress resolves them against the protected pod; egress resolves them per
  destination pod.
- **`endPort`** becomes `port a:b`.
- **`policyTypes`** follows the API defaults.
- **Host-network and finished pods** are neither targets nor peers.

The agent rebuilds the whole anchor on any Pod, Namespace or NetworkPolicy event (debounced),
loads it with `pfctl -a kube-np/policy -f -` when it changed, and reloads it every minute in case
someone flushed it. It leaves the rules in place when it stops, so policies stay enforced across
restarts.

## Results

- `render_test.go`: selectors, namespaceSelector, ipBlock/except, named ports, endPort,
  egress-only, host-network and finished pods.
- Upstream e2e `[Feature:NetworkPolicy]` (excluding SCTP): **43/43** in 3 minutes. It covers
  deny/allow by pod and namespace selectors, ports and named ports, ipBlock with except,
  egress, multiple policies, and policy updates and deletion.

## Known limits

- Traffic from the node itself is always allowed to its pods (`from (self)`), which kubelet
  probes need. Other implementations do the same. A side effect: connections kube-proxy SNATs to
  a node address (NodePort with `externalTrafficPolicy: Cluster`) come from `(self)` and pass.
- Changes to a policy don't cut connections that are already established; their states remain.
  iptables-based implementations behave the same, through conntrack.
- IPv4 and IPv6 rules are rendered alike, but only IPv4 is tested (the cluster is IPv4).
- AdminNetworkPolicy / ClusterNetworkPolicy tiers (research/03 §3.2) are not implemented.
- The 5 SCTP variants (`[Feature:SCTPConnectivity] [LinuxOnly]`) weren't counted at first: agnhost's
  SCTP library (`ishidawataru/sctp`) was Linux-only. With it ported (research/23) they pass.
