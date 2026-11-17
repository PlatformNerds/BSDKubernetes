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

## The one blocker: abstract Unix sockets

Envoy crash-looped with:

    unable to bind domain socket with base_id=0, id=0, errno=2 (see --base-id option)

Envoy's hot restart uses Linux **abstract-namespace** Unix sockets: `sun_path` starts with NUL and has no
filesystem entry. FreeBSD has no abstract namespace, so the Linuxulator treats the name as a path and gets ENOENT.
Hot restart is useless in Kubernetes, where pods are replaced rather than hot-restarted, and it can be disabled
through Envoy Gateway's own API without touching the manifest:

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata: {name: bsd, namespace: envoy-gateway-system}
spec:
  extraArgs: ["--disable-hot-restart"]
  provider: {type: Kubernetes, kubernetes: {envoyService: {type: NodePort}}}   # no LoadBalancer on bare metal
```

The GatewayClass's `parametersRef` points at it. `NodePort` is only there because there is no LoadBalancer
implementation. With the default `LoadBalancer` type, traffic worked the same and the Gateway reported "No
addresses have been assigned", as on bare-metal Linux without MetalLB.

**Should the kernel support abstract sockets?** Other users: D-Bus, systemd notify, X11, Chromium, gRPC's
`unix-abstract:` scheme. It would need a per-vnet name table in the Unix-domain socket layer (`uipc_usrreq.c`)
for bind/connect/getsockname, plus Linuxulator sockaddr translation. Linux scopes the names to the network
namespace, which here is the pod's vnet. That's a real kernel feature, not a shim. It's deferred until a workload
needs it without a switch.

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

## Not tested yet

TLS listeners (cert-manager-issued certificates), GRPCRoute/TLSRoute, rate limiting (the `ratelimit` image),
and other implementations (NGINX Gateway Fabric, Traefik, which also ships FreeBSD binaries, so a native-jail
data plane is possible).
