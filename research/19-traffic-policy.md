# 19. Service traffic policies in the pf kube-proxy

*2026-11-18. Two FreeBSD 15.1 arm64 nodes, kernel `bsdk8s15`, kube-proxy pf mode.*

## The bug

kube-proxy's pf mode (dfr's proxier plus our fixes) wrote every rule of a Service port (ClusterIP, NodePorts,
host-originated rules) against one endpoint set, `allLocallyReachableEndpoints`. That set ignores the traffic
policies:

- **`externalTrafficPolicy: Local`** is meant to keep the client's source address by only using endpoints on the
  node the traffic arrived at. Instead, external NodePort traffic was round-robined to endpoints on other nodes too
  and masqueraded to the node's address, so the client address was lost on about half the connections. A node with
  no local endpoint accepted the traffic instead of dropping it, so a load balancer's health check couldn't steer
  clients away from it.
- **`internalTrafficPolicy: Local`**: ClusterIP traffic still reached endpoints on other nodes.

Found while spreading the demo Gateway's two Envoy replicas, whose Service uses `externalTrafficPolicy: Local`.
Measured with the old kube-proxy: of 6 requests from node 2 to node 1's Gateway NodePort, 3 went across to the Envoy
on node 2, which logged the client as `192.168.100.1` (node 1's own address) instead of `192.168.100.2`.

## The fix (kubernetes `bsdk8s-v1.36.1` b4ce4a26065, patch 0023)

Each kind of traffic now gets its own pf target (endpoints and port), as in the iptables proxier:

| Traffic | Endpoints |
|---|---|
| to the ClusterIP | internal: local only with `internalTrafficPolicy: Local`, else all |
| to a NodePort, from outside the cluster | external: local only with `externalTrafficPolicy: Local` (not masqueraded), else all. Local with no local endpoint: `block drop` |
| to a NodePort, from this node's pods, with `externalTrafficPolicy: Local` | all (short-circuit): an `rdr from <pod CIDR>` placed ahead of the external rule |
| to a NodePort, from the node itself, with `externalTrafficPolicy: Local` | all (the `pass out on lo0 ... rdr-to` host rule) |

Choosing among endpoint port groups of equal size is now deterministic.

**`detectLocalMode: NodeCIDR`.** The short-circuit uses kube-proxy's local-traffic detector, like iptables. Its
default, the cluster CIDR, also matches pods on *other* nodes: the bsd-hostgw CNI doesn't masquerade pod-to-node
traffic, so a pod calling another node's NodePort arrives with its pod address and was short-circuited. The upstream
e2e test expects that to fail like any external client. Linux CNIs usually masquerade it. The cluster's kube-proxy
ConfigMap now sets `detectLocalMode: NodeCIDR`, and so does the demo's `build-cluster.sh` (kubernetes-demo
`def2ac0`).

## Verification

Manual, with a NodePort and a ClusterIP Service whose only endpoints (two nginx pods) are on node 1:

| Case | Expected (Linux) | Result |
|---|---|---|
| node 1 host → node 2 NodePort (external, no local endpoint) | dropped | timeout |
| node 2 host → its own NodePort | short-circuit | 200 |
| pod on node 2 → node 2 NodePort | short-circuit | 200 |
| node 2 host → node 1 NodePort (external, local endpoint) | client address kept | 200, nginx logs `192.168.100.2` |
| pod on node 2 → `internalTrafficPolicy: Local` ClusterIP | no local endpoint | timeout |
| node 1 host → the same ClusterIP | local endpoints | 200 |

Gateway, after the change: each node's Envoy receives all of that node's NodePort traffic and logs the real client
address.

Upstream e2e (unmodified `e2e.test`, `--focus='(Services|Conntrack).*TrafficPolicy'`, LoadBalancer tests skipped):
**9/9 passed**. Before the detector change, 7/8 passed; the failure was "should fallback to local terminating
endpoints when there are no ready endpoints with externalTrafficPolicy=Local", for the reason above. No run of
these tests exists for the old kube-proxy: they aren't conformance tests, so earlier runs skipped them.

Unit tests: `pkg/proxy/pf` `TestWriteServiceRules{Order,ExternalLocal,ExternalLocalNoLocalEndpoint}`.

## Consequence for the demo Gateway

With Local enforced, a node without an Envoy pod drops Gateway traffic sent to its NodePort. The EnvoyProxy `bsd` now
spreads the replicas: `topologySpreadConstraints` on `kubernetes.io/hostname`, `maxSkew: 1`, `DoNotSchedule`,
`matchLabelKeys: [pod-template-hash]` (each rollout spreads its own pods; without it the old pods counted and both new
replicas landed on one node), and `nodeTaintsPolicy: Honor` (a cordoned node doesn't keep a replica Pending during a
drain). 211/212 requests returned 200 during that rollout (old kube-proxy).

## Health, `healthCheckNodePort`, external and LoadBalancer IPs (patch 0024, b4ce4a26065 → 67a12b4950a)

Found while checking `healthCheckNodePort`:

- **`/healthz` and `/livez` returned 503 on both nodes.** The proxier only marked itself updated at startup, while
  every Service change queued an update, so the health server reported kube-proxy stale from the first change
  on. A liveness probe would restart kube-proxy forever; a load balancer using `/healthz` would take every node
  out. It now marks each completed sync, as the iptables proxier does: 200 on both nodes, still 200 100 s after the
  last change.
- **`healthCheckNodePort` wasn't served.** The per-service health server was created but never synced. Now, for a
  `type: LoadBalancer`, `externalTrafficPolicy: Local` Service backed by two pods on node 1: node 1 answers 200
  (`"localEndpoints": 2`), node 2 answers 503.
- **`externalIPs` and LoadBalancer ingress IPs had no rules**, so MetalLB-style bare-metal load balancers couldn't
  work. They now take traffic on the service port with the same rules as NodePorts: external policy, pod and
  node short-circuit, masquerading to remote endpoints, `block drop` with Local and no local endpoint. The node's
  own connections use an outbound `rdr-to`, since the address needn't be on `lo0`.

Verified with an external IP (`192.168.100.200`) and an LB ingress IP set by hand in the Service status
(`192.168.100.201`), both host-routed from node 2 to node 1: 200, and nginx logged the real client `192.168.100.2`.
Node 1's host and a pod on node 2 also got 200. On node 2 (no local endpoint) the anchor holds the pod
short-circuit, the host `rdr-to` and the `block drop`. I couldn't send truly external traffic to node 2's LB IP:
the only other host is node 1, whose own rules short-circuit it, as on Linux.

Upstream e2e (traffic policy, externalIP, health-check focus; LoadBalancer, Serial and Disruptive tests skipped):
**10/10 passed**, including "should be possible to connect to a service via ExternalIP when the external IP is not
assigned to a node". "should implement NodePort and HealthCheckNodePort correctly when ExternalTrafficPolicy
changes" skips itself without a LoadBalancer provider.

## Not done

- **`loadBalancerSourceRanges`**: not enforced (no firewall rules for LB IPs).
- **Rebalancing**: after a drained node comes back, both Envoy replicas stay on the other node until the next
  rollout (as on Linux; that's the descheduler's job). Meanwhile the returning node drops Gateway NodePort traffic.
