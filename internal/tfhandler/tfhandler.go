package tfhandler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/galleybytes/monitor/projects/terraform-operator-remote-controller/pkg/tfoapiclient"
	"github.com/galleybytes/terraform-operator-api/pkg/common/models"
	"github.com/gammazero/deque"
	tfv1alpha2 "github.com/isaaguilar/terraform-operator/pkg/apis/tf/v1alpha2"
	gocache "github.com/patrickmn/go-cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

var (
	terraformResource = schema.GroupVersionResource{
		Group:    tfv1alpha2.SchemeGroupVersion.Group,
		Version:  tfv1alpha2.SchemeGroupVersion.Version,
		Resource: "terraforms",
	}
)

type Result struct {
	data      interface{}
	isSuccess *bool
	errMsg    string
}

type informer struct {
	dynamicinformer.DynamicSharedInformerFactory
	client *tfoapiclient.Client
	cache  *gocache.Cache
	queue  *deque.Deque[tfv1alpha2.Terraform]
}

func NewInformer(dynamicClient *dynamic.DynamicClient, clientName, host, user, password string) informer {
	// token := tfoapiclient.AccessToken(host, user, password)
	clientset, err := tfoapiclient.NewTfoApiClientset(host, user, password)
	if err != nil {
		log.Fatal(err)
	}

	client := clientset.Cluster(clientName)

	tfhandler := informer{
		// httpClient: &http.Client{},
		// host:       host,
		// token:      token,
		// user:     user,
		// password: password,
		client: client,
		cache:  gocache.New(10*time.Minute, 10*time.Minute),
		queue:  &deque.Deque[tfv1alpha2.Terraform]{},
	}

	// clusterID, err := tfhandler.registerCluster(clientName)
	// if err != nil {
	// 	log.Panic(err)
	// }
	// tfhandler.clusterID = clusterID

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

	postResult, err := i.client.Event().Post(context.TODO(), tf)
	// postRequest, err := i.clusterResourceRequest("POST", "event", *tf)
	if err != nil {
		log.Println(err.Error())
		return
	}

	// postResult, err := i.doRequest(postRequest)
	// if err != nil {
	// 	log.Printf("ERROR in request to add resource: %s", err)
	// 	return
	// }

	if postResult.IsSuccess == nil {
		log.Printf("Result of request is unknown")
		return
	}

	if *postResult.IsSuccess {
		return
	}

	if !strings.Contains(postResult.ErrMsg, "TFOResource already exists") {
		log.Println(postResult.ErrMsg)
		return
	}

	// The result was that the resource already exists so do a put to update
	// putRequest, err := i.clusterResourceRequest("PUT", "event", *tf)
	// if err != nil {
	// 	log.Println(err.Error())
	// 	return
	// }

	putResult, err := i.client.Event().Post(context.TODO(), tf)
	// putResult, err := i.doRequest(putRequest)
	if err != nil {
		log.Printf("ERROR in request to add resource: %s", err)
		return
	}

	if putResult.IsSuccess == nil {
		log.Printf("Result of request is unknown")
		return
	}

	if !*putResult.IsSuccess {
		log.Println(putResult.ErrMsg)
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

	// putRequest, err := i.clusterResourceRequest("PUT", "event", *tfnew)
	// if err != nil {
	// 	log.Println(err.Error())
	// 	return
	// }

	// putResult, err := i.doRequest(putRequest)
	putResult, err := i.client.Event().Post(context.TODO(), tfnew)
	if err != nil {
		log.Printf("ERROR in request to add resource: %s", err)
		return
	}

	if putResult.IsSuccess == nil {
		log.Printf("Result of request is unknown")
		return
	}

	if !*putResult.IsSuccess {
		log.Println(putResult.ErrMsg)
	}
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

func assertTf(obj interface{}) (*tfv1alpha2.Terraform, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var tf tfv1alpha2.Terraform
	err = json.Unmarshal(b, &tf)
	if err != nil {
		return nil, err
	}
	return &tf, nil
}

func (i informer) clusterResourceRequest(method, apiResource string, tf tfv1alpha2.Terraform) (*http.Request, error) {
	jsonData, err := json.Marshal(tf)
	if err != nil {
		return nil, fmt.Errorf("ERROR marshaling added tf: %s", err)
	}
	// url := fmt.Sprintf("%s/api/v1/cluster/%s/%s", i.host, i.clusterID, apiResource)
	url := ""
	request, err := http.NewRequest(method, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("ERROR setting up request to add resource: %s", err)
	}
	return request, nil
}

// func (i informer) clusterBaseRequest(clusterName string) (*http.Request, error) {
// 	jsonData := []byte(fmt.Sprintf(`{"cluster_name":"%s"}`, clusterName))
// 	url := fmt.Sprintf("%s/api/v1/cluster", i.host)
// 	request, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
// 	if err != nil {
// 		return nil, fmt.Errorf("ERROR setting up request to get cluster id: %s", err)
// 	}
// 	return request, nil
// }

// registerCluster make an api request to get the clusterID or register a new cluster + get the new clusterID
// func (i informer) registerCluster(clientName string) (string, error) {
// 	request, err := i.clusterBaseRequest(clientName)
// 	if err != nil {
// 		return "", err
// 	}
// 	result, err := i.doRequest(request)
// 	if err != nil {
// 		return "", err
// 	}

// 	if result.isSuccess == nil {
// 		return "", fmt.Errorf("Result of request is unknown")
// 	}

// 	if !*result.isSuccess {
// 		return "", fmt.Errorf(result.errMsg)
// 	}

// 	var clusterIDUInt uint
// 	apiResponse := result.data.(api.Response)
// 	for _, i := range apiResponse.Data.([]interface{}) {
// 		cluster, err := assertClusterModel(i)
// 		if err != nil {
// 			return "", err
// 		}
// 		clusterIDUInt = cluster.ID
// 	}

// 	return strconv.FormatUint(uint64(clusterIDUInt), 10), nil
// }

// func (i *informer) doRequest(request *http.Request) (*Result, error) {
// 	request.Header.Set("Token", i.token)
// 	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
// 	response, err := i.httpClient.Do(request)
// 	if err != nil {
// 		return nil, err
// 	}
// 	defer response.Body.Close()

// 	if response.StatusCode == http.StatusUnauthorized {
// 		// token := tfoapiclient.AccessToken(i.host, i.user, i.password)
// 		// i.token = token
// 		// return i.doRequest(request)
// 		return nil, fmt.Errorf("Unauth")
// 	}

// 	if response.StatusCode == http.StatusNoContent {
// 		return &Result{isSuccess: boolp(true)}, nil
// 	}

// 	var data interface{}
// 	var hasData bool = true
// 	var status200 bool = true
// 	var errMsg string
// 	if response.StatusCode != 200 {
// 		status200 = false
// 		errMsg += fmt.Sprintf("request to %s returned a %d", request.URL, response.StatusCode)
// 	}

// 	responseBody, err := ioutil.ReadAll(response.Body)
// 	if err != nil {
// 		return nil, err
// 	}

// 	structuredResponse := api.Response{}
// 	err = json.Unmarshal(responseBody, &structuredResponse)
// 	if err != nil {
// 		status200 = false
// 		errMsg += fmt.Sprintf(" with the following response in the body: %s", string(responseBody))
// 	} else {
// 		data = structuredResponse
// 		if !status200 {
// 			errMsg += fmt.Sprintf(": %s", structuredResponse.StatusInfo.Message)
// 		}
// 	}

// 	return &Result{data: data, isSuccess: boolp(status200 && hasData), errMsg: fmt.Sprint(errMsg)}, nil
// }

func assertClusterModel(i interface{}) (*models.Cluster, error) {
	b, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	var cluster models.Cluster
	err = json.Unmarshal(b, &cluster)
	if err != nil {
		return nil, err
	}
	return &cluster, nil
}

func boolp(b bool) *bool {
	return &b
}
