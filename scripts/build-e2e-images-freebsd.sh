#!/bin/sh
# Build FreeBSD equivalents of the Kubernetes e2e/conformance test images and tag them with the
# *upstream* names (registry.k8s.io/e2e-test-images/...), so the unmodified e2e.test binary uses
# them on FreeBSD nodes. CRI-O refuses the Linux images ("no match for platform freebsd/<arch>"),
# so without these every conformance test that starts a pod fails at image pull. Same approach
# Windows nodes use (upstream publishes Windows variants of these images).
#
# Run as root on a FreeBSD host (buildah + pkg). Images land in the local containers storage
# shared with CRI-O; e2e pods use IfNotPresent so nothing is pulled.
#
#   sudo scripts/build-e2e-images-freebsd.sh <k8s-src> <agnhost-binary> [<nnp-binary>]
#     <k8s-src>         kubernetes source tree (for test/images/* assets)
#     <agnhost-binary>  FreeBSD build of test/images/agnhost for this host's arch
#     <nnp-binary>      FreeBSD build of test/images/nonewprivs (optional)
set -eu
src=$(cd "$1" && pwd); agnhost=$2; nnp=${3:-}
img=$src/test/images
reg=registry.k8s.io/e2e-test-images
rel=$(freebsd-version -u | sed 's/-p[0-9]*$//')
arch=$(uname -m); march=$(uname -p)
base=localhost/freebsd-runtime:$rel
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT

# Image tags come from test/utils/image/manifest.go (what e2e.test actually pulls), NOT from
# test/images/*/VERSION (those are the versions the next image build would publish).
manifest=$src/test/utils/image/manifest.go
mtag() { sed -n "s/.*configs\[$1\] = Config{[^,]*, \"[^\"]*\", \"\([^\"]*\)\"}.*/\1/p" "$manifest"; }
for n in Agnhost AgnhostPrev Kitten Nautilus BusyBox Nginx NginxNew Pause GlibcDnsTesting NonRoot Nonewprivs; do
  [ -n "$(mtag $n)" ] || { echo "no tag for $n in $manifest"; exit 1; }
done

# Official FreeBSD "runtime" base image (sh + base utilities), sha256-checked.
if ! buildah inspect --type image "$base" >/dev/null 2>&1; then
  url=https://download.freebsd.org/releases/OCI-IMAGES/$rel/$march/Latest
  f=FreeBSD-$rel-$arch-$march-container-image-runtime.txz
  fetch -q -o "$work/$f" "$url/$f"; fetch -q -o "$work/CHECKSUM.SHA256" "$url/CHECKSUM.SHA256"
  [ "$(sha256 -q "$work/$f")" = "$(grep "($f)" "$work/CHECKSUM.SHA256" | awk '{print $NF}')" ] || { echo "checksum mismatch"; exit 1; }
  id=$(podman load -q -i "$work/$f" | awk '{print $NF}')
  podman tag "$id" "$base"
fi

# new <from>: start a working container, mount it; sets $c and $m
new() { c=$(buildah from -q "$1"); m=$(buildah mount "$c"); }
# pkgs <pkg...>: install FreeBSD packages into the mounted image root
# Afterwards rebuild the image's ld-elf.so.hints: a booted system runs ldconfig at startup, an
# image never does, so packages without an RPATH (e.g. nginx -> libpcre2) wouldn't find their libs.
pkgs() {
  ASSUME_ALWAYS_YES=yes pkg -r "$m" install -q -y "$@" >/dev/null
  pkg -r "$m" clean -a -y -q >/dev/null 2>&1 || true
  mkdir -p "$m/var/run"
  chroot "$m" /sbin/ldconfig -elf /lib /usr/lib /usr/local/lib
}
# bbfill: link busybox applets for commands the FreeBSD runtime base lacks (head, which, wget,
# nslookup, ...). Upstream images are Alpine, where busybox provides them; tests' shell snippets
# rely on them. Existing FreeBSD binaries are kept.
# (-L too: links into the image are absolute, so -e would resolve them on the build host)
has() { [ -e "$1" ] || [ -L "$1" ]; }
bbfill() {
  for a in $(chroot "$m" /usr/local/bin/busybox --list); do
    has "$m/bin/$a" || has "$m/usr/bin/$a" || has "$m/sbin/$a" || has "$m/usr/sbin/$a" || \
      has "$m/usr/local/bin/$a" || ln -s /usr/local/bin/busybox "$m/bin/$a"
  done
}
# hostbin <path...>: copy FreeBSD base-system binaries and their shared libraries from the build
# host into the image (the host runs the same release as the base image).
hostbin() {
  for b in "$@"; do
    install -m 755 "$b" "$m$b"
    for l in $(ldd -f '%p\n' "$b"); do [ -e "$m$l" ] || install -m 444 "$l" "$m$l"; done
  done
}
# commit <name:tag> [extra tags...]: label, commit, tag
commit() {
  buildah config --os freebsd --arch "$arch" \
    --label org.opencontainers.image.source=https://github.com/PlatformNerds/BSDKubernetes \
    --label io.bsdk8s.e2e-equivalent-of="$1" "$c"
  buildah umount "$c" >/dev/null
  buildah commit -q --rm "$c" "$1" >/dev/null
  first=$1; shift
  for t in "$@"; do buildah tag "$first" "$t"; done
}

