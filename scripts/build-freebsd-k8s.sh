#!/bin/sh
# Build FreeBSD Kubernetes binaries from upstream v1.36.1 + the patch series in patches/.
# Runs on any host with Go (cross-compiles, CGO_ENABLED=0). Output: ./_output/freebsd-<arch>/
# Usage: scripts/build-freebsd-k8s.sh [k8s-src-dir] [arch...]
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
src=${1:-"$here/../upstream/k8s-freebsd"}; shift 2>/dev/null || true
arches=${*:-"amd64 arm64"}
P="$here/patches/kubernetes/v1.36.1"
if [ ! -d "$src/.git" ]; then
  git clone -q --depth 1 --branch v1.36.1 https://github.com/kubernetes/kubernetes.git "$src"
  git -C "$src" -c user.name=bsdk8s -c user.email=bsdk8s@localhost am -q "$P"/kubelet/*.patch
  # proxy/ and kubeadm/ series both start with the shared possibleCPUs stub (0001); skip it.
  for d in proxy kubeadm; do
    for p in "$P/$d"/*.patch; do
      case "$p" in */0001-Add-stub-for-possibleCPUs*) continue;; esac
      git -C "$src" -c user.name=bsdk8s -c user.email=bsdk8s@localhost am -q -3 "$p"
    done
  done
  git -C "$src" -c user.name=bsdk8s -c user.email=bsdk8s@localhost am -q -3 "$P"/bsdk8s/*.patch
fi
cd "$src"
for arch in $arches; do
  out="$here/_output/freebsd-$arch"; mkdir -p "$out"
  for c in kubelet kube-proxy kubeadm kube-apiserver kube-controller-manager kube-scheduler kubectl; do
    CGO_ENABLED=0 GOOS=freebsd GOARCH=$arch GOTOOLCHAIN=auto go build -trimpath -o "$out/$c" ./cmd/$c && echo "ok   $arch $c" || echo "FAIL $arch $c"
  done
done
