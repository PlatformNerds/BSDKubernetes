#!/bin/sh
# Publish the FreeBSD e2e test images (scripts/build-e2e-images-freebsd.sh) to a registry that
# CRI-O uses as a mirror of registry.k8s.io/e2e-test-images, as image indexes holding both
#   freebsd/<arch>  our build, from local containers storage, and
#   linux/<arch>    upstream's image for this architecture, referenced by digest,
# the way upstream publishes Windows variants next to the Linux ones.
#
# Local tags are enough for pods that use IfNotPresent. Tests that pull for real (pullPolicy
# Always, e.g. image volumes, or the image-pull tests) go to the registry, where these images
# have no FreeBSD variant. With the mirror, FreeBSD pods get the FreeBSD build and pods under
# the "linux" runtime handler still get upstream's Linux image.
#
# Run as root on a FreeBSD host with podman, skopeo and jq:
#   sudo scripts/e2e-image-mirror.sh <mirror>          # e.g. 192.168.100.1:5000 (plain HTTP)
# CRI-O side: /usr/local/etc/containers/registries.conf.d/20-e2e-mirror.conf
#   [[registry]]
#   prefix = "registry.k8s.io/e2e-test-images"
#   location = "registry.k8s.io/e2e-test-images"
#   [[registry.mirror]]
#   location = "<mirror>/e2e-test-images"
#   insecure = true
set -eu
mirror=$1
up=registry.k8s.io
arch=$(uname -m)

for img in $(podman images --format '{{.Repository}}:{{.Tag}}' | grep "^$up/e2e-test-images/" | grep -v -- '-prev' | sort -u); do
  path=${img#$up/}                       # e2e-test-images/<name>:<tag>
  list=localhost/e2e-mirror/${path#e2e-test-images/}
  d=
  podman manifest rm "$list" >/dev/null 2>&1 || true
  podman manifest create "$list" >/dev/null
  podman manifest add "$list" "containers-storage:$img" >/dev/null
  # Upstream's Linux image for this architecture, if upstream publishes the tag.
  if raw=$(skopeo inspect --raw "docker://$img" 2>/dev/null); then
    d=$(echo "$raw" | jq -r --arg a "$arch" \
      '.manifests[]? | select(.platform.os=="linux" and .platform.architecture==$a) | .digest' | head -1)
    if [ -n "$d" ]; then
      podman manifest add "$list" "docker://$up/${path%%:*}@$d" >/dev/null
    else
      echo "note: $img has no linux/$arch image upstream"
    fi
  else
    echo "note: $img not found upstream; mirroring the FreeBSD image only"
  fi
  podman manifest push -q --all --tls-verify=false "$list" "docker://$mirror/$path" >/dev/null
  podman manifest rm "$list" >/dev/null
  echo "mirrored $path: freebsd/$arch${d:+ + linux/$arch}"
done
