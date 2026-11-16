package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Minimal admission.k8s.io/v1 AdmissionReview types (no client-go needed).
type admissionReview struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Request    *admissionRequest  `json:"request,omitempty"`
	Response   *admissionResponse `json:"response,omitempty"`
}

type admissionRequest struct {
	UID       string                `json:"uid"`
	Kind      struct{ Kind string } `json:"kind"`
	Namespace string                `json:"namespace"`
	Name      string                `json:"name"`
	Operation string                `json:"operation"`
	Object    json.RawMessage       `json:"object"`
}

type admissionResponse struct {
	UID       string            `json:"uid"`
	Allowed   bool              `json:"allowed"`
	Patch     []byte            `json:"patch,omitempty"`
	PatchType string            `json:"patchType,omitempty"`
	Warnings  []string          `json:"warnings,omitempty"`
	AuditAnn  map[string]string `json:"auditAnnotations,omitempty"`
}

func handler(cfg Config, rc *registryClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var review admissionReview
		if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
			http.Error(w, "bad AdmissionReview", http.StatusBadRequest)
			return
		}
		req := review.Request
		m := pinnedOnly
		switch r.URL.Path {
		case "/mutate-force":
			m = forceLinux
		case "/mutate-auto":
			m = autoImages
		}
		var decide decider
		if rc != nil {
			// Stay well inside the webhook timeout; on timeout the pod is
			// left unchanged (and the lookup is retried next time).
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			decide = func(images []string) (imageDecision, string) {
				return decideImages(ctx, rc, cfg.NodeOS, cfg.NodeArch, images)
			}
		}
		resp := &admissionResponse{UID: req.UID, Allowed: true}
		ops, why, warning, err := mutate(cfg, req.Kind.Kind, req.Object, m, decide)
		if warning != "" {
			resp.Warnings = append(resp.Warnings, warning)
			log.Printf("%s %s %s/%s: %s", req.Operation, req.Kind.Kind, req.Namespace, nameOf(req), warning)
		}
		switch {
		case err != nil:
			// Never block admission: placement is best effort.
			log.Printf("%s %s/%s: %v", req.Kind.Kind, req.Namespace, req.Name, err)
			resp.Warnings = []string{"bsd-placement: " + err.Error()}
		case ops != nil:
			patch, _ := json.Marshal(ops)
			resp.Patch, resp.PatchType = patch, "JSONPatch"
			resp.AuditAnn = map[string]string{"placement": why}
			log.Printf("%s %s %s/%s: %s", req.Operation, req.Kind.Kind, req.Namespace, nameOf(req), why)
		}
		review.Request, review.Response = nil, resp
		out, _ := json.Marshal(review)
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}
}

func nameOf(req *admissionRequest) string {
	if req.Name != "" {
		return req.Name
	}
	var o struct {
		Metadata struct{ GenerateName string } `json:"metadata"`
	}
	json.Unmarshal(req.Object, &o)
	return o.Metadata.GenerateName + "*"
}

func main() {
	addr := flag.String("listen", ":8443", "HTTPS listen address")
	cert := flag.String("tls-cert", "/certs/tls.crt", "serving certificate")
	key := flag.String("tls-key", "/certs/tls.key", "serving key")
	rc := flag.String("runtime-class", "linux", "RuntimeClass for Linux workloads")
	sel := flag.String("node-selector", "bsdk8s.io/linux-abi=true", "nodeSelector added to rewritten pods (k=v,...)")
	nodeOS := flag.String("node-os", "freebsd", "kubernetes.io/os value of the target nodes")
	nodeArch := flag.String("node-arch", runtime.GOARCH, "architecture of the target nodes for image lookups (\"\" = any)")
	cacheTTL := flag.Duration("image-cache-ttl", 10*time.Minute, "how long to cache an image's platforms (auto namespaces)")
	every := flag.Duration("reconcile-interval", 30*time.Second, "how often to look for Linux-pinned pods admitted without the webhook (0 disables)")
	minAge := flag.Duration("reconcile-min-age", time.Minute, "only act on pods pending at least this long")
	flag.Parse()

	ns, err := parseSelector(*sel)
	if err != nil {
		log.Fatal(err)
	}
	cfg := Config{RuntimeClass: *rc, NodeSelector: ns, NodeOS: *nodeOS, NodeArch: *nodeArch}
	mux := http.NewServeMux()
	reg := newRegistryClient(*cacheTTL)
	h := handler(cfg, reg)
	mux.Handle("/mutate", h)
	mux.Handle("/mutate-force", h)
	mux.Handle("/mutate-auto", h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if *every > 0 {
		c, err := inClusterClient()
		if err != nil {
			log.Printf("reconciler disabled: %v", err)
		} else {
			self, _ := os.ReadFile(saDir + "/namespace")
			go reconcileLoop(ctx, c, cfg, strings.TrimSpace(string(self)), *every, *minAge)
		}
	}
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("bsd-placement: runtimeClass=%s nodeSelector=%s nodeOS=%s listening on %s",
		cfg.RuntimeClass, strings.Join(sortedKeys(ns), ","), cfg.NodeOS, *addr)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServeTLS(*cert, *key); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Print("bsd-placement: stopped")
}
