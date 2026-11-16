package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Image-aware placement ("auto" namespaces): ask the registry which
// platforms an image is published for. A pod whose images all have a
// FreeBSD variant runs natively; one with a Linux-only image is sent to the
// Linux RuntimeClass. Anonymous pulls only: images that need credentials
// (or any lookup error) leave the pod unchanged.

const (
	mtOCIIndex     = "application/vnd.oci.image.index.v1+json"
	mtDockerList   = "application/vnd.docker.distribution.manifest.list.v2+json"
	mtOCIManifest  = "application/vnd.oci.image.manifest.v1+json"
	mtDockerSchema = "application/vnd.docker.distribution.manifest.v2+json"
)

type imageRef struct {
	registry string // host[:port], "registry-1.docker.io" for Docker Hub
	repo     string
	ref      string // tag or digest
}

// parseImage splits an image reference the way containerd/Docker do.
func parseImage(image string) (imageRef, error) {
	if image == "" {
		return imageRef{}, errors.New("empty image")
	}
	name, ref := image, "latest"
	digest := ""
	if i := strings.Index(name, "@"); i >= 0 {
		name, digest = name[:i], name[i+1:]
	}
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, ref = name[:i], name[i+1:]
	}
	if digest != "" {
		ref = digest // name:tag@digest pulls by digest
	}
	reg := "docker.io"
	if i := strings.Index(name, "/"); i >= 0 {
		first := name[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			reg, name = first, name[i+1:]
		}
	}
	if reg == "docker.io" || reg == "index.docker.io" {
		reg = "registry-1.docker.io"
		if !strings.Contains(name, "/") {
			name = "library/" + name
		}
	}
	return imageRef{registry: reg, repo: name, ref: ref}, nil
}

type registryClient struct {
	http       *http.Client
	scheme     string // "https"; tests use "http"
	ttl        time.Duration
	maxEntries int
	sem        chan struct{} // bounds concurrent registry lookups
	mu         sync.Mutex
	cache      map[string]cacheEntry
	inflight   map[string]*inflightLookup
	nowFunc    func() time.Time
}

type inflightLookup struct {
	done chan struct{}
	oses map[string]bool
	err  error
}

// publicOnly refuses connections to loopback, private, link-local and other
// non-public addresses. Image references (and the auth realms and redirects
// registries return) are chosen by whoever creates the pod, so without this
// the webhook could be used to probe the cluster network.
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
		return fmt.Errorf("refusing to connect to non-public address %s", ip)
	}
	return nil
}

type cacheEntry struct {
	oses    map[string]bool
	err     error
	expires time.Time
}

func newRegistryClient(ttl time.Duration) *registryClient {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: publicOnly}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConnsPerHost: 4,
	}
	return &registryClient{
		http: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.URL.Scheme != "https" {
					return errors.New("refusing non-https redirect")
				}
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
		scheme:     "https",
		ttl:        ttl,
		maxEntries: 4096,
		sem:        make(chan struct{}, 8),
		cache:      map[string]cacheEntry{},
		inflight:   map[string]*inflightLookup{},
		nowFunc:    time.Now,
	}
}

// platforms returns the set of OSes the image is published for (e.g.
// {"linux":true,"freebsd":true}). Results, including errors, are cached.
func (c *registryClient) platforms(ctx context.Context, image string) (map[string]bool, error) {
	c.mu.Lock()
	if e, ok := c.cache[image]; ok && c.nowFunc().Before(e.expires) {
		c.mu.Unlock()
		return e.oses, e.err
	}
	// Coalesce concurrent lookups of the same image (e.g. a 100-replica
	// rollout): one registry request, everyone waits for it.
	if f, ok := c.inflight[image]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.oses, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &inflightLookup{done: make(chan struct{})}
	c.inflight[image] = f
	c.mu.Unlock()

	f.oses, f.err = c.boundedLookup(ctx, image)
	c.mu.Lock()
	delete(c.inflight, image)
	if ctx.Err() == nil { // don't cache our own deadline
		ttl := c.ttl
		if f.err != nil {
			ttl = c.ttl / 10 // retry failures sooner
		}
		c.evictLocked()
		c.cache[image] = cacheEntry{oses: f.oses, err: f.err, expires: c.nowFunc().Add(ttl)}
	}
	c.mu.Unlock()
	close(f.done)
	return f.oses, f.err
}

func (c *registryClient) boundedLookup(ctx context.Context, image string) (map[string]bool, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.lookup(ctx, image)
}

// evictLocked keeps the cache bounded: drop expired entries, and if it is
// still full, drop everything (simple, and lookups are cheap to redo).
func (c *registryClient) evictLocked() {
	if len(c.cache) < c.maxEntries {
		return
	}
	now := c.nowFunc()
	for k, e := range c.cache {
		if !now.Before(e.expires) {
			delete(c.cache, k)
		}
	}
	if len(c.cache) >= c.maxEntries {
		c.cache = map[string]cacheEntry{}
	}
}

