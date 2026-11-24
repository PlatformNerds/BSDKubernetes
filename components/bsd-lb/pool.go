package main

import (
	"fmt"
	"net/netip"
	"strings"
)

// Pool is a set of addresses the controller hands out to LoadBalancer
// Services, from ranges ("a-b") and prefixes ("a/n"), IPv4 and IPv6.
type Pool struct {
	ranges []addrRange
}

type addrRange struct{ first, last netip.Addr }

// ParsePool parses a comma-separated list of ranges and prefixes. For an
// IPv4 prefix shorter than /31 the network and broadcast addresses are left
// out; for an IPv6 prefix shorter than /127, the Subnet-Router anycast
// address (the first one).
func ParsePool(s string) (*Pool, error) {
	p := &Pool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var r addrRange
		if strings.Contains(part, "/") {
			pfx, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, err
			}
			pfx = pfx.Masked()
			r.first = pfx.Addr()
			r.last = lastAddr(pfx)
			if pfx.Addr().Is4() && pfx.Bits() < 31 {
				r.first = r.first.Next()
				r.last = r.last.Prev()
			}
			if pfx.Addr().Is6() && pfx.Bits() < 127 {
				r.first = r.first.Next()
			}
		} else {
			a, b, ok := strings.Cut(part, "-")
			if !ok {
				return nil, fmt.Errorf("pool %q: want a range a-b or a prefix a/n", part)
			}
			var err error
			if r.first, err = netip.ParseAddr(strings.TrimSpace(a)); err != nil {
				return nil, err
			}
			if r.last, err = netip.ParseAddr(strings.TrimSpace(b)); err != nil {
				return nil, err
			}
			if r.first.Is4() != r.last.Is4() {
				return nil, fmt.Errorf("pool %q: mixes IPv4 and IPv6", part)
			}
		}
		if r.first.Zone() != "" || r.last.Zone() != "" || r.first.Is4In6() || r.last.Is4In6() {
			return nil, fmt.Errorf("pool %q: no zones or IPv4-mapped addresses", part)
		}
		if r.last.Less(r.first) {
			return nil, fmt.Errorf("pool %q: empty range", part)
		}
		p.ranges = append(p.ranges, r)
	}
	if len(p.ranges) == 0 {
		return nil, fmt.Errorf("empty pool")
	}
	return p, nil
}

// lastAddr returns the last address of a prefix (of either family).
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// Contains reports whether a is in the pool.
func (p *Pool) Contains(a netip.Addr) bool {
	for _, r := range p.ranges {
		if a.Is4() == r.first.Is4() && !a.Less(r.first) && !r.last.Less(a) {
			return true
		}
	}
	return false
}

// Has reports whether the pool has addresses of the family (IPv6 if v6).
func (p *Pool) Has(v6 bool) bool {
	for _, r := range p.ranges {
		if r.first.Is6() == v6 {
			return true
		}
	}
	return false
}

// Allocate returns the lowest pool address of the family (IPv6 if v6) not in
// used.
func (p *Pool) Allocate(used map[netip.Addr]bool, v6 bool) (netip.Addr, bool) {
	for _, r := range p.ranges {
		if r.first.Is6() != v6 {
			continue
		}
		for a := r.first; ; a = a.Next() {
			if !used[a] {
				return a, true
			}
			if a == r.last {
				break
			}
		}
	}
	return netip.Addr{}, false
}

func (p *Pool) String() string {
	var s []string
	for _, r := range p.ranges {
		s = append(s, r.first.String()+"-"+r.last.String())
	}
	return strings.Join(s, ",")
}
