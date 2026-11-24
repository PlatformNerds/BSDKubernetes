package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"net"
	"net/netip"
	"slices"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
)

const (
	leasePrefix   = "bsd-lb-speaker-"
	leaseDuration = 30 * time.Second
	leaseRenew    = 10 * time.Second
	// excludeLabel keeps a node out of external load balancers, as for cloud
	// providers.
	excludeLabel = "node.kubernetes.io/exclude-from-external-load-balancers"
)

// announcer answers ARP (IPv4) and NDP (IPv6) for a set of addresses on one
// interface.
type announcer interface {
	SetAddrs(addrs []netip.Addr)
}

// electionInput is what the election needs, so it can be tested alone.
type electionInput struct {
	// speakers: nodes that are Ready, not excluded, and whose speaker lease
	// is current.
	speakers []string
	// local: with externalTrafficPolicy Local, the nodes with a ready
	// endpoint of the Service; nil with Cluster.
	local map[string]bool
}

// winner returns the node that announces ip, or "" when no node may. Every
// speaker computes the same answer from the same API objects: the eligible
// node with the lowest hash of node name and address, which spreads
// addresses over nodes (MetalLB's layer 2 mode does the same).
func winner(ip netip.Addr, in electionInput) string {
	best, bestHash := "", ""
	for _, n := range in.speakers {
		if in.local != nil && !in.local[n] {
			continue
		}
		sum := sha256.Sum256([]byte(n + "#" + ip.String()))
		h := string(sum[:])
		if best == "" || h < bestHash {
			best, bestHash = n, h
		}
	}
	return best
}

func nodeReady(n *v1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == v1.NodeReady {
			return c.Status == v1.ConditionTrue
		}
	}
	return false
}

type speaker struct {
	cs        kubernetes.Interface
	node      string
	namespace string
	subnets   []netip.Prefix
	ann       announcer
	factory   informers.SharedInformerFactory
	// leases: only the speakers' namespace (the Role grants no more).
	leases   informers.SharedInformerFactory
	recorder record.EventRecorder
	// owner: the node last seen announcing each address, for events.
	owner map[netip.Addr]string
	kick  chan struct{}
}

func runSpeaker(ctx context.Context, cs kubernetes.Interface, node, namespace, ifname string) error {
	iface, subnets, err := pickInterface(ifname, cs, ctx, node)
	if err != nil {
		return err
	}
	ann, err := newResponder(ctx, iface)
	if err != nil {
		return err
	}
	log.Printf("speaker: node %s, interface %s (%s, %v)", node, iface.Name, iface.HardwareAddr, subnets)

	bc := record.NewBroadcaster()
	bc.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: cs.CoreV1().Events("")})
	defer bc.Shutdown()

	s := &speaker{
		cs: cs, node: node, namespace: namespace, subnets: subnets, ann: ann,
		factory:  informers.NewSharedInformerFactory(cs, 10*time.Minute),
		leases:   informers.NewSharedInformerFactoryWithOptions(cs, 10*time.Minute, informers.WithNamespace(namespace)),
		recorder: bc.NewRecorder(scheme.Scheme, v1.EventSource{Component: "bsd-lb-speaker", Host: node}),
		owner:    map[netip.Addr]string{},
		kick:     make(chan struct{}, 1),
	}
	kick := func(interface{}) { s.trigger() }
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: kick, DeleteFunc: kick,
		UpdateFunc: func(_, o interface{}) { s.trigger() },
	}
	for _, inf := range []cache.SharedIndexInformer{
		s.factory.Core().V1().Services().Informer(),
		s.factory.Core().V1().Nodes().Informer(),
		s.factory.Discovery().V1().EndpointSlices().Informer(),
		s.leases.Coordination().V1().Leases().Informer(),
	} {
		if _, err := inf.AddEventHandler(handler); err != nil {
			return err
		}
	}
	s.factory.Start(ctx.Done())
	s.leases.Start(ctx.Done())
	for t, ok := range s.factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("cache for %v did not sync", t)
		}
	}
	for t, ok := range s.leases.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("cache for %v did not sync", t)
		}
	}

	go s.renewLease(ctx)
	tick := time.NewTicker(leaseRenew)
	defer tick.Stop()
	for {
		s.reconcile()
		select {
		case <-ctx.Done():
			ann.SetAddrs(nil)
			return nil
		case <-s.kick:
		case <-tick.C: // leases expire without an event
		}
	}
}

func (s *speaker) trigger() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// renewLease keeps this node's speaker lease current. A node whose speaker
// is gone stops being elected once the lease runs out, although the node
// itself may still be Ready.
func (s *speaker) renewLease(ctx context.Context) {
	leases := s.cs.CoordinationV1().Leases(s.namespace)
	name := leasePrefix + s.node
	for {
		now := metav1.NewMicroTime(time.Now())
		l, err := leases.Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			_, err = leases.Create(ctx, &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app": "bsd-lb-speaker"}},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       ptr.To(s.node),
					LeaseDurationSeconds: ptr.To(int32(leaseDuration / time.Second)),
					RenewTime:            &now,
				},
			}, metav1.CreateOptions{})
		case err == nil:
			l.Spec.HolderIdentity = ptr.To(s.node)
			l.Spec.LeaseDurationSeconds = ptr.To(int32(leaseDuration / time.Second))
			l.Spec.RenewTime = &now
			_, err = leases.Update(ctx, l, metav1.UpdateOptions{})
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("speaker: lease %s: %v", name, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(leaseRenew):
		}
	}
}

