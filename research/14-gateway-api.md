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
| `unsupported TCP socket option TCP_INFO (11)` | Linux `tcp_info` stats (getsockopt) |
| `unsupported setsockopt level 6 optname 18` | `TCP_USER_TIMEOUT` (gRPC keepalive); FreeBSD has no direct equivalent |

`TCP_USER_TIMEOUT` is the only one with a behaviour effect: gRPC's keepalive timeout can't abort a connection
whose peer has silently vanished. That takes longer to detect, but nothing fails.

## Not tested yet

TLS listeners (cert-manager-issued certificates), GRPCRoute/TLSRoute, rate limiting (the `ratelimit` image),
and other implementations (NGINX Gateway Fabric, Traefik, which also ships FreeBSD binaries, so a native-jail
data plane is possible).
