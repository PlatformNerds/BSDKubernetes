// bsd-lb gives LoadBalancer Services an address on bare-metal FreeBSD
// clusters (research/25):
//
//   - controller (one replica): assigns each LoadBalancer Service an IPv4
//     address from a pool and manages the cleanup finalizer;
//   - speaker (DaemonSet): one node per address answers ARP for it on the
//     node network, like MetalLB's layer 2 mode. pf rules from kube-proxy
//     then deliver the traffic, as for NodePorts.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	everything = labels.Everything()
	metaUpdate = metav1.UpdateOptions{}
)

func main() {
	mode := flag.String("mode", "", "controller or speaker")
	pool := flag.String("pool", "", "controller: addresses to assign, e.g. 192.168.100.200-192.168.100.250,10.0.0.0/28")
	ifname := flag.String("interface", "", "speaker: interface to answer ARP on (default: the one holding the node's InternalIP)")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig (default: in-cluster)")
	flag.Parse()

	var cfg *rest.Config
	var err error
	if *kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		log.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch *mode {
	case "controller":
		p, err := ParsePool(*pool)
		if err != nil {
			log.Fatalf("-pool: %v", err)
		}
		err = runController(ctx, cs, p)
	case "speaker":
		node := os.Getenv("NODE_NAME")
		ns := os.Getenv("POD_NAMESPACE")
		if node == "" || ns == "" {
			log.Fatal("speaker: NODE_NAME and POD_NAMESPACE must be set")
		}
		err = runSpeaker(ctx, cs, node, ns, *ifname)
	default:
		log.Fatal("-mode must be controller or speaker")
	}
	if err != nil {
		log.Fatal(err)
	}
}
