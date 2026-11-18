package main

// Rendering Kubernetes NetworkPolicy (networking.k8s.io/v1) as pf rules for the pods of one
// node (research/03 §3, research/22).
//
// Where: pod traffic crosses the host side of the pod's epair, a member of the CNI bridge, and
// pf filters bridge members (net.link.bridge.pfil_member=1). From the host's point of view:
//
//	egress of pod P  = packets "in" from P's address
//	ingress of pod P = packets "out" to P's address
//
// Filter rules run after translation, so Service traffic is matched by endpoint address, as
// NetworkPolicy requires. pf keeps a separate state per direction of a forwarded connection,
// so a state created by the client's egress rule doesn't skip the server's ingress rules.
//
// Per local pod, in one anchor:
//
//	ingress-isolated:  pass out quick from (self) to P keep state     # kubelet probes
//	                   pass out quick [proto] from <peers> to P [port] keep state   # per allow
//	                   block drop out quick to P
//	otherwise, if egress-isolated:
//	                   pass out quick to P keep state                  # replies to P's peers
//	egress-isolated:   pass in quick [proto] from P to <peers> [port] keep state
//	                   block drop in quick from P
//	otherwise, if ingress-isolated:
//	                   pass in quick from P keep state                 # replies to P's own
//	                                                                   # connections
// Pods no policy selects get no rules (pf's default pass).

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// allow is one pass rule: traffic between the pod and addrs (nil: any address), of proto
// ("": any protocol) to ports ("": any port; "80" or "8000:8080").
type allow struct {
	addrs []string
	proto string
	ports string
}

type renderer struct {
	pods       []*v1.Pod
	namespaces map[string]*v1.Namespace
	policies   []*netv1.NetworkPolicy
	tables     map[string][]string // name -> entries
}

// Render returns the anchor contents for node's pods.
func Render(node string, pods []*v1.Pod, namespaces []*v1.Namespace, policies []*netv1.NetworkPolicy) string {
	r := &renderer{namespaces: map[string]*v1.Namespace{}, tables: map[string][]string{}}
	for _, p := range pods {
		if isPeerPod(p) {
			r.pods = append(r.pods, p)
		}
	}
	sort.Slice(r.pods, func(i, j int) bool { return podKey(r.pods[i]) < podKey(r.pods[j]) })
	for _, ns := range namespaces {
		r.namespaces[ns.Name] = ns
	}
	r.policies = append(r.policies, policies...)
	sort.Slice(r.policies, func(i, j int) bool {
		return r.policies[i].Namespace+"/"+r.policies[i].Name < r.policies[j].Namespace+"/"+r.policies[j].Name
	})

	var rules []string
	for _, p := range r.pods {
		if p.Spec.NodeName != node {
			continue
		}
		rules = append(rules, r.podRules(p)...)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# bsd-netpol: NetworkPolicy for the pods of node %s\n", node)
	names := make([]string, 0, len(r.tables))
	for n := range r.tables {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "table <%s> const { %s }\n", n, strings.Join(r.tables[n], " "))
	}
	for _, rule := range rules {
		b.WriteString(rule)
		b.WriteByte('\n')
	}
	return b.String()
}

func podKey(p *v1.Pod) string { return p.Namespace + "/" + p.Name }

// isPeerPod: pods that have their own address. Host-network pods use the node's addresses and
// are neither policy targets nor pod peers (like other implementations); finished pods have
// released their address.
func isPeerPod(p *v1.Pod) bool {
	if p.Spec.HostNetwork || len(podIPs(p)) == 0 {
		return false
	}
	return p.Status.Phase != v1.PodSucceeded && p.Status.Phase != v1.PodFailed
}

func podIPs(p *v1.Pod) []string {
	var ips []string
	for _, ip := range p.Status.PodIPs {
		if ip.IP != "" {
			ips = append(ips, ip.IP)
		}
	}
	if len(ips) == 0 && p.Status.PodIP != "" {
		ips = []string{p.Status.PodIP}
	}
	return ips
}

