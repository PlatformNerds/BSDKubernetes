package main

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"slices"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

const (
	// cleanupFinalizer is the finalizer the cloud service controller puts on
	// LoadBalancer Services; the e2e tests check for it.
	cleanupFinalizer = "service.kubernetes.io/load-balancer-cleanup"
	// lbClass selects this implementation explicitly; Services without a
	// class are handled too, as by a cloud provider.
	lbClass = "bsdk8s.io/bsd-lb"
)

// ours reports whether this implementation owns the Service's load balancer
// (or did, before its type changed: a class can only be set on type
// LoadBalancer, so a former LoadBalancer Service has none).
func ours(svc *v1.Service) bool {
	return svc.Spec.LoadBalancerClass == nil || *svc.Spec.LoadBalancerClass == lbClass
}

// wantsLB reports whether the Service needs a load balancer address now.
func wantsLB(svc *v1.Service) bool {
	return svc.DeletionTimestamp == nil && svc.Spec.Type == v1.ServiceTypeLoadBalancer && ours(svc)
}

// families returns the Service's IP families in order, as "is IPv6" flags
// (IPv4 alone if it lists none).
func families(svc *v1.Service) []bool {
	var f []bool
	for _, fam := range svc.Spec.IPFamilies {
		f = append(f, fam == v1.IPv6Protocol)
	}
	if len(f) == 0 {
		f = []bool{false}
	}
	return f
}

// ingressIPs returns the addresses in a Service's load balancer status.
func ingressIPs(svc *v1.Service) []netip.Addr {
	var ips []netip.Addr
	for _, in := range svc.Status.LoadBalancer.Ingress {
		if a, err := netip.ParseAddr(in.IP); err == nil {
			ips = append(ips, a)
		}
	}
	return ips
}

// pickAddrs chooses one address per family of the Service: the one it
// already has if it is a free pool address, else the deprecated
// spec.loadBalancerIP if it is of that family and free, else the lowest free
// one. A family the pool has no addresses for is skipped. It returns the
// addresses and, for each family it could not serve, why.
func pickAddrs(pool *Pool, svc *v1.Service, used map[netip.Addr]bool) ([]netip.Addr, []string) {
	var addrs []netip.Addr
	var problems []string
	have := ingressIPs(svc)
	requested, reqErr := netip.ParseAddr(svc.Spec.LoadBalancerIP)
	for _, v6 := range families(svc) {
		name := "IPv4"
		if v6 {
			name = "IPv6"
		}
		if !pool.Has(v6) {
			problems = append(problems, "the pool has no "+name+" addresses")
			continue
		}
		var ip netip.Addr
		for _, a := range have {
			if a.Is6() == v6 && pool.Contains(a) && !used[a] {
				ip = a
				break
			}
		}
		if !ip.IsValid() && reqErr == nil && requested.Is6() == v6 {
			if !pool.Contains(requested) || used[requested] {
				problems = append(problems, fmt.Sprintf("loadBalancerIP %s is not a free address of the pool", requested))
				continue
			}
			ip = requested
		}
		if !ip.IsValid() {
			a, ok := pool.Allocate(used, v6)
			if !ok {
				problems = append(problems, "no free "+name+" address in the pool")
				continue
			}
			ip = a
		}
		used[ip] = true
		addrs = append(addrs, ip)
	}
	return addrs, problems
}

type controller struct {
	cs       kubernetes.Interface
	services corelisters.ServiceLister
	queue    workqueue.TypedRateLimitingInterface[string]
	pool     *Pool
	recorder record.EventRecorder
	// assigned holds the addresses this controller wrote, by Service key,
	// until the informer cache shows them.
	assigned map[string][]netip.Addr
}

func runController(ctx context.Context, cs kubernetes.Interface, pool *Pool) error {
	factory := informers.NewSharedInformerFactory(cs, 10*time.Minute)
	svcInformer := factory.Core().V1().Services()

	bc := record.NewBroadcaster()
	bc.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: cs.CoreV1().Events("")})
	defer bc.Shutdown()

	c := &controller{
		cs:       cs,
		services: svcInformer.Lister(),
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "bsd-lb"}),
		pool:     pool,
		recorder: bc.NewRecorder(scheme.Scheme, v1.EventSource{Component: "bsd-lb-controller"}),
		assigned: map[string][]netip.Addr{},
	}
	defer c.queue.ShutDown()

	enqueue := func(obj interface{}) {
		if key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
			c.queue.Add(key)
		}
	}
	if _, err := svcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    enqueue,
		UpdateFunc: func(_, obj interface{}) { enqueue(obj) },
		DeleteFunc: enqueue,
	}); err != nil {
		return err
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), svcInformer.Informer().HasSynced) {
		return fmt.Errorf("service cache did not sync")
	}
	log.Printf("controller: pool %s", pool)

	// One worker: allocations are serialized, so two Services can't get the
	// same address.
	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		for c.processNext(ctx) {
		}
	}, time.Second)
	<-ctx.Done()
	return nil
}

