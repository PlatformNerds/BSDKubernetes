# 11: bsd-placement — unmodified Linux manifests on FreeBSD nodes (2026-11-16)

**Result [verified]:** cert-manager v1.16.2 installs from its unmodified release manifest. All three of its
Linux images run under the Linuxulator on FreeBSD nodes. It issues a self-signed CA and then a CA-signed
certificate. cainjector patches webhook CA bundles. Its validating admission webhook, a Linux binary in a jail,
rejects invalid objects for the API server. Before this, an unmodified manifest stayed `Pending` forever.

## The problem

Upstream manifests and charts pin pods to Linux nodes with either form:

```yaml
nodeSelector: {kubernetes.io/os: linux}
# or
affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms:
  [{matchExpressions: [{key: kubernetes.io/os, operator: In, values: [linux]}]}]}}}
```

FreeBSD nodes are labeled `kubernetes.io/os=freebsd`, and that label must not lie: dfr's kubelet, kube-proxy and
CoreDNS images select on it. research/10 had to hand-edit metrics-server's manifest to run it.

## What bsd-placement does

`components/bsd-placement` is a mutating admission webhook. It is stdlib-only Go, a ~6 MB static FreeBSD binary
in a scratch image, and has unit tests. It handles pods (CREATE) and DaemonSets (CREATE/UPDATE). For a pod spec
with **no `runtimeClassName`** that pins Linux, it does the following:

1. Removes `kubernetes.io/os=linux` and `beta.kubernetes.io/os=linux` from `nodeSelector`, and rewrites required
   affinity `kubernetes.io/os In [linux]` to `In [freebsd]`. Other values, other keys and preferred terms are left
   alone.
2. Sets `runtimeClassName: linux`. CRI-O then pulls the linux/arm64 image and runs it under the Linux ABI
   (research/10).
3. Adds the RuntimeClass's own scheduling selector, `bsdk8s.io/linux-abi=true`. It has to do this itself because
   the in-tree RuntimeClass admission plugin runs before webhooks and is not re-run after them.
4. Records the change in the `bsdk8s.io/placement` annotation, e.g. `runtimeClassName=linux (nodeSelector
   kubernetes.io/os=linux)`, plus an audit annotation.

Why DaemonSets: the DaemonSet controller decides which nodes get a pod from the *template's* selector and affinity.
If only pods were rewritten, a Linux-pinned DaemonSet would never create a pod on a FreeBSD node. Deployments,
StatefulSets and Jobs are *not* rewritten. Their pods are, so GitOps tools see no drift on those objects.

A namespace labeled `bsdk8s.io/default-runtime=linux` gets every pod treated as Linux, pinned or not, through a
second webhook entry (`/mutate-force`). That covers manifests with no OS pin at all, e.g. `kubectl create deploy
--image=nginx`. A namespace can opt out with `bsdk8s.io/placement=ignore`.

`failurePolicy: Ignore`: if the webhook is down, pods are admitted unchanged. Linux-pinned pods then stay
`Pending`, which is today's behaviour, instead of the cluster blocking every pod create.

## Verification

| Test | Result |
|---|---|
| Unit tests: nodeSelector, affinity, untouched cases (no pin, freebsd pin, runtimeClass set, preferred-only, NotIn, already allows freebsd, Deployment kind), force, DaemonSet + idempotency, handler + malformed input | pass |
| Deployment `nginx:1.27-alpine` with `nodeSelector kubernetes.io/os: linux`, 2 replicas | Running on both nodes, RuntimeClass `linux` |
| DaemonSet `busybox:1.36` with required affinity `kubernetes.io/os In [linux]` | template rewritten to `In [freebsd]`; 1 pod per node; `uname -sr` = `Linux 5.15.0` |
| cert-manager v1.16.2 release manifest (sha256 `1d51cdec…`), unmodified | 3/3 pods Running, 0 restarts; Certificates Ready; webhook denies invalid Certificate |

## Image-aware placement (`bsdk8s.io/default-runtime=auto`)

For namespaces labeled `auto`, a third webhook entry (`/mutate-auto`, 10 s timeout) decides pods *without* an OS pin
by what their images are published for. Each container and init-container image is looked up in its registry:
anonymous Bearer token flow, OCI index / Docker manifest list, and for single-arch images the config blob's `os`.

