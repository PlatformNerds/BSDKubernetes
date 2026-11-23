package main

import (
	"fmt"
	"net/netip"
	"strings"
)

// Pool is a set of IPv4 addresses the controller hands out to LoadBalancer
// Services, from ranges ("a.b.c.d-a.b.c.e") and prefixes ("a.b.c.d/n").
type Pool struct {
	ranges []addrRange
}

type addrRange struct{ first, last netip.Addr }

// ParsePool parses a comma-separated list of ranges and prefixes. For a
// prefix shorter than /31 the network and broadcast addresses are left out.
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
			if !pfx.Addr().Is4() {
				return nil, fmt.Errorf("pool %q: only IPv4 is supported", part)
			}
			pfx = pfx.Masked()
			r.first = pfx.Addr()
			r.last = lastAddr(pfx)
			if pfx.Bits() < 31 {
				r.first = r.first.Next()
				r.last = r.last.Prev()
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
		}
		if !r.first.Is4() || !r.last.Is4() {
			return nil, fmt.Errorf("pool %q: only IPv4 is supported", part)
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

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	host := 32 - p.Bits()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	if host > 0 {
		v |= (uint32(1) << host) - 1
	}
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// Contains reports whether a is in the pool.
func (p *Pool) Contains(a netip.Addr) bool {
	for _, r := range p.ranges {
		if !a.Less(r.first) && !r.last.Less(a) {
			return true
		}
	}
	return false
}

// Allocate returns the lowest pool address not in used.
func (p *Pool) Allocate(used map[netip.Addr]bool) (netip.Addr, bool) {
	for _, r := range p.ranges {
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
