package main

import (
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func pod(ns, name, node, ip string, l map[string]string, ports ...v1.ContainerPort) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: l},
		Spec:       v1.PodSpec{NodeName: node, Containers: []v1.Container{{Name: "c", Ports: ports}}},
		Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: ip, PodIPs: []v1.PodIP{{IP: ip}}},
	}
}

func ns(name string, l map[string]string) *v1.Namespace {
	if l == nil {
		l = map[string]string{}
	}
	l["kubernetes.io/metadata.name"] = name
	return &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
}

func sel(l map[string]string) *metav1.LabelSelector { return &metav1.LabelSelector{MatchLabels: l} }

func tcp(p int) netv1.NetworkPolicyPort {
	pr := v1.ProtocolTCP
	port := intstr.FromInt32(int32(p))
	return netv1.NetworkPolicyPort{Protocol: &pr, Port: &port}
}

// split separates rule lines from table lines, dropping comments.
func split(s string) (rules, tables []string) {
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		switch {
		case strings.HasPrefix(l, "#"):
		case strings.HasPrefix(l, "table "):
			tables = append(tables, l)
		default:
			rules = append(rules, l)
		}
	}
	return
}

func want(t *testing.T, got string, wantRules ...string) {
	t.Helper()
	rules, _ := split(got)
	if strings.Join(rules, "\n") != strings.Join(wantRules, "\n") {
		t.Errorf("rules:\n%s\nwant:\n%s\n(full output:\n%s)", strings.Join(rules, "\n"), strings.Join(wantRules, "\n"), got)
	}
}

func tableName(t *testing.T, tables []string, contains string) string {
	t.Helper()
	if len(tables) != 1 || !strings.Contains(tables[0], contains) {
		t.Fatalf("want one table containing %q, got %v", contains, tables)
	}
	return strings.Fields(tables[0])[1]
}

var (
	nsA     = ns("a", nil)
	nsB     = ns("b", map[string]string{"team": "x"})
	server  = pod("a", "server", "n1", "10.0.0.2", map[string]string{"app": "server"}, v1.ContainerPort{Name: "http", ContainerPort: 8080})
	client  = pod("a", "client", "n2", "10.0.1.3", map[string]string{"app": "client"})
	clientB = pod("b", "client", "n2", "10.0.1.4", map[string]string{"app": "client"})
)

func TestNoPolicyNoRules(t *testing.T) {
	want(t, Render("n1", []*v1.Pod{server, client}, []*v1.Namespace{nsA}, nil))
}

func TestDenyAllIngress(t *testing.T) {
	np := &netv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "deny"}}
	want(t, Render("n1", []*v1.Pod{server, client}, []*v1.Namespace{nsA}, []*netv1.NetworkPolicy{np}),
		"pass out quick from (self) to 10.0.0.2 keep state",
		"block drop out quick to 10.0.0.2",
		"pass in quick from 10.0.0.2 keep state")
	// Pods of the policy on other nodes get no rules on this one.
	want(t, Render("n2", []*v1.Pod{server, client}, []*v1.Namespace{nsA}, []*netv1.NetworkPolicy{np}),
		"pass out quick from (self) to 10.0.1.3 keep state",
		"block drop out quick to 10.0.1.3",
		"pass in quick from 10.0.1.3 keep state")
}

func TestAllowFromPodSelectorPort(t *testing.T) {
	np := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "allow"},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: *sel(map[string]string{"app": "server"}),
			Ingress: []netv1.NetworkPolicyIngressRule{{
				From:  []netv1.NetworkPolicyPeer{{PodSelector: sel(map[string]string{"app": "client"})}},
				Ports: []netv1.NetworkPolicyPort{tcp(8080)},
			}},
		},
	}
	out := Render("n1", []*v1.Pod{server, client, clientB}, []*v1.Namespace{nsA, nsB}, []*netv1.NetworkPolicy{np})
	_, tables := split(out)
	// Only a/client: a podSelector alone means the policy's own namespace.
	name := tableName(t, tables, "{ 10.0.1.3 }")
	want(t, out,
		"pass out quick from (self) to 10.0.0.2 keep state",
		"pass out quick proto tcp from <"+strings.Trim(name, "<>")+"> to 10.0.0.2 port 8080 keep state",
		"block drop out quick to 10.0.0.2",
		"pass in quick from 10.0.0.2 keep state")
}