# --- agnhost: test/images/agnhost/Dockerfile (alpine + network tools + /agnhost) -------------
new "$base"
pkgs curl bash bind-tools iperf iperf3 busybox
hostbin /usr/bin/nc /usr/bin/getent   # e2e runs `nc -v -t -w 2` (OpenBSD nc, like netcat-openbsd)
bbfill
install -m 755 "$agnhost" "$m/agnhost"
ln -sf agnhost "$m/agnhost-2"
mkdir -p "$m/uploads"
install -m 644 "$img/agnhost/porter/localhost.crt" "$img/agnhost/porter/localhost.key" "$m/"
pw -R "$m" groupadd -n group-defined-in-image -g 50000
pw -R "$m" useradd -n user-defined-in-image -u 1000 -G group-defined-in-image -s /bin/sh
buildah config --entrypoint '["/agnhost"]' --cmd pause --port 80 --port 8080 --port 8081 --port 9376 --port 5000 "$c"
commit "$reg/agnhost:$(mtag Agnhost)" "$reg/agnhost:$(mtag AgnhostPrev)"
echo "built agnhost"

# --- kitten / nautilus: agnhost + html + test-webserver --------------------------------------
for k in Kitten Nautilus; do
  d=$(echo $k | tr A-Z a-z)
  new "$reg/agnhost:$(mtag Agnhost)"
  cp "$img/$d/html/"* "$m/"
  buildah config --cmd test-webserver "$c"
  commit "$reg/$d:$(mtag $k)"
  echo "built $d"
done

# --- busybox: FreeBSD runtime base + busybox applets (wget, nslookup, ... missing from base) --
new "$base"
pkgs busybox
# Upstream's busybox image *is* busybox: make its applets win over FreeBSD's (tests compare e.g.
# busybox sh error text "can't create"), then fill the rest.
for a in $(chroot "$m" /usr/local/bin/busybox --list); do
  for d in bin usr/bin sbin usr/sbin; do if has "$m/$d/$a"; then ln -sf /usr/local/bin/busybox "$m/$d/$a"; fi; done
done
bbfill
buildah config --cmd sh "$c"
commit "$reg/busybox:$(mtag BusyBox)"
echo "built busybox"

# --- nonroot: busybox + USER 1234 -----------------------------------------------------------
new "$reg/busybox:$(mtag BusyBox)"
buildah config --user 1234 "$c"
commit "$reg/nonroot:$(mtag NonRoot)"
echo "built nonroot"

# --- glibc-dns-testing (formerly jessie-dnsutils): dig/host/nslookup + getent + nc ----------
new "$base"
pkgs bind-tools busybox
hostbin /usr/bin/getent /usr/bin/nc
bbfill
commit "$reg/glibc-dns-testing:$(mtag GlibcDnsTesting)"
echo "built glibc-dns-testing"

# --- nonewprivs: setuid nnp helper on busybox --------------------------------------------------
if [ -n "$nnp" ]; then
  new "$reg/busybox:$(mtag BusyBox)"
  install -m 4755 "$nnp" "$m/usr/local/bin/nnp"
  buildah config --cmd nnp "$c"
  commit "$reg/nonewprivs:$(mtag Nonewprivs)"
  # buildah <= 1.43 on FreeBSD drops setuid bits when it stores the committed layer
  # (containers/storage pkg/archive passed uint32(os.FileMode) to fchmodat; fixed upstream in
  # container-libs 8f9b4818cc). The layer tar is correct, so re-load it through podman, whose
  # unpack keeps the bit, and verify: without setuid the AllowPrivilegeEscalation tests are void.
  t=$work/nonewprivs.tar
  podman save -q -o "$t" "$reg/nonewprivs:$(mtag Nonewprivs)"
  podman rmi -f "$reg/nonewprivs:$(mtag Nonewprivs)" >/dev/null
  podman load -q -i "$t" >/dev/null; rm -f "$t"
  mm=$(podman image mount "$reg/nonewprivs:$(mtag Nonewprivs)")
  [ -u "$mm/usr/local/bin/nnp" ] || { echo "nonewprivs: nnp lost its setuid bit"; exit 1; }
  podman image unmount "$reg/nonewprivs:$(mtag Nonewprivs)" >/dev/null
  echo "built nonewprivs"
fi

# --- nginx (both versions conformance uses) --------------------------------------------------
new "$base"
pkgs nginx
# the package's post-install script creates this symlink; `pkg -r` into an image root doesn't
[ -e "$m/usr/local/www/nginx" ] || ln -s nginx-dist "$m/usr/local/www/nginx"
buildah config --entrypoint '["/usr/local/sbin/nginx", "-g", "daemon off;"]' --cmd '' --port 80 "$c"
commit "$reg/nginx:$(mtag NginxNew)" "$reg/nginx:$(mtag Nginx)"
echo "built nginx"

# --- pause: reuse the control-plane pause image (scripts/build-images-freebsd.sh) -------------
if buildah inspect --type image ghcr.io/platformnerds/pause:3.10.2 >/dev/null 2>&1; then
  buildah tag ghcr.io/platformnerds/pause:3.10.2 registry.k8s.io/pause:$(mtag Pause)
  echo "tagged pause"
fi
