//go:build freebsd

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

var (
	socketPaths = flag.String("socket", "", "comma-separated unix sockets to listen on (default: bsdk8s-mount-proxy.sock in the kubelet's plugins/ and plugins/kubernetes.io/csi/)")
	kubeletRoot = flag.String("kubelet-root-dir", "/var/db/kubelet", "the kubelet's root directory: mount targets must be below its pods/ or plugins/")
)

// defaultSockets are where CSI node plugins can reach the proxy. Some mount
// the whole plugins/ directory (csi-driver-host-path); most mount only
// plugins/kubernetes.io/csi/ (the kubelet's staging directory) and their own
// plugins/<driver>/ (the e2e mock driver, EBS, GCE PD, ...).
func defaultSockets(root string) []string {
	return []string{
		filepath.Join(root, "plugins", "bsdk8s-mount-proxy.sock"),
		filepath.Join(root, "plugins", "kubernetes.io", "csi", "bsdk8s-mount-proxy.sock"),
	}
}

func listen(path string) net.Listener {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		log.Fatal(err)
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		log.Fatal(err)
	}
	// root only: the CSI plugin containers run as root
	if err := os.Chmod(path, 0o600); err != nil {
		log.Fatal(err)
	}
	return l
}

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	paths := defaultSockets(*kubeletRoot)
	if *socketPaths != "" {
		paths = strings.Split(*socketPaths, ",")
	}
	for _, p := range paths {
		l := listen(p)
		log.Printf("listening on %s, kubelet root %s", p, *kubeletRoot)
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					log.Printf("accept: %v", err)
					continue
				}
				go serve(c.(*net.UnixConn))
			}
		}()
	}
	select {}
}

func serve(c *net.UnixConn) {
	defer c.Close()
	var req Request
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		log.Printf("bad request: %v", err)
		return
	}
	resp := handle(c, &req)
	if err := json.NewEncoder(c).Encode(resp); err != nil {
		log.Printf("reply: %v", err)
	}
}

func handle(c *net.UnixConn, req *Request) *Response {
	pid, err := peerPid(c)
	if err != nil {
		return fail("peer credentials: %v", err)
	}
	v, err := callerView(pid)
	if err != nil {
		return fail("caller pid %d: %v", pid, err)
	}
	switch req.Op {
	case "list":
		all, err := hostMounts()
		if err != nil {
			return fail("%v", err)
		}
		var mps []MountPoint
		for _, m := range all {
			p, ok := v.fromHost(m.On)
			if !ok {
				continue
			}
			opts := []string{"rw"}
			if m.ReadOnly {
				opts = []string{"ro"}
			}
			dev := m.From
			if m.Type == "nullfs" {
				if d, ok := v.fromHost(m.From); ok {
					dev = d
				}
			}
			mps = append(mps, MountPoint{Device: dev, Path: p, Type: m.Type, Opts: opts})
		}
		return &Response{Mounts: mps}
	case "mount":
		target := v.toHost(req.Target)
		if !allowedTarget(target) {
			return fail("mount target %s (%s on the host) is not below %s/pods or %s/plugins", req.Target, target, *kubeletRoot, *kubeletRoot)
		}
		fstype, opts, bind := mountOptions(req.FSType, req.Options)
		source := req.Source
		if bind {
			source = v.toHost(req.Source)
		}
		args := []string{"-t", fstype}
		if len(opts) > 0 {
			args = append(args, "-o", strings.Join(opts, ","))
		}
		args = append(args, source, target)
		log.Printf("pid %d (root %s): mount %s", pid, v.root, strings.Join(args, " "))
		if out, err := exec.Command("/sbin/mount", args...).CombinedOutput(); err != nil {
			return fail("mount %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return &Response{}
	case "unmount":
		target := v.toHost(req.Target)
		if !allowedTarget(target) {
			return fail("unmount target %s (%s on the host) is not below %s/pods or %s/plugins", req.Target, target, *kubeletRoot, *kubeletRoot)
		}
		log.Printf("pid %d (root %s): umount %s", pid, v.root, target)
		if out, err := exec.Command("/sbin/umount", target).CombinedOutput(); err != nil {
			return fail("umount %s: %v: %s", target, err, strings.TrimSpace(string(out)))
		}
		return &Response{}
	}
	return fail("unknown op %q", req.Op)
}

func fail(format string, a ...any) *Response {
	msg := fmt.Sprintf(format, a...)
	log.Print(msg)
	return &Response{Error: msg}
}

func allowedTarget(t string) bool {
	root := filepath.Clean(*kubeletRoot)
	return strings.HasPrefix(t, root+"/pods/") || strings.HasPrefix(t, root+"/plugins/")
}

// peerPid returns the pid of the process on the other end of c (FreeBSD's
// struct xucred has it in cr_pid, which x/sys's Xucred leaves unnamed).
func peerPid(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Xucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	if cred.Uid != 0 {
		return 0, fmt.Errorf("uid %d is not root", cred.Uid)
	}
	// cr_pid shares a union with a pointer after cr_groups.
	const crPidOffset = 80
	if unsafe.Sizeof(*cred) < crPidOffset+4 {
		return 0, errors.New("unexpected struct xucred layout")
	}
	pid := *(*int32)(unsafe.Add(unsafe.Pointer(cred), crPidOffset))
	if pid <= 0 {
		return 0, errors.New("no pid in the peer credentials")
	}
	return int(pid), nil
}

// callerView returns the file system view of the jail process pid runs in.
func callerView(pid int) (*view, error) {
	out, err := exec.Command("/bin/ps", "-o", "jid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %v", err)
	}
	jid := strings.TrimSpace(string(out))
	root := "/"
	if jid != "0" {
		out, err := exec.Command("/usr/sbin/jls", "-j", jid, "path").Output()
		if err != nil {
			return nil, fmt.Errorf("jls -j %s: %v", jid, err)
		}
		root = strings.TrimSpace(string(out))
	}
	all, err := hostMounts()
	if err != nil {
		return nil, err
	}
	return newView(root, all), nil
}

func hostMounts() ([]hostMount, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	buf := make([]unix.Statfs_t, n)
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	ms := make([]hostMount, 0, n)
	for _, s := range buf[:n] {
		ms = append(ms, hostMount{
			From:     unix.ByteSliceToString(s.Mntfromname[:]),
			On:       unix.ByteSliceToString(s.Mntonname[:]),
			Type:     unix.ByteSliceToString(s.Fstypename[:]),
			ReadOnly: s.Flags&unix.MNT_RDONLY != 0,
		})
	}
	return ms, nil
}
