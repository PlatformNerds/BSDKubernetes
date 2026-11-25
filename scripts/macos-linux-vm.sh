#!/bin/sh
# Debian 13 (arm64) VM on this Mac for a stock Linux control plane with FreeBSD workers (research/27).
# Same layout as macos-freebsd-vm.sh: user-mode NAT NIC for internet + ssh, and a second NIC on the
# cluster vde switch, 192.168.100.<NODE>/24 and fd00:100::<NODE hex>/64.
#   scripts/macos-linux-vm.sh create   # download image, cloud-init, boot
#   scripts/macos-linux-vm.sh start|stop|ssh [cmd]|console|serial|monitor <cmd>
# Env: NODE (10) VM_DIR (~/bsdk8s/vm-linux-<NODE>) VM_CPUS (4) VM_MEM (6G) VM_DISK (40G)
#      SSH_PORT (2221+NODE) VDE_SOCK (~/bsdk8s/vde.ctl) KEY (~/bsdk8s/vm/id_ed25519)
# The VM's user is bsdk8s with passwordless sudo, like the FreeBSD nodes.
set -e
NODE=${NODE:-10}
VM_DIR=${VM_DIR:-$HOME/bsdk8s/vm-linux-$NODE}
VM_CPUS=${VM_CPUS:-4}; VM_MEM=${VM_MEM:-6G}; VM_DISK=${VM_DISK:-40G}
SSH_PORT=${SSH_PORT:-$((2221 + NODE))}
VDE_SOCK=${VDE_SOCK:-$HOME/bsdk8s/vde.ctl}
HOSTNAME_VM=bsdk8s-linux-$NODE
IMG=debian-13-genericcloud-arm64.qcow2
URL=https://cloud.debian.org/images/cloud/trixie/latest
FW=$(brew --prefix qemu)/share/qemu/edk2-aarch64-code.fd
KEY=${KEY:-$HOME/bsdk8s/vm/id_ed25519}
MAC0=52:54:00:bd:00:$(printf %02x "$NODE"); MAC1=52:54:00:bd:01:$(printf %02x "$NODE")

mkdir -p "$VM_DIR"; cd "$VM_DIR"

monitor() { printf '%s\n' "$1" | nc -U -w 2 monitor.sock | tr -d '\r' | grep -v '^QEMU .*monitor\|^(qemu)'; }
ssh_vm() { ssh -q -i "$KEY" -p "$SSH_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -o ConnectTimeout=5 bsdk8s@127.0.0.1 "$@"; }

start() {
  [ -f vm.pid ] && kill -0 "$(cat vm.pid)" 2>/dev/null && { echo "already running"; return; }
  command -v vde_switch >/dev/null || brew install vde
  { [ -f "$VDE_SOCK.pid" ] && kill -0 "$(cat "$VDE_SOCK.pid")" 2>/dev/null; } || { vde_switch -d -s "$VDE_SOCK" -p "$VDE_SOCK.pid" -M "$VDE_SOCK.mgmt" &&
    echo "started vde_switch $VDE_SOCK"; }
  qemu-system-aarch64 -name "$HOSTNAME_VM" -M virt,highmem=on -accel hvf -cpu host \
    -smp "$VM_CPUS" -m "$VM_MEM" \
    -drive if=pflash,format=raw,readonly=on,file="$FW" \
    -drive if=pflash,format=raw,file=vars.fd \
    -drive if=virtio,format=qcow2,file=disk.qcow2,cache=writeback,discard=unmap \
    -drive if=virtio,format=raw,readonly=on,file=seed.iso \
    -netdev user,id=n0,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22 \
    -device virtio-net-pci,netdev=n0,mac=$MAC0 \
    -netdev "vde,id=n1,sock=$VDE_SOCK" \
    -device virtio-net-pci,netdev=n1,mac=$MAC1 \
    -device virtio-rng-pci \
    -display none \
    -chardev socket,id=ser0,path=console.sock,server=on,wait=off,logfile=console.log,logappend=on -serial chardev:ser0 \
    -monitor unix:monitor.sock,server,nowait \
    -daemonize -pidfile vm.pid
  printf "booting"; i=0
  until ssh_vm true 2>/dev/null; do i=$((i+1)); [ $i -gt 120 ] && { echo " timeout; see $VM_DIR/console.log"; exit 1; }; printf .; sleep 3; done
  echo " up: ssh -i $KEY -p $SSH_PORT bsdk8s@127.0.0.1"
}

create() {
  command -v qemu-system-aarch64 >/dev/null || brew install qemu
  [ -f "$IMG" ] || { curl -fLO "$URL/$IMG" && curl -fLO "$URL/SHA512SUMS" &&
    [ "$(shasum -a 512 "$IMG" | cut -d' ' -f1)" = "$(awk -v f="$IMG" '$2 == f {print $1}' SHA512SUMS)" ] ||
    { echo "checksum mismatch for $IMG"; rm -f "$IMG"; exit 1; }; }
  [ -f disk.qcow2 ] || { cp "$IMG" disk.qcow2 && qemu-img resize -q disk.qcow2 "$VM_DISK"; }
  [ -f vars.fd ] || dd if=/dev/zero of=vars.fd bs=1m count=64 2>/dev/null
  rm -rf seed seed.iso; mkdir seed
  printf 'instance-id: %s\nlocal-hostname: %s\n' "$HOSTNAME_VM" "$HOSTNAME_VM" > seed/meta-data
  cat > seed/network-config <<EOF
version: 2
ethernets:
  uplink:
    match: {macaddress: "$MAC0"}
    set-name: uplink0
    dhcp4: true
  cluster:
    match: {macaddress: "$MAC1"}
    set-name: cluster0
    addresses: [192.168.100.$NODE/24, "fd00:100::$(printf %x "$NODE")/64"]
EOF
  cat > seed/user-data <<EOF
#cloud-config
users:
  - name: bsdk8s
    gecos: BSDKubernetes dev
    groups: [sudo]
    shell: /bin/bash
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    ssh_authorized_keys:
      - $(cat "$KEY.pub")
package_update: true
packages: [chrony, curl, gpg, ca-certificates, conntrack, socat, ethtool, iptables, tcpdump, jq]
runcmd:
  - "systemctl enable --now chrony"
EOF
  hdiutil makehybrid -quiet -iso -joliet -default-volume-name cidata -o seed.iso seed
  start
}

case "${1:-}" in
  create) create ;;
  start) start ;;
  stop) ssh_vm sudo poweroff || monitor system_powerdown || true; sleep 5
    [ -f vm.pid ] && kill -0 "$(cat vm.pid)" 2>/dev/null && { monitor quit 2>/dev/null || kill "$(cat vm.pid)"; }; rm -f vm.pid ;;
  monitor) shift; monitor "$*" ;;
  serial) echo "serial console (Ctrl-C to leave)"; nc -U console.sock ;;
  ssh) shift; ssh_vm "$@" ;;
  console) tail -f console.log ;;
  *) echo "usage: $0 create|start|stop|ssh [cmd]|console|serial|monitor <cmd>"; exit 1 ;;
esac
