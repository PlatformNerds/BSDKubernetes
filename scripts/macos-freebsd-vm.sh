#!/bin/sh
# FreeBSD 15.1 arm64 dev VM on Apple Silicon (QEMU + HVF), provisioned via cloud-init (nuageinit).
#   scripts/macos-freebsd-vm.sh create   # download image, make disk + seed, first boot
#   scripts/macos-freebsd-vm.sh start|stop|ssh [cmd...]|console
#   scripts/macos-freebsd-vm.sh serial          # interactive serial console (works when ssh doesn't)
#   scripts/macos-freebsd-vm.sh monitor 'info status'   # QEMU monitor command, e.g.
#       'dump-guest-memory -z vmcore.elf' to capture a hung guest before resetting it
#   NODE=2 scripts/macos-freebsd-vm.sh create   # more nodes for a multi-node cluster
# Env: NODE (1) VM_DIR (~/bsdk8s/vm, ~/bsdk8s/vm-<NODE> for NODE>1) VM_CPUS (8) VM_MEM (12G)
#      VM_DISK (80G) SSH_PORT (2221+NODE) API_PORT (6443, node 1 only)
#
# Every node has a second NIC (vtnet1), 192.168.100.<NODE>/24: the cluster network.
# vtnet0 is per-VM user-mode NAT (internet, ssh). The cluster network is a vde virtual switch on
# this Mac (CLUSTER_NET=vde, the default; the script starts vde_switch on VDE_SOCK when needed), so
# any number of nodes share one L2 segment. CLUSTER_NET=dgram is the older point-to-point UDP link
# between node 1 and node 2 only. (A shared multicast "socket" network delivers nothing on macOS:
# multicast goes out en0 and is subject to the Local Network privacy permission.)
# Env: CLUSTER_NET (vde|dgram) VDE_SOCK (~/bsdk8s/vde.ctl)
set -eu
NODE=${NODE:-1}
if [ "$NODE" = 1 ]; then VM_DIR=${VM_DIR:-$HOME/bsdk8s/vm}; else VM_DIR=${VM_DIR:-$HOME/bsdk8s/vm-$NODE}; fi
VM_CPUS=${VM_CPUS:-8}; VM_MEM=${VM_MEM:-12G}; VM_DISK=${VM_DISK:-80G}
SSH_PORT=${SSH_PORT:-$((2221 + NODE))}; API_PORT=${API_PORT:-6443}
CLUSTER_NET=${CLUSTER_NET:-vde}; VDE_SOCK=${VDE_SOCK:-$HOME/bsdk8s/vde.ctl}
PEER=$((3 - NODE))   # CLUSTER_NET=dgram: 1 <-> 2
HOSTNAME_VM=bsdk8s; [ "$NODE" = 1 ] || HOSTNAME_VM=bsdk8s-$NODE
REL=15.1-RELEASE
IMG=FreeBSD-$REL-arm64-aarch64-BASIC-CLOUDINIT-zfs.qcow2
URL=https://download.freebsd.org/releases/VM-IMAGES/$REL/aarch64/Latest
FW=$(brew --prefix qemu)/share/qemu/edk2-aarch64-code.fd
KEY=${KEY:-$HOME/bsdk8s/vm/id_ed25519}   # one key for all nodes
mkdir -p "$VM_DIR"; cd "$VM_DIR"

# One QEMU monitor command; prints its output.
monitor() { [ -S monitor.sock ] || { echo "no monitor.sock (VM started by an older script?)"; return 1; }
  printf '%s\n' "$1" | nc -U -w 2 monitor.sock | tr -d '\r' | grep -v '^QEMU .*monitor\|^(qemu)'; }

