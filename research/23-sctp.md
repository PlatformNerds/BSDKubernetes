# 23. SCTP: Services, NetworkPolicy, HostPorts

*2026-11-19. Two FreeBSD 15.1 arm64 nodes, kernel `bsdk8s20`.*

Kubernetes treats SCTP as a third Service protocol next to TCP and UDP. FreeBSD has a native
SCTP stack (`sctp.ko`) and pf has tracked and NATed SCTP since 14.0, so most of it should just
work. Until now no SCTP test could even run. Two things were missing, and pf had one deliberate
gap.

## 1. agnhost had no SCTP on FreeBSD

agnhost's SCTP server and client (`porter`, `netexec`, `connect`) use
`github.com/ishidawataru/sctp`. That library had only a Linux implementation. Elsewhere every
call returned "SCTP is unsupported on freebsd/arm64", so the tests failed before sending a
packet.

FreeBSD has the same RFC 6458 one-to-one socket API. What differs:

- socket option numbers (`SCTP_INITMSG` is 3, not 2; `SCTP_BINDX_ADD_ADDR` is `0x8001`, not 100);
- message flags (`SCTP_EOF` is `0x100`, not `1 << 4`) and `MSG_NOTIFICATION` (`0x2000`);
- `struct sctp_sndrcvinfo` is 128 bytes, not 32. Linux's 32 are a prefix of it, and the kernel
  ignores a shorter cmsg;
- `struct sctp_event_subscribe` has an 11th field;
- sockaddrs carry `sa_len`;
- `SCTP_BINDX_ADD_ADDR` takes a bare sockaddr, one per call;
- `SCTP_GET_LOCAL_ADDRESSES` / `SCTP_GET_PEER_ADDRESSES` return sockaddrs packed by `sa_len`;
- at EOF, with DATA_IO events on, FreeBSD still attaches a zeroed SNDRCV cmsg. "0 bytes and no
  cmsg" therefore never happens; "0 bytes and not a notification" is EOF.

The port (kubernetes patch 0034, in `vendor/`) moves Linux's constants and address code to a
`!freebsd` file unchanged and adds `sctp_freebsd.go`.

A standalone check on node 2 exercised every function, 3 of 3 runs:

- multi-homed listen (127.0.0.1 + the node address) and dial via `SCTP_CONNECT_X`;
- SndRcvInfo in both directions (stream, PPID) and event subscription;
- default send parameters;
- local and peer address lists;
- EOF and a refused connect.

Not ported (return `ErrUnsupported`): `GetStatus`, `SCTPGetPrimaryPeerAddr`, `PeelOff`. Their
FreeBSD structs differ from the Linux ones the package exposes.

## 2. pf never changes SCTP ports

With agnhost fixed, pod → ClusterIP over SCTP still failed: every connection was refused. The
rdr rule was right (`rdr ... port 95 -> { ... } port 8081`), but the states said
`10.244.1.32:95 (10.101.211.105:95)`: the address was translated and the port was not.

That is deliberate in pf (`6053adafaa`, "pf: add SCTP NAT support"): "we refuse to change ports
for SCTP, to avoid interfering with multihomed connections". `pf_get_transaddr()` returns before
computing the rdr port for SCTP. Two consequences follow:

- SCTP's CRC32c covers the ports (unlike TCP's and UDP's checksums, it doesn't cover the
  addresses), so pf never has to recompute it;
- a Kubernetes Service whose `targetPort` differs from its `port`, and every SCTP NodePort,
  sends connections to the wrong port.

The multihoming cost is real. Say the server advertises other addresses in its INIT-ACK. The
client's backup paths then go to those addresses on the Service port, which the server doesn't
listen on, so the paths stay unconfirmed. The primary path is unaffected. Linux kube-proxy
(iptables DNAT) has the same trade-off. A pod normally has one address, so there is nothing
else to reach.

**Fix** (freebsd-src patch 0033; branches `bsdk8s/pf-sctp-rdr-port` on main and `-15.1`): a
sysctl, `net.pf.sctp_rdr_ports`, off by default so pf's behaviour doesn't change.

- When the sysctl is set, rdr computes the SCTP destination port like TCP's. The source port is
  still never changed.
- `pf_change_ap()` flags an SCTP port change. Once the header is copied back,
  `pf_sctp_cksum_fixup()` recomputes the CRC32c with pf's existing `pf_sctp_checksum()`.
- It skips that while the checksum is still left to the stack (`CSUM_SCTP`: a packet the node
  itself sends, checksummed on output).
- `pf_packet_rework_nat()` now copies the SCTP header back too.
- New ATF test `sctp:rdr_port_v4`.
- pf.4 documents the sysctl.

The nodes set it in `/etc/sysctl.conf`, as does `build-cluster.sh` when the kernel has it. The
demo also loads `sctp.ko` at boot. kube-proxy logs once per Service when an SCTP Service needs a
port change that pf won't do (kubernetes patch 0035). Otherwise that failure is silent.

## Results

| Check | Before | After |
|---|---|---|
| SCTP e2e (`SCTP\|sctp`, without the dual-stack variant) | 0 could run | **10/10** |
| ... of which pod → ClusterIP (`Granular Checks: Services ... pod-Service: sctp`) | refused | passes |
| ... the 5 NetworkPolicy SCTP tests (bsd-netpol, research/22) | 0 could run | 5/5 |
| ... SCTP HostPort, basic SCTP Service with endpoints | 0 could run | 2/2 |
| ... intra-pod, endpoint → Service | 0 could run | 2/2 |
| SCTP connect through a ClusterIP, from a pod (node 2) | refused | 12-35 ms (direct to the pod: 12 ms) |
| `[sig-network] Services` (regression) | 48/48 | 48/48 |
| `[Feature:NetworkPolicy]` (regression) | 43/43 | 43/43 |
| pf ATF `sctp` + `nat64:sctp_*` on node 2 | | 13 pass, `pfsync` skipped (no `carp`) |

The intra-pod test is skipped unless e2e gets `--num-nodes=2` (it checks the flag, not the
cluster). Upstream skips node → pod and node → Service for SCTP everywhere (`DialFromNode` has no
SCTP, kubernetes#96482). The dual-stack variant needs an IPv6 cluster.

One bug was found only by timing. The first version of the pf patch translated the port on every
packet but the first. The SCTP case of `pf_translate_compat()` never asked for the header to be
copied back, since it never used to change it. The INIT therefore went out with its address
translated and its port not, and was lost. The client's retransmission one RTO later
(`net.inet.sctp.rto_initial`, 1 s) went through the state path and worked. So every connection
through a Service took about 1050 ms instead of 12 ms, and all the e2e tests still passed. Fixed
before the patch was published.

The pf ATF tests are timing-sensitive on these VMs, with or without the patch. The servers are
given `sleep 1` to start. Upstream's own `rdr_v4` failed 2 of 5 repeated runs on a busy node,
and `basic_v4` (no rdr at all) 1 of 7. `rdr_port_v4` passed 6 of 7.

## Limits

- The multihoming trade-off above. It is opt-in for that reason.
- The library port covers what Kubernetes and agnhost use, plus events and send/receive info.
  Association status, primary address and peel-off are not ported.
- The dual-stack SCTP variant needs an IPv6 cluster (this one is IPv4-only).
