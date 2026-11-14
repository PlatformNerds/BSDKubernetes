#!/bin/sh
# FreeBSD 15.1 arm64 dev VM on Apple Silicon (QEMU + HVF), provisioned via cloud-init (nuageinit).
#   scripts/macos-freebsd-vm.sh create   # download image, make disk + seed, first boot
#   scripts/macos-freebsd-vm.sh start|stop|ssh [cmd...]|console
# Env: VM_DIR (~/bsdk8s/vm) VM_CPUS (8) VM_MEM (12G) VM_DISK (80G) SSH_PORT (2222) API_PORT (6443)
set -eu
VM_DIR=${VM_DIR:-$HOME/bsdk8s/vm}
VM_CPUS=${VM_CPUS:-8}; VM_MEM=${VM_MEM:-12G}; VM_DISK=${VM_DISK:-80G}
SSH_PORT=${SSH_PORT:-2222}; API_PORT=${API_PORT:-6443}
REL=15.1-RELEASE
IMG=FreeBSD-$REL-arm64-aarch64-BASIC-CLOUDINIT-zfs.qcow2
URL=https://download.freebsd.org/releases/VM-IMAGES/$REL/aarch64/Latest
FW=$(brew --prefix qemu)/share/qemu/edk2-aarch64-code.fd
KEY=$VM_DIR/id_ed25519
mkdir -p "$VM_DIR"; cd "$VM_DIR"

ssh_vm() { ssh -q -i "$KEY" -p "$SSH_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -o ConnectTimeout=5 bsdk8s@127.0.0.1 "$@"; }

start() {
  [ -f vm.pid ] && kill -0 "$(cat vm.pid)" 2>/dev/null && { echo "already running"; return; }
  qemu-system-aarch64 -name bsdk8s -M virt,highmem=on -accel hvf -cpu host \
    -smp "$VM_CPUS" -m "$VM_MEM" \
    -drive if=pflash,format=raw,readonly=on,file="$FW" \
    -drive if=pflash,format=raw,file=vars.fd \
    -drive if=virtio,format=qcow2,file=disk.qcow2,cache=writeback,discard=unmap \
    -drive if=virtio,format=raw,readonly=on,file=seed.iso \
    -netdev user,id=n0,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22,hostfwd=tcp:127.0.0.1:$API_PORT-:6443 \
    -device virtio-net-pci,netdev=n0 -device virtio-rng-pci \
    -display none -serial file:console.log -monitor none \
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
  printf 'instance-id: bsdk8s-1\nlocal-hostname: bsdk8s\n' > seed/meta-data
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
EOF
  hdiutil makehybrid -quiet -iso -joliet -default-volume-name cidata -o seed.iso seed
  start
}

case "${1:-}" in
  create) create ;;
  start) start ;;
  stop) ssh_vm sudo shutdown -p now || true; sleep 5; [ -f vm.pid ] && kill "$(cat vm.pid)" 2>/dev/null; rm -f vm.pid ;;
  ssh) shift; ssh_vm "$@" ;;
  console) tail -f console.log ;;
  *) echo "usage: $0 create|start|stop|ssh [cmd]|console"; exit 1 ;;
esac
