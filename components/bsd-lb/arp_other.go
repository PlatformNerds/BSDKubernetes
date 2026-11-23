//go:build !freebsd

package main

import (
	"context"
	"fmt"
	"net"
)

func newARPResponder(_ context.Context, iface *net.Interface) (announcer, error) {
	return nil, fmt.Errorf("the speaker answers ARP through bpf(4) on FreeBSD only")
}