func (r *renderer) podRules(p *v1.Pod) []string {
	var ingress, egress []allow
	var inIso, egIso bool
	for _, np := range r.policies {
		if np.Namespace != p.Namespace || !matches(&np.Spec.PodSelector, p.Labels) {
			continue
		}
		in, eg := policyTypes(np)
		if in {
			inIso = true
			for _, rule := range np.Spec.Ingress {
				ingress = append(ingress, r.expand(np.Namespace, rule.From, rule.Ports, p, false)...)
			}
		}
		if eg {
			egIso = true
			for _, rule := range np.Spec.Egress {
				egress = append(egress, r.expand(np.Namespace, rule.To, rule.Ports, p, true)...)
			}
		}
	}
	if !inIso && !egIso {
		return nil
	}

	var out []string
	for _, ip := range podIPs(p) {
		out = append(out, fmt.Sprintf("# %s %s ingress:%s egress:%s", podKey(p), ip, iso(inIso), iso(egIso)))
		if inIso {
			out = append(out, fmt.Sprintf("pass out quick from (self) to %s keep state", ip))
			for _, a := range dedup(ingress) {
				if rule, ok := r.rule("out", a, ip); ok {
					out = append(out, rule)
				}
			}
			out = append(out, fmt.Sprintf("block drop out quick to %s", ip))
		} else {
			out = append(out, fmt.Sprintf("pass out quick to %s keep state", ip))
		}
		if egIso {
			for _, a := range dedup(egress) {
				if rule, ok := r.rule("in", a, ip); ok {
					out = append(out, rule)
				}
			}
			out = append(out, fmt.Sprintf("block drop in quick from %s", ip))
		} else {
			out = append(out, fmt.Sprintf("pass in quick from %s keep state", ip))
		}
	}
	return out
}

func iso(b bool) string {
	if b {
		return "isolated"
	}
	return "open"
}

// policyTypes follows the API defaulting: Ingress always, Egress if there are egress rules.
func policyTypes(np *netv1.NetworkPolicy) (ingress, egress bool) {
	if len(np.Spec.PolicyTypes) == 0 {
		return true, len(np.Spec.Egress) > 0
	}
	for _, t := range np.Spec.PolicyTypes {
		switch t {
		case netv1.PolicyTypeIngress:
			ingress = true
		case netv1.PolicyTypeEgress:
			egress = true
		}
	}
	return ingress, egress
}

func matches(sel *metav1.LabelSelector, l map[string]string) bool {
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false // an invalid selector selects nothing
	}
	return s.Matches(labels.Set(l))
}

// peers resolves one rule's peers. any is true when the rule has no peers (all addresses).
func (r *renderer) peers(ns string, peers []netv1.NetworkPolicyPeer) (any bool, blocks [][]string, pods []*v1.Pod) {
	if len(peers) == 0 {
		return true, nil, nil
	}
	seen := map[string]bool{}
	for _, peer := range peers {
		if peer.IPBlock != nil {
			e := []string{peer.IPBlock.CIDR}
			for _, x := range peer.IPBlock.Except {
				e = append(e, "!"+x)
			}
			blocks = append(blocks, e)
			continue
		}
		if peer.PodSelector == nil && peer.NamespaceSelector == nil {
			continue
		}
		for _, p := range r.pods {
			if peer.NamespaceSelector == nil {
				if p.Namespace != ns {
					continue
				}
			} else {
				nsObj := r.namespaces[p.Namespace]
				if nsObj == nil || !matches(peer.NamespaceSelector, nsObj.Labels) {
					continue
				}
			}
			if peer.PodSelector != nil && !matches(peer.PodSelector, p.Labels) {
				continue
			}
			if !seen[podKey(p)] {
				seen[podKey(p)] = true
				pods = append(pods, p)
			}
		}
	}
	return false, blocks, pods
}

