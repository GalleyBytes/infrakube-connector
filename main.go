package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/galleybytes/monitor/projects/terraform-operator-remote-controller/internal/tfhandler"
	"github.com/galleybytes/monitor/projects/terraform-operator-remote-controller/pkg/tfoapiclient"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

func kubernetesConfig(kubeconfigPath string) *rest.Config {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		log.Fatal("Failed to get config for clientset")
	}
	return config
}

func readFile(filename string) []byte {
	if filename == "" {
		return []byte{}
	}
	b, err := os.ReadFile(filename)
	if err != nil {
		log.Panic(err)
	}
	return b
}

func main() {
	var insecureSkipVerify bool
	klog.InitFlags(flag.CommandLine)
	flag.BoolVar(&insecureSkipVerify, "insecure-skip-verify", false, "Allow conneting to API server without unverified HTTPS")
	flag.Parse()
	flag.Set("logtostderr", "false")
	flag.Set("alsologtostderr", "false")
	klog.SetOutput(io.Discard)
	kubeconfig := os.Getenv("KUBECONFIG")
	clientName := os.Getenv("CLIENT_NAME")
	proto := os.Getenv("TFO_API_PROTOCOL")
	host := os.Getenv("TFO_API_HOST")
	port := os.Getenv("TFO_API_PORT")
	user := os.Getenv("TFO_API_LOGIN_USER")
	password := os.Getenv("TFO_API_LOGIN_PASSWORD")
	clusterManifest := readFile(os.Getenv("TFO_API_CLUSTER_MANIFEST"))
	vClusterManifest := readFile(os.Getenv("TFO_API_VCLUSTER_MANIFEST"))
	url := fmt.Sprintf("%s://%s:%s", proto, host, port)
	clientSetup := tfoapiclient.ClientSetup{
		ClusterName:      clientName,
		ClusterManifest:  clusterManifest,
		VClusterManifest: vClusterManifest,
	}
	tfinformer := tfhandler.NewInformer(kubernetesConfig(kubeconfig), clientSetup, url, user, password, insecureSkipVerify)
	tfinformer.Run()
	os.Exit(1) // should this be 0 instead?
}
