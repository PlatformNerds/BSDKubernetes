// Package main is bsdk8s-mount-proxy: mounts for CSI node plugins in jails.
//
// FreeBSD has no mount propagation. A CSI node plugin runs in a jail and sees
// the kubelet's directories through nullfs, so a mount it makes there is
// invisible to the kubelet and the container runtime. On Linux the plugin's
// pod uses mountPropagation: Bidirectional; Windows has the same problem and
// solves it with csi-proxy, a host service that mounts on the plugin's
// behalf. This is FreeBSD's: the plugin (through k8s.io/mount-utils, built
// from the BSDKubernetes kubernetes fork) sends mount, unmount and list
// requests over a unix socket, and the proxy runs them on the host, with the
// caller's paths translated to host paths.
package main

// Request is one request; one per connection, JSON.
type Request struct {
	Op      string   `json:"op"` // "mount", "unmount", "list"
	Source  string   `json:"source,omitempty"`
	Target  string   `json:"target,omitempty"`
	FSType  string   `json:"fstype,omitempty"`
	Options []string `json:"options,omitempty"`
}

// MountPoint is a mount as the caller sees it.
type MountPoint struct {
	Device string   `json:"device"`
	Path   string   `json:"path"`
	Type   string   `json:"type"`
	Opts   []string `json:"opts,omitempty"`
}

// Response answers a Request.
type Response struct {
	Error  string       `json:"error,omitempty"`
	Mounts []MountPoint `json:"mounts,omitempty"`
}