// speakers returns the nodes that may announce.
func (s *speaker) speakers() []string {
	nodes, _ := s.factory.Core().V1().Nodes().Lister().List(labels.Everything())
	leases, _ := s.leases.Coordination().V1().Leases().Lister().Leases(s.namespace).List(labels.Everything())
	live := map[string]bool{}
	now := time.Now()
	for _, l := range leases {
		if l.Spec.HolderIdentity == nil || l.Spec.RenewTime == nil || l.Spec.LeaseDurationSeconds == nil {
			continue
		}
		if l.Name != leasePrefix+*l.Spec.HolderIdentity {
			continue
		}
		if now.Sub(l.Spec.RenewTime.Time) <= time.Duration(*l.Spec.LeaseDurationSeconds)*time.Second {
			live[*l.Spec.HolderIdentity] = true
		}
	}
	var out []string
	for _, n := range nodes {
		if _, excluded := n.Labels[excludeLabel]; excluded || !nodeReady(n) || !live[n.Name] {
			continue
		}
		out = append(out, n.Name)
	}
	slices.Sort(out)
	return out
}

// localNodes returns the nodes with a ready endpoint of svc.
func (s *speaker) localNodes(svc *v1.Service) map[string]bool {
	sel := labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: svc.Name})
	slices_, _ := s.factory.Discovery().V1().EndpointSlices().Lister().EndpointSlices(svc.Namespace).List(sel)
	out := map[string]bool{}
	for _, sl := range slices_ {
		for _, ep := range sl.Endpoints {
			if ep.NodeName == nil {
				continue
			}
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			out[*ep.NodeName] = true
		}
	}
	return out
}

func (s *speaker) reconcile() {
	svcs, _ := s.factory.Core().V1().Services().Lister().List(labels.Everything())
	speakers := s.speakers()
	var mine []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, svc := range svcs {
		if !wantsLB(svc) {
			continue
		}
		in := electionInput{speakers: speakers}
		if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyLocal {
			in.local = s.localNodes(svc)
		}
		for _, ip := range ingressIPs(svc) {
			if !inSubnets(s.subnets, ip) || seen[ip] {
				continue
			}
			seen[ip] = true
			w := winner(ip, in)
			if w == s.node {
				mine = append(mine, ip)
				if s.owner[ip] != s.node {
					s.recorder.Eventf(svc, v1.EventTypeNormal, "nodeAssigned",
						"announcing %s from node %s", ip, s.node)
					log.Printf("speaker: announcing %s (%s/%s)", ip, svc.Namespace, svc.Name)
				}
			} else if s.owner[ip] == s.node {
				log.Printf("speaker: no longer announcing %s (%s/%s), now %q", ip, svc.Namespace, svc.Name, w)
			}
			s.owner[ip] = w
		}
	}
	for ip := range s.owner {
		if !seen[ip] {
			delete(s.owner, ip)
		}
	}
	slices.SortFunc(mine, func(a, b netip.Addr) int { return a.Compare(b) })
	s.ann.SetAddrs(mine)
}

// pickInterface returns the interface to announce on (the one named, else
// the one holding one of the node's InternalIPs) and the subnets on it, IPv4
// and IPv6 (not link-local): addresses in them are announced there, by ARP
// or by NDP.
func pickInterface(name string, cs kubernetes.Interface, ctx context.Context, node string) (*net.Interface, []netip.Prefix, error) {
	want := map[netip.Addr]bool{}
	if name == "" {
		n, err := cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil {
			return nil, nil, err
		}
		for _, a := range n.Status.Addresses {
			if a.Type == v1.NodeInternalIP {
				if ip, err := netip.ParseAddr(a.Address); err == nil {
					want[ip] = true
				}
			}
		}
		if len(want) == 0 {
			return nil, nil, fmt.Errorf("node %s has no InternalIP; set -interface", node)
		}
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, nil, err
	}
	for i := range ifs {
		iface := &ifs[i]
		if name != "" && iface.Name != name {
			continue
		}
		var subnets []netip.Prefix
		holds := false
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			p, err := netip.ParsePrefix(ipn.String())
			if err != nil || p.Addr().IsLinkLocalUnicast() {
				continue
			}
			subnets = append(subnets, p.Masked())
			holds = holds || want[p.Addr()]
		}
		if (name != "" || holds) && len(subnets) > 0 {
			return iface, subnets, nil
		}
	}
	if name != "" {
		return nil, nil, fmt.Errorf("interface %s has no usable address", name)
	}
	return nil, nil, fmt.Errorf("no interface holds an InternalIP of node %s", node)
}

// inSubnets reports whether a is in one of the subnets.
func inSubnets(subnets []netip.Prefix, a netip.Addr) bool {
	for _, p := range subnets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
