package main

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"slices"
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

func hasIPv4(svc *v1.Service) bool {
	if len(svc.Spec.IPFamilies) == 0 {
		return true
	}
	return slices.Contains(svc.Spec.IPFamilies, v1.IPv4Protocol)
}

// ingressIPs returns the IPv4 addresses in a Service's load balancer status.
func ingressIPs(svc *v1.Service) []netip.Addr {
	var ips []netip.Addr
	for _, in := range svc.Status.LoadBalancer.Ingress {
		if a, err := netip.ParseAddr(in.IP); err == nil && a.Is4() {
			ips = append(ips, a)
		}
	}
	return ips
}

type controller struct {
	cs       kubernetes.Interface
	services corelisters.ServiceLister
	queue    workqueue.TypedRateLimitingInterface[string]
	pool     *Pool
	recorder record.EventRecorder
	// assigned holds the addresses this controller wrote, by Service key,
	// until the informer cache shows them.
	assigned map[string]netip.Addr
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
		assigned: map[string]netip.Addr{},
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
	for k, a := range c.assigned {
		if k != key {
			used[a] = true
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

	if !hasIPv4(svc) {
		c.recorder.Event(svc, v1.EventTypeWarning, "AllocationFailed", "bsd-lb only allocates IPv4 addresses")
		return nil
	}

	used, err := c.used(key)
	if err != nil {
		return err
	}
	// Keep a valid address the Service already has; else take the requested
	// spec.loadBalancerIP (deprecated) if free; else the lowest free one.
	var ip netip.Addr
	for _, a := range ingressIPs(svc) {
		if c.pool.Contains(a) && !used[a] {
			ip = a
			break
		}
	}
	if !ip.IsValid() {
		if a, err := netip.ParseAddr(svc.Spec.LoadBalancerIP); err == nil && svc.Spec.LoadBalancerIP != "" {
			if !c.pool.Contains(a) || used[a] {
				c.recorder.Eventf(svc, v1.EventTypeWarning, "AllocationFailed",
					"loadBalancerIP %s is not a free address of the pool", a)
				return nil
			}
			ip = a
		}
	}
	if !ip.IsValid() {
		a, ok := c.pool.Allocate(used)
		if !ok {
			c.recorder.Event(svc, v1.EventTypeWarning, "AllocationFailed", "no free address in the pool")
			return fmt.Errorf("pool exhausted")
		}
		ip = a
	}
	c.assigned[key] = ip

	vip := v1.LoadBalancerIPModeVIP
	want := []v1.LoadBalancerIngress{{IP: ip.String(), IPMode: &vip}}
	cur := svc.Status.LoadBalancer.Ingress
	if len(cur) == 1 && cur[0].IP == want[0].IP && cur[0].Hostname == "" &&
		cur[0].IPMode != nil && *cur[0].IPMode == vip && len(cur[0].Ports) == 0 {
		return nil
	}
	s := svc.DeepCopy()
	s.Status.LoadBalancer.Ingress = want
	if _, err := c.cs.CoreV1().Services(ns).UpdateStatus(ctx, s, metaUpdate); err != nil {
		return err
	}
	c.recorder.Eventf(svc, v1.EventTypeNormal, "IPAllocated", "assigned %s", ip)
	log.Printf("controller: %s: assigned %s", key, ip)
	return nil
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
