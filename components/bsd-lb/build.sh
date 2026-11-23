#!/bin/sh
# Build the bsd-lb image on a FreeBSD host: one static binary on the static base image
# (scripts/build-images-freebsd.sh). The same image runs the controller and the speaker.
#   [IMAGE=localhost/bsd-lb:dev] [BIN=<prebuilt freebsd binary>] ./build.sh
set -eu
IMAGE=${IMAGE:-localhost/bsd-lb:dev}
cd "$(dirname "$0")"
if [ -z "${BIN:-}" ]; then
  CGO_ENABLED=0 go build -trimpath -ldflags=-s -o bsd-lb .
  BIN=bsd-lb
fi
rel=$(freebsd-version -u | sed 's/-p[0-9]*$//')
base=localhost/freebsd-static:$rel
buildah inspect --type image "$base" >/dev/null 2>&1 || {
  echo "$base missing: run scripts/build-images-freebsd.sh first"; exit 1; }
c=$(buildah from -q "$base")
buildah copy -q "$c" "$BIN" /usr/local/bin/bsd-lb
buildah config --os freebsd --arch "$(uname -m)" --entrypoint '["/usr/local/bin/bsd-lb"]' \
  --label org.opencontainers.image.source=https://github.com/PlatformNerds/BSDKubernetes "$c"
buildah commit -q --rm "$c" "$IMAGE" >/dev/null
echo "built $IMAGE"