| Images | Result |
|---|---|
| every image has a `freebsd` variant | unchanged: runs natively in a jail |
| some image is Linux-only, the rest FreeBSD or Linux | RuntimeClass `linux` (reason in the annotation: `linux-only image: …`) |
| an image with neither (e.g. Windows-only), a lookup error, or the 3 s budget exceeded | unchanged, plus an admission **warning** shown by kubectl |

Results are cached per image reference: 10 min (`--image-cache-ttl`), and failures for 1/10 of that. Pinned pods
are still rewritten by the pin rule, before any lookup.

Live lookups from the Mac, uncached, in 150–900 ms:

| Image | Published OSes |
|---|---|
| `nginx:1.27-alpine`, `busybox:1.36` | linux |
| `quay.io/jetstack/cert-manager-controller:v1.16.2` | linux |
| `registry.k8s.io/metrics-server/metrics-server:v0.7.2` | linux |
| `registry.k8s.io/e2e-test-images/agnhost:2.63.0` | linux, windows |
| `ghcr.io/freebsd/freebsd-runtime:15.0` | freebsd |

That is the ecosystem reality: outside the official FreeBSD base images, almost nothing publishes `freebsd`.

**Why opt-in and not cluster-wide.** The nodes carry locally built FreeBSD images under upstream names, e.g. the
e2e images (`agnhost`), which the registry lists as linux/windows only. A cluster-wide lookup would send every e2e pod
to the Linux ABI. Locally built images aren't visible to a registry lookup. A node-side check (CRI `ImageStatus`)
would be needed to honour them. Private images need pull credentials, which aren't implemented, so they fall back
to "unchanged".

## Availability: the drain incident and the reconciler

Rebooting node 2 for a kernel update meant draining it. The drain evicted the (then single) bsd-placement replica
**and** cert-manager's pods at the same moment. The ReplicaSets recreated cert-manager's pods while the webhook was
down. `failurePolicy: Ignore` admitted them unchanged, and they stayed `Pending` for good: `runtimeClassName`
can't be changed on an existing pod. The fix has two parts:

- **Stay up:** 2 replicas, `topologySpreadConstraints` (`DoNotSchedule`, `matchLabelKeys: [pod-template-hash]`)
  for one replica per node, and a PodDisruptionBudget `minAvailable: 1`. A single-node drain can no longer take
  the webhook down. Graceful shutdown on SIGTERM gives exit 0 instead of `Error`.
- **Self-heal:** the webhook process also runs a reconciler, every 30 s by default. It lists unscheduled
  `Pending` pods older than 1 minute that it would have rewritten. It deletes the controller-owned ones, with
  a UID precondition, so their controller recreates them through the webhook. Bare pods are only logged. The
  RBAC is `pods: list, delete` and `namespaces: get`. Namespaces labeled `bsdk8s.io/placement=ignore` and its own
  namespace are skipped. This is unit-tested, and it was verified live: it deleted the 3 stuck cert-manager pods,
  and their replacements were Running 10 s later.

The force webhook (namespace label) is not covered by the reconciler. It doesn't look up namespace labels for that.

## Independent review (2026-11-16)

A Go reviewer subagent, given the code but not my conclusions, ran `go vet` and `go test -race` (both clean). It found
no critical defects. All six medium findings are fixed, with tests:

