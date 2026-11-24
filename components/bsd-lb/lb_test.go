package main

import (
	"bytes"
	"net"
	"net/netip"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestParsePool(t *testing.T) {
	p, err := ParsePool("192.168.100.200-192.168.100.202, 10.0.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	for s, want := range map[string]bool{
		"192.168.100.199": false, "192.168.100.200": true, "192.168.100.202": true, "192.168.100.203": false,
		"10.0.0.0": false, "10.0.0.1": true, "10.0.0.2": true, "10.0.0.3": false,
	} {
		if got := p.Contains(netip.MustParseAddr(s)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", s, got, want)
		}
	}
	for _, bad := range []string{"", "1.2.3.4", "1.2.3.9-1.2.3.4", "10.0.0.1-fd00::1", "fe80::1%vtnet0-fe80::9%vtnet0", "x/24"} {
		if _, err := ParsePool(bad); err == nil {
			t.Errorf("ParsePool(%q) accepted", bad)
		}
	}
	// /31 and /32 keep every address.
	p, _ = ParsePool("10.1.0.0/31")
	if !p.Contains(netip.MustParseAddr("10.1.0.0")) || !p.Contains(netip.MustParseAddr("10.1.0.1")) {
		t.Error("/31 lost an address")
	}
}

func TestAllocate(t *testing.T) {
	p, _ := ParsePool("192.168.100.200-192.168.100.201,10.0.0.8/30")
	used := map[netip.Addr]bool{}
	var got []string
	for {
		a, ok := p.Allocate(used, false)
		if !ok {
			break
		}
		used[a] = true
		got = append(got, a.String())
	}
	want := []string{"192.168.100.200", "192.168.100.201", "10.0.0.9", "10.0.0.10"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	delete(used, netip.MustParseAddr("192.168.100.201"))
	if a, _ := p.Allocate(used, false); a.String() != "192.168.100.201" {
		t.Errorf("freed address not reused: %v", a)
	}
}

func TestWinner(t *testing.T) {
	ip := netip.MustParseAddr("192.168.100.200")
	nodes := []string{"a", "b", "c"}
	w := winner(ip, electionInput{speakers: nodes})
	if w == "" {
		t.Fatal("no winner")
	}
	// Same answer whatever the order.
	if w2 := winner(ip, electionInput{speakers: []string{"c", "a", "b"}}); w2 != w {
		t.Errorf("order changed the winner: %s vs %s", w, w2)
	}
	// Local policy: only nodes with endpoints.
	if w := winner(ip, electionInput{speakers: nodes, local: map[string]bool{"b": true}}); w != "b" {
		t.Errorf("local winner %q, want b", w)
	}
	if w := winner(ip, electionInput{speakers: nodes, local: map[string]bool{}}); w != "" {
		t.Errorf("no local endpoints but winner %q", w)
	}
	// Addresses spread over nodes.
	seen := map[string]bool{}
	for i := 1; i < 50; i++ {
		seen[winner(netip.AddrFrom4([4]byte{10, 0, 0, byte(i)}), electionInput{speakers: nodes})] = true
	}
	if len(seen) != 3 {
		t.Errorf("50 addresses went to %d of 3 nodes", len(seen))
	}
}

func TestARP(t *testing.T) {
	mac := net.HardwareAddr{2, 0, 0, 0, 0, 1}
	peer := net.HardwareAddr{2, 0, 0, 0, 0, 2}
	vip := [4]byte{192, 168, 100, 200}
	peerIP := [4]byte{192, 168, 100, 1}
	bcast := net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	reqFrame := arpFrame(bcast, peer, 1, peer, peerIP, make(net.HardwareAddr, 6), vip)
	req, ok := parseARPRequest(reqFrame)
	if !ok || req.targetIP != vip || req.senderIP != peerIP || !bytes.Equal(req.senderMAC, peer) {
		t.Fatalf("parse: %v %+v", ok, req)
	}
	rep := arpReply(mac, req)
	if !bytes.Equal(rep[0:6], peer) || !bytes.Equal(rep[6:12], mac) || rep[21] != 2 ||
		!bytes.Equal(rep[22:28], mac) || !bytes.Equal(rep[28:32], vip[:]) ||
		!bytes.Equal(rep[32:38], peer) || !bytes.Equal(rep[38:42], peerIP[:]) {
		t.Fatalf("bad reply % x", rep)
	}
	if _, ok := parseARPRequest(rep); ok {
		t.Error("a reply parsed as a request")
	}
	g := gratuitousARP(mac, vip)
	if gr, ok := parseARPRequest(g); !ok || gr.targetIP != vip || gr.senderIP != vip {
		t.Errorf("gratuitous ARP: %v %+v", ok, gr)
	}
}

func TestOwnership(t *testing.T) {
	lb := &v1.Service{Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer}}
	if !wantsLB(lb) {
		t.Error("classless LoadBalancer not handled")
	}
	lb.Spec.LoadBalancerClass = ptr.To(lbClass)
	if !wantsLB(lb) {
		t.Error("own class not handled")
	}
	lb.Spec.LoadBalancerClass = ptr.To("example.com/other")
	if wantsLB(lb) || ours(lb) {
		t.Error("other class handled")
	}
	cip := &v1.Service{Spec: v1.ServiceSpec{Type: v1.ServiceTypeClusterIP}}
	if wantsLB(cip) || !ours(cip) {
		t.Error("ClusterIP: wantsLB or not ours (release must run after a type change)")
	}
}

func TestPoolIPv6(t *testing.T) {
	p, err := ParsePool("192.168.100.200-192.168.100.201, fd00:100::200/126")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Has(false) || !p.Has(true) {
		t.Fatal("families missing")
	}
	// The /126 leaves out its Subnet-Router anycast address (::200).
	for s, want := range map[string]bool{
		"fd00:100::200": false, "fd00:100::201": true, "fd00:100::203": true, "fd00:100::204": false,
		"192.168.100.200": true,
	} {
		if got := p.Contains(netip.MustParseAddr(s)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", s, got, want)
		}
	}
	used := map[netip.Addr]bool{}
	var got []string
	for {
		a, ok := p.Allocate(used, true)
		if !ok {
			break
		}
		used[a] = true
		got = append(got, a.String())
	}
	if strings.Join(got, ",") != "fd00:100::201,fd00:100::202,fd00:100::203" {
		t.Errorf("IPv6 allocations %v", got)
	}
	if v4, _ := ParsePool("10.0.0.0/30"); v4.Has(true) {
		t.Error("IPv4 pool claims IPv6")
	}
}

func TestPickAddrs(t *testing.T) {
	pool, _ := ParsePool("192.168.100.200-192.168.100.201,fd00:100::200/126")
	dual := &v1.Service{Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer,
		IPFamilies: []v1.IPFamily{v1.IPv6Protocol, v1.IPv4Protocol}}}
	addrs, problems := pickAddrs(pool, dual, map[netip.Addr]bool{})
	if len(problems) != 0 || len(addrs) != 2 || addrs[0].String() != "fd00:100::201" || addrs[1].String() != "192.168.100.200" {
		t.Fatalf("dual: %v %v", addrs, problems)
	}
	// An address the Service has stays; a used one doesn't.
	dual.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: "192.168.100.201"}, {IP: "fd00:100::202"}}
	addrs, _ = pickAddrs(pool, dual, map[netip.Addr]bool{netip.MustParseAddr("fd00:100::202"): true})
	if addrs[0].String() != "fd00:100::201" || addrs[1].String() != "192.168.100.201" {
		t.Errorf("keep: %v", addrs)
	}
	// A family the pool lacks is reported, the other one served.
	v4pool, _ := ParsePool("10.0.0.1-10.0.0.2")
	addrs, problems = pickAddrs(v4pool, dual, map[netip.Addr]bool{})
	if len(addrs) != 1 || !addrs[0].Is4() || len(problems) != 1 {
		t.Errorf("v4-only pool: %v %v", addrs, problems)
	}
	// No families listed: IPv4.
	plain := &v1.Service{Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer}}
	if addrs, _ := pickAddrs(pool, plain, map[netip.Addr]bool{}); len(addrs) != 1 || !addrs[0].Is4() {
		t.Errorf("plain: %v", addrs)
	}
}

