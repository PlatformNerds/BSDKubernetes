package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var cfg = Config{RuntimeClass: "linux", NodeSelector: map[string]string{"bsdk8s.io/linux-abi": "true"}, NodeOS: "freebsd"}

// apply applies a JSONPatch with only the ops mutate emits (add/replace on
// object members) and returns the result.
func apply(t *testing.T, obj string, ops []patchOp) map[string]any {
	t.Helper()
	var o map[string]any
	if err := json.Unmarshal([]byte(obj), &o); err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		parts := strings.Split(strings.TrimPrefix(op.Path, "/"), "/")
		cur := o
		for _, p := range parts[:len(parts)-1] {
			next, ok := cur[p].(map[string]any)
			if !ok {
				t.Fatalf("%s: parent %q missing", op.Path, p)
			}
			cur = next
		}
		last := strings.NewReplacer("~1", "/", "~0", "~").Replace(parts[len(parts)-1])
		if _, exists := cur[last]; op.Op == "replace" && !exists {
			t.Fatalf("replace of missing %s", op.Path)
		}
		b, _ := json.Marshal(op.Value)
		var v any
		json.Unmarshal(b, &v)
		cur[last] = v
	}
	return o
}

func mustMutate(t *testing.T, kind, obj string, force bool) (map[string]any, string) {
	t.Helper()
	ops, why, err := mutate(cfg, kind, []byte(obj), force)
	if err != nil {
		t.Fatal(err)
	}
	if ops == nil {
		return nil, ""
	}
	return apply(t, obj, ops), why
}

func TestPodNodeSelector(t *testing.T) {
	o, why := mustMutate(t, "Pod", `{"metadata":{"name":"p"},"spec":{"nodeSelector":{"kubernetes.io/os":"linux","disk":"ssd"},"containers":[{"name":"c"}]}}`, false)
	if o == nil {
		t.Fatal("expected mutation")
	}
	spec := o["spec"].(map[string]any)
	if spec["runtimeClassName"] != "linux" {
		t.Errorf("runtimeClassName = %v", spec["runtimeClassName"])
	}
	ns := spec["nodeSelector"].(map[string]any)
	if _, ok := ns["kubernetes.io/os"]; ok || ns["disk"] != "ssd" || ns["bsdk8s.io/linux-abi"] != "true" {
		t.Errorf("nodeSelector = %v", ns)
	}
	ann := o["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann[annotation] != why || !strings.Contains(why, "nodeSelector kubernetes.io/os=linux") {
		t.Errorf("annotation = %v, why = %q", ann, why)
	}
}

func TestPodUntouched(t *testing.T) {
	for name, obj := range map[string]string{
		"no pin":          `{"spec":{"containers":[]}}`,
		"freebsd pin":     `{"spec":{"nodeSelector":{"kubernetes.io/os":"freebsd"}}}`,
		"runtimeClass":    `{"spec":{"runtimeClassName":"other","nodeSelector":{"kubernetes.io/os":"linux"}}}`,
		"preferred only":  `{"spec":{"affinity":{"nodeAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"weight":1,"preference":{"matchExpressions":[{"key":"kubernetes.io/os","operator":"In","values":["linux"]}]}}]}}}}`,
		"NotIn windows":   `{"spec":{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/os","operator":"NotIn","values":["windows"]}]}]}}}}}`,
		"already allowed": `{"spec":{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/os","operator":"In","values":["linux","freebsd"]}]}]}}}}}`,
	} {
		if o, _ := mustMutate(t, "Pod", obj, false); o != nil {
			t.Errorf("%s: unexpected mutation: %v", name, o)
		}
	}
	if o, _ := mustMutate(t, "Deployment", `{"spec":{"template":{"spec":{"nodeSelector":{"kubernetes.io/os":"linux"}}}}}`, false); o != nil {
		t.Error("Deployment must not be mutated (its pods are)")
	}
}

func TestPodAffinity(t *testing.T) {
	o, _ := mustMutate(t, "Pod", `{"metadata":{"annotations":{"a":"b"}},"spec":{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/os","operator":"In","values":["linux"]},{"key":"kubernetes.io/arch","operator":"In","values":["amd64","arm64"]}]}]}}}}}`, false)
	if o == nil {
		t.Fatal("expected mutation")
	}
	b, _ := json.Marshal(o["spec"].(map[string]any)["affinity"])
	if !strings.Contains(string(b), `"values":["freebsd"]`) || !strings.Contains(string(b), `["amd64","arm64"]`) {
		t.Errorf("affinity = %s", b)
	}
	ann := o["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann["a"] != "b" || ann[annotation] == nil {
		t.Errorf("annotations = %v", ann)
	}
}

