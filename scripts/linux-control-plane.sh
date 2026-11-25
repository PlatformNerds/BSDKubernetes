#!/bin/sh
# Stock Kubernetes control plane on Debian 13 (arm64 or amd64) that FreeBSD workers join (research/27).
# Nothing here is patched: upstream containerd/runc/CNI plugins and kubeadm/kubelet/kubectl from
# pkgs.k8s.io. What makes FreeBSD workers fit:
#  - pod routes: every node routes other nodes' podCIDRs via their InternalIP (host-gw). This node runs
#    linux-hostgw (systemd), the FreeBSD workers bsd-hostgw (kubernetes-demo), with ServiceAccount
#    kube-system/bsd-hostgw whose token the workers get as HOSTGW_TOKEN.
#  - kube-proxy: kubeadm's DaemonSet already selects kubernetes.io/os=linux; a copy named
#    kube-proxy-freebsd runs the FreeBSD image (pf proxier) on kubernetes.io/os=freebsd, same ConfigMap.
# Run on the VM as a sudo-capable user:  NODE_IP=192.168.100.10 sh linux-control-plane.sh
# Env: NODE_IP (required) K8S_VERSION (1.36.1) CONTAINERD_VERSION (2.4.2) RUNC_VERSION (1.5.3)
#      CNI_VERSION (1.10.0) POD_SUBNET (10.244.0.0/16) SERVICE_SUBNET (10.96.0.0/12)
#      FREEBSD_KUBE_PROXY_IMAGE (ghcr.io/platformnerds/kube-proxy:v<K8S_VERSION>)
set -eu
NODE_IP=${NODE_IP:?NODE_IP is required (cluster-network address of this node)}
K8S_VERSION=${K8S_VERSION:-1.36.1}
CONTAINERD_VERSION=${CONTAINERD_VERSION:-2.4.2}
RUNC_VERSION=${RUNC_VERSION:-1.5.3}
CNI_VERSION=${CNI_VERSION:-1.10.0}
POD_SUBNET=${POD_SUBNET:-10.244.0.0/16}
SERVICE_SUBNET=${SERVICE_SUBNET:-10.96.0.0/12}
FREEBSD_KUBE_PROXY_IMAGE=${FREEBSD_KUBE_PROXY_IMAGE:-ghcr.io/platformnerds/kube-proxy:v${K8S_VERSION}}
ARCH=$(dpkg --print-architecture)
K8S_MINOR=$(echo "$K8S_VERSION" | cut -d. -f1,2)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

# Kernel prerequisites.
printf 'overlay\nbr_netfilter\n' | sudo tee /etc/modules-load.d/k8s.conf >/dev/null
sudo modprobe overlay; sudo modprobe br_netfilter
printf 'net.bridge.bridge-nf-call-iptables=1\nnet.bridge.bridge-nf-call-ip6tables=1\nnet.ipv4.ip_forward=1\n' |
	sudo tee /etc/sysctl.d/99-k8s.conf >/dev/null
sudo sysctl -q --system
sudo swapoff -a

# containerd + runc + CNI plugins from upstream releases (Debian 13 ships containerd 1.7).
fetch() { curl -fsSL -o "$tmp/$2" "$1"; }
if ! containerd --version 2>/dev/null | grep -q "v${CONTAINERD_VERSION}"; then
	gh=https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}
	fetch "$gh/containerd-${CONTAINERD_VERSION}-linux-${ARCH}.tar.gz" ctr.tgz
	fetch "$gh/containerd-${CONTAINERD_VERSION}-linux-${ARCH}.tar.gz.sha256sum" ctr.sha
	(cd "$tmp" && [ "$(sha256sum ctr.tgz | cut -d' ' -f1)" = "$(cut -d' ' -f1 ctr.sha)" ])
	sudo tar -C /usr/local -xzf "$tmp/ctr.tgz"
	sudo curl -fsSL -o /etc/systemd/system/containerd.service \
		"https://raw.githubusercontent.com/containerd/containerd/v${CONTAINERD_VERSION}/containerd.service"
fi
if ! runc --version 2>/dev/null | grep -q "^runc version ${RUNC_VERSION}"; then
	gh=https://github.com/opencontainers/runc/releases/download/v${RUNC_VERSION}
	fetch "$gh/runc.${ARCH}" runc; fetch "$gh/runc.sha256sum" runc.sha
	(cd "$tmp" && [ "$(sha256sum runc | cut -d' ' -f1)" = "$(awk -v f="runc.${ARCH}" '$2 == f || $2 == "*"f {print $1}' runc.sha)" ])
	sudo install -m 755 "$tmp/runc" /usr/local/sbin/runc
