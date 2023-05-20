package tfhandler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	tfv1alpha2 "github.com/isaaguilar/terraform-operator/pkg/apis/tf/v1alpha2"
	"github.com/pkg/errors"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
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
		name := string(tf.UID)
		namespace := tf.Namespace

		log.Printf("Checking for resources that belong to %s", name)
		result, err := i.clientset.Resource(name).Poll().Read(ctx, &tf)
		if err != nil {
			log.Println(err)
			i.requeueAfter(tf, 30*time.Second)
			continue
		}

		if !result.IsSuccess {
			log.Printf("ERROR '%s' poll response unsuccessful: %s", name, result.ErrMsg)
			if strings.Contains(result.ErrMsg, fmt.Sprintf(`terraforms.tf.isaaguilar.com "%s" not found`, name)) {
				// Do not requeue since this resource is not even registered with the API
				continue
			}
			i.requeueAfter(tf, 30*time.Second)
			continue
		}

		if strings.Contains(result.Data.StatusInfo.Message, "workflow has not completed") {
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
		}
		for _, item := range list {
			i.createOrUpdateResource([]byte(item.(string)), namespace)
		}
		log.Printf("Done handling '%s'", name)
	}
}

func (i informer) createOrUpdateResource(b []byte, namespace string) {
	obj := unstructured.Unstructured{}
	err := json.Unmarshal(b, &obj)
	if err != nil {
		log.Println("ERROR: could not unmarshal resource:", err)
		return
	}
	gvk := obj.GetObjectKind().GroupVersionKind()

	var dynamicClient dynamic.ResourceInterface
	namespaceableResourceClient, isNamespaced, err := getDynamicClientOnKind(gvk.Version, gvk.Kind, i.config)
	if err != nil {
		log.Println("ERROR: could not get a client to handle resource:", err)
		return
	}
	if isNamespaced {
		dynamicClient = namespaceableResourceClient.Namespace(namespace)
	} else {
		dynamicClient = namespaceableResourceClient
	}
	obj.SetResourceVersion("")
	_, err = dynamicClient.Create(i.ctx, &obj, metav1.CreateOptions{})
	if err != nil {
		if kerrors.IsAlreadyExists(err) {
			log.Println("Resource already exists. Will update resource instead.")
			_, err := dynamicClient.Update(i.ctx, &obj, metav1.UpdateOptions{})
			if err != nil {
				log.Println("ERROR: could not update resource:", err)
				return
			}
			log.Println("Resource has been updated")
		} else {
			log.Println("ERROR: could not create resource: ", err)
		}
	} else {
		log.Println("Resource has been created")
	}
}

// getDynamicClientOnUnstructured returns a dynamic client on an Unstructured type. This client can be further namespaced.
func getDynamicClientOnKind(apiversion string, kind string, config *rest.Config) (dynamic.NamespaceableResourceInterface, bool, error) {
	gvk := schema.FromAPIVersionAndKind(apiversion, kind)
	apiRes, err := getAPIResourceForGVK(gvk, config)
	if err != nil {
		log.Printf("[ERROR] unable to get apiresource from unstructured: %s , error %s", gvk.String(), err)
		return nil, false, errors.Wrapf(err, "unable to get apiresource from unstructured: %s", gvk.String())
	}
	gvr := schema.GroupVersionResource{
		Group:    apiRes.Group,
		Version:  apiRes.Version,
		Resource: apiRes.Name,
	}
	intf, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Printf("[ERROR] unable to get dynamic client %s", err)
		return nil, false, err
	}
	res := intf.Resource(gvr)
	return res, apiRes.Namespaced, nil
}

func getAPIResourceForGVK(gvk schema.GroupVersionKind, config *rest.Config) (metav1.APIResource, error) {
	res := metav1.APIResource{}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		log.Printf("[ERROR] unable to create discovery client %s", err)
		return res, err
	}
	resList, err := discoveryClient.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		log.Printf("[ERROR] unable to retrieve resource list for: %s , error: %s", gvk.GroupVersion().String(), err)
		return res, err
	}
	for _, resource := range resList.APIResources {
		// if a resource contains a "/" it's referencing a subresource. we don't support suberesource for now.
		if resource.Kind == gvk.Kind && !strings.Contains(resource.Name, "/") {
			res = resource
			res.Group = gvk.Group
			res.Version = gvk.Version
			break
		}
	}
	return res, nil
}

func shouldPoll(tf tfv1alpha2.Terraform) bool {
	return tf.Spec.OutputsSecret != ""
}

func (i informer) requeueAfter(tf tfv1alpha2.Terraform, t time.Duration) {
	go func() {
		time.Sleep(t)
		i.queue.PushBack(tf)
	}()
}