func TestForce(t *testing.T) {
	obj := `{"spec":{"containers":[]}}`
	if o, _ := mustMutate(t, "Pod", obj, false); o != nil {
		t.Fatal("unpinned pod mutated without force")
	}
	o, why := mustMutate(t, "Pod", obj, true)
	if o == nil || o["spec"].(map[string]any)["runtimeClassName"] != "linux" || !strings.Contains(why, "namespace default") {
		t.Errorf("force: %v %q", o, why)
	}
}

func TestDaemonSet(t *testing.T) {
	o, _ := mustMutate(t, "DaemonSet", `{"metadata":{"name":"ds"},"spec":{"template":{"metadata":{"labels":{"app":"x"}},"spec":{"nodeSelector":{"kubernetes.io/os":"linux"}}}}}`, false)
	if o == nil {
		t.Fatal("expected mutation")
	}
	tmpl := o["spec"].(map[string]any)["template"].(map[string]any)
	if tmpl["spec"].(map[string]any)["runtimeClassName"] != "linux" {
		t.Errorf("template spec = %v", tmpl["spec"])
	}
	if tmpl["metadata"].(map[string]any)["annotations"].(map[string]any)[annotation] == nil {
		t.Error("template not annotated")
	}
	// Idempotent: the mutated object is left alone.
	b, _ := json.Marshal(o)
	if o2, _ := mustMutate(t, "DaemonSet", string(b), false); o2 != nil {
		t.Error("second pass mutated again")
	}
}

func TestHandler(t *testing.T) {
	review := `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{"uid":"u1","kind":{"group":"","version":"v1","kind":"Pod"},"namespace":"default","operation":"CREATE","object":{"metadata":{"generateName":"web-"},"spec":{"nodeSelector":{"kubernetes.io/os":"linux"}}}}}`
	rec := httptest.NewRecorder()
	handler(cfg)(rec, httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewBufferString(review)))
	var out admissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err, rec.Body.String())
	}
	r := out.Response
	if r == nil || r.UID != "u1" || !r.Allowed || r.PatchType != "JSONPatch" || len(r.Patch) == 0 || out.Request != nil {
		t.Fatalf("response = %s", rec.Body.String())
	}
	// Malformed object: still allowed, with a warning.
	rec = httptest.NewRecorder()
	handler(cfg)(rec, httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewBufferString(`{"request":{"uid":"u2","kind":{"kind":"Pod"},"object":"x"}}`)))
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Response.Allowed || len(out.Response.Warnings) == 0 {
		t.Errorf("malformed: %s", rec.Body.String())
	}
}

func TestHandlerForcePath(t *testing.T) {
	review := `{"request":{"uid":"u3","kind":{"kind":"Pod"},"object":{"spec":{"containers":[]}}}}`
	for path, want := range map[string]bool{"/mutate": false, "/mutate-force": true} {
		rec := httptest.NewRecorder()
		handler(cfg)(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(review)))
		var out admissionReview
		json.Unmarshal(rec.Body.Bytes(), &out)
		if got := len(out.Response.Patch) > 0; got != want {
			t.Errorf("%s: patched = %v, want %v", path, got, want)
		}
	}
}

func TestFindStuck(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	old := now.Add(-5 * time.Minute).Format(time.RFC3339)
	young := now.Add(-5 * time.Second).Format(time.RFC3339)
	pod := func(name, ns, created, phase, node, extra string) string {
		return `{"metadata":{"name":"` + name + `","namespace":"` + ns + `","uid":"u-` + name + `","creationTimestamp":"` + created + `"` + extra +
			`},"spec":{"nodeName":"` + node + `","nodeSelector":{"kubernetes.io/os":"linux"}},"status":{"phase":"` + phase + `"}}`
	}
	owned := `,"ownerReferences":[{"kind":"ReplicaSet","name":"rs","controller":true}]`
	list := `{"items":[` + strings.Join([]string{
		pod("stuck", "app", old, "Pending", "", owned),
		pod("bare", "app", old, "Pending", "", ""),
		pod("young", "app", young, "Pending", "", owned),
		pod("scheduled", "app", old, "Pending", "n1", owned),
		pod("ignored", "skip", old, "Pending", "", owned),
		pod("deleting", "app", old, "Pending", "", owned+`,"deletionTimestamp":"`+old+`"`),
		`{"metadata":{"name":"fbsd","namespace":"app","creationTimestamp":"` + old + `"},"spec":{"nodeSelector":{"kubernetes.io/os":"freebsd"}},"status":{"phase":"Pending"}}`,
	}, ",") + `]}`
	got, err := findStuck(cfg, []byte(list), func(ns string) bool { return ns == "skip" }, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, sp := range got {
		names[sp.meta.Name] = sp.owner
	}
	if len(names) != 2 || names["stuck"] != "ReplicaSet/rs" || names["bare"] != "" {
		t.Errorf("findStuck = %v, want stuck (ReplicaSet/rs) and bare", names)
	}
	if _, ok := names["bare"]; !ok {
		t.Error("bare pod not reported")
	}
}
