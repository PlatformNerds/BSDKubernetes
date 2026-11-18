#!/bin/sh
# Build the bsd-netpol image on a FreeBSD host: the static agent plus pfctl, its libraries and
# the runtime linker (the static base image has none), like the kube-proxy image
# (scripts/build-images-freebsd.sh).
#   [IMAGE=localhost/bsd-netpol:dev] [BIN=<prebuilt freebsd binary>] ./build.sh
set -eu
IMAGE=${IMAGE:-localhost/bsd-netpol:dev}
cd "$(dirname "$0")"
if [ -z "${BIN:-}" ]; then
  CGO_ENABLED=0 go build -trimpath -ldflags=-s -o bsd-netpol .
  BIN=bsd-netpol
fi
rel=$(freebsd-version -u | sed 's/-p[0-9]*$//')
base=localhost/freebsd-static:$rel
buildah inspect --type image "$base" >/dev/null 2>&1 || {
  echo "$base missing: run scripts/build-images-freebsd.sh first"; exit 1; }
c=$(buildah from -q "$base")
buildah copy -q "$c" "$BIN" /usr/local/bin/bsd-netpol
buildah copy -q "$c" /sbin/pfctl /sbin/
for l in $(ldd -f '%p\n' /sbin/pfctl) /libexec/ld-elf.so.1; do
  buildah copy -q "$c" "$l" "$(dirname "$l")/"
done
# pfctl resolves protocol and service names while parsing rules.
for f in /etc/protocols /etc/services /etc/netconfig; do buildah copy -q "$c" "$f" /etc/; done
buildah config --os freebsd --arch "$(uname -m)" --entrypoint '["/usr/local/bin/bsd-netpol"]' \
  --label org.opencontainers.image.source=https://github.com/PlatformNerds/BSDKubernetes "$c"
buildah commit -q --rm "$c" "$IMAGE" >/dev/null
echo "built $IMAGE"
