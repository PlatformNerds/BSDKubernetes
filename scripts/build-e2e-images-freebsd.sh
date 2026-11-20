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
#   Environment:
#     BUSYBOX=<binary>  BusyBox for the busybox and nonroot images, built by
#                       scripts/busybox-freebsd/build.sh at the version the busybox tag names
#                       (tests compare its messages; the FreeBSD package may be a newer
#                       release). Without it the package is used, with a warning on a mismatch.
#     SAMPLE_APISERVER=<binary>, ETCD=<binary>
#                       FreeBSD builds for the aggregator test's sample-apiserver Deployment. Use
#                       the sources upstream's images were built from: sample-apiserver tag 1.29.2
#                       is k8s.io/sample-apiserver v0.29.2 (git history of
#                       test/images/sample-apiserver), etcd 3.6.8-0 is etcd v3.6.8:
#                         (sample-apiserver) go mod tidy && GOOS=freebsd CGO_ENABLED=0 go build .
#                         (etcd/server)      GOOS=freebsd CGO_ENABLED=0 go build -o etcd .
#                       Images are only built when these are set.
#     COREDNS=<binary>  CoreDNS for /coredns in agnhost (DNS tests run it with a hosts-plugin
#                       Corefile). Upstream bundles 1.6.2, which predates Go's freebsd/arm64
#                       port; use the cluster's FreeBSD CoreDNS build.
#     REGRESSION_74839=<binary>
#                       FreeBSD build of test/images/regression-issue-74839 (kubernetes patch
#                       0036: reads TCP with bpf(4), as FreeBSD gives raw sockets no TCP; the
#                       pod needs CAP_NET_RAW, which CRI-O maps to /dev/bpf); the conntrack
#                       invalid-state test.
#     NFS_PROVISIONER=<binary>
#                       FreeBSD build of the in-tree NFS driver's dynamic provisioner
#                       (registry.k8s.io/sig-storage/nfs-provisioner): kubernetes-sigs/
#                       nfs-ganesha-server-and-external-provisioner with the FreeBSD port in
#                       patches/nfs-provisioner (unfs3 instead of NFS Ganesha, NFSv3 only):
#                         GOOS=freebsd CGO_ENABLED=0 go build ./cmd/nfs-provisioner
#     UNFSD=<binary>    unfsd for it, built from unfs3 0.11.0 + patches/unfs3 (nanosecond file
#                       times; the package's unfsd reports whole seconds, so clients keep stale
#                       data after a same-size rewrite within the second). Without it the
#                       package is used, with a warning.
#   Also needs skopeo (agnhost's fake registry, from ghcr.io/platformnerds/pause:3.10.2).
#     ONLY="<names>"    build only these images (agnhost kitten nautilus busybox nonroot
#                       glibc-dns-testing nonewprivs nginx sample-apiserver etcd
#                       regression-issue-74839 volume-nfs nfs-provisioner pause)
set -eu
src=$(cd "$1" && pwd); agnhost=$2; nnp=${3:-}
busybox=${BUSYBOX:-}; sampleapiserver=${SAMPLE_APISERVER:-}; etcd=${ETCD:-}
coredns=${COREDNS:-}; regression74839=${REGRESSION_74839:-}; nfsprovisioner=${NFS_PROVISIONER:-}; unfsd=${UNFSD:-}
want() { [ -z "${ONLY:-}" ] && return 0; case " $ONLY " in *" $1 "*) return 0;; esac; return 1; }
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
# fake_registry_pause <dir>: lay out the FreeBSD pause image the way `agnhost
# fake-registry-server` serves it: <dir>/pause/manifests/testing holds the manifest digest,
# manifests and blobs live at <kind>/sha256/<xx>/<hash>/data. Needs skopeo.
fake_registry_pause() {
  _img=ghcr.io/platformnerds/pause:3.10.2
  if ! buildah inspect --type image "$_img" >/dev/null 2>&1; then
    echo "WARNING: no $_img; agnhost's fake registry is empty (private-registry pull tests fail)"
    return 0
  fi
  _t=$(mktemp -d); _r=$1/pause
  skopeo copy -q "containers-storage:$_img" "dir:$_t/p"
  _put() { _h=${2#sha256:}; _f=$_r/$1/sha256/$(echo "$_h" | cut -c1-2)/$_h/data
           mkdir -p "$(dirname "$_f")"; cp "$3" "$_f"; }
  _md=sha256:$(sha256 -q "$_t/p/manifest.json")
  _put manifests "$_md" "$_t/p/manifest.json"
  echo "$_md" > "$_r/manifests/testing"
  for _b in "$_t"/p/*; do
    case ${_b##*/} in manifest.json|version) continue ;; esac
    _put blobs "sha256:${_b##*/}" "$_b"
  done
  rm -rf "$_t"
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
if want agnhost; then
new "$base"
pkgs curl bash bind-tools iperf iperf3 busybox
# Tests run `/bin/bash` by absolute path (Alpine's bash package installs it there; FreeBSD's
# lives in /usr/local/bin): the probe-while-terminating tests failed with CreateContainerError.
ln -sf /usr/local/bin/bash "$m/bin/bash"
if [ -n "$coredns" ]; then
  install -m 755 "$coredns" "$m/coredns"
