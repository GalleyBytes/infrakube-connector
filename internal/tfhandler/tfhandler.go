package tfhandler

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/galleybytes/monitor/projects/terraform-operator-remote-controller/pkg/tfoapiclient"
	tfv1beta1 "github.com/galleybytes/terraform-operator/pkg/apis/tf/v1beta1"
	"github.com/gammazero/deque"
	gocache "github.com/patrickmn/go-cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

var (
	terraformResource = schema.GroupVersionResource{
		Group:    tfv1beta1.SchemeGroupVersion.Group,
		Version:  tfv1beta1.SchemeGroupVersion.Version,
		Resource: "terraforms",
	}
)

type informer struct {
	dynamicinformer.DynamicSharedInformerFactory
	ctx         context.Context
	config      *rest.Config
	clientset   *tfoapiclient.Clientset
	cache       *gocache.Cache
	queue       *deque.Deque[tfv1beta1.Terraform]
	clusterName string
}

func NewInformer(config *rest.Config, clientName, host, user, password string) informer {
	dynamicClient := dynamic.NewForConfigOrDie(config)

	clientset, err := tfoapiclient.NewClientset(host, user, password)
	if err != nil {
		log.Fatal(err)
	}

	// One time registration of the cluster. Returns success if already exist or if is created successfully.
	err = clientset.Cluster(clientName).Register()
	if err != nil {
		log.Fatal(err)
	}

	tfhandler := informer{
		ctx:         context.TODO(),
		config:      config,
		clientset:   clientset,
		clusterName: clientName,
		cache:       gocache.New(10*time.Minute, 10*time.Minute),
		queue:       &deque.Deque[tfv1beta1.Terraform]{},
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    tfhandler.addEvent,
		UpdateFunc: tfhandler.updateEvent,
		DeleteFunc: tfhandler.deleteEvent,
	}
	informer := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
	informer.ForResource(terraformResource).Informer().AddEventHandler(handler)
	tfhandler.DynamicSharedInformerFactory = informer
	return tfhandler
}

func (i informer) Run() {
	log.Println("Starting informer")
	stopCh := make(chan struct{})
	defer close(stopCh)
	i.backgroundQueueWorker()
	i.Start(stopCh)
	<-stopCh
	log.Println("Stopped informer")
}

func (i informer) addEvent(obj interface{}) {
	tf, err := assertTf(obj)
	if err != nil {
		log.Printf("ERROR in add event: %s", err)
		return
	}
	log.Printf("Add event observed '%s'", tf.Name)

	postResult, err := i.clientset.Cluster(i.clusterName).Event().Create(context.TODO(), tf)
	if err != nil {
		log.Println(err.Error())
		return
	}

	if postResult.IsSuccess {
		return
	}

	if !strings.Contains(postResult.ErrMsg, "TFOResource already exists") {
		log.Println(postResult.ErrMsg)
		return
	}

	putResult, err := i.clientset.Cluster(i.clusterName).Event().Update(context.TODO(), tf)
	if err != nil {
		log.Printf("ERROR in request to add resource: %s", err)
		return
	}

	if !putResult.IsSuccess {
		log.Printf("ERROR %s", putResult.ErrMsg)
		return
	}

	i.queue.PushBack(*tf)
}

func (i informer) updateEvent(old, new interface{}) {
	tfold, err := assertTf(old)
	if err != nil {
		log.Printf("ERROR in update event: %s", err)
		return
	}
	tfnew, err := assertTf(new)
	if err != nil {
		log.Printf("ERROR in update event: %s", err)
		return
	}
	if tfold.Generation != tfnew.Generation {
		log.Println("Generation change: ", tfnew.Name)
	} else {
		log.Println("Observed update event: ", tfnew.Name)
	}

	putResult, err := i.clientset.Cluster(i.clusterName).Event().Update(context.TODO(), tfnew)
	if err != nil {
		log.Printf("ERROR in request to add resource: %s", err)
		return
	}

	if !putResult.IsSuccess {
		log.Printf("ERROR %s", putResult.ErrMsg)
		return
	}

	i.queue.PushBack(*tfnew)
}

func (i informer) deleteEvent(obj interface{}) {
	tf, err := assertTf(obj)
	if err != nil {
		log.Printf("ERROR in delete event: %s", err)
		return
	}
	_ = tf
	log.Println("delete event")
}

func assertTf(obj interface{}) (*tfv1beta1.Terraform, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var tf tfv1beta1.Terraform
	err = json.Unmarshal(b, &tf)
	if err != nil {
		return nil, err
	}
	return &tf, nil
}
