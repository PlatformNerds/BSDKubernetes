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

	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// bpfDirIn is BPF_D_IN from <net/bpf.h>: see only packets the interface
// receives, not the replies this process sends.
const bpfDirIn = 0

// responder answers ARP requests (IPv4) and Neighbor Solicitations (IPv6)
// for the addresses it holds, through bpf(4) on one interface. The addresses
// are not configured on the node: pf's rdr rules (kube-proxy) take the
// traffic that arrives for them.
type responder struct {
	fd    int
	iface *net.Interface
	// mcast joins the solicited-node multicast groups of the IPv6
	// addresses, so the interface passes their solicitations up.
	mcast *ipv6.PacketConn
	mu    sync.Mutex
	addrs map[netip.Addr]bool
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

// filterInsns accepts ARP requests and ICMPv6 Neighbor Solicitations (IPv6
// with no extension headers); jumps are relative to the next instruction.
var filterInsns = []unix.BpfInsn{
	/* 0 */ {Code: unix.BPF_LD | unix.BPF_H | unix.BPF_ABS, K: 12}, // ethertype
	/* 1 */ {Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 2, K: 0x0806},
	/* 2 */ {Code: unix.BPF_LD | unix.BPF_H | unix.BPF_ABS, K: 20}, // ARP op
	/* 3 */ {Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 5, Jf: 6, K: 1},
	/* 4 */ {Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 5, K: ethTypeIPv6},
	/* 5 */ {Code: unix.BPF_LD | unix.BPF_B | unix.BPF_ABS, K: ethHdrLen + 6}, // next header
	/* 6 */ {Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 3, K: ipProtoICMPv6},
	/* 7 */ {Code: unix.BPF_LD | unix.BPF_B | unix.BPF_ABS, K: ethHdrLen + ip6HdrLen}, // ICMPv6 type
	/* 8 */ {Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: icmp6NeighSol},
	/* 9 */ {Code: unix.BPF_RET | unix.BPF_K, K: 0xffff},
	/* 10 */ {Code: unix.BPF_RET | unix.BPF_K, K: 0},
}

func newResponder(ctx context.Context, iface *net.Interface) (announcer, error) {
	if len(iface.HardwareAddr) != 6 {
		return nil, fmt.Errorf("interface %s has no Ethernet address", iface.Name)
	}
	fd, err := unix.Open("/dev/bpf", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/bpf (the pod needs NET_RAW): %w", err)
	}
	fail := func(what string, err error) (announcer, error) {
		unix.Close(fd)
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	var ifr ifreq
	copy(ifr.name[:], iface.Name)
	if err := ioctlPtr(fd, unix.BIOCSETIF, unsafe.Pointer(&ifr)); err != nil {
		return fail("BIOCSETIF "+iface.Name, err)
	}
	one := uint32(1)
	for _, req := range []uint{unix.BIOCIMMEDIATE, unix.BIOCSHDRCMPLT} {
		if err := ioctlPtr(fd, req, unsafe.Pointer(&one)); err != nil {
			return fail(fmt.Sprintf("bpf ioctl %#x", req), err)
		}
	}
	dir := uint32(bpfDirIn)
	if err := ioctlPtr(fd, unix.BIOCSDIRECTION, unsafe.Pointer(&dir)); err != nil {
		return fail("BIOCSDIRECTION", err)
	}
	prog := unix.BpfProgram{Len: uint32(len(filterInsns)), Insns: &filterInsns[0]}
	if err := ioctlPtr(fd, unix.BIOCSETF, unsafe.Pointer(&prog)); err != nil {
		return fail("BIOCSETF", err)
	}
	var blen uint32
	if err := ioctlPtr(fd, unix.BIOCGBLEN, unsafe.Pointer(&blen)); err != nil {
		return fail("BIOCGBLEN", err)
	}

	r := &responder{fd: fd, iface: iface, addrs: map[netip.Addr]bool{}}
	// IPv6 multicast membership, for solicitations sent to a solicited-node
	// group; without IPv6 on the node, IPv6 addresses just aren't announced.
	if c, err := net.ListenPacket("udp6", "[::]:0"); err == nil {
		r.mcast = ipv6.NewPacketConn(c)
	} else {
		log.Printf("speaker: no IPv6 socket, IPv6 addresses won't be announced: %v", err)
	}
	go r.readLoop(ctx, int(blen))
	return r, nil
}

// SetAddrs replaces the announced addresses. For new ones it sends
// gratuitous ARP or unsolicited Neighbor Advertisements, so neighbours that
// cached another node's address move over.
func (r *responder) SetAddrs(addrs []netip.Addr) {
	next := map[netip.Addr]bool{}
	var added, removed []netip.Addr
	r.mu.Lock()
	for _, a := range addrs {
		next[a] = true
		if !r.addrs[a] {
			added = append(added, a)
		}
	}
	for a := range r.addrs {
		if !next[a] {
			removed = append(removed, a)
		}
	}
	r.addrs = next
	r.mu.Unlock()

	for _, a := range removed {
		if a.Is6() && r.mcast != nil {
			r.mcast.LeaveGroup(r.iface, &net.UDPAddr{IP: solicitedNode(a).AsSlice()}) //nolint:errcheck
		}
	}
	for _, a := range added {
		if a.Is6() {
			if r.mcast == nil {
				continue
			}
			if err := r.mcast.JoinGroup(r.iface, &net.UDPAddr{IP: solicitedNode(a).AsSlice()}); err != nil {
				log.Printf("speaker: join %v for %v: %v", solicitedNode(a), a, err)
			}
		}
		go r.announce(a)
	}
}

// announce tells the link that a is here, three times a second apart, while
// it is still held.
func (r *responder) announce(a netip.Addr) {
	for i := 0; i < 3; i++ {
		if !r.holds(a) {
			return
		}
		var frame []byte
		if a.Is4() {
			frame = gratuitousARP(r.iface.HardwareAddr, a.As4())
		} else {
			frame = unsolicitedNA(r.iface.HardwareAddr, a)
		}
		if err := r.send(frame); err != nil {
			log.Printf("speaker: announcing %v: %v", a, err)
		}
		time.Sleep(time.Second)
	}
}

func (r *responder) send(frame []byte) error {
	_, err := unix.Write(r.fd, frame)
	return err
}

func (r *responder) holds(a netip.Addr) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addrs[a]
}

func bpfWordAlign(x int) int {
	const a = int(unsafe.Sizeof(uintptr(0)))
	return (x + a - 1) &^ (a - 1)
}

func (r *responder) readLoop(ctx context.Context, blen int) {
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

func (r *responder) handle(frame []byte) {
	var reply []byte
	var target netip.Addr
	if req, ok := parseARPRequest(frame); ok {
		target = netip.AddrFrom4(req.targetIP)
		if r.holds(target) {
			reply = arpReply(r.iface.HardwareAddr, req)
		}
	} else if ns, ok := parseNS(frame); ok {
		target = ns.target
		if r.holds(target) {
			reply = ndpReply(r.iface.HardwareAddr, ns)
		}
	}
	if reply == nil {
		return
	}
	if err := r.send(reply); err != nil {
		log.Printf("speaker: answering for %v: %v", target, err)
	}
}
