//go:build freebsd

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// bpfDirIn is BPF_D_IN from <net/bpf.h>: see only packets the interface
// receives, not the replies this process sends.
const bpfDirIn = 0

// arpResponder answers ARP requests for the addresses it holds, through
// bpf(4) on one interface. The addresses are not configured on the node:
// pf's rdr rules (kube-proxy) take the traffic that arrives for them.
type arpResponder struct {
	fd    int
	iface *net.Interface
	mu    sync.Mutex
	addrs map[[4]byte]bool
}

type ifreq struct {
	name [unix.IFNAMSIZ]byte
	_    [16]byte
}

func ioctlPtr(fd int, req uint, arg unsafe.Pointer) error {
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

func newARPResponder(ctx context.Context, iface *net.Interface) (announcer, error) {
	if len(iface.HardwareAddr) != 6 {
		return nil, fmt.Errorf("interface %s has no Ethernet address", iface.Name)
	}
	fd, err := unix.Open("/dev/bpf", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/bpf (the pod needs NET_RAW): %w", err)
	}
	var ifr ifreq
	copy(ifr.name[:], iface.Name)
	if err := ioctlPtr(fd, unix.BIOCSETIF, unsafe.Pointer(&ifr)); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("BIOCSETIF %s: %w", iface.Name, err)
	}
	one := uint32(1)
	for _, req := range []uint{unix.BIOCIMMEDIATE, unix.BIOCSHDRCMPLT} {
		if err := ioctlPtr(fd, req, unsafe.Pointer(&one)); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("bpf ioctl %#x: %w", req, err)
		}
	}
	dir := uint32(bpfDirIn)
	if err := ioctlPtr(fd, unix.BIOCSDIRECTION, unsafe.Pointer(&dir)); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("BIOCSDIRECTION: %w", err)
	}
	// ARP requests only: ethertype 0x0806 at 12, opcode 1 at 20.
	insns := []unix.BpfInsn{
		{Code: unix.BPF_LD | unix.BPF_H | unix.BPF_ABS, K: 12},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 3, K: 0x0806},
		{Code: unix.BPF_LD | unix.BPF_H | unix.BPF_ABS, K: 20},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: 0xffff},
		{Code: unix.BPF_RET | unix.BPF_K, K: 0},
	}
	prog := unix.BpfProgram{Len: uint32(len(insns)), Insns: &insns[0]}
	if err := ioctlPtr(fd, unix.BIOCSETF, unsafe.Pointer(&prog)); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("BIOCSETF: %w", err)
	}
	var blen uint32
	if err := ioctlPtr(fd, unix.BIOCGBLEN, unsafe.Pointer(&blen)); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("BIOCGBLEN: %w", err)
	}

	r := &arpResponder{fd: fd, iface: iface, addrs: map[[4]byte]bool{}}
	go r.readLoop(ctx, int(blen))
	return r, nil
}

// SetAddrs replaces the announced addresses and sends gratuitous ARP for the
// new ones, so neighbours that cached another node's address move over.
func (r *arpResponder) SetAddrs(addrs []netip.Addr) {
	next := map[[4]byte]bool{}
	var added [][4]byte
	r.mu.Lock()
	for _, a := range addrs {
		b := a.As4()
		next[b] = true
		if !r.addrs[b] {
			added = append(added, b)
		}
	}
	r.addrs = next
	r.mu.Unlock()
	for _, b := range added {
		go func(b [4]byte) {
			for i := 0; i < 3; i++ {
				r.mu.Lock()
				still := r.addrs[b]
				r.mu.Unlock()
				if !still {
					return
				}
				if err := r.send(gratuitousARP(r.iface.HardwareAddr, b)); err != nil {
					log.Printf("speaker: gratuitous ARP for %v: %v", netip.AddrFrom4(b), err)
				}
				time.Sleep(time.Second)
			}
		}(b)
	}
}

func (r *arpResponder) send(frame []byte) error {
	_, err := unix.Write(r.fd, frame)
	return err
}

func bpfWordAlign(x int) int {
	const a = int(unsafe.Sizeof(uintptr(0)))
	return (x + a - 1) &^ (a - 1)
}

func (r *arpResponder) readLoop(ctx context.Context, blen int) {
	buf := make([]byte, blen)
	for ctx.Err() == nil {
		n, err := unix.Read(r.fd, buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			log.Fatalf("speaker: bpf read: %v", err)
		}
		for i := 0; i+int(unsafe.Sizeof(unix.BpfHdr{})) <= n; {
			h := (*unix.BpfHdr)(unsafe.Pointer(&buf[i]))
			start := i + int(h.Hdrlen)
			end := start + int(h.Caplen)
			if end > n {
				break
			}
			r.handle(buf[start:end])
			i += bpfWordAlign(int(h.Hdrlen) + int(h.Caplen))
		}
	}
}

func (r *arpResponder) handle(frame []byte) {
	req, ok := parseARPRequest(frame)
	if !ok {
		return
	}
	r.mu.Lock()
	mine := r.addrs[req.targetIP]
	r.mu.Unlock()
	if !mine {
		return
	}
	if err := r.send(arpReply(r.iface.HardwareAddr, req)); err != nil {
		log.Printf("speaker: ARP reply for %v: %v", netip.AddrFrom4(req.targetIP), err)
	}
}
