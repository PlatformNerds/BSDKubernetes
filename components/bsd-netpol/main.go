// bsd-netpol enforces Kubernetes NetworkPolicy on a FreeBSD node with pf.
//
// It runs on every FreeBSD node (DaemonSet, host network, privileged so CRI-O gives the jail
// allow.pf), watches Pods, Namespaces and NetworkPolicies, renders the rules for this node's
// pods (render.go) and loads them into one pf anchor, by default kube-np/policy. The node's
// main ruleset must evaluate that anchor before kube-proxy's filter anchors:
//
//	anchor "kube-np/*"
//	anchor "cni-rdr/*"
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig (default: in-cluster)")
	node := flag.String("node-name", os.Getenv("NODE_NAME"), "this node's name")
	anchor := flag.String("anchor", "kube-np/policy", "pf anchor to load the rules into")
	pfctl := flag.String("pfctl", "/sbin/pfctl", "pfctl binary")
	resync := flag.Duration("resync", time.Minute, "reload the rules at least this often")
	dryRun := flag.Bool("dry-run", false, "print the rules instead of loading them")
	flag.Parse()
	if *node == "" {
		log.Fatal("--node-name (or NODE_NAME) is required")
	}

	var cfg *rest.Config
	var err error
	if *kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		log.Fatalf("kubernetes config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	f := informers.NewSharedInformerFactory(cs, 0)
	pods := f.Core().V1().Pods()
	nss := f.Core().V1().Namespaces()
	nps := f.Networking().V1().NetworkPolicies()
	dirty := make(chan struct{}, 1)
	kick := func() {
		select {
		case dirty <- struct{}{}:
		default:
		}
	}
	h := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { kick() },
		UpdateFunc: func(any, any) { kick() },
		DeleteFunc: func(any) { kick() },
	}
	for _, inf := range []cache.SharedIndexInformer{pods.Informer(), nss.Informer(), nps.Informer()} {
		if _, err := inf.AddEventHandler(h); err != nil {
			log.Fatalf("event handler: %v", err)
		}
	}
	f.Start(ctx.Done())
	for t, ok := range f.WaitForCacheSync(ctx.Done()) {
		if !ok {
			log.Fatalf("cache sync failed for %v", t)
		}
	}
	log.Printf("bsd-netpol on node %s, anchor %s", *node, *anchor)

	last := ""
	warned := false
	tick := time.NewTicker(*resync)
	defer tick.Stop()
	for {
		// Coalesce bursts of events (a Deployment rollout) into one load.
		time.Sleep(100 * time.Millisecond)
		select {
		case <-dirty:
		default:
		}

		allPods, _ := pods.Lister().List(labels.Everything())
		allNS, _ := nss.Lister().List(labels.Everything())
		allNP, _ := nps.Lister().List(labels.Everything())
		rules := Render(*node, allPods, allNS, allNP)
		if *dryRun {
			if rules != last {
				fmt.Print(rules)
			}
			last = rules
		} else if rules != last {
			if err := load(*pfctl, *anchor, rules); err != nil {
				log.Printf("loading %s: %v", *anchor, err)
				last = "" // retry on the next event or resync
			} else {
				log.Printf("loaded %s: %d policies, %d rules", *anchor, len(allNP), countRules(rules))
				last = rules
			}
			if !warned && !referenced(*pfctl, *anchor) {
				log.Printf("WARNING: the main pf ruleset does not evaluate %q; add `anchor \"%s/*\"` before kube-proxy's anchors", *anchor, parent(*anchor))
				warned = true
			}
		}

		select {
		case <-ctx.Done():
			// Leave the rules loaded: policies stay enforced while the agent restarts.
			return
		case <-dirty:
		case <-tick.C:
			last = "" // reload in case someone flushed the anchor
		}
	}
}

func load(pfctl, anchor, rules string) error {
	cmd := exec.Command(pfctl, "-a", anchor, "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func referenced(pfctl, anchor string) bool {
	out, err := exec.Command(pfctl, "-sr").Output()
	if err != nil {
		return true // can't tell; don't warn
	}
	p := parent(anchor)
	return bytes.Contains(out, []byte(`anchor "`+p+`/*"`)) || bytes.Contains(out, []byte(`anchor "`+anchor+`"`))
}

func parent(anchor string) string {
	if i := strings.LastIndex(anchor, "/"); i > 0 {
		return anchor[:i]
	}
	return anchor
}

func countRules(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "pass ") || strings.HasPrefix(l, "block ") {
			n++
		}
	}
	return n
}
