# 14: Gateway API with Envoy Gateway (2026-11-16)

ingress-nginx is being retired; Gateway API is the replacement path. research/13's fixes are generic and apply here
too: digest-pinned Linux images, capability names, and non-root low ports per pod.

**Result [verified]:** Envoy Gateway **v1.10.0** installs from its unmodified `install.yaml` (sha256 `f87f4d59…`).
That manifest includes the Gateway API CRDs (10 `gateway.networking.k8s.io` CRDs).

- **Control plane:** the `envoy-gateway` controller and its cert-gen Job, both Linux images.
- **Data plane:** the Envoy proxy that the controller creates per Gateway, `envoyproxy/envoy:distroless-v1.40.2`,
  pinned by digest. It is C++ and runs under the Linuxulator in a jail.
- **Gateway status:** GatewayClass `eg` Accepted; Gateway `demo` **Programmed=True** at address 192.168.100.2;
  HTTPRoute `demo` Accepted and ResolvedRefs.
- **Traffic:** `demo.bsdk8s.test` → `lnx-nginx`. **200/200** requests returned 200, alternating between both
  nodes' NodePort; an unknown host gets 404.

## Placement: first real use of `auto` mode

None of Envoy Gateway's pods pin an OS: no `kubernetes.io/os` selector. The pin rule would therefore leave them
alone, and the FreeBSD runtime would fail to find a FreeBSD image. Instead, `envoy-gateway-system` was labeled
`bsdk8s.io/default-runtime=auto` before applying. bsd-placement looked each image up and sent every pod to
RuntimeClass `linux`. This included the Envoy proxies, which the controller creates at runtime:

    runtimeClassName=linux (linux-only image: envoyproxy/gateway:v1.10.0)

## The one blocker: abstract Unix sockets (fixed 2026-11-17)

Envoy crash-looped with:

    unable to bind domain socket with base_id=0, id=0, errno=2 (see --base-id option)

Envoy's hot restart uses Linux **abstract-namespace** Unix sockets: `sun_path` starts with NUL and has no
filesystem entry. FreeBSD had no abstract namespace, so the Linuxulator treated the name as a path and got ENOENT.

~~**Workaround:** `extraArgs: ["--disable-hot-restart"]` in the EnvoyProxy.~~ No longer needed. The EnvoyProxy is
now just:

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata: {name: bsd, namespace: envoy-gateway-system}
spec:
  provider: {type: Kubernetes, kubernetes: {envoyService: {type: NodePort}}}   # no LoadBalancer on bare metal