else
  echo "WARNING: no COREDNS=; agnhost has no /coredns (DNS tests that run it will fail)"
fi
hostbin /usr/bin/nc /usr/bin/getent   # e2e runs `nc -v -t -w 2` (OpenBSD nc, like netcat-openbsd)
# Port-forward tests probe readiness with `netstat -na | grep LISTEN | grep 80` (busybox's netstat
# on Alpine; FreeBSD's prints `tcp4 0 0 *.80 *.* LISTEN`, which matches the same way).
hostbin /usr/bin/netstat
bbfill
install -m 755 "$agnhost" "$m/agnhost"
ln -sf agnhost "$m/agnhost-2"
mkdir -p "$m/uploads"
install -m 644 "$img/agnhost/porter/localhost.crt" "$img/agnhost/porter/localhost.key" "$m/"
# `agnhost fake-registry-server` (the private-registry pull tests) serves /var/registry. Upstream
# bakes registry.k8s.io/pause:3.10.2 into it as pause:testing (fakeregistryserver/prepare_registry.sh);
# serve the FreeBSD pause image instead, so the pod the test pulls it for runs on this node.
fake_registry_pause "$m/var/registry"
pw -R "$m" groupadd -n group-defined-in-image -g 50000
pw -R "$m" useradd -n user-defined-in-image -u 1000 -G group-defined-in-image -s /bin/sh
buildah config --entrypoint '["/agnhost"]' --cmd pause --port 80 --port 8080 --port 8081 --port 9376 --port 5000 "$c"
commit "$reg/agnhost:$(mtag Agnhost)" "$reg/agnhost:$(mtag AgnhostPrev)"
echo "built agnhost"
fi

# --- kitten / nautilus: agnhost + html + test-webserver --------------------------------------
for k in Kitten Nautilus; do
  d=$(echo $k | tr A-Z a-z)
  want $d || continue
  new "$reg/agnhost:$(mtag Agnhost)"
  cp "$img/$d/html/"* "$m/"
  buildah config --cmd test-webserver "$c"
  commit "$reg/$d:$(mtag $k)"
  echo "built $d"
done

# --- busybox: FreeBSD runtime base + busybox applets (wget, nslookup, ... missing from base) --
if want busybox; then
new "$base"
if [ -n "$busybox" ]; then
  mkdir -p "$m/usr/local/bin"
  install -m 755 "$busybox" "$m/usr/local/bin/busybox"
else
  pkgs busybox
fi
# The tag names the BusyBox release (1.37.0-1 -> 1.37.0), and tests compare its messages: 1.38
# prefixes `sh -c` errors with "line 0:", which upstream's 1.37.0 image doesn't.
bbver=$(chroot "$m" /usr/local/bin/busybox | sed -n '1s/^BusyBox v\([0-9.]*\).*/\1/p')
case "$(mtag BusyBox)" in
  "$bbver"|"$bbver"-*) ;;
  *) echo "WARNING: busybox image $(mtag BusyBox) contains BusyBox $bbver;" \
       "build the matching release with scripts/busybox-freebsd/build.sh and pass BUSYBOX=" ;;
