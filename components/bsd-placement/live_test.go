package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// BSD_PLACEMENT_LIVE=1 go test -run TestLiveRegistries -v
func TestLiveRegistries(t *testing.T) {
	if os.Getenv("BSD_PLACEMENT_LIVE") == "" {
		t.Skip("set BSD_PLACEMENT_LIVE=1 to query public registries")
	}
	rc := newRegistryClient(time.Minute)
	for _, img := range []string{
		"nginx:1.27-alpine",
		"busybox:1.36",
		"quay.io/jetstack/cert-manager-controller:v1.16.2",
		"registry.k8s.io/metrics-server/metrics-server:v0.7.2",
		"registry.k8s.io/e2e-test-images/agnhost:2.63.0",
		"ghcr.io/freebsd/freebsd-runtime:15.0",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		start := time.Now()
		oses, err := rc.platforms(ctx, img)
		cancel()
		var l []string
		for k := range oses {
			l = append(l, k)
		}
		t.Logf("%-55s %-20s %v err=%v", img, strings.Join(l, ","), time.Since(start).Round(time.Millisecond), err)
	}
}