func (c *controller) processNext(ctx context.Context) bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)
	if err := c.sync(ctx, key); err != nil {
		log.Printf("controller: %s: %v", key, err)
		c.queue.AddRateLimited(key)
		return true
	}
	c.queue.Forget(key)
	return true
}

// used returns the addresses held by Services other than key.
func (c *controller) used(key string) (map[netip.Addr]bool, error) {
	all, err := c.services.List(everything)
	if err != nil {
		return nil, err
	}
	used := map[netip.Addr]bool{}
	for _, s := range all {
		k, _ := cache.MetaNamespaceKeyFunc(s)
		if k == key || !ours(s) || s.Spec.Type != v1.ServiceTypeLoadBalancer {
			continue
		}
		for _, a := range ingressIPs(s) {
			used[a] = true
		}
	}
	for k, as := range c.assigned {
		if k != key {
			for _, a := range as {
				used[a] = true
			}
		}
	}
	return used, nil
}

func (c *controller) sync(ctx context.Context, key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil
	}
	svc, err := c.services.Services(ns).Get(name)
	if apierrors.IsNotFound(err) {
		delete(c.assigned, key)
		return nil
	}
	if err != nil {
		return err
	}

	if !wantsLB(svc) {
		if !ours(svc) {
			return nil
		}
		return c.release(ctx, key, svc)
	}

	if !slices.Contains(svc.Finalizers, cleanupFinalizer) {
		s := svc.DeepCopy()
		s.Finalizers = append(s.Finalizers, cleanupFinalizer)
		_, err := c.cs.CoreV1().Services(ns).Update(ctx, s, metaUpdate)
		return err // the update brings the Service back through the queue
	}

	used, err := c.used(key)
	if err != nil {
		return err
	}
	addrs, problems := pickAddrs(c.pool, svc, used)
	for _, p := range problems {
		c.recorder.Event(svc, v1.EventTypeWarning, "AllocationFailed", p)
	}
	if len(addrs) == 0 {
		if len(problems) > 0 {
			return fmt.Errorf("%s", strings.Join(problems, "; "))
		}
		return nil
	}
	c.assigned[key] = addrs

	vip := v1.LoadBalancerIPModeVIP
	var want []v1.LoadBalancerIngress
	for _, a := range addrs {
		want = append(want, v1.LoadBalancerIngress{IP: a.String(), IPMode: &vip})
	}
	if ingressEqual(svc.Status.LoadBalancer.Ingress, want) {
		return nil
	}
	s := svc.DeepCopy()
	s.Status.LoadBalancer.Ingress = want
	if _, err := c.cs.CoreV1().Services(ns).UpdateStatus(ctx, s, metaUpdate); err != nil {
		return err
	}
	c.recorder.Eventf(svc, v1.EventTypeNormal, "IPAllocated", "assigned %v", addrs)
	log.Printf("controller: %s: assigned %v", key, addrs)
	return nil
}

// ingressEqual reports whether a status already holds exactly want.
func ingressEqual(cur, want []v1.LoadBalancerIngress) bool {
	if len(cur) != len(want) {
		return false
	}
	for i := range cur {
		if cur[i].IP != want[i].IP || cur[i].Hostname != "" || len(cur[i].Ports) != 0 ||
			cur[i].IPMode == nil || *cur[i].IPMode != *want[i].IPMode {
			return false
		}
	}
	return true
}

// release clears the load balancer status of a Service that no longer wants
// one (deleted, or its type changed), then drops the finalizer.
func (c *controller) release(ctx context.Context, key string, svc *v1.Service) error {
	delete(c.assigned, key)
	ns := svc.Namespace
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		s := svc.DeepCopy()
		s.Status.LoadBalancer.Ingress = nil
		updated, err := c.cs.CoreV1().Services(ns).UpdateStatus(ctx, s, metaUpdate)
		if err != nil {
			return err
		}
		log.Printf("controller: %s: released %v", key, ingressIPs(svc))
		svc = updated
	}
	if i := slices.Index(svc.Finalizers, cleanupFinalizer); i >= 0 {
		s := svc.DeepCopy()
		s.Finalizers = slices.Delete(s.Finalizers, i, i+1)
		if _, err := c.cs.CoreV1().Services(ns).Update(ctx, s, metaUpdate); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
