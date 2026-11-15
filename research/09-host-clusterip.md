# 09: Host-network clients and ClusterIPs on pf (implemented 2026-11-15)

**Problem.** pf `rdr` rules only translate packets *arriving* on an interface. Connections the node itself opens to
a ClusterIP were never redirected. That covers host-network pods, the kubelet, and kube-apiserver calling webhooks
and aggregated APIs, the root cause behind `--enable-aggregator-routing` in research/07. Those packets left through
the default route to a Service address that exists nowhere.

**Dev-VM trap.** With QEMU user networking (`vtnet0`), the problem *looked* fixed. slirp accepts any outbound TCP
connection itself and relays DNS out to the Mac, which answered `NXDOMAIN`, so `nc -z` and `drill` both "worked".
A capture on `vtnet0` showed the packets leaving unredirected. Verify with real data (an HTTP status code, a
resolved name), not with a successful connect.

## Fix

FreeBSD 15's pf accepts OpenBSD-style translation in filter rules, and `rdr-to` works on **outbound** packets,
including locally originated ones. kube-proxy (`PlatformNerds/kubernetes` `15b6e679254`, patch `bsdk8s/0010`) now
adds this rule to each Service anchor:

```
pass out quick inet proto udp from any to 10.96.0.10 port 53 rdr-to { 10.244.0.206, 10.244.0.207 } port 53 round-robin
```

Requirements and pitfalls found on the way:

| Finding | Consequence |
|---|---|
| pf rejects an anchor whose `rdr`/`nat` rules don't all come before its filter rules | The first version put the rule mid-anchor: **every NodePort Service failed to load** (caught by the e2e run, not the per-rule unit test). Now written last; a unit test checks the order. |
| `match out ... rdr-to` translates but creates no state; with no `pass` rule the default pass is stateless | Replies aren't un-translated (worked once, then failed). Use `pass out quick ... keep state`. |
| One rule can't carry both `nat-to` and `rdr-to`; pf picks the nat pool (`pf_get_transaddr`) | The source must come from routing instead: the redirected packet keeps the source of the interface the Service CIDR routes through. A multi-homed node must route the Service CIDR via its node-IP interface. The demo adds `route_k8ssvc`. Linux kube-proxy masquerades instead. |
| The main ruleset only had `rdr-anchor`/`nat-anchor "cni-rdr/*"` | The demo now adds `anchor "cni-rdr/*"` (filter). |
| `pfctl -a X -F all` also flushes the global state table | Anchors are flushed with `-F nat` + `-F rules`. |
| Outbound redirected states print as `src -> endpoint (frontend)` and are killed by `src -> endpoint` | Stale UDP state cleanup (research/07) handles both directions. |

## Kernel bug: sticky-address on outbound rdr-to

`pass out ... rdr-to { a, b } round-robin sticky-address` redirected the first connection correctly and every later
one from the same client **to the client's own address**. The cause is `pf_create_state()` in
`sys/netpfil/pf/pf.c`: it stores the sticky translated address as `nk->addr[1]`. That index is the destination
only for inbound packets; for outbound ones it's the (unchanged) source.

Fix: choose `didx` for redirections and `sidx` for NAT. The commit is on `PlatformNerds/freebsd-src`, branches
`bsdk8s/pf-sticky-rdr-out` (main) and `-15.1` (patch `freebsd-src/0001`, upstream candidate). Inbound rdr and
outbound nat behave exactly as before.

Verified on node 2 with a rebuilt `pf.ko`. Before: 1 of 6 DNS queries answered, source node `client -> client`.
After: 6 of 6, source node `192.168.100.2 -> 10.244.0.207`.

kube-proxy doesn't emit `sticky-address` on host rules, so it works on unpatched kernels. The trade-off:
**`sessionAffinity: ClientIP` isn't honoured for host-network clients.**

## Verified (two nodes, stock pf on node 1, patched on node 2)

- From both hosts: API server via `10.96.0.1:443` → HTTP 200 (endpoint is a host IP on node 1). CoreDNS via
  `10.96.0.10` resolves real names over UDP and TCP, with endpoints local or remote.
- e2e `Conntrack should be able to preserve UDP traffic when server pod cycles for a ClusterIP service and client
  is hostNetwork` passes. It failed in the previous run.
- Pod → NodePort and ClusterIP matrix: all six pod cases connect; host → the other node's NodePort connects.

**Not covered:** a host calling a NodePort on **its own** address. That traffic loops through `lo0`, and a
`pass out ... rdr-to` rule for it loads but doesn't work: the redirected packet apparently can't be forwarded from
`lo0`. Linux handles this in iptables' OUTPUT chain. It's open.