| Finding | Fix |
|---|---|
| `name:tag@sha256:…` parsed the tag into the repo | Strip the tag; pull by digest |
| **SSRF**: in `auto` namespaces the pod author picks the registry host, auth realm and redirects; errors were echoed back in admission warnings, a port-scan oracle | The dialer refuses loopback, private, link-local, CGNAT and other non-public addresses (checked on the resolved IP, so DNS tricks don't help). The auth realm must use the registry's scheme. Redirects must be https, at most 5 |
| Unbounded cache, no coalescing (Docker Hub anonymous rate limit) | Concurrent lookups of one image share one request (20 concurrent → 2 HTTP requests). At most 8 lookups run at once. Cache capped at 4096 entries |
| Reconciler read up to 64 MiB of pending pods inside a 64 Mi pod | Paged list (`limit=200`); memory limit raised to 128 Mi |
| Any `controller: true` owner was assumed to recreate the pod | Deletes only ReplicaSet, StatefulSet, DaemonSet and Job pods; others (e.g. Argo Workflows) are reported |
| SA token read once; kubelet rotates it (~1 h expiry) | Re-read the token file at most once a minute |

Also fixed:
- Delete precondition now also includes `resourceVersion`, so a pod that got scheduled after the list isn't deleted.
- The reported-pods set is pruned.
- An auth realm that already carries a query string works.

Still open (low):
- **Architecture is ignored.** A `freebsd/arm64`-only image counts as native on an amd64 node.
- **The reconciler only repairs pinned pods**, not `force`/`auto` ones.
- **RuntimeClass `overhead`/`tolerations` aren't copied.** Ours defines neither.
- **The serving cert isn't hot-reloaded.**

## Found on the way

- **Pods had no egress at all.** The demo CNI config has `"ipMasq": false`, so the `<cni-nat>` table pf NATs from
  stays empty. dfr's README notes NAT is off for multi-node setups. Nothing in the cluster needed the outside world
  until bsd-placement's registry lookups did. Then CoreDNS forwards to QEMU's resolver timed out, and pods saw
  `bad address` after 5 s. Fix: a pf rule `nat on <egress> from 10.244.0.0/16 to ! 10.244.0.0/16`. It NATs pod
  egress to anything outside the pod CIDR, so pod-to-pod traffic keeps its IPs even on single-NIC nodes. pf reloads
  don't lose it, unlike `<cni-nat>` entries.
- **IPv6 resolver unreachable from pods.** The node's `resolv.conf` also lists QEMU's `fec0::3`, which CoreDNS
  picked for a share of its forwards (`no route to host`). The kubelet's `resolvConf` file is now IPv4-only.
- **No CA roots in the scratch image**: `x509: certificate signed by unknown authority`. `build.sh` now copies
  `/etc/ssl/cert.pem`.
- **Live result in an `auto` namespace:**

  | Pod | Result |
  |---|---|
  | `nginx:1.27-alpine`, `quay.io/jetstack/cert-manager-cainjector:v1.16.2` | RuntimeClass `linux`; `uname` = Linux |
  | `ghcr.io/freebsd/freebsd-runtime:15.0` | left native; `uname` = FreeBSD |
  | `10.96.0.1:443/x/y:z` | refused by the SSRF guard, with a warning |

  Admission took 0–1 s.

- **Short image names.** CRI-O had no `unqualified-search-registries`, so `image: nginx:1.27-alpine` failed with
  `ImageInspectError` ("short-name did not resolve"). containerd and Docker default to docker.io, and manifests rely
  on it. Fixed with `registries.conf.d/10-docker-io.conf` (`unqualified-search-registries = ["docker.io"]`,
  `short-name-mode = "permissive"`). Added to the demo `build-cluster.sh`.
- **Linuxulator:** `unsupported fcntl cmd 36`, i.e. Linux `F_OFD_GETLK` (open-file-description locks), logged by
  cert-manager's controller and metrics-server. It is non-fatal for both.
- The webhook `clientConfig.service.path` may not carry a query string, so it uses a separate path.

## Honest limits

- **Selector-driven, not image-driven.** PLAN §5.6 wanted placement decided by the image's platforms: FreeBSD image
  → jail, Linux-only → `linux`, otherwise `kata-bhyve`. This version trusts the manifest's OS pin and does no
  registry lookups. A Linux-pinned pod whose Linuxulator support is weak, say one needing `unshare`, io_uring or
  eBPF, is still sent to the Linux ABI and fails at runtime. Adding a registry lookup is the next step. It would
  need image-pull credentials and caching, and a per-image allow/deny list.
- **Mixed clusters.** With real Linux nodes present, a Linux-pinned pod is still moved to FreeBSD nodes. Opt a
  namespace out with `bsdk8s.io/placement=ignore`; a node-OS preference policy is not implemented.
- **Mutated DaemonSets** differ from their source manifest, so Argo CD/Flux will report drift unless told to ignore
  `spec.template.spec.{runtimeClassName,nodeSelector,affinity}`.
- **Serving certs** come from a self-signed CA made by `deploy/install.sh` (10-year), with no rotation. Using
  cert-manager for them is possible now that it runs.
- **Windows-style `spec.os.name: linux`** is not rewritten. Pods setting it still reach FreeBSD nodes, and the
  kubelet admits it, as in research/10.
