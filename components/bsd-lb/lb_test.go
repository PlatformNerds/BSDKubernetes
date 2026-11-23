package main

import (
	"bytes"
	"net"
	"net/netip"
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
	for _, bad := range []string{"", "1.2.3.4", "1.2.3.9-1.2.3.4", "fd00::/120", "x/24"} {
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
		a, ok := p.Allocate(used)
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
	if a, _ := p.Allocate(used); a.String() != "192.168.100.201" {
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
