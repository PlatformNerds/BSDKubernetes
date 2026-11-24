package main

import (
	"encoding/binary"
	"net"
	"net/netip"
)

// IPv6 Neighbor Discovery over Ethernet (RFC 4861): the Neighbor
// Solicitations the responder answers and the Neighbor Advertisements it
// sends, the IPv6 counterpart of ARP.

const (
	ethTypeIPv6     = 0x86dd
	ipProtoICMPv6   = 58
	icmp6NeighSol   = 135
	icmp6NeighAdv   = 136
	ndOptTgtLLAddr  = 2
	naFlagSolicited = 0x40
	naFlagOverride  = 0x20
	ethHdrLen       = 14
	ip6HdrLen       = 40
	nsLen           = 24 // type, code, checksum, reserved, target
	naLen           = 24 + 8
)

type neighborSolicitation struct {
	srcMAC net.HardwareAddr
	srcIP  netip.Addr // :: for duplicate address detection
	target netip.Addr
}

// parseNS decodes an Ethernet frame holding an ICMPv6 Neighbor Solicitation
// with no extension headers.
func parseNS(f []byte) (neighborSolicitation, bool) {
	if len(f) < ethHdrLen+ip6HdrLen+nsLen ||
		binary.BigEndian.Uint16(f[12:]) != ethTypeIPv6 ||
		f[ethHdrLen]>>4 != 6 ||
		f[ethHdrLen+6] != ipProtoICMPv6 ||
		f[ethHdrLen+7] != 255 || // hop limit: only from the link
		f[ethHdrLen+ip6HdrLen] != icmp6NeighSol || f[ethHdrLen+ip6HdrLen+1] != 0 {
		return neighborSolicitation{}, false
	}
	var ns neighborSolicitation
	ns.srcMAC = net.HardwareAddr(append([]byte(nil), f[6:12]...))
	ns.srcIP = netip.AddrFrom16([16]byte(f[ethHdrLen+8 : ethHdrLen+24]))
	ns.target = netip.AddrFrom16([16]byte(f[ethHdrLen+ip6HdrLen+8 : ethHdrLen+ip6HdrLen+24]))
	return ns, true
}

// solicitedNode returns the solicited-node multicast address of a
// (ff02::1:ffXX:XXXX), which Neighbor Solicitations for a are sent to.
func solicitedNode(a netip.Addr) netip.Addr {
	b := a.As16()
	return netip.AddrFrom16([16]byte{0xff, 0x02, 11: 1, 12: 0xff, 13: b[13], 14: b[14], 15: b[15]})
}

// multicastMAC returns the Ethernet address of an IPv6 multicast group.
func multicastMAC(a netip.Addr) net.HardwareAddr {
	b := a.As16()
	return net.HardwareAddr{0x33, 0x33, b[12], b[13], b[14], b[15]}
}

// neighborAdvert builds a Neighbor Advertisement for target at mac, sent to
// dst (dstMAC on the wire).
func neighborAdvert(mac net.HardwareAddr, target, dst netip.Addr, dstMAC net.HardwareAddr, flags byte) []byte {
	f := make([]byte, ethHdrLen+ip6HdrLen+naLen)
	copy(f[0:6], dstMAC)
	copy(f[6:12], mac)
	binary.BigEndian.PutUint16(f[12:], ethTypeIPv6)
	ip := f[ethHdrLen:]
	ip[0] = 0x60
	binary.BigEndian.PutUint16(ip[4:], naLen)
	ip[6] = ipProtoICMPv6
	ip[7] = 255
	src16, dst16 := target.As16(), dst.As16()
	copy(ip[8:24], src16[:])
	copy(ip[24:40], dst16[:])
	na := ip[ip6HdrLen:]
	na[0] = icmp6NeighAdv
	na[4] = flags
	copy(na[8:24], src16[:])
	na[24] = ndOptTgtLLAddr
	na[25] = 1 // length in units of 8 bytes
	copy(na[26:32], mac)
	binary.BigEndian.PutUint16(na[2:], icmp6Checksum(src16, dst16, na))
	return f
}

// ndpReply answers ns: its target is at mac. A solicitation from :: (duplicate
// address detection) is answered to all nodes, unsolicited.
func ndpReply(mac net.HardwareAddr, ns neighborSolicitation) []byte {
	if ns.srcIP.IsUnspecified() {
		return unsolicitedNA(mac, ns.target)
	}
	return neighborAdvert(mac, ns.target, ns.srcIP, ns.srcMAC, naFlagSolicited|naFlagOverride)
}

// unsolicitedNA announces to all nodes that target is at mac (the IPv6
// counterpart of a gratuitous ARP).
func unsolicitedNA(mac net.HardwareAddr, target netip.Addr) []byte {
	all := netip.MustParseAddr("ff02::1")
	return neighborAdvert(mac, target, all, multicastMAC(all), naFlagOverride)
}

// icmp6Checksum computes the ICMPv6 checksum of msg (whose checksum field
// is zero) over the IPv6 pseudo-header.
func icmp6Checksum(src, dst [16]byte, msg []byte) uint16 {
	var sum uint32
	add := func(b []byte) {
		for i := 0; i+1 < len(b); i += 2 {
			sum += uint32(binary.BigEndian.Uint16(b[i:]))
		}
		if len(b)%2 == 1 {
			sum += uint32(b[len(b)-1]) << 8
		}
	}
	add(src[:])
	add(dst[:])
	var l [8]byte
	binary.BigEndian.PutUint32(l[0:], uint32(len(msg)))
	l[7] = ipProtoICMPv6
	add(l[:])
	add(msg)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
