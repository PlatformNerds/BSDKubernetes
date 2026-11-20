# nfs-provisioner on FreeBSD

`kubernetes-sigs/nfs-ganesha-server-and-external-provisioner`, master `b8617ca` (the source of
`registry.k8s.io/sig-storage/nfs-provisioner`, which the in-tree NFS e2e driver starts in every test).

- `0001` updates `golang.org/x/sys` and `x/net` (the 2019 versions lack freebsd/arm64). The patch
  carries `go.mod`/`go.sum` only; run `go mod vendor` after applying it.
- `0002` runs the provisioner's own NFS server (`run-server`, the default) as unfs3 on FreeBSD: NFS
  Ganesha has no FreeBSD port, and the kernel nfsd needs a privileged pod, which the provisioner's
  pod isn't. NFSv3 only.

Build: `GOOS=freebsd CGO_ENABLED=0 go build ./cmd/nfs-provisioner`, then
`NFS_PROVISIONER=<binary> ONLY=nfs-provisioner scripts/build-e2e-images-freebsd.sh ...`.
