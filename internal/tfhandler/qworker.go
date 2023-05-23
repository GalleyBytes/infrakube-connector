package tfhandler

import (
	"context"
	"log"
	"time"

	tfv1alpha2 "github.com/isaaguilar/terraform-operator/pkg/apis/tf/v1alpha2"
)

func (i informer) backgroundQueueWorker() {
	go i.worker()
}

func (i informer) worker() {
	ctx := context.TODO()
	log.Println("Queue worker started")
	for {
		if i.queue.Len() == 0 {
			// log.Println("No queue events found")
			time.Sleep(3 * time.Second)
			continue
		}

		tf := i.queue.PopFront()

		log.Printf("Will do work with %s/%s", tf.Namespace, tf.Name)

		if !shouldPoll(tf) {
			continue
		}

		result, err := i.clientset.Resource(string(tf.UID)).Poll().Read(ctx, &tf)
		if err != nil {
			log.Println(result)
			continue
		}

		log.Printf("Checking with API server for data: %+v", result)

		// Make an API call to check if resources need to be pulled from the hub
		// The API should be responsible for correctly storing and naming resources
		/*

			For example:

			1. tf-resourceA in ClusterA wants to save a secret named "tf-out"
			2. tf-resourceB in ClusterB wants to save a secret named "tf-out"

			Because both want to save a secret named "tf-out", the hub cannot create both secrets with the same name.
			How can this be handled?

			Option #1 is to just create a uid + a map table that allows a quick lookup.
			Pros:
			- simple to implement
			- quick to use
			Cons:
			- data will need to persist ie another storage layer (or more usage of current storage layer?)

			Option #2 use labels on resources
			Pros:
			- simpler to implement
			- k8s native lookups
			- no extra data layer
			- **no need to keep track of uid name**




		*/
	}
}

func shouldPoll(tf tfv1alpha2.Terraform) bool {
	if tf.Spec.OutputsSecret != "" {
		return false
	}

	return true
}
