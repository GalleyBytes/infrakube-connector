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

		failureRequeueRate := 100 * time.Second

		result, err := i.clientset.Cluster(i.clusterName).Poll(namespace, name).Read(ctx, &tf)
		if err != nil {
			i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf(".... ERROR: %s)", err))
			continue
		}

		if result == nil {
			i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf(".... API did not return results)"))
			continue
		}

		if strings.Contains(result.Data.StatusInfo.Message, "workflow has not completed") {
			i.requeueAfter(tf, 30*time.Second, fmt.Sprintf(".... Waiting for workflow completion)"))
			continue
		}

		if !result.IsSuccess {
			// Even after a failure, continue to check for a success. There is a possibility a debug session
			// may fix the session.
			i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf(".... %s)", result.ErrMsg))
			continue
		}

		list, ok := result.Data.Data.([]interface{})
		// Validate the API response structure. If faiure are detected, requeue and hope it was a network blip.
		// ... else there might be breaking changes in either this client or in the API.
		if !ok {
			i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf(".... ERROR api response in unexpected format %T)", result.Data.Data))
			continue
		}

		for _, item := range list {
			_, ok := item.(string)
			if !ok {
				i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf(".... ERROR api response item in unexpected format %T)", item))
				continue MainLoop
			}
			_, err := base64.StdEncoding.DecodeString(item.(string))
			if err != nil {
				i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf(".... ERROR api response item cannot be decoded)"))
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

func (i informer) requeueAfter(tf tfv1beta1.Terraform, t time.Duration, msg string) {
	go func() {
		time.Sleep(t)
		i.queue.PushBack(tf)
	}()

	log.Printf(".... %s \t(%s/%s)", msg, tf.Namespace, tf.Name)

}
