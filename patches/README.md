# Patch series

Applied on top of upstream `kubernetes/kubernetes` tag **v1.36.1**, in this order (see `scripts/build-freebsd-k8s.sh`):

| Dir | Source | Author | Notes |
|---|---|---|---|
| `kubernetes/v1.36.1/kubelet/` | `dfr/kubernetes` branch `freebsd-kubelet-v1.36.1` | Doug Rabson | FreeBSD cadvisor (racct), container manager, mount-utils, subpath, emptyDir, security context |
| `kubernetes/v1.36.1/proxy/` | `dfr/kubernetes` branch `freebsd-proxy-v1.36.1` | Doug Rabson | kube-proxy `pf` mode |
| `kubernetes/v1.36.1/kubeadm/` | `dfr/kubernetes` branch `freebsd-kubeadm-v1.36.1` | Doug Rabson | kubeadm on FreeBSD |
| `kubernetes/v1.36.1/bsdk8s/` | this project | BSDKubernetes | cgo-free kubelet build (enables cross-compile) + rctl racct parsing fixes |

All patches are Apache-2.0 like Kubernetes itself. dfr's patches are unreviewed upstream WIP. The intent is to
contribute our fixes back to `dfr/kubernetes`, not to fork his work.