func TestNamespaceSelectorAndNamedPort(t *testing.T) {
	port := intstr.FromString("http")
	np := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "ns"},
		Spec: netv1.NetworkPolicySpec{
			Ingress: []netv1.NetworkPolicyIngressRule{{
				From: []netv1.NetworkPolicyPeer{{
					NamespaceSelector: sel(map[string]string{"team": "x"}),
					PodSelector:       sel(map[string]string{"app": "client"}),
				}},
				Ports: []netv1.NetworkPolicyPort{{Port: &port}},
			}},
		},
	}
	out := Render("n1", []*v1.Pod{server, client, clientB}, []*v1.Namespace{nsA, nsB}, []*netv1.NetworkPolicy{np})
	_, tables := split(out)
	tableName(t, tables, "{ 10.0.1.4 }")
	if !strings.Contains(out, "proto tcp from <") || !strings.Contains(out, "port 8080 keep state") {
		t.Errorf("named port http not resolved to tcp 8080:\n%s", out)
	}
}

func TestIPBlockExceptAndEndPort(t *testing.T) {
	end := int32(9000)
	p := tcp(8000)
	p.EndPort = &end
	np := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "cidr"},
		Spec: netv1.NetworkPolicySpec{
			Ingress: []netv1.NetworkPolicyIngressRule{{
				From:  []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"10.0.1.0/24"}}}},
				Ports: []netv1.NetworkPolicyPort{p},
			}},
		},
	}
	out := Render("n1", []*v1.Pod{server}, []*v1.Namespace{nsA}, []*netv1.NetworkPolicy{np})
	_, tables := split(out)
	tableName(t, tables, "{ !10.0.1.0/24 10.0.0.0/8 }")
	if !strings.Contains(out, "port 8000:9000 keep state") {
		t.Errorf("endPort range missing:\n%s", out)
	}
}

func TestEgressOnlyWithNamedPort(t *testing.T) {
	port := intstr.FromString("http")
	np := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "egress"},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: *sel(map[string]string{"app": "client"}),
			PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeEgress},
			Egress: []netv1.NetworkPolicyEgressRule{
				{Ports: []netv1.NetworkPolicyPort{{Port: &port}}}, // named port on any pod
			},
		},
	}
	out := Render("n2", []*v1.Pod{server, client}, []*v1.Namespace{nsA}, []*netv1.NetworkPolicy{np})
	_, tables := split(out)
	name := tableName(t, tables, "{ 10.0.0.2 }")
	want(t, out,
		"pass out quick to 10.0.1.3 keep state",
		"pass in quick proto tcp from 10.0.1.3 to <"+strings.Trim(name, "<>")+"> port 8080 keep state",
		"block drop in quick from 10.0.1.3")
}

func TestAllowAllIngressRule(t *testing.T) {
	np := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "all"},
		Spec:       netv1.NetworkPolicySpec{Ingress: []netv1.NetworkPolicyIngressRule{{}}},
	}
	want(t, Render("n1", []*v1.Pod{server}, []*v1.Namespace{nsA}, []*netv1.NetworkPolicy{np}),
		"pass out quick from (self) to 10.0.0.2 keep state",
		"pass out quick from any to 10.0.0.2 keep state",
		"block drop out quick to 10.0.0.2",
		"pass in quick from 10.0.0.2 keep state")
}

func TestUnmatchedPeerAllowsNothing(t *testing.T) {
	np := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "nobody"},
		Spec: netv1.NetworkPolicySpec{Ingress: []netv1.NetworkPolicyIngressRule{{
			From: []netv1.NetworkPolicyPeer{{PodSelector: sel(map[string]string{"app": "nope"})}},
		}}},
	}
	want(t, Render("n1", []*v1.Pod{server, client}, []*v1.Namespace{nsA}, []*netv1.NetworkPolicy{np}),
		"pass out quick from (self) to 10.0.0.2 keep state",
		"block drop out quick to 10.0.0.2",
		"pass in quick from 10.0.0.2 keep state")
}

func TestHostNetworkAndFinishedPodsIgnored(t *testing.T) {
	hn := pod("a", "hn", "n1", "192.168.0.1", map[string]string{"app": "server"})
	hn.Spec.HostNetwork = true
	done := pod("a", "done", "n1", "10.0.0.9", map[string]string{"app": "server"})
	done.Status.Phase = v1.PodSucceeded
	np := &netv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "deny"}}
	want(t, Render("n1", []*v1.Pod{hn, done}, []*v1.Namespace{nsA}, []*netv1.NetworkPolicy{np}))
}