func (c *registryClient) lookup(ctx context.Context, image string) (map[string]bool, error) {
	r, err := parseImage(image)
	if err != nil {
		return nil, err
	}
	var token string
	body, mt, err := c.get(ctx, r, "/manifests/"+r.ref, []string{mtOCIIndex, mtDockerList, mtOCIManifest, mtDockerSchema}, &token)
	if err != nil {
		return nil, err
	}
	var m struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Platform struct {
				OS string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("%s: decode manifest: %w", image, err)
	}
	if mt == "" || strings.HasPrefix(mt, "text/") || mt == "application/json" {
		mt = m.MediaType
	}
	oses := map[string]bool{}
	switch mt {
	case mtOCIIndex, mtDockerList:
		for _, d := range m.Manifests {
			if d.Platform.OS != "" && d.Platform.OS != "unknown" { // "unknown" = attestations
				oses[d.Platform.OS] = true
			}
		}
	case mtOCIManifest, mtDockerSchema:
		if m.Config.Digest == "" {
			return nil, fmt.Errorf("%s: manifest without config", image)
		}
		cfg, _, err := c.get(ctx, r, "/blobs/"+m.Config.Digest, nil, &token)
		if err != nil {
			return nil, err
		}
		var ic struct {
			OS string `json:"os"`
		}
		if err := json.Unmarshal(cfg, &ic); err != nil || ic.OS == "" {
			return nil, fmt.Errorf("%s: no os in image config", image)
		}
		oses[ic.OS] = true
	default:
		return nil, fmt.Errorf("%s: unsupported manifest type %q", image, mt)
	}
	if len(oses) == 0 {
		return nil, fmt.Errorf("%s: no platforms in index", image)
	}
	return oses, nil
}

// get fetches /v2/<repo><path>, doing the anonymous Bearer token dance once
// if the registry asks for it.
func (c *registryClient) get(ctx context.Context, r imageRef, path string, accept []string, token *string) ([]byte, string, error) {
	u := c.scheme + "://" + r.registry + "/v2/" + r.repo + path
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, "", err
		}
		if len(accept) > 0 {
			req.Header.Set("Accept", strings.Join(accept, ", "))
		}
		if *token != "" {
			req.Header.Set("Authorization", "Bearer "+*token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, "", err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && *token == "" {
			t, err := c.anonymousToken(ctx, resp.Header.Get("WWW-Authenticate"))
			if err != nil {
				return nil, "", fmt.Errorf("%s/%s: %w", r.registry, r.repo, err)
			}
			*token = t
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("GET %s: %s", u, resp.Status)
		}
		return data, strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]), nil
	}
	return nil, "", fmt.Errorf("GET %s: unauthorized", u)
}

func (c *registryClient) anonymousToken(ctx context.Context, challenge string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("unsupported auth challenge %q", challenge)
	}
	p := map[string]string{}
	for _, kv := range splitParams(params) {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			p[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	realm, err := url.Parse(p["realm"])
	if err != nil || realm.Host == "" {
		return "", errors.New("auth challenge without a valid realm")
	}
	if realm.Scheme != c.scheme {
		return "", fmt.Errorf("refusing %s auth realm", realm.Scheme)
	}
	q := realm.Query()
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	if p["scope"] != "" {
		q.Set("scope", p["scope"])
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint: %s", resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", err
	}
	if t.Token != "" {
		return t.Token, nil
	}
	if t.AccessToken != "" {
		return t.AccessToken, nil
	}
	return "", errors.New("token endpoint returned no token")
}

// splitParams splits `a="x,y",b=z` on commas outside quotes.
func splitParams(s string) []string {
	var out []string
	inQuote, start := false, 0
	for i, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ',' && !inQuote:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// imageDecision is the outcome of image-aware placement for a pod.
type imageDecision int

const (
	keepNative imageDecision = iota // every image has a FreeBSD variant
	useLinux                        // some image is Linux-only, none FreeBSD-only
	undecided                       // lookup failed or images conflict
)

// decideImages looks at every container image of a pod spec.
func decideImages(ctx context.Context, rc *registryClient, nodeOS string, images []string) (imageDecision, string) {
	needLinux, needNative := []string{}, []string{}
	for _, img := range images {
		oses, err := rc.platforms(ctx, img)
		if err != nil {
			return undecided, err.Error()
		}
		switch {
		case oses[nodeOS]:
			// runs natively
		case oses["linux"]:
			needLinux = append(needLinux, img)
		default:
			needNative = append(needNative, img) // e.g. windows-only
		}
	}
	switch {
	case len(needNative) > 0:
		return undecided, "no " + nodeOS + " or linux variant: " + strings.Join(needNative, ", ")
	case len(needLinux) > 0:
		return useLinux, "linux-only image: " + strings.Join(needLinux, ", ")
	}
	return keepNative, ""
}

// podImages returns the images of all (init, ephemeral-free) containers.
func podImages(spec map[string]any) []string {
	var out []string
	for _, key := range []string{"initContainers", "containers"} {
		cs, _ := spec[key].([]any)
		for _, c := range cs {
			m, _ := c.(map[string]any)
			if img, _ := m["image"].(string); img != "" {
				out = append(out, img)
			}
		}
	}
	return out
}
