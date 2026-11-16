package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The webhook uses failurePolicy Ignore, so a pod created while it is
// unavailable (e.g. evicted in the same drain as the workloads) is admitted
// unchanged and, pinned to kubernetes.io/os=linux, stays Pending forever:
// runtimeClassName cannot be changed on an existing pod.  The reconciler
// finds such pods and deletes those a controller will recreate, so the new
// pod goes through the webhook.  Bare pods are only reported.

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

type apiClient struct {
	base  string
	token string
	http  *http.Client
}

func inClusterClient() (*apiClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in a cluster")
	}
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no CA certificates in %s/ca.crt", saDir)
	}
	return &apiClient{
		base:  "https://" + net.JoinHostPort(host, port),
		token: strings.TrimSpace(string(token)),
		http: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
	}, nil
}

func (c *apiClient) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type podMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	Labels            map[string]string `json:"labels"`
	OwnerReferences   []struct {
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		Controller *bool  `json:"controller"`
	} `json:"ownerReferences"`
}

// stuckPod is a pending, unscheduled pod the webhook would have rewritten.
type stuckPod struct {
	meta  podMeta
	owner string // "Kind/name" of the controlling owner, "" for a bare pod
}

// findStuck returns the pods in list (a PodList) that the webhook would
// rewrite, are older than minAge, not being deleted, and in a namespace that
// is not excluded.
func findStuck(cfg Config, list []byte, excluded func(ns string) bool, now time.Time, minAge time.Duration) ([]stuckPod, error) {
	var pl struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(list, &pl); err != nil {
		return nil, err
	}
	var out []stuckPod
	for _, raw := range pl.Items {
		var p struct {
			Metadata podMeta `json:"metadata"`
			Spec     struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		m := p.Metadata
		if p.Status.Phase != "Pending" || p.Spec.NodeName != "" || m.DeletionTimestamp != nil ||
			now.Sub(m.CreationTimestamp) < minAge || excluded(m.Namespace) {
			continue
		}
		ops, _, err := mutate(cfg, "Pod", raw, false)
		if err != nil || ops == nil {
			continue
		}
		sp := stuckPod{meta: m}
		for _, o := range m.OwnerReferences {
			if o.Controller != nil && *o.Controller {
				sp.owner = o.Kind + "/" + o.Name
			}
		}
		out = append(out, sp)
	}
	return out, nil
}

func reconcileLoop(ctx context.Context, c *apiClient, cfg Config, self string, every, minAge time.Duration) {
	reported := map[string]bool{}
	for {
		if err := reconcileOnce(ctx, c, cfg, self, minAge, reported); err != nil {
			log.Printf("reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func reconcileOnce(ctx context.Context, c *apiClient, cfg Config, self string, minAge time.Duration, reported map[string]bool) error {
	q := url.Values{"fieldSelector": {"status.phase=Pending,spec.nodeName="}}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/api/v1/pods?"+q.Encode(), nil, &raw); err != nil {
		return err
	}
	nsIgnored := map[string]bool{}
	excluded := func(ns string) bool {
		if ns == self {
			return true
		}
		if v, ok := nsIgnored[ns]; ok {
			return v
		}
		var n struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		ignored := false
		if err := c.do(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(ns), nil, &n); err != nil {
			ignored = true // unknown: leave it alone
		} else {
			ignored = n.Metadata.Labels["bsdk8s.io/placement"] == "ignore"
		}
		nsIgnored[ns] = ignored
		return ignored
	}
	stuck, err := findStuck(cfg, raw, excluded, time.Now(), minAge)
	if err != nil {
		return err
	}
	for _, sp := range stuck {
		m := sp.meta
		if sp.owner == "" {
			if !reported[m.UID] {
				log.Printf("reconcile: bare pod %s/%s is pinned to Linux but was admitted without the webhook; recreate it", m.Namespace, m.Name)
				reported[m.UID] = true
			}
			continue
		}
		// Precondition on the UID: never delete a newer pod of the same name.
		body := fmt.Sprintf(`{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"uid":%q}}`, m.UID)
		path := "/api/v1/namespaces/" + url.PathEscape(m.Namespace) + "/pods/" + url.PathEscape(m.Name)
		if err := c.do(ctx, http.MethodDelete, path, strings.NewReader(body), nil); err != nil {
			log.Printf("reconcile: delete %s/%s: %v", m.Namespace, m.Name, err)
			continue
		}
		log.Printf("reconcile: deleted %s/%s (owner %s): Linux-pinned, admitted while the webhook was unavailable", m.Namespace, m.Name, sp.owner)
	}
	return nil
}
