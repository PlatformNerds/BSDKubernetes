# bsd-placement

Mutating admission webhook that runs unmodified Linux manifests on FreeBSD nodes. Pods and DaemonSets pinned to
`kubernetes.io/os=linux` that have no `runtimeClassName` get these changes:
- They switch to RuntimeClass `linux`, which runs them under the Linux ABI in jails.
- They are re-pinned to nodes labeled `bsdk8s.io/linux-abi=true`.

Design, tests and limits: [research/11](../../research/11-bsd-placement.md).

```sh
# on a FreeBSD node (Go + buildah), on every node that may run it:
./build.sh                    # -> localhost/bsd-placement:dev (scratch image, static binary)
./deploy/install.sh           # self-signed CA + cert, Secret, Deployment, MutatingWebhookConfiguration
go test ./...
```

Namespace labels:
- `bsdk8s.io/default-runtime=linux` treats every pod as Linux.
- `bsdk8s.io/default-runtime=auto` decides by image: a pod with a Linux-only image (per its registry) runs as
  Linux, and a pod whose images all publish `freebsd` runs natively. A lookup failure leaves the pod unchanged
  and returns a warning.
- `bsdk8s.io/placement=ignore` skips the namespace.

Live registry check: `BSD_PLACEMENT_LIVE=1 go test -run TestLiveRegistries -v`.

Flags: `--image-cache-ttl` (10m), `--reconcile-interval` (30s), `--reconcile-min-age` (1m), `--runtime-class` (default `linux`), `--node-selector` (`bsdk8s.io/linux-abi=true`), `--node-os` (`freebsd`).