```

The GatewayClass's `parametersRef` points at it. `NodePort` is only there because there is no LoadBalancer
implementation. With the default `LoadBalancer` type, traffic worked the same and the Gateway reported "No
addresses have been assigned", as on bare-metal Linux without MetalLB.

### The kernel fix: abstract socket addresses

Kernel `bsdk8s/linux-abstract-unix-15.1`, patch freebsd-src/0017; both nodes boot `kernel.bsdk8s12`. It is done
in the Unix-domain socket layer, not as a Linuxulator shim, so native FreeBSD programs get the same behavior:

- **Names.** A `sun_path` starting with NUL is looked up in a table in `uipc_usrreq.c`, not in the file system. The
  name is all `sun_len - offsetof(sun_path)` bytes, NULs included. `bind` on a taken name gives EADDRINUSE; the name
  is released when the socket is closed; there are no permission checks. All of this is as on Linux.
- **Scope.** Names are per **VNET**, the counterpart of Linux's per-network-namespace scope. Containers of one pod
  share the pod's VNET, so they see each other's names; other pods and the host don't.
- **Locking.** The table's mutex plays the role the vnode mutex plays for file-system sockets in
  `unp_connectat()`, so a socket found in the table can't be freed before it is locked.
- **Linuxulator.** Address translation keeps embedded NULs and strips only the padding of names passed with
  Linux's longer `sizeof(struct sockaddr_un)`. Linux **autobind** (`bind` with only the family) picks a unique
  five-hex-digit name.

Verified with a static Linux Go test (`abstest`), on the host and in pods:

| Check | Before (bsdk8s11) | After (bsdk8s12) |
|---|---|---|
| `net.Listen("unix", "@name")`, dial, data, `LocalAddr`/`RemoteAddr` | bind: ENOENT | pass |
| second bind of the same name | - | EADDRINUSE |
| dial after the listener closes; rebind | - | ECONNREFUSED; rebind works |
| `SOCK_DGRAM` sendto/recvfrom, sender address | bind: ENOENT | pass |
| names that differ after an embedded NUL; `getsockname` length | ENOENT | distinct; length 6 for `"\0a\0b"` |
| 110-byte padded bind, exact-length connect | ENOENT | pass |
| autobind (`addrlen` 2) | EINVAL | `"\0" + 5 hex`, length 8 |
| file-system Unix sockets | pass | pass |
| host vs. non-VNET jail vs. VNET jail (same name) | - | shared, shared, separate |
| container → container in one pod; pod → other pod; host → pod | - | pong; ECONNREFUSED; ECONNREFUSED |
| 8 dialers x 200 against a listener restarted every 50 ms | - | no panic; only ECONNREFUSED while unbound |

**Envoy with hot restart on:** with `extraArgs` removed, the new Envoy pod starts without the `base_id` error, holds
its four hot-restart datagram sockets, and the Gateway returns 200 over HTTP and HTTPS through both node IPs.
`sockstat` prints these sockets' local address as empty, because it prints `sun_path` as a C string.

**Not done:**
- **FreeBSD main.** main has reworked `unp_connectat()` into `unp_connectat_peer()`/`unp_vnode_peer()`, so the
  15.1 patch doesn't cherry-pick. A main version needs its own port and a main kernel to test it on.
- **Other Linux behavior.** Linux also autobinds on `connect`/`sendto` with `SO_PASSCRED`; that isn't done.
  `getsockname` on an unbound socket still returns length 16, where Linux returns 2.

## Linuxulator noise (non-fatal)

| Message | Meaning |
|---|---|
| `unsupported prctl option 1398164801` | `PR_SET_VMA` (names anonymous mappings): cosmetic |
| `unsupported fcntl cmd 36` | `F_OFD_GETLK` probe in etcd's fileutil (research/10) |
| ~~`unsupported TCP socket option TCP_INFO (11)`~~ | Linux `tcp_info` stats (getsockopt). **Fixed**, see below |
| ~~`unsupported setsockopt level 6 optname 18`~~ | `TCP_USER_TIMEOUT` (gRPC keepalive). **Fixed**, see below |

`TCP_USER_TIMEOUT` is the only one with a behaviour effect: gRPC's keepalive timeout can't abort a connection
whose peer has silently vanished. That takes longer to detect, but nothing fails.

**Fixed 2026-11-17 (kernel `bsdk8s11`, both nodes).** FreeBSD main already had both options; 15.1 did not.
- Backported from main: `TCP_USER_TIMEOUT` mapped to `TCP_MAXUNACKTIME` (milliseconds rounded up to seconds;
  enforced by the default `freebsd` TCP stack), the sockopt copyout fix, and `TCP_INFO` translated to the Linux
  `struct tcp_info` (freebsd-src/0013-0015, branch `bsdk8s/linux-tcp-sockopts-15.1`).
- Found while testing the backport, and present in main too: `TCP_INFO` copied out the Linux struct (280 bytes)
  with the native struct's length (256), and every Linux getsockopt reported the full option length instead of
  the bytes copied. Fixed in freebsd-src/0016 (main-based branch `bsdk8s/linux-sockopt-copyout-len`).

Measured with a static Linux Go client in a pod writing to a server pod whose `eth0` is then taken down
(`ifconfig -j <jid> eth0 down`):

| | Old kernel | `bsdk8s11` |
|---|---|---|
| `setsockopt(TCP_USER_TIMEOUT, 3000)` | `ENOPROTOOPT` | ok, reads back 3000 |
| Connection after the peer vanishes, timeout 3 s | writes block (no abort within 40 s) | aborted ~3.5 s after the cut |
| `TCP_INFO`, 104/232/512-byte buffer | `EINVAL` | length 104/232/280, nothing written past it |

The abort surfaces as `EPIPE` on the next write, where Linux returns `ETIMEDOUT`: FreeBSD's send path checks
"can't send more" before the pending socket error. Callers (gRPC included) treat both as a dead connection.

## HTTPS listener with a cert-manager certificate (2026-11-17)

**Result [verified]:** an HTTPS listener terminating TLS with a cert-manager certificate works, and in-place
renewal is picked up live. Nothing FreeBSD-specific was needed.

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: demo-gw, namespace: default}
spec:
  secretName: demo-gw-tls
  dnsNames: ["demo.bsdk8s.test"]
  issuerRef: {kind: ClusterIssuer, name: demo-ca}   # CA issuer from research/11
---
# added to Gateway "demo" next to the http listener
  - name: https
    port: 443
    protocol: HTTPS
    hostname: demo.bsdk8s.test
    tls:
      mode: Terminate
      certificateRefs: [{kind: Secret, name: demo-gw-tls}]
    allowedRoutes: {namespaces: {from: Same}}
```

- The existing HTTPRoute attached to both listeners (no `sectionName`). `curl --cacert` with the CA from the
  secret: 200 through **both** node IPs, TLS 1.3 (`TLS_AES_256_GCM_SHA384`), verify OK. A hostname with no
  listener is refused.
- **Renewal:** changing the Certificate's `dnsNames` makes cert-manager rewrite the secret in place. The new
  serial was in the secret after ~1 s, and Envoy served it ~1 s later through both node IPs. **90/90 requests
  made during the rotation returned 200.**
- **Gotcha (upstream behaviour, not FreeBSD):** *deleting* the secret makes Envoy Gateway mark the listener
  invalid and drop port 443 from the proxy Service. When cert-manager recreates the secret, the port comes back
  with a **new random NodePort** (30362 became 32452 here). Normal renewals don't delete the secret, but anything
  pointing at a NodePort should pin it, or use a LoadBalancer.

## Not tested yet

GRPCRoute/TLSRoute, rate limiting (the `ratelimit` image),
and other implementations (NGINX Gateway Fabric, Traefik, which also ships FreeBSD binaries, so a native-jail
data plane is possible).
