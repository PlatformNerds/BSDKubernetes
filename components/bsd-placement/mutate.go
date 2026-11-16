// Package main implements bsd-placement, a mutating admission webhook that
// lets unmodified Linux workloads (Helm charts, upstream manifests) schedule
// onto FreeBSD nodes, where they run under the Linux ABI via RuntimeClass.
//
// Upstream manifests pin pods with nodeSelector kubernetes.io/os=linux (or the
// equivalent required node affinity), which a FreeBSD node never matches.
// For such pods, and for DaemonSet pod templates (the DaemonSet controller
// picks nodes from the template, before any pod exists), the webhook:
//
//   - removes kubernetes.io/os=linux (and beta.kubernetes.io/os=linux) from
//     nodeSelector and rewrites required affinity "kubernetes.io/os In [linux]"
//     to "In [freebsd]",
//   - sets runtimeClassName to the Linux RuntimeClass (default "linux"),
//   - adds the RuntimeClass's scheduling nodeSelector (nodes with the Linux
//     ABI), since the in-tree RuntimeClass admission plugin runs before
//     webhooks and will not add it,
//   - records what it did in the bsdk8s.io/placement annotation.
//
// Pods that already set runtimeClassName are never touched. A namespace can
// opt every pod in (images with no OS pin at all) via the "force" webhook.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	osLabel     = "kubernetes.io/os"
	betaOSLabel = "beta.kubernetes.io/os"
	annotation  = "bsdk8s.io/placement"
)

// Config controls the rewrite.
type Config struct {
	RuntimeClass string            // RuntimeClass to use for Linux pods
	NodeSelector map[string]string // added so pods land on Linux-ABI nodes
	NodeOS       string            // value of kubernetes.io/os on those nodes
}

// podSpecPath returns where the pod spec lives inside an object of the given
// kind, as a JSON pointer prefix.
func podSpecPath(kind string) (string, bool) {
	switch kind {
	case "Pod":
		return "/spec", true
	case "DaemonSet":
		return "/spec/template/spec", true
	}
	return "", false
}

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// mutate returns the JSONPatch for obj (raw JSON of a Pod or DaemonSet), or
// nil when the object needs no change. force applies the rewrite even when the
// pod spec has no Linux OS pin.
func mutate(cfg Config, kind string, obj []byte, force bool) ([]patchOp, string, error) {
	base, ok := podSpecPath(kind)
	if !ok {
		return nil, "", nil
	}
	var o map[string]any
	if err := json.Unmarshal(obj, &o); err != nil {
		return nil, "", fmt.Errorf("decode %s: %w", kind, err)
	}
	spec := lookup(o, strings.Split(strings.TrimPrefix(base, "/"), "/")...)
	if spec == nil {
		return nil, "", nil
	}
	if rc, _ := spec["runtimeClassName"].(string); rc != "" {
		return nil, "", nil
	}

	var reasons []string
	nodeSelector := map[string]string{}
	if ns, ok := spec["nodeSelector"].(map[string]any); ok {
		for k, v := range ns {
			s, _ := v.(string)
			nodeSelector[k] = s
		}
	}
	for _, l := range []string{osLabel, betaOSLabel} {
		if nodeSelector[l] == "linux" {
			delete(nodeSelector, l)
			reasons = append(reasons, "nodeSelector "+l+"=linux")
		}
	}
	affinity, affinityChanged := rewriteAffinity(spec["affinity"], cfg.NodeOS)
	if affinityChanged {
		reasons = append(reasons, "nodeAffinity "+osLabel+" In [linux]")
	}
	if len(reasons) == 0 && !force {
		return nil, "", nil
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "namespace default")
	}
	for k, v := range cfg.NodeSelector {
		nodeSelector[k] = v
	}

	ops := []patchOp{
		{Op: "add", Path: base + "/runtimeClassName", Value: cfg.RuntimeClass},
		{Op: "add", Path: base + "/nodeSelector", Value: nodeSelector},
	}
	if affinityChanged {
		ops = append(ops, patchOp{Op: "replace", Path: base + "/affinity", Value: affinity})
	}
	why := "runtimeClassName=" + cfg.RuntimeClass + " (" + strings.Join(reasons, ", ") + ")"
	ops = append(ops, annotationOps(o, why)...)
	return ops, why, nil
}

// rewriteAffinity rewrites required node affinity expressions that only allow
// kubernetes.io/os=linux so they allow nodeOS instead. Preferred terms are
// left alone (they never block scheduling).
func rewriteAffinity(a any, nodeOS string) (any, bool) {
	terms := lookupSlice(a, "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms")
	changed := false
	for _, t := range terms {
		term, _ := t.(map[string]any)
		exprs, _ := term["matchExpressions"].([]any)
		for _, e := range exprs {
			expr, _ := e.(map[string]any)
			key, _ := expr["key"].(string)
			op, _ := expr["operator"].(string)
			if (key != osLabel && key != betaOSLabel) || op != "In" {
				continue
			}
			vals, _ := expr["values"].([]any)
			hasLinux, hasNode := false, false
			for _, v := range vals {
				hasLinux = hasLinux || v == "linux"
				hasNode = hasNode || v == nodeOS
			}
			if hasLinux && !hasNode {
				out := make([]any, 0, len(vals))
				for _, v := range vals {
					if v == "linux" {
						v = nodeOS
					}
					out = append(out, v)
				}
				expr["values"] = out
				changed = true
			}
		}
	}
	return a, changed
}

// annotationOps records the rewrite on the object itself (for a DaemonSet,
// also on its pod template so the pods carry it).
func annotationOps(o map[string]any, why string) []patchOp {
	var ops []patchOp
	paths := [][]string{{"metadata"}}
	if lookup(o, "spec", "template") != nil {
		paths = append(paths, []string{"spec", "template", "metadata"})
	}
	for _, p := range paths {
		ptr := "/" + strings.Join(p, "/")
		md := lookup(o, p...)
		switch {
		case md == nil:
			ops = append(ops, patchOp{Op: "add", Path: ptr, Value: map[string]any{"annotations": map[string]string{annotation: why}}})
		case md["annotations"] == nil:
			ops = append(ops, patchOp{Op: "add", Path: ptr + "/annotations", Value: map[string]string{annotation: why}})
		default:
			ops = append(ops, patchOp{Op: "add", Path: ptr + "/annotations/" + escape(annotation), Value: why})
		}
	}
	return ops
}

func escape(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

func lookup(o map[string]any, path ...string) map[string]any {
	cur := o
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

func lookupSlice(a any, path ...string) []any {
	m, ok := a.(map[string]any)
	if !ok {
		return nil
	}
	last := path[len(path)-1]
	m = lookup(m, path[:len(path)-1]...)
	if m == nil {
		return nil
	}
	s, _ := m[last].([]any)
	return s
}

// parseSelector parses "k=v,k2=v2".
func parseSelector(s string) (map[string]string, error) {
	out := map[string]string{}
	if s == "" {
		return out, nil
	}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("bad selector %q", kv)
		}
		out[k] = v
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
