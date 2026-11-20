package main

import (
	"path/filepath"
	"strings"
)

// hostMount is an entry of the host's mount table.
type hostMount struct {
	From, On, Type string
	ReadOnly       bool
}

// view is a caller's view of the file system: its jail's root and the mounts
// below it. The host (jail 0) has root "/" and sees everything as is.
type view struct {
	root   string
	mounts []hostMount // the host's mounts at or below root
}

func newView(root string, all []hostMount) *view {
	v := &view{root: filepath.Clean(root)}
	for _, m := range all {
		if under(m.On, v.root) {
			v.mounts = append(v.mounts, m)
		}
	}
	return v
}

// under reports whether p is dir or below it.
func under(p, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// rel returns p relative to dir ("" for dir itself), p under dir.
func rel(p, dir string) string {
	if dir == "/" {
		return strings.TrimPrefix(p, "/")
	}
	return strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
}

// toHost translates an absolute path in the caller's view to the host path
// it names: through the nullfs mount that covers it, if any, else below the
// jail root. Symbolic links are not followed.
func (v *view) toHost(p string) string {
	p = filepath.Clean("/" + p)
	host := filepath.Join(v.root, p)
	best := ""
	from := ""
	for _, m := range v.mounts {
		if m.Type != "nullfs" || !under(host, m.On) || len(m.On) <= len(best) {
			continue
		}
		best, from = m.On, m.From
	}
	if best == "" {
		return host
	}
	return filepath.Join(from, rel(host, best))
}

// fromHost translates a host path to the caller's view, if it can see it.
func (v *view) fromHost(h string) (string, bool) {
	if under(h, v.root) {
		return filepath.Join("/", rel(h, v.root)), true
	}
	// Seen through a nullfs mount of the jail: the longest source wins.
	best := hostMount{}
	for _, m := range v.mounts {
		if m.Type == "nullfs" && under(h, m.From) && len(m.From) > len(best.From) {
			best = m
		}
	}
	if best.From == "" {
		return "", false
	}
	return filepath.Join("/", rel(best.On, v.root), rel(h, best.From)), true
}

// mountOptions turns Linux-style mount options into FreeBSD ones, as
// k8s.io/mount-utils does on FreeBSD: a bind mount is nullfs.
func mountOptions(fstype string, options []string) (string, []string, bool) {
	bind := fstype == "nullfs"
	var opts []string
	for _, o := range options {
		switch o {
		case "bind", "rbind":
			bind = true
		case "remount", "rprivate", "private", "rshared", "shared", "rslave", "slave", "defaults":
		default:
			opts = append(opts, o)
		}
	}
	if bind {
		fstype = "nullfs"
	}
	return fstype, opts, bind
}