ssh_vm() { ssh -q -i "$KEY" -p "$SSH_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -o ConnectTimeout=5 bsdk8s@127.0.0.1 "$@"; }

start() {
  [ -f vm.pid ] && kill -0 "$(cat vm.pid)" 2>/dev/null && { echo "already running"; return; }
  case $CLUSTER_NET in
  vde)
    command -v vde_switch >/dev/null || brew install vde
    if ! { [ -f "$VDE_SOCK.pid" ] && kill -0 "$(cat "$VDE_SOCK.pid")" 2>/dev/null; }; then
      vde_switch -d -s "$VDE_SOCK" -p "$VDE_SOCK.pid" -M "$VDE_SOCK.mgmt" && echo "started vde_switch $VDE_SOCK"
    fi
    clusternet="vde,id=n1,sock=$VDE_SOCK" ;;
  dgram)
    [ "$NODE" -le 2 ] || { echo "CLUSTER_NET=dgram links node 1 and node 2 only"; exit 1; }
    clusternet="dgram,id=n1,local.type=inet,local.host=127.0.0.1,local.port=$((20000 + NODE)),remote.type=inet,remote.host=127.0.0.1,remote.port=$((20000 + PEER))" ;;
  *) echo "CLUSTER_NET must be vde or dgram"; exit 1 ;;
  esac
  apifwd=""; [ "$NODE" = 1 ] && apifwd=",hostfwd=tcp:127.0.0.1:$API_PORT-:6443"
  qemu-system-aarch64 -name "$HOSTNAME_VM" -M virt,highmem=on -accel hvf -cpu host \
    -smp "$VM_CPUS" -m "$VM_MEM" \
    -drive if=pflash,format=raw,readonly=on,file="$FW" \
    -drive if=pflash,format=raw,file=vars.fd \
    -drive if=virtio,format=qcow2,file=disk.qcow2,cache=writeback,discard=unmap \
    -drive if=virtio,format=raw,readonly=on,file=seed.iso \
    -netdev user,id=n0,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22$apifwd \
    -device virtio-net-pci,netdev=n0,mac=52:54:00:bd:00:$(printf %02x "$NODE") \
    -netdev "$clusternet" \
    -device virtio-net-pci,netdev=n1,mac=52:54:00:bd:01:$(printf %02x "$NODE") \
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
  [ -f "$IMG" ] || { curl -fO "$URL/$IMG.xz" && curl -fO "$URL/CHECKSUM.SHA256" &&
    [ "$(shasum -a 256 "$IMG.xz" | cut -d' ' -f1)" = "$(grep "($IMG.xz)" CHECKSUM.SHA256 | awk '{print $NF}')" ] &&
    xz -dk "$IMG.xz"; }
  [ -f "$KEY" ] || ssh-keygen -q -t ed25519 -N '' -C bsdk8s-vm -f "$KEY"
  [ -f disk.qcow2 ] || { cp "$IMG" disk.qcow2 && qemu-img resize -q disk.qcow2 "$VM_DISK"; }
  [ -f vars.fd ] || { dd if=/dev/zero of=vars.fd bs=1m count=64 2>/dev/null; }
  rm -rf seed seed.iso; mkdir seed
  printf 'instance-id: bsdk8s-%s\nlocal-hostname: %s\n' "$NODE" "$HOSTNAME_VM" > seed/meta-data
  cat > seed/user-data <<EOF
#cloud-config
users:
  - name: bsdk8s
    gecos: BSDKubernetes dev
    groups: wheel
    shell: /bin/sh
    ssh_authorized_keys:
      - $(cat "$KEY.pub")
# Quote every runcmd item: an unquoted ": " makes YAML parse it as a mapping, and nuageinit then
# crashes ("attempt to concatenate a table value") and skips the whole runcmd stage.
runcmd:
  - "pkg install -y sudo"
  - "echo '%wheel ALL=(ALL:ALL) NOPASSWD: ALL' > /usr/local/etc/sudoers.d/wheel"
  - "grep -q kern.racct.enable /boot/loader.conf || echo 'kern.racct.enable=1' >> /boot/loader.conf"
  - "sysrc ifconfig_vtnet1='inet 192.168.100.$NODE/24' && service netif start vtnet1"
  - "sed -i '' -e '/^pool /s/iburst$/iburst maxpoll 6/' /etc/ntp.conf && echo 'tinker panic 0 stepout 30' | cat - /etc/ntp.conf > /tmp/ntp.conf && mv /tmp/ntp.conf /etc/ntp.conf"
  - "sysrc ntpd_enable=YES ntpd_sync_on_start=YES && service ntpd start"
EOF
  hdiutil makehybrid -quiet -iso -joliet -default-volume-name cidata -o seed.iso seed
  start
}

case "${1:-}" in
  create) create ;;
  start) start ;;
  stop) ssh_vm sudo shutdown -p now || monitor system_powerdown || true; sleep 5
    [ -f vm.pid ] && kill -0 "$(cat vm.pid)" 2>/dev/null && { monitor quit 2>/dev/null || kill "$(cat vm.pid)"; }; rm -f vm.pid ;;
  monitor) shift; monitor "$*" ;;
  serial) echo "serial console (Ctrl-C to leave)"; nc -U console.sock ;;
  ssh) shift; ssh_vm "$@" ;;
  console) tail -f console.log ;;
  *) echo "usage: $0 create|start|stop|ssh [cmd]|console|serial|monitor <cmd>"; exit 1 ;;
esac
