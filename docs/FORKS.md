# Forks: develop here first, upstream later

Strategy: **fork first, then upstream.** We develop in public PlatformNerds forks so we can move fast and carry WIP.
Once a topic is stable and tested on real FreeBSD hosts, we split it into clean commits and open PRs: to `dfr/*`
first (he owns the FreeBSD ports of these projects), then to the real upstream projects.

## The forks

Since 2026-11-14 these are **real GitHub forks** (public), so PRs can be opened directly from them to `dfr/*` or to the
fork-network root. (Before that they were private mirrors; those were replaced by these forks with all branches
verified identical, then deleted.)

| Fork | Parent → network root | Our working branch (default) | Status |
|---|---|---|---|
| [PlatformNerds/kubernetes](https://github.com/PlatformNerds/kubernetes) | `dfr/kubernetes` → `kubernetes/kubernetes` | **`bsdk8s-v1.36.1`**: dfr kubelet + kube-proxy(pf) + kubeadm merged, plus cgo-free kubelet/rctl parsing fix and container resource limits | ✅ all k8s binaries build for FreeBSD amd64/arm64 (CI) |
| [PlatformNerds/cri-o](https://github.com/PlatformNerds/cri-o) | `dfr/cri-o` → `cri-o/cri-o` | **`bsdk8s-1.37`** (= `dfr/freebsd-wip-1.37.0`, ~1.4 kLOC FreeBSD WIP) | ✅ builds; **must be built natively with cgo** (a `CGO_ENABLED=0` cross-build compiles but panics on the first mount) |
| [PlatformNerds/ocijail](https://github.com/PlatformNerds/ocijail) | `dfr/ocijail` | **`bsdk8s-main`**: resource limits via rctl + jail type-assert fix | ✅ running in the dev VM (demo builds it from source) |
| [PlatformNerds/cni-plugins](https://github.com/PlatformNerds/cni-plugins) | `dfr/plugins` → `containernetworking/plugins` | **`freebsd`** (unchanged) | the branch behind FreeBSD's `net/containernetworking-plugins` port |
| [PlatformNerds/freebsd-src](https://github.com/PlatformNerds/freebsd-src) | `freebsd/freebsd-src` | **`bsdk8s/allow-pf`** (main), **`bsdk8s/allow-pf-15.1`** (tested backport) | kernel work (K-series); `allow.pf` booted and verified |
| [PlatformNerds/kubernetes-demo](https://github.com/PlatformNerds/kubernetes-demo) | `dfr/kubernetes-demo` | **`bsdk8s`**: builds CRI-O, k8s and ocijail from our forks (`BSDK8S_GIT_BASE`, `*_BRANCH` overrides), arm64 support, fixes | single-node FreeBSD cluster bootstrap |

> Correction to the original research: CRI-O's FreeBSD *pod* support is **not fully upstream**. dfr's demo builds
> from `dfr/cri-o` branch `freebsd-wip-1.37.0`, which carries stats, devices, RunAsUser and sandbox
> validation work. Getting that upstream is part of our job.

## Branch conventions

- **Upstream-tracking branches** (`master`/`main`, dfr's `freebsd-*`) are kept identical to the parent by
  `scripts/sync-forks.sh` (GitHub merge-upstream, fast-forward only). Nobody commits to them.
- **`bsdk8s-<base>`** (e.g. `bsdk8s-v1.36.1`, `bsdk8s-1.37`, `bsdk8s-main`) is our integration branch per upstream base. It's the default branch.
- **`bsdk8s/<topic>`** is a feature branch, merged into the integration branch. Keep commits small and topic-pure
  (one subsystem per commit, upstream-style messages) so they can be cherry-picked upstream.
- **Upstream PR branches** are named for the change (e.g. `fix-jail-param-validation`) and **based on the parent's branch**
  (e.g. `dfr/ocijail:main`, `dfr/kubernetes:freebsd-kubelet-v1.36.1`), never on `bsdk8s-*`. They contain only that change plus its tests.
- New upstream version: create `bsdk8s-<newbase>` from the new dfr/upstream base and rebase or cherry-pick the topic commits.

## Syncing

```sh
scripts/sync-forks.sh            # gh merge-upstream for the tracking branches; never touches our branches
```

## Upstreaming

1. Pick a topic whose commits are tested on FreeBSD 15.x (and ideally covered by CI and the project's own test suite).
2. Create a PR branch from the parent's branch in our fork, cherry-pick the topic commits, and add tests.
3. Open the PR from `PlatformNerds/<repo>:<pr-branch>` to `dfr/<repo>` first (keep his series authoritative), then
   upstream (`cri-o/cri-o`, `kubernetes/kubernetes` via a KEP where needed, `containernetworking/plugins`).
4. After merge, drop the commit from our integration branch on the next rebase.

Queued PRs: see the "Upstream candidates" section of [research/06](../research/06-resource-limits.md) and docs/MAC-DEV-VM.md §4.
