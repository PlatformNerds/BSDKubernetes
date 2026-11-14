#!/bin/sh
# Build the kubeadm control-plane images (kube-apiserver, kube-controller-manager, kube-scheduler,
# kube-proxy, etcd, coredns, pause) for the *host* FreeBSD architecture, on top of the official
# FreeBSD "static" OCI base image. Run as root on a FreeBSD host (uses buildah; images land in
# the local containers storage that CRI-O also uses, so kubeadm finds them without a registry).
#
#   sudo scripts/build-images-freebsd.sh <bindir> [registry]
#     <bindir>  : dir with kube-apiserver kube-controller-manager kube-scheduler kube-proxy etcd coredns
#                 (FreeBSD binaries for this arch; see scripts/build-freebsd-k8s.sh) and pause.c
#     registry  : image prefix (default ghcr.io/platformnerds)
set -eu
bindir=$(cd "$1" && pwd); reg=${2:-ghcr.io/platformnerds}
K8S=v1.36.1 ETCD=3.6.5-0 COREDNS=v1.14.2 PAUSE=3.10.2
rel=$(freebsd-version -u | sed 's/-p[0-9]*$//')            # e.g. 15.1-RELEASE
arch=$(uname -m); march=$(uname -p)                          # arm64/aarch64 or amd64/amd64
base=localhost/freebsd-static:$rel
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT

# 1. Official FreeBSD base image (verified against the release checksum file)
if ! buildah inspect --type image "$base" >/dev/null 2>&1; then
  url=https://download.freebsd.org/releases/OCI-IMAGES/$rel/$march/Latest
  f=FreeBSD-$rel-$arch-$march-container-image-static.txz
  fetch -q -o "$work/$f" "$url/$f"; fetch -q -o "$work/CHECKSUM.SHA256" "$url/CHECKSUM.SHA256"
  [ "$(sha256 -q "$work/$f")" = "$(grep "($f)" "$work/CHECKSUM.SHA256" | awk '{print $NF}')" ] || { echo "checksum mismatch"; exit 1; }
  id=$(podman load -q -i "$work/$f" | awk '{print $NF}')
  podman tag "$id" "$base"
fi

# 2. pause (upstream build/pause/linux/pause.c is portable C)
cc -O2 -static -DVERSION="$PAUSE" -o "$work/pause" "$bindir/pause.c"

img() { # name tag binary [entrypoint...]
  name=$1 tag=$2 bin=$3; shift 3
  c=$(buildah from -q "$base")
  dest=/usr/local/bin/
  [ "$name" = pause ] && dest=/   # CRI-O's pause_command defaults to /pause (upstream image layout)
  buildah copy -q "$c" "$bin" $dest
  if [ "$name" = kube-proxy ]; then   # pf mode shells out to pfctl: add it, its libs, the runtime
    buildah copy -q "$c" /sbin/pfctl /sbin/   # linker (the static base has none) and the /etc files
    for l in $(ldd -f '%p\n' /sbin/pfctl) /libexec/ld-elf.so.1; do   # pfctl needs to parse rules
      buildah copy -q "$c" "$l" "$(dirname "$l")/"
    done
    for f in /etc/protocols /etc/services /etc/netconfig; do buildah copy -q "$c" "$f" /etc/; done
  fi
  [ $# -gt 0 ] && buildah config --entrypoint "$(printf '%s\n' "$@" | jq -Rnc '[inputs]')" "$c"
  buildah config --os freebsd --arch "$arch" --label org.opencontainers.image.source=https://github.com/PlatformNerds/BSDKubernetes "$c"
  buildah commit -q --rm "$c" "$reg/$name:$tag" >/dev/null
  echo "built $reg/$name:$tag ($arch)"
}

for c in kube-apiserver kube-controller-manager kube-scheduler kube-proxy; do img $c $K8S "$bindir/$c"; done
img etcd    $ETCD    "$bindir/etcd"
img coredns $COREDNS "$bindir/coredns" /usr/local/bin/coredns
img pause   $PAUSE   "$work/pause"     /pause
