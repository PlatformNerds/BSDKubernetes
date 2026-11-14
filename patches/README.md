# Patch series

Applied on top of upstream `kubernetes/kubernetes` tag **v1.36.1**, in this order (see `scripts/build-freebsd-k8s.sh`):

| Dir | Source | Author | Notes |
|---|---|---|---|
| `kubernetes/v1.36.1/kubelet/` | `dfr/kubernetes` branch `freebsd-kubelet-v1.36.1` | Doug Rabson | FreeBSD cadvisor (racct), container manager, mount-utils, subpath, emptyDir, security context |
| `kubernetes/v1.36.1/proxy/` | `dfr/kubernetes` branch `freebsd-proxy-v1.36.1` | Doug Rabson | kube-proxy `pf` mode |
| `kubernetes/v1.36.1/kubeadm/` | `dfr/kubernetes` branch `freebsd-kubeadm-v1.36.1` | Doug Rabson | kubeadm on FreeBSD |
| `kubernetes/v1.36.1/bsdk8s/` | this project | BSDKubernetes | cgo-free kubelet build (enables cross-compile) + rctl racct parsing fixes |

Outside Kubernetes (the branches are the source of truth; these are exports for review):

| Dir | Applies to | Notes |
|---|---|---|
| `ocijail/` | `dfr/ocijail` `main` → our `bsdk8s-main` | jail param fix, rctl resource limits, `allow.pf`, file mounts below devfs, `noNewPrivileges` (0009) |
| `cri-o/` | `dfr/cri-o` `freebsd-wip-1.37.0` → our `bsdk8s-1.37` | backport of the containers/storage fix for setuid/setgid bits lost when unpacking layers on FreeBSD |

All patches are Apache-2.0 like Kubernetes itself. dfr's patches are unreviewed upstream WIP. The intent is to
contribute our fixes back to `dfr/kubernetes`, not to fork his work.
