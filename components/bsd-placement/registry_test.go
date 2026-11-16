package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseImage(t *testing.T) {
	for in, want := range map[string]imageRef{
		"nginx":                            {"registry-1.docker.io", "library/nginx", "latest"},
		"nginx:1.27-alpine":                {"registry-1.docker.io", "library/nginx", "1.27-alpine"},
		"bitnami/redis:7":                  {"registry-1.docker.io", "bitnami/redis", "7"},
		"docker.io/library/busybox":        {"registry-1.docker.io", "library/busybox", "latest"},
		"quay.io/jetstack/cert-manager:v1": {"quay.io", "jetstack/cert-manager", "v1"},
		"localhost:5000/x/y:z":             {"localhost:5000", "x/y", "z"},
		"registry.k8s.io/pause@sha256:abc": {"registry.k8s.io", "pause", "sha256:abc"},
		"ghcr.io/o/r:v1@sha256:def":        {"ghcr.io", "o/r", "sha256:def"},
		"nginx:1.25@sha256:abc":            {"registry-1.docker.io", "library/nginx", "sha256:abc"},
	} {
		got, err := parseImage(in)
		if err != nil || got != want {
			t.Errorf("parseImage(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
}

// fakeRegistry serves:
//
//	multi:v1  index with linux + freebsd (+ an attestation "unknown")
//	lnx:v1    index with linux only
//	single:v1 single OCI manifest whose config says linux
//
// and requires an anonymous Bearer token like Docker Hub.
func fakeRegistry(t *testing.T, hits *int32) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if r.URL.Query().Get("scope") == "" {
				http.Error(w, "no scope", 400)
				return
			}
			w.Write([]byte(`{"token":"t0k"}`))
			return
		}
		atomic.AddInt32(hits, 1)
		if r.Header.Get("Authorization") != "Bearer t0k" {
			repo := strings.TrimSuffix(strings.SplitN(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/", 2)[0], "/")
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="fake",scope="repository:`+repo+`:pull"`)
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/v2/o/multi/manifests/v1":
			w.Header().Set("Content-Type", mtOCIIndex)
			w.Write([]byte(`{"manifests":[{"platform":{"os":"linux","architecture":"arm64"}},{"platform":{"os":"freebsd","architecture":"arm64"}},{"platform":{"os":"unknown"}}]}`))
		case "/v2/o/lnx/manifests/v1":
			w.Header().Set("Content-Type", mtDockerList)
			w.Write([]byte(`{"manifests":[{"platform":{"os":"linux"}},{"platform":{"os":"unknown"}}]}`))
		case "/v2/o/single/manifests/v1":
			w.Header().Set("Content-Type", mtOCIManifest)
			w.Write([]byte(`{"config":{"digest":"sha256:cfg"}}`))
		case "/v2/o/single/blobs/sha256:cfg":
			w.Write([]byte(`{"os":"linux","architecture":"arm64"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

func testClient(srv *httptest.Server) (*registryClient, string) {
	rc := newRegistryClient(time.Minute)
	rc.scheme = "http"
	rc.http.Transport = http.DefaultTransport // the fake registry is on loopback
	return rc, strings.TrimPrefix(srv.URL, "http://")
}

func TestPlatforms(t *testing.T) {
	var hits int32
	srv := fakeRegistry(t, &hits)
	defer srv.Close()
	rc, host := testClient(srv)
	ctx := context.Background()
	for img, want := range map[string]string{"/o/multi:v1": "freebsd,linux", "/o/lnx:v1": "linux", "/o/single:v1": "linux"} {
		oses, err := rc.platforms(ctx, host+img)
		if err != nil {
			t.Fatalf("%s: %v", img, err)
		}
		got := strings.Join(sortedKeys(boolsToStrings(oses)), ",")
		if got != want {
			t.Errorf("%s: platforms = %s, want %s", img, got, want)
		}
	}
	if _, err := rc.platforms(ctx, host+"/o/missing:v1"); err == nil {
		t.Error("missing image: expected error")
	}
	before := atomic.LoadInt32(&hits)
	rc.platforms(ctx, host+"/o/multi:v1")
	if atomic.LoadInt32(&hits) != before {
		t.Error("cached lookup hit the registry")
	}
}

func boolsToStrings(m map[string]bool) map[string]string {
	out := map[string]string{}
	for k := range m {
		out[k] = ""
	}
	return out
}

func TestDecideAndAutoMutate(t *testing.T) {
	var hits int32
	srv := fakeRegistry(t, &hits)
	defer srv.Close()
	rc, host := testClient(srv)
	decide := func(images []string) (imageDecision, string) {
		return decideImages(context.Background(), rc, "freebsd", images)
	}
	pod := func(images ...string) string {
		var cs []string
		for _, i := range images {
			cs = append(cs, `{"name":"c","image":"`+host+i+`"}`)
		}
		return `{"metadata":{"name":"p"},"spec":{"containers":[` + strings.Join(cs, ",") + `]}}`
	}
	cases := []struct {
		images  []string
		mutated bool
		warn    bool
	}{
		{[]string{"/o/multi:v1"}, false, false},                // has a FreeBSD variant: native
		{[]string{"/o/lnx:v1"}, true, false},                   // Linux-only: RuntimeClass linux
		{[]string{"/o/multi:v1", "/o/single:v1"}, true, false}, // one Linux-only image decides
		{[]string{"/o/missing:v1"}, false, true},               // lookup error: unchanged + warning
	}
	for _, c := range cases {
		ops, why, warning, err := mutate(cfg, "Pod", []byte(pod(c.images...)), autoImages, decide)
		if err != nil {
			t.Fatal(err)
		}
		if (ops != nil) != c.mutated || (warning != "") != c.warn {
			t.Errorf("%v: mutated=%v (%q) warning=%q", c.images, ops != nil, why, warning)
		}
		if c.mutated && !strings.Contains(why, "linux-only image") {
			t.Errorf("%v: why = %q", c.images, why)
		}
	}
	// pinnedOnly ignores images entirely.
	if ops, _, _, _ := mutate(cfg, "Pod", []byte(pod("/o/lnx:v1")), pinnedOnly, decide); ops != nil {
		t.Error("pinnedOnly mutated an unpinned pod")
	}
}

func TestSplitParams(t *testing.T) {
	got := splitParams(`realm="https://a/token",service="reg",scope="repository:a/b:pull,push"`)
	if len(got) != 3 || !strings.HasSuffix(got[2], `pull,push"`) {
		t.Errorf("splitParams = %q", got)
	}
}

func TestPublicOnly(t *testing.T) {
	for addr, ok := range map[string]bool{
		"8.8.8.8:443": true, "[2606:4700::1111]:443": true,
		"127.0.0.1:443": false, "10.96.0.1:443": false, "192.168.100.1:22": false, "169.254.169.254:80": false,
		"[::1]:443": false, "[fe80::1]:443": false, "100.64.1.1:443": false, "0.0.0.0:443": false, "[::ffff:10.0.0.1]:443": false,
	} {
		if err := publicOnly("tcp", addr, nil); (err == nil) != ok {
			t.Errorf("publicOnly(%s) = %v, want allowed=%v", addr, err, ok)
		}
	}
	// The real client refuses a registry on loopback.
	rc := newRegistryClient(time.Minute)
	rc.scheme = "http"
	if _, err := rc.platforms(context.Background(), "127.0.0.1:1/x:y"); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Errorf("loopback registry: err = %v", err)
	}
}

func TestCoalescingAndEviction(t *testing.T) {
	var hits int32
	srv := fakeRegistry(t, &hits)
	defer srv.Close()
	rc, host := testClient(srv)
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() { rc.platforms(context.Background(), host+"/o/lnx:v1"); done <- struct{}{} }()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	// 401 + authorized retry = 2 hits for one lookup; 20 uncoalesced would be 40.
	if h := atomic.LoadInt32(&hits); h > 4 {
		t.Errorf("20 concurrent lookups made %d registry requests", h)
	}
	rc.maxEntries = 3
	for i := 0; i < 10; i++ {
		rc.mu.Lock()
		rc.evictLocked()
		rc.cache[strings.Repeat("x", i+1)] = cacheEntry{expires: time.Now().Add(time.Hour)}
		rc.mu.Unlock()
	}
	if n := len(rc.cache); n > 3 {
		t.Errorf("cache has %d entries, max 3", n)
	}
}

func TestRealmWithQuery(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"access_token":"a"}`))
	}))
	defer srv.Close()
	rc, _ := testClient(srv)
	tok, err := rc.anonymousToken(context.Background(), `Bearer realm="`+srv.URL+`/t?x=1",service="s",scope="repository:a:pull"`)
	if err != nil || tok != "a" || !strings.Contains(gotQuery, "x=1") || !strings.Contains(gotQuery, "service=s") {
		t.Errorf("token=%q err=%v query=%q", tok, err, gotQuery)
	}
	if _, err := rc.anonymousToken(context.Background(), `Bearer realm="ftp://x/t"`); err == nil {
		t.Error("non-http(s) realm accepted")
	}
}
