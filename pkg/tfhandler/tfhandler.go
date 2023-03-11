package tfhandler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/galleybytes/terraform-operator-api/pkg/api"
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
	httpClient *http.Client
	host       string
	token      string
	cache      *gocache.Cache
}

func NewInformer(dynamicClient *dynamic.DynamicClient, host, user, password string) informer {
	token := accessToken(host, user, password)

	tfhandler := informer{
		httpClient: &http.Client{},
		host:       host,
		token:      token,
		cache:      gocache.New(10*time.Minute, 10*time.Minute),
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

	postRequest, err := i.fmtRequest("POST", *tf)
	if err != nil {
		log.Println(err.Error())
		return
	}

	postResult, err := i.doRequest(postRequest)
	if err != nil {
		log.Printf("ERROR in request to add resource: %s", err)
		return
	}

	if postResult.isSuccess == nil {
		log.Printf("Result of request is unknown")
		return
	}

	if *postResult.isSuccess {
		return
	}

	if !strings.Contains(postResult.errMsg, "TFOResource already exists") {
		log.Println(postResult.errMsg)
		return
	}

	// The result was that the resource already exists so do a put to update
	putRequest, err := i.fmtRequest("PUT", *tf)
	if err != nil {
		log.Println(err.Error())
		return
	}

	putResult, err := i.doRequest(putRequest)
	if err != nil {
		log.Printf("ERROR in request to add resource: %s", err)
		return
	}

	if putResult.isSuccess == nil {
		log.Printf("Result of request is unknown")
		return
	}

	if !*putResult.isSuccess {
		log.Println(putResult.errMsg)
	}

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

	putRequest, err := i.fmtRequest("PUT", *tfnew)
	if err != nil {
		log.Println(err.Error())
		return
	}

	putResult, err := i.doRequest(putRequest)
	if err != nil {
		log.Printf("ERROR in request to add resource: %s", err)
		return
	}

	if putResult.isSuccess == nil {
		log.Printf("Result of request is unknown")
		return
	}

	if !*putResult.isSuccess {
		log.Println(putResult.errMsg)
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

func (i informer) fmtRequest(method string, tf tfv1alpha2.Terraform) (*http.Request, error) {
	jsonData, err := json.Marshal(tf)
	if err != nil {
		return nil, fmt.Errorf("ERROR marshaling added tf: %s", err)

	}
	url := fmt.Sprintf("%s/api/v1/cluster/1/event", i.host)
	request, err := http.NewRequest(method, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("ERROR setting up request to add resource: %s", err)
	}

	return request, nil

}

func (i informer) doRequest(request *http.Request) (*Result, error) {
	request.Header.Set("Token", i.token)
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	response, err := i.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNoContent {
		return &Result{isSuccess: boolp(true)}, nil
	}

	var data interface{}
	var hasData bool = true
	var status200 bool = true
	var errMsg string
	if response.StatusCode != 200 {
		status200 = false
		errMsg += fmt.Sprintf("request to %s returned a %d", request.URL, response.StatusCode)
	}

	responseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	structuredResponse := api.Response{}
	err = json.Unmarshal(responseBody, &structuredResponse)
	if err != nil {
		return nil, err
	}

	if !status200 {
		errMsg += fmt.Sprintf(": %s", structuredResponse.StatusInfo.Message)
	}

	return &Result{data: data, isSuccess: boolp(status200 && hasData), errMsg: fmt.Sprint(errMsg)}, nil
}

func accessToken(host, user, password string) string {
	client := http.Client{}

	url := fmt.Sprintf("%s/login", host)
	jsonData, err := json.Marshal(map[string]interface{}{
		"user":     user,
		"password": password,
	})
	if err != nil {
		log.Panic(err)
	}
	request, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Panic(err)
	}

	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	response, err := client.Do(request)
	if err != nil {
		log.Fatal(err)
	}
	defer response.Body.Close()

	if response.StatusCode != 200 {
		log.Panicf("request to %s returned a %d", request.URL, response.StatusCode)
	}

	responseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Panic(err)
	}

	structuredResponse := api.Response{}
	err = json.Unmarshal(responseBody, &structuredResponse)
	if err != nil {
		log.Panic(err)
	}
	data := structuredResponse.Data.([]interface{})
	if len(data) != 1 {
		log.Panicf("Expected 1 result in response to api server but got %d", len(data))
	}

	return data[0].(string)
}

func boolp(b bool) *bool {
	return &b
}
