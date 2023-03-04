package main

import (
	"log"
	"os"

	"github.com/galleybytes/monitor/projects/terraform-operator-remote-controller/pkg/tfhandler"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func kubernetesConfig(kubeconfigPath string) *rest.Config {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		log.Fatal("Failed to get config for clientset")
	}
	return config
}

func main() {
	config := kubernetesConfig(os.Getenv("KUBECONFIG"))
	// client := kubernetes.NewForConfigOrDie(config)
	dynamicClient := dynamic.NewForConfigOrDie(config)
	tfinformer := tfhandler.NewInformer(dynamicClient)

	tfinformer.Run()

	os.Exit(1) // should this be 0 instead?
}
