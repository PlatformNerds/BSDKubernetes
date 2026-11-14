# Forks: develop here first, upstream later

Strategy: **fork first, then upstream.** We develop in private PlatformNerds forks so we can move fast and
carry WIP. Once a topic is stable and tested on real FreeBSD hosts, we split it into clean commits and offer it
upstream: to `dfr/*` first (he owns the FreeBSD ports of these projects), then to the real upstream projects.

## The forks

GitHub can't make a fork of a public repo private, so these are **private mirrors** (full history, all refs):

| Fork (private) | Mirrors | Our working branch (default) | Status |
|---|---|---|---|
| [PlatformNerds/kubernetes](https://github.com/PlatformNerds/kubernetes) | `dfr/kubernetes` `master`, `freebsd-*` + upstream `master` as `upstream-master`, tags v1.36.1, v1.31.0-alpha.0 | **`bsdk8s-v1.36.1`**: dfr kubelet + kube-proxy(pf) + kubeadm merged + our cgo-free/rctl fix | ✅ all k8s binaries cross-compile for FreeBSD amd64/arm64 |
| [PlatformNerds/cri-o](https://github.com/PlatformNerds/cri-o) | `cri-o/cri-o` `main` + all `dfr/cri-o` branches under `dfr/*` | **`bsdk8s-1.37`** (= `dfr/freebsd-wip-1.37.0`, ~1.4 kLOC FreeBSD WIP) | ✅ `crio` cross-compiles for FreeBSD |
| [PlatformNerds/ocijail](https://github.com/PlatformNerds/ocijail) | `dfr/ocijail` (all branches + tags) | `main` | C++/Bazel; build on FreeBSD |
| [PlatformNerds/cni-plugins](https://github.com/PlatformNerds/cni-plugins) | `dfr/plugins` (all branches + tags) | **`freebsd`** | the branch behind FreeBSD's `net/containernetworking-plugins` port |
| [PlatformNerds/kubernetes-demo](https://github.com/PlatformNerds/kubernetes-demo) | `dfr/kubernetes-demo` `main` | **`bsdk8s`**: builds CRI-O and k8s from our forks (`BSDK8S_GIT_BASE`, `CRIO_BRANCH`, `K8S_BRANCH` override) | single-node FreeBSD cluster bootstrap |

> Correction to the original research: CRI-O's FreeBSD *pod* support is **not fully upstream**. dfr's demo builds
> from `dfr/cri-o` branch `freebsd-wip-1.37.0`, which carries stats, devices, RunAsUser and sandbox
> validation work. Getting that upstream is part of our job.

## Branch conventions

- **Upstream-owned refs** (`master`, `main`, `freebsd-*`, `dfr/*`, `upstream-master`, tags) are **mirror-only**.
  `scripts/sync-forks.sh` updates them, and nobody commits to them.
- **`bsdk8s-<base>`** (e.g. `bsdk8s-v1.36.1`, `bsdk8s-1.37`) is our integration branch per upstream base version. It's the default branch.
- **`bsdk8s/<topic>`** is a feature branch, opened as a PR into the integration branch inside the private fork. Keep commits small and
  topic-pure (one subsystem per commit, upstream-style messages) so they can later be cherry-picked upstream.
- New upstream version: create `bsdk8s-<newbase>` from the new dfr/upstream base and rebase or cherry-pick the topic commits.

## Syncing

```sh
scripts/sync-forks.sh            # needs SSH push to PlatformNerds; never touches bsdk8s* branches
```

## Upstreaming (later)

1. Pick a topic whose commits are tested on FreeBSD 15.x (and ideally covered by CI).
2. Create a **public** fork (e.g. `PlatformNerds/kubernetes-public`, or a personal fork), push only that topic branch.
3. PR order: **`dfr/<repo>` first** (keep his series authoritative), then upstream (`cri-o/cri-o`, `kubernetes/kubernetes`
   via a KEP where needed, `containernetworking/plugins`).
4. After merge, drop the commit from our integration branch on the next rebase.

First candidate: `kubelet/cadvisor: make FreeBSD build cgo-free; fix rctl racct parsing` (on `bsdk8s-v1.36.1`).
