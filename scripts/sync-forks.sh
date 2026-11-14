#!/bin/sh
# Bring the upstream-tracking branches of the PlatformNerds forks up to date with their
# parents (dfr/*). The forks are real GitHub forks, so this uses GitHub's merge-upstream API
# (fast-forward only; it fails loudly instead of creating merge commits).
# Our own branches (bsdk8s*, PR topic branches) are never touched: rebase those by hand.
# Usage: scripts/sync-forks.sh        (needs `gh` authenticated as a PlatformNerds member)
set -u
sync() { # fork  branch...
  repo=$1; shift
  for b in "$@"; do
    if out=$(gh api -X POST "repos/PlatformNerds/$repo/merge-upstream" -f branch="$b" --jq .message 2>&1); then
      echo "ok    $repo:$b  $out"
    else
      echo "FAIL  $repo:$b  $out"
    fi
  done
}

sync kubernetes      master freebsd-kubelet-v1.36.1 freebsd-proxy-v1.36.1 freebsd-kubeadm-v1.36.1 \
                     freebsd-kubelet freebsd-proxy freebsd-kubeadm freebsd-build
sync cri-o           main freebsd-wip-1.37.0 freebsd-wip
sync ocijail         main
sync cni-plugins     freebsd main
sync kubernetes-demo main

# Note: kubernetes/kubernetes, cri-o/cri-o and containernetworking/plugins are the fork-network
# roots, so PRs can target either dfr/<repo> or the real upstream.
