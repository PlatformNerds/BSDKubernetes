#!/bin/sh
# Build the bsd-placement image on a FreeBSD host (static binary, scratch image).
set -eu
IMAGE=${IMAGE:-localhost/bsd-placement:dev}
cd "$(dirname "$0")"
CGO_ENABLED=0 go build -trimpath -ldflags=-s -o bsd-placement .
c=$(buildah from scratch)
buildah copy "$c" bsd-placement /bsd-placement
buildah config --os freebsd --arch "$(go env GOARCH)" --entrypoint '["/bsd-placement"]' --user 65534 "$c"
buildah commit --rm "$c" "$IMAGE"
