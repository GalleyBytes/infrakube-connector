package tfhandler

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/galleybytes/monitor/projects/terraform-operator-remote-controller/pkg/util"
	tfv1beta1 "github.com/galleybytes/terraform-operator/pkg/apis/tf/v1beta1"
	"github.com/isaaguilar/kedge"
	"k8s.io/client-go/rest"
)

func (i informer) backgroundQueueWorker() {
	go i.worker()
}

func (i informer) worker() {
	ctx := context.TODO()
	log.Println("Queue worker started")
MainLoop:
	for {
		if i.queue.Len() == 0 {
			// log.Println("No queue events found")
			time.Sleep(3 * time.Second)
			continue
		}

		tf := i.queue.PopFront()
		if !shouldPoll(tf) {
			continue
		}
		name := tf.Name
		namespace := tf.Namespace

		log.Printf("Checking for resources that belong to %s", name)
		result, err := i.clientset.Cluster(i.clusterName).Poll(namespace, name).Read(ctx, &tf)
		if err != nil {
			log.Println(err)
			i.requeueAfter(tf, 30*time.Second)
			continue
		}

		if strings.Contains(result.Data.StatusInfo.Message, "workflow has not completed") {
			i.requeueAfter(tf, 30*time.Second)
			continue
		}

		if !result.IsSuccess {
			log.Printf("INFO '%s' poll request was successful but %s", name, result.ErrMsg)
			if strings.Contains(result.ErrMsg, fmt.Sprintf(`terraforms.tf.galleybytes.com "%s" not found`, name)) {
				// Do not requeue since this resource is not even registered with the API
				continue
			}
			i.requeueAfter(tf, 30*time.Second)
			continue
		}

		list, ok := result.Data.Data.([]interface{})
		if !ok {
			log.Printf("ERROR '%s' poll response in unexpected format %T", name, result.Data.Data)
			i.requeueAfter(tf, 30*time.Second)
			continue
		}

		for _, item := range list {
			_, ok := item.(string)
			if !ok {
				log.Printf("ERROR '%s' response item in unexpected format %T", name, item)
				i.requeueAfter(tf, 30*time.Second)
				continue MainLoop
			}
			_, err := base64.StdEncoding.DecodeString(item.(string))
			if err != nil {
				log.Printf("ERROR '%s' response item cannot be decoded", name)
				i.requeueAfter(tf, 30*time.Second)
				continue MainLoop
			}
		}

		for _, item := range list {
			b, _ := base64.StdEncoding.DecodeString(item.(string))
			applyRawManifest(ctx, kedge.KubernetesConfig(os.Getenv("KUBECONFIG")), b, namespace)

		}
		log.Printf("Done handling '%s'", name)
	}
}

func applyRawManifest(c context.Context, config *rest.Config, raw []byte, namespace string) error {
	tempfile, err := os.CreateTemp(util.Tmpdir(), "*manifest")
	if err != nil {
		return err
	}
	defer os.Remove(tempfile.Name())

	err = os.WriteFile(tempfile.Name(), raw, 0755)
	if err != nil {
		return fmt.Errorf("whoa! error here: %s", err)
	}

	err = kedge.Apply(config, tempfile.Name(), namespace, []string{})
	if err != nil {
		return fmt.Errorf("error applying manifest: %s", err)
	}
	// Should this call block until the vcluster is up and running?
	return nil
}

func shouldPoll(tf tfv1beta1.Terraform) bool {
	if true {
		return true
	}
	return tf.Spec.OutputsSecret != ""
}

func (i informer) requeueAfter(tf tfv1beta1.Terraform, t time.Duration) {
	go func() {
		time.Sleep(t)
		i.queue.PushBack(tf)
	}()
}
