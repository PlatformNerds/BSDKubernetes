package main

import "testing"

func TestTranslate(t *testing.T) {
	root := "/var/db/containers/storage/zfs/graph/abc"
	all := []hostMount{
		{From: "zroot/ROOT/default", On: "/", Type: "zfs"},
		{From: "zroot/c/abc", On: root, Type: "zfs"},
		{From: "/var/db/kubelet/pods", On: root + "/var/db/kubelet/pods", Type: "nullfs"},
		{From: "/var/db/kubelet/plugins", On: root + "/var/db/kubelet/plugins", Type: "nullfs"},
		{From: "/var/lib/csi-hostpath-data", On: root + "/csi-data-dir", Type: "nullfs"},
		{From: "devfs", On: root + "/dev", Type: "devfs"},
		{From: "/var/lib/csi-hostpath-data/v1", On: "/var/db/kubelet/pods/p/volumes/kubernetes.io~csi/pv/mount", Type: "nullfs"},
		{From: "zroot/other", On: "/var/db/other", Type: "zfs"},
	}
	v := newView(root, all)
	for in, want := range map[string]string{
		"/var/db/kubelet/pods/p/volumes/kubernetes.io~csi/pv/mount": "/var/db/kubelet/pods/p/volumes/kubernetes.io~csi/pv/mount",
		"/csi-data-dir/v1": "/var/lib/csi-hostpath-data/v1",
		"/csi-data-dir":    "/var/lib/csi-hostpath-data",
		"/etc/passwd":      root + "/etc/passwd",
		"/dev/null":        root + "/dev/null",
		"/../../etc":       root + "/etc",
	} {
		if got := v.toHost(in); got != want {
			t.Errorf("toHost(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"/var/db/kubelet/pods/p/volumes/kubernetes.io~csi/pv/mount": "/var/db/kubelet/pods/p/volumes/kubernetes.io~csi/pv/mount",
		"/var/lib/csi-hostpath-data/v1":                             "/csi-data-dir/v1",
		root + "/dev":                                               "/dev",
		root:                                                        "/",
	} {
		if got, ok := v.fromHost(in); !ok || got != want {
			t.Errorf("fromHost(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"/var/db/other", "/"} {
		if got, ok := v.fromHost(in); ok {
			t.Errorf("fromHost(%q) = %q, want invisible", in, got)
		}
	}
	host := newView("/", all)
	if got := host.toHost("/var/db/other"); got != "/var/db/other" {
		t.Errorf("host toHost = %q", got)
	}
}

func TestMountOptions(t *testing.T) {
	fs, opts, bind := mountOptions("", []string{"bind", "ro"})
	if fs != "nullfs" || !bind || len(opts) != 1 || opts[0] != "ro" {
		t.Errorf("bind,ro -> %q %v %v", fs, opts, bind)
	}
	fs, opts, bind = mountOptions("tmpfs", []string{"size=1m"})
	if fs != "tmpfs" || bind || len(opts) != 1 {
		t.Errorf("tmpfs -> %q %v %v", fs, opts, bind)
	}
}