esac
# Upstream's busybox image *is* busybox: make its applets win over FreeBSD's (tests compare e.g.
# busybox sh error text "can't create"), then fill the rest.
for a in $(chroot "$m" /usr/local/bin/busybox --list); do
  for d in bin usr/bin sbin usr/sbin; do if has "$m/$d/$a"; then ln -sf /usr/local/bin/busybox "$m/$d/$a"; fi; done
done
bbfill
buildah config --cmd sh "$c"
commit "$reg/busybox:$(mtag BusyBox)"
echo "built busybox (BusyBox $bbver)"
fi

# --- nonroot: busybox + USER 1234 -----------------------------------------------------------
if want nonroot; then
new "$reg/busybox:$(mtag BusyBox)"
buildah config --user 1234 "$c"
commit "$reg/nonroot:$(mtag NonRoot)"
echo "built nonroot"
fi

# --- glibc-dns-testing (formerly jessie-dnsutils): dig/host/nslookup + getent + nc ----------
if want glibc-dns-testing; then
new "$base"
pkgs bind-tools busybox
hostbin /usr/bin/getent /usr/bin/nc
bbfill
commit "$reg/glibc-dns-testing:$(mtag GlibcDnsTesting)"
echo "built glibc-dns-testing"
fi

# --- nonewprivs: setuid nnp helper on busybox --------------------------------------------------
if [ -n "$nnp" ] && want nonewprivs; then
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
if want nginx; then
new "$base"
pkgs nginx
# the package's post-install script creates this symlink; `pkg -r` into an image root doesn't
[ -e "$m/usr/local/www/nginx" ] || ln -s nginx-dist "$m/usr/local/www/nginx"
buildah config --entrypoint '["/usr/local/sbin/nginx", "-g", "daemon off;"]' --cmd '' --port 80 "$c"
commit "$reg/nginx:$(mtag NginxNew)" "$reg/nginx:$(mtag Nginx)"
echo "built nginx"
fi

# --- sample-apiserver + etcd: the aggregator test's Deployment (test/e2e/apimachinery) --------
# The test runs `/usr/local/bin/etcd` in the etcd container and the image entrypoint
# (/sample-apiserver) in the other, like upstream's images.
if [ -n "$sampleapiserver" ] && want sample-apiserver; then
  new "$base"
  install -m 755 "$sampleapiserver" "$m/sample-apiserver"
  buildah config --entrypoint '["/sample-apiserver"]' --cmd '' "$c"
  commit "$reg/sample-apiserver:$(mtag APIServer)"
  echo "built sample-apiserver"
fi
if [ -n "$etcd" ] && want etcd; then
  new "$base"
  mkdir -p "$m/usr/local/bin"
  install -m 755 "$etcd" "$m/usr/local/bin/etcd"
  buildah config --cmd /usr/local/bin/etcd "$c"
  commit "registry.k8s.io/etcd:$(mtag Etcd)"
  echo "built etcd"
fi

# --- regression-issue-74839: the conntrack invalid-state test's server ------------------------
if [ -n "$regression74839" ] && want regression-issue-74839; then
  new "$base"
  install -m 755 "$regression74839" "$m/regression-issue-74839"
  buildah config --entrypoint '["/regression-issue-74839"]' --cmd '' "$c"
  commit "$reg/regression-issue-74839:$(mtag RegressionIssue74839)"
  echo "built regression-issue-74839"
fi