// expand turns one ingress or egress rule of a policy selecting target into allows.
func (r *renderer) expand(ns string, peerList []netv1.NetworkPolicyPeer, ports []netv1.NetworkPolicyPort, target *v1.Pod, egress bool) []allow {
	anyPeer, blocks, pods := r.peers(ns, peerList)
	if len(ports) == 0 {
		ports = []netv1.NetworkPolicyPort{{}} // all protocols, all ports
	}
	var res []allow
	add := func(addrs []string, proto, port string) {
		res = append(res, allow{addrs: addrs, proto: proto, ports: port})
	}
	addAll := func(proto, port string) {
		if anyPeer {
			add(nil, proto, port)
			return
		}
		for _, blk := range blocks {
			add(blk, proto, port)
		}
		if ips := allIPs(pods); len(ips) > 0 {
			add(ips, proto, port)
		}
	}
	for _, pp := range ports {
		proto := ""
		if pp.Port != nil || pp.Protocol != nil {
			proto = "tcp"
			if pp.Protocol != nil {
				proto = strings.ToLower(string(*pp.Protocol))
			}
		}
		if pp.Port == nil || pp.Port.Type == 0 { // all ports, or a number
			port := ""
			if pp.Port != nil {
				port = fmt.Sprint(pp.Port.IntVal)
				if pp.EndPort != nil && *pp.EndPort > pp.Port.IntVal {
					port = fmt.Sprintf("%d:%d", pp.Port.IntVal, *pp.EndPort)
				}
			}
			addAll(proto, port)
			continue
		}
		// A named port is a port of the destination pod.
		name := pp.Port.StrVal
		if !egress {
			if n, ok := namedPort(target, name, proto); ok {
				addAll(proto, fmt.Sprint(n))
			}
			continue
		}
		// Egress: only pods have named ports; resolve per destination pod. With no peers
		// that is every pod (ipBlock peers can't match a named port).
		dests := pods
		if anyPeer {
			dests = r.pods
		}
		byPort := map[int32][]*v1.Pod{}
		for _, d := range dests {
			if n, ok := namedPort(d, name, proto); ok {
				byPort[n] = append(byPort[n], d)
			}
		}
		nums := make([]int32, 0, len(byPort))
		for n := range byPort {
			nums = append(nums, n)
		}
		sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
		for _, n := range nums {
			add(allIPs(byPort[n]), proto, fmt.Sprint(n))
		}
	}
	return res
}

func namedPort(p *v1.Pod, name, proto string) (int32, bool) {
	if proto == "" {
		proto = "tcp"
	}
	cs := append(append([]v1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...)
	for _, c := range cs {
		for _, cp := range c.Ports {
			cpProto := strings.ToLower(string(cp.Protocol))
			if cpProto == "" {
				cpProto = "tcp"
			}
			if cp.Name == name && cpProto == proto {
				return cp.ContainerPort, true
			}
		}
	}
	return 0, false
}

func allIPs(pods []*v1.Pod) []string {
	var ips []string
	for _, p := range pods {
		ips = append(ips, podIPs(p)...)
	}
	return ips
}

func dedup(as []allow) []allow {
	seen := map[string]bool{}
	var out []allow
	for _, a := range as {
		k := strings.Join(sortedCopy(a.addrs), ",") + "|" + a.proto + "|" + a.ports
		if a.addrs == nil {
			k = "*" + k
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, a)
		}
	}
	return out
}

func sortedCopy(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}

// table returns the name of a table holding entries, registering it once.
func (r *renderer) table(entries []string) string {
	e := sortedCopy(entries)
	h := sha256.Sum256([]byte(strings.Join(e, " ")))
	name := "np_" + hex.EncodeToString(h[:6])
	r.tables[name] = e
	return name
}

// rule renders an allow for pod address ip; dir "out" is ingress (peer -> ip), "in" egress.
func (r *renderer) rule(dir string, a allow, ip string) (string, bool) {
	peer := "any"
	if a.addrs != nil {
		if len(a.addrs) == 0 {
			return "", false
		}
		peer = "<" + r.table(a.addrs) + ">"
	}
	var b strings.Builder
	b.WriteString("pass " + dir + " quick")
	if a.proto != "" {
		b.WriteString(" proto " + a.proto)
	}
	if dir == "out" {
		b.WriteString(" from " + peer + " to " + ip)
	} else {
		b.WriteString(" from " + ip + " to " + peer)
	}
	if a.ports != "" {
		b.WriteString(" port " + a.ports)
	}
	b.WriteString(" keep state")
	return b.String(), true
}