fi
if [ ! -x /opt/cni/bin/bridge ] || ! /opt/cni/bin/bridge 2>&1 | grep -q "v${CNI_VERSION}"; then
	gh=https://github.com/containernetworking/plugins/releases/download/v${CNI_VERSION}
	fetch "$gh/cni-plugins-linux-${ARCH}-v${CNI_VERSION}.tgz" cni.tgz
	fetch "$gh/cni-plugins-linux-${ARCH}-v${CNI_VERSION}.tgz.sha256" cni.sha
	(cd "$tmp" && [ "$(sha256sum cni.tgz | cut -d' ' -f1)" = "$(cut -d' ' -f1 cni.sha)" ])
	sudo mkdir -p /opt/cni/bin; sudo tar -C /opt/cni/bin -xzf "$tmp/cni.tgz"
fi
sudo mkdir -p /etc/containerd
containerd config default | sed 's/SystemdCgroup = false/SystemdCgroup = true/' |
	sudo tee /etc/containerd/config.toml >/dev/null
grep -q 'SystemdCgroup = true' /etc/containerd/config.toml ||
	echo "warning: could not set SystemdCgroup in containerd config"
sudo systemctl daemon-reload; sudo systemctl enable containerd; sudo systemctl restart containerd

# kubeadm, kubelet, kubectl from pkgs.k8s.io.
curl -fsSL "https://pkgs.k8s.io/core:/stable:/v${K8S_MINOR}/deb/Release.key" |
	sudo gpg --batch --yes --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v${K8S_MINOR}/deb/ /" |
	sudo tee /etc/apt/sources.list.d/kubernetes.list >/dev/null
sudo apt-get update -qq
pkgver=$(apt-cache madison kubeadm | awk -v v="$K8S_VERSION" '$3 ~ "^"v"-" {print $3; exit}')
[ -n "$pkgver" ] || { echo "kubeadm $K8S_VERSION not in pkgs.k8s.io"; exit 1; }
sudo apt-mark unhold kubelet kubeadm kubectl >/dev/null 2>&1 || true
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --allow-downgrades \
	kubelet="$pkgver" kubeadm="$pkgver" kubectl="$pkgver" >/dev/null
sudo apt-mark hold kubelet kubeadm kubectl >/dev/null

# Control plane.
if [ ! -f /etc/kubernetes/admin.conf ]; then
	cat > "$tmp/kubeadm.yaml" <<EOF
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint: {advertiseAddress: ${NODE_IP}}
nodeRegistration:
  kubeletExtraArgs:
  - {name: node-ip, value: ${NODE_IP}}
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v${K8S_VERSION}
networking: {podSubnet: ${POD_SUBNET}, serviceSubnet: ${SERVICE_SUBNET}}
EOF
	sudo kubeadm init --config "$tmp/kubeadm.yaml"
fi
mkdir -p "$HOME/.kube"; sudo install -o "$(id -u)" -g "$(id -g)" -m 600 /etc/kubernetes/admin.conf "$HOME/.kube/config"
self=$(hostname)
until cidr=$(kubectl get node "$self" -o jsonpath='{.spec.podCIDR}') && [ -n "$cidr" ]; do sleep 2; done

# Pod network on this node: bridge + host-local on the node's podCIDR. No ipMasq in the plugin
# (it would SNAT pod-to-pod traffic to other nodes); masquerade only traffic leaving the pod network.
sudo mkdir -p /etc/cni/net.d
sudo tee /etc/cni/net.d/10-hostgw.conflist >/dev/null <<EOF
{
  "cniVersion": "1.0.0",
  "name": "hostgw",
  "plugins": [
    {"type": "bridge", "bridge": "cni0", "isGateway": true, "ipMasq": false, "hairpinMode": true,
     "ipam": {"type": "host-local", "ranges": [[{"subnet": "${cidr}"}]], "routes": [{"dst": "0.0.0.0/0"}]}},
    {"type": "portmap", "capabilities": {"portMappings": true}}
  ]
}
EOF
sudo tee /etc/systemd/system/k8s-pod-masq.service >/dev/null <<EOF
[Unit]
Description=Masquerade pod traffic leaving ${POD_SUBNET}
After=network-online.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'iptables -t nat -C POSTROUTING -s ${POD_SUBNET} ! -d ${POD_SUBNET} -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -s ${POD_SUBNET} ! -d ${POD_SUBNET} -j MASQUERADE'
[Install]
WantedBy=multi-user.target
EOF