func TestNDP(t *testing.T) {
	mac := net.HardwareAddr{2, 0, 0, 0, 0, 1}
	peer := net.HardwareAddr{2, 0, 0, 0, 0, 2}
	vip := netip.MustParseAddr("fd00:100::201")
	peerIP := netip.MustParseAddr("fd00:100::1")
	if g := solicitedNode(vip).String(); g != "ff02::1:ff00:201" {
		t.Errorf("solicited-node %s", g)
	}
	// A solicitation as a neighbour would send it: built with neighborAdvert's
	// layout, then turned into an NS for vip.
	ns := neighborAdvert(peer, peerIP, solicitedNode(vip), multicastMAC(solicitedNode(vip)), 0)
	ns[ethHdrLen+ip6HdrLen] = icmp6NeighSol
	copy(ns[ethHdrLen+ip6HdrLen+8:], vip.AsSlice())
	req, ok := parseNS(ns)
	if !ok || req.target != vip || req.srcIP != peerIP || !bytes.Equal(req.srcMAC, peer) {
		t.Fatalf("parse: %v %+v", ok, req)
	}
	na := ndpReply(mac, req)
	if _, ok := parseNS(na); ok {
		t.Error("an advertisement parsed as a solicitation")
	}
	if !bytes.Equal(na[0:6], peer) || !bytes.Equal(na[6:12], mac) {
		t.Errorf("ethernet % x", na[:12])
	}
	ip := na[ethHdrLen:]
	if !bytes.Equal(ip[8:24], vip.AsSlice()) || !bytes.Equal(ip[24:40], peerIP.AsSlice()) || ip[7] != 255 {
		t.Errorf("ip header % x", ip[:40])
	}
	msg := ip[ip6HdrLen:]
	if msg[0] != icmp6NeighAdv || msg[4] != naFlagSolicited|naFlagOverride ||
		!bytes.Equal(msg[8:24], vip.AsSlice()) || msg[24] != ndOptTgtLLAddr || !bytes.Equal(msg[26:32], mac) {
		t.Errorf("advertisement % x", msg)
	}
	// The checksum verifies: summing the message with it in place gives 0.
	if c := icmp6Checksum([16]byte(ip[8:24]), [16]byte(ip[24:40]), msg); c != 0 {
		t.Errorf("checksum does not verify: %#x", c)
	}
	// Duplicate address detection (from ::) is answered to all nodes.
	req.srcIP = netip.IPv6Unspecified()
	if u := ndpReply(mac, req); !bytes.Equal(u[0:6], multicastMAC(netip.MustParseAddr("ff02::1"))) ||
		u[ethHdrLen+ip6HdrLen+4] != naFlagOverride {
		t.Errorf("DAD reply % x", u[:60])
	}
}
