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

## Found on the way

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
