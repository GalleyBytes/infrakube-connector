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

		// TODO Keep track of how many attempts queued items have done. Remove after a certain threshold.

		tf := i.queue.PopFront()
		if !shouldPoll(tf) {
			continue
		}
		name := tf.Name
		namespace := tf.Namespace

		if i.clusterName == "" || name == "" || namespace == "" {
			// The resulting request will be malformed unless all vars are defined
			// TODO Determine what causes undefined fields
			continue
		}

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
			log.Printf(".... %s \t(%s/%s)", result.ErrMsg, namespace, name)
			if strings.Contains(result.ErrMsg, fmt.Sprintf(`terraforms.tf.galleybytes.com "%s" not found`, name)) {
				// Do not requeue since this resource is not even registered with the API
				continue
			}
			i.requeueAfter(tf, 30*time.Second)
			continue
		}

		// The result returned from the API was successful. Validate the data before removing from queue
		list, ok := result.Data.Data.([]interface{})
		if !ok {
			log.Printf("ERROR api response in unexpected format %T \t(%s/%s)", result.Data.Data, namespace, name)
			i.requeueAfter(tf, 30*time.Second)
			continue
		}

		for _, item := range list {
			_, ok := item.(string)
			if !ok {
				log.Printf("ERROR api response item in unexpected format %T \t(%s/%s)", item, namespace, name)
				i.requeueAfter(tf, 30*time.Second)
				continue MainLoop
			}
			_, err := base64.StdEncoding.DecodeString(item.(string))
			if err != nil {
				log.Printf("ERROR api response item cannot be decoded \t(%s/%s)", namespace, name)
				i.requeueAfter(tf, 30*time.Second)
				continue MainLoop
			}
		}

		for _, item := range list {
			b, _ := base64.StdEncoding.DecodeString(item.(string))
			applyRawManifest(ctx, kedge.KubernetesConfig(os.Getenv("KUBECONFIG")), b, namespace)
		}
		log.Printf("Done handling \t(%s/%s)", namespace, name)
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
	log.Printf(".... Waiting for workflow completion. \t(%s/%s)", tf.Namespace, tf.Name)
}