# linux-hostgw: the Linux counterpart of bsd-hostgw (routes to other nodes' podCIDRs).
sudo tee /usr/local/libexec/linux-hostgw >/dev/null <<'EOF'
#!/bin/sh
# Route every other node's podCIDRs via its InternalIP of the same family (host-gw).
KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}; INTERVAL=${INTERVAL:-30}; self=$(hostname)
while :; do
	kubectl --kubeconfig "$KUBECONFIG" get nodes -o \
	    jsonpath='{range .items[*]}{.metadata.name} {.spec.podCIDRs[*]} | {.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' |
	while read -r line; do
		name=${line%% *}; rest=${line#* }; cidrs=${rest%%|*}; ips=${rest#*|}
		[ "$name" = "$self" ] && continue
		for cidr in $cidrs; do
			gw=
			for ip in $ips; do
				case $cidr in
				*:*) case $ip in *:*) gw=$ip; break ;; esac ;;
				*) case $ip in *:*) ;; *) gw=$ip; break ;; esac ;;
				esac
			done
			[ -z "$gw" ] && continue
			cur=$(ip route show "$cidr" | awk '{for (i = 1; i < NF; i++) if ($i == "via") print $(i+1)}')
			[ "$cur" = "$gw" ] || { ip route replace "$cidr" via "$gw" && echo "route $cidr -> $gw ($name)"; }
		done
	done
	sleep "$INTERVAL"
done
EOF
sudo chmod 755 /usr/local/libexec/linux-hostgw
sudo tee /etc/systemd/system/linux-hostgw.service >/dev/null <<'EOF'
[Unit]
Description=host-gw pod routes to other Kubernetes nodes
After=kubelet.service
[Service]
ExecStart=/usr/local/libexec/linux-hostgw
Restart=always
[Install]
WantedBy=multi-user.target
EOF
sudo systemctl daemon-reload
sudo systemctl enable --now k8s-pod-masq linux-hostgw

# FreeBSD workers: bsd-hostgw credentials and the FreeBSD kube-proxy DaemonSet.
kubectl apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata: {name: bsd-hostgw, namespace: kube-system}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: bsd-hostgw}
rules: [{apiGroups: [""], resources: [nodes], verbs: [get, list, watch]}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: bsd-hostgw}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: bsd-hostgw}
subjects: [{kind: ServiceAccount, name: bsd-hostgw, namespace: kube-system}]
EOF
kubectl -n kube-system get ds kube-proxy -o json |
	jq --arg img "$FREEBSD_KUBE_PROXY_IMAGE" '
	  del(.metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp, .metadata.generation,
	      .metadata.annotations, .metadata.managedFields, .status)
	  | .metadata.name = "kube-proxy-freebsd"
	  | .spec.selector.matchLabels["k8s-app"] = "kube-proxy-freebsd"
	  | .spec.template.metadata.labels["k8s-app"] = "kube-proxy-freebsd"
	  | .spec.template.spec.nodeSelector = {"kubernetes.io/os": "freebsd"}
	  | .spec.template.spec.containers[0].image = $img' |
	kubectl apply -f - >/dev/null

echo
echo "Control plane ready. Join a FreeBSD worker (kubernetes-demo bsdk8s branch) with:"
join=$(sudo kubeadm token create --print-join-command)
echo "  NODE_IP=<worker ip> JOIN_ENDPOINT=${NODE_IP}:6443 \\"
echo "  JOIN_TOKEN=$(echo "$join" | awk '{for (i = 1; i < NF; i++) if ($i == "--token") print $(i+1)}') \\"
echo "  JOIN_CA_HASH=$(echo "$join" | awk '{for (i = 1; i < NF; i++) if ($i == "--discovery-token-ca-cert-hash") print $(i+1)}') \\"
echo "  HOSTGW_TOKEN=\$(kubectl -n kube-system create token bsd-hostgw --duration=8760h) \\"
echo "  ./build-cluster.sh <uplink if>"
