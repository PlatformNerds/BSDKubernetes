#!/bin/sh
# Build BusyBox for FreeBSD at the version upstream's e2e busybox image ships, the way the
# FreeBSD port (sysutils/busybox) builds it.
#
# Why: the e2e busybox image tag (test/utils/image/manifest.go, e.g. 1.37.0-1) names a BusyBox
# version, and tests compare its output. The FreeBSD package follows the ports tree (1.38.0 at
# the time of writing), and 1.38 changed ash's error prefix for `sh -c` from
# "/bin/sh: can't create ..." to "/bin/sh: line 0: can't create ...".
#
# port-<version>/ holds sysutils/busybox from freebsd-ports as of the last commit at that
# version (1.37.0: dbd2ad70f2f4, "mark as PIE_UNSAFE"). Runs on FreeBSD; needs gmake and gsed.
#
#   scripts/busybox-freebsd/build.sh 1.37.0 <outdir>     # -> <outdir>/busybox
set -eu
ver=$1; out=$(mkdir -p "$2" && cd "$2" && pwd)
port=$(cd "$(dirname "$0")/port-$ver" && pwd)
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT

f=busybox-$ver.tar.bz2
fetch -q -o "$work/$f" "https://busybox.net/downloads/$f"
want=$(sed -n "s/^SHA256 ($f) = //p" "$port/distinfo")
[ "$(sha256 -q "$work/$f")" = "$want" ] || { echo "$f: checksum mismatch"; exit 1; }
tar -xjf "$work/$f" -C "$work"
src=$work/busybox-$ver

# The port's do-patch, post-patch and do-configure steps.
for p in "$port"/files/patch-*; do patch -s -p0 -d "$src" < "$p"; done
# Ours (local/): the df applet, which the port leaves out because it reads Linux's mount
# table; upstream's busybox image has it, and tests run `df` (e.g. the memory emptyDir size).
for p in "$(dirname "$port")"/local/patch-*; do patch -s -p0 -d "$src" < "$p"; done
sed -i '' -e 's|\([[:<:]]\)sed\([[:blank:]]-n\)|\1gsed\2|' "$src/scripts/gen_build_files.sh"
find "$src" -name '*.c' -print0 | xargs -0 sed -i '' -e 's|<alloca.h>|<stdlib.h>|; s|<malloc.h>|<stdlib.h>|'
find "$src" -name '*.[ch]' -print0 | xargs -0 sed -i '' -e 's|"/proc|"/compat/linux/proc|'
cp "$port/files/data-.config" "$src/.config"
sed -i '' -e 's|^# CONFIG_DF is not set|CONFIG_DF=y|' \
    -e 's|^# CONFIG_FEATURE_DF_FANCY is not set|CONFIG_FEATURE_DF_FANCY=y|' "$src/.config"
# Link FreeBSD's libregex (the GNU-compatible regex grep(1) uses) statically, ahead of libc's
# POSIX-only regcomp. Upstream's image is Alpine, whose musl regex takes \b, \w, \< ...; tests
# use them (the NFS PV tests run `id -G | grep -E '\b777\b'`), and libc's regcomp rejects them
# ("trailing backslash"). CONFIG_EXTRA_LDLIBS, because trylink drops LDLIBS entries the link
# succeeds without; static, so the image needs no extra shared library.
sed -i '' -e 's|^CONFIG_EXTRA_LDLIBS=.*|CONFIG_EXTRA_LDLIBS=":libregex.a"|' "$src/.config"
grep -q '^CONFIG_EXTRA_LDLIBS=":libregex.a"' "$src/.config"
# CFLAGS: the ports framework's defaults, plus BB_GLOBAL_CONST='' (the port sets it for armv6
# only). BusyBox declares the pointer to each applet's globals const and fills it in through a
# cast, and clang optimizes on the const: 1.37.0's awk dereferenced a stale pointer and
# crashed on any program on arm64 (`echo x | awk '{print $1}'` -> SIGSEGV).
cflags="-O2 -pipe -fstack-protector-strong -fno-strict-aliasing -DBB_GLOBAL_CONST="
mk() { gmake -C "$src" CC=cc HOSTCC=cc CXX=c++ HOSTCXX=c++ CFLAGS="$cflags" SKIP_STRIP=y "$@"; }
mk oldconfig </dev/null >/dev/null
# Only the binary: the default target also builds the docs, which need perl.
mk -j"$(sysctl -n hw.ncpu)" busybox >/dev/null
install -m 755 "$src/busybox" "$out/busybox"
"$out/busybox" | head -1
