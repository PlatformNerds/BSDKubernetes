package main

import (
	"encoding/binary"
	"net"
)

// ARP over Ethernet for IPv4 (RFC 826), the frames the responder reads and
// writes.

type arpRequest struct {
	senderMAC net.HardwareAddr
	senderIP  [4]byte
	targetIP  [4]byte
}

const arpFrameLen = 14 + 28

// parseARPRequest decodes an Ethernet frame holding an ARP request for an
// IPv4 address.
func parseARPRequest(f []byte) (arpRequest, bool) {
	if len(f) < arpFrameLen ||
		binary.BigEndian.Uint16(f[12:]) != 0x0806 || // ARP
		binary.BigEndian.Uint16(f[14:]) != 1 || // Ethernet
		binary.BigEndian.Uint16(f[16:]) != 0x0800 || // IPv4
		f[18] != 6 || f[19] != 4 ||
		binary.BigEndian.Uint16(f[20:]) != 1 { // request
		return arpRequest{}, false
	}
	var r arpRequest
	r.senderMAC = net.HardwareAddr(append([]byte(nil), f[22:28]...))
	copy(r.senderIP[:], f[28:32])
	copy(r.targetIP[:], f[38:42])
	return r, true
}

func arpFrame(dst, src net.HardwareAddr, op uint16, sha net.HardwareAddr, spa [4]byte, tha net.HardwareAddr, tpa [4]byte) []byte {
	f := make([]byte, arpFrameLen)
	copy(f[0:6], dst)
	copy(f[6:12], src)
	binary.BigEndian.PutUint16(f[12:], 0x0806)
	binary.BigEndian.PutUint16(f[14:], 1)
	binary.BigEndian.PutUint16(f[16:], 0x0800)
	f[18], f[19] = 6, 4
	binary.BigEndian.PutUint16(f[20:], op)
	copy(f[22:28], sha)
	copy(f[28:32], spa[:])
	copy(f[32:38], tha)
	copy(f[38:42], tpa[:])
	return f
}

// arpReply answers req: target IP is at mac.
func arpReply(mac net.HardwareAddr, req arpRequest) []byte {
	return arpFrame(req.senderMAC, mac, 2, mac, req.targetIP, req.senderMAC, req.senderIP)
}

// gratuitousARP announces that ip is at mac (a broadcast request for itself).
func gratuitousARP(mac net.HardwareAddr, ip [4]byte) []byte {
	bcast := net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	return arpFrame(bcast, mac, 1, mac, ip, make(net.HardwareAddr, 6), ip)
}
