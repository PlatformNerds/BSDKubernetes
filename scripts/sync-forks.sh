#!/bin/sh
# Pull new commits from upstream / dfr into the PlatformNerds forks.
# Upstream-owned refs are mirrored untouched; our bsdk8s* branches are never overwritten.
# Usage: scripts/sync-forks.sh [work-dir]   (needs SSH push access to PlatformNerds)
set -eu
w=${1:-"$HOME/bsdk8s/forks"}; mkdir -p "$w"; cd "$w"
pn=git@github.com:PlatformNerds

sync() { # name  src-url  fetch-refspecs...
  name=$1 src=$2; shift 2
  [ -d "$name.git" ] || git init -q --bare "$name.git"
  git -C "$name.git" fetch -q "$src" "$@"
  # push everything except our own branches (bsdk8s*), which are developed in the fork
  git -C "$name.git" for-each-ref --format='%(refname)' refs/heads refs/tags \
    | grep -v '^refs/heads/bsdk8s' \
    | sed 's/.*/+&:&/' | xargs git -C "$name.git" push -q "$pn/$name.git"
  echo "synced $name"
}

sync kubernetes      https://github.com/dfr/kubernetes.git '+refs/heads/master:refs/heads/master' '+refs/heads/freebsd-*:refs/heads/freebsd-*'
git -C kubernetes.git fetch -q https://github.com/kubernetes/kubernetes.git '+refs/heads/master:refs/heads/upstream-master'
git -C kubernetes.git push -q "$pn/kubernetes.git" '+refs/heads/upstream-master:refs/heads/upstream-master'
sync cri-o           https://github.com/cri-o/cri-o.git '+refs/heads/main:refs/heads/main'
git -C cri-o.git fetch -q https://github.com/dfr/cri-o.git '+refs/heads/*:refs/heads/dfr/*'
git -C cri-o.git push -q "$pn/cri-o.git" '+refs/heads/dfr/*:refs/heads/dfr/*'
sync ocijail         https://github.com/dfr/ocijail.git '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*'
sync cni-plugins     https://github.com/dfr/plugins.git '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*'
sync kubernetes-demo https://github.com/dfr/kubernetes-demo.git '+refs/heads/main:refs/heads/main'