# --- volume/nfs: test/images/volume/nfs (nfs-utils + run_nfs.sh) -----------------------------
# The kernel NFS server in a privileged container, from the base system's rpcbind, mountd and
# nfsd. It needs allow.nfsd (CRI-O grants it to privileged pods with their own network), and,
# since a container inherits its pod jail's vnet, security.jail.nfsd_inherited_vnet=1
# (freebsd-src patch 0036) and nfsd.ko loaded on the node. TCP only: the kernel RPC code
# refuses UDP transports in jails.
if want volume-nfs; then
  new "$base"
  hostbin /usr/sbin/rpcbind /usr/sbin/mountd /usr/sbin/nfsd /usr/bin/showmount /usr/bin/rpcinfo
  mkdir -p "$m/exports" "$m/tmp" "$m/var/db" "$m/var/run" "$m/usr/local/bin"
  install -m 644 "$img/volume/nfs/index.html" "$m/tmp/index.html"
  cat > "$m/usr/local/bin/run_nfs.sh" <<'NFSEOF'
#!/bin/sh
# FreeBSD version of test/images/volume/nfs/run_nfs.sh: export the given directories
# (read-write, root not squashed; the first one is the NFSv4 root, like fsid=0), copy
# index.html into them, start rpcbind, mountd and nfsd, print "NFS started".
gid=
while getopts "G:" opt; do case $opt in G) gid=$OPTARG ;; esac; done
shift $((OPTIND - 1))
: > /etc/exports
v4root=
for i in "$@"; do
  echo "$i -maproot=root" >> /etc/exports
  [ -n "$v4root" ] || v4root=$i
  if [ -n "$gid" ]; then chmod 070 "$i"; chgrp "$gid" "$i"; fi
  cp /tmp/index.html "$i/"; chmod 644 "$i/index.html"
  echo "Serving $i"
done
[ -z "$v4root" ] || echo "V4: $v4root" >> /etc/exports
stop() {
  echo "Stopping NFS"
  pkill -x nfsd; pkill -x mountd; pkill -x rpcbind
  : > /etc/exports
  exit 0
}
trap stop TERM
rpcbind
mountd
nfsd -t -n 4
echo "NFS started"
while :; do sleep 5; done
NFSEOF
  chmod 755 "$m/usr/local/bin/run_nfs.sh"
  buildah config --entrypoint '["/usr/local/bin/run_nfs.sh"]' --cmd '/exports /' \
    --port 2049/tcp --port 20048/tcp --volume /exports "$c"
  commit "$reg/volume/nfs:$(mtag VolumeNFSServer)"
  echo "built volume/nfs"
fi

# --- nfs-provisioner: the in-tree NFS driver's provisioner pod (test/e2e/storage/utils) -------
# Upstream's image runs NFS Ganesha; the FreeBSD build runs unfs3 (a user-space NFSv3 server:
# the pod isn't privileged, so the kernel nfsd can't run in it). It needs rpcbind and rpcinfo.
if [ -n "$nfsprovisioner" ] && want nfs-provisioner; then
  new "$base"
  if [ -n "$unfsd" ]; then
    mkdir -p "$m/usr/local/sbin"
    install -m 755 "$unfsd" "$m/usr/local/sbin/unfsd"
  else
    echo "WARNING: no UNFSD=; using the unfs3 package, which reports whole-second file times"
    pkgs unfs3
  fi
  hostbin /usr/sbin/rpcbind /usr/bin/rpcinfo
  install -m 755 "$nfsprovisioner" "$m/nfs-provisioner"
  mkdir -p "$m/export" "$m/var/run" "$m/var/db"
  buildah config --entrypoint '["/nfs-provisioner"]' --cmd '[]' \
    --port 2049/tcp --port 20048/tcp --port 111/tcp --port 111/udp --volume /export "$c"
  commit "registry.k8s.io/sig-storage/nfs-provisioner:$(mtag NFSProvisioner)"
  echo "built nfs-provisioner"
fi

# --- pause: reuse the control-plane pause image (scripts/build-images-freebsd.sh) -------------
if want pause && buildah inspect --type image ghcr.io/platformnerds/pause:3.10.2 >/dev/null 2>&1; then
  buildah tag ghcr.io/platformnerds/pause:3.10.2 registry.k8s.io/pause:$(mtag Pause)
  echo "tagged pause"
fi
