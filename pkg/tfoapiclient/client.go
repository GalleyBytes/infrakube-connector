package tfoapiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"

	"github.com/galleybytes/terraform-operator-api/pkg/api"
	"github.com/galleybytes/terraform-operator-api/pkg/common/models"
	tfv1alpha2 "github.com/isaaguilar/terraform-operator/pkg/apis/tf/v1alpha2"
)

type CRUDInterface interface {
	Create(context.Context, any) (*Result, error)
	Read(context.Context, any) (*Result, error)
	Update(context.Context, any) (*Result, error)
	Delete(context.Context, any) (*Result, error)
}

type CRUDClient struct {
	Client
	url string
}

func NewCRUDResource(client Client, url string) CRUDClient {
	return CRUDClient{Client: client, url: url}
}

type Result struct {
	Data      interface{}
	IsSuccess bool
	ErrMsg    string
}

type config struct {
	Host       string
	Username   string
	Password   string
	Token      string
	ClientName string
}

type httpHandler struct {
	http.Client
	config config
}

type clientset struct {
	httpHandler
}

func NewClientset(host, username, password string) (*clientset, error) {

	tfoapiClientset := clientset{httpHandler: httpHandler{
		Client: http.Client{},
		config: config{
			Host:     host,
			Username: username,
			Password: password,
		},
	},
	}
	err := tfoapiClientset.authenticate()
	if err != nil {
		return nil, err
	}

	return &tfoapiClientset, nil
}

func (c *clientset) UnauthenticatedClient() *Client {
	c.httpHandler.config.Token = ""
	return &Client{
		clientset: *c,
	}
}

func (c *clientset) Cluster(clientName string) *Client {
	clientID, err := c.getClientID(clientName)
	if err != nil {
		log.Print(err)
	}

	return &Client{
		clientset: *c,
		clientID:  clientID,
	}
}

func (c *clientset) authenticate() error {
	result, err := c.UnauthenticatedClient().AccessToken().Create(context.TODO(), map[string]any{
		"user":     c.config.Username,
		"password": c.config.Password,
	})

	if err != nil {
		return err
	}
	if !result.IsSuccess {
		return fmt.Errorf(result.ErrMsg)
	}

	data := result.Data.(api.Response).Data.([]any)
	if len(data) != 1 {
		return fmt.Errorf("expected 1 result in response to api server but got %d", len(data))
	}

	token, ok := data[0].(string)
	if !ok {
		return fmt.Errorf("token expected as string but was %T", data[0])
	}
	c.httpHandler.config.Token = token
	return nil
}

type Client struct {
	clientset
	clientID string
}

func (c Client) AccessToken() CRUDClient {
	return NewCRUDResource(c, fmt.Sprintf("%s/login", c.httpHandler.config.Host))
}

func (c Client) Event() CRUDClient {
	return NewCRUDResource(c, fmt.Sprintf("%s/api/v1/cluster/%s/event", c.httpHandler.config.Host, c.clientID))
	// return c
}
func (c Client) ResourcePoll() CRUDClient {
	return NewCRUDResource(c, fmt.Sprintf("%s/api/v1/cluster/%s/resource-poll", c.httpHandler.config.Host, c.clientID))
}

func (c CRUDClient) Update(ctx context.Context, data any) (*Result, error) {

	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("ERROR marshaling added tf: %s", err)
	}
	request, err := http.NewRequest("PUT", c.url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("ERROR setting up request to add resource: %s", err)
	}

	return c.clientset.do(request)

}
func (c CRUDClient) Create(ctx context.Context, data any) (*Result, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("ERROR marshaling added tf: %s", err)
	}
	request, err := http.NewRequest("POST", c.url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("ERROR setting up request to add resource: %s", err)
	}
	return c.clientset.do(request)
}

func (c CRUDClient) Delete(ctx context.Context, tf *tfv1alpha2.Terraform) (*Result, error) {
	return nil, nil
}

// func (c CRUDClient) Get() (any, error)    { return nil, nil }

func (c CRUDClient) Read(ctx context.Context, data any) (*Result, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("ERROR marshaling added tf: %s", err)
	}
	request, err := http.NewRequest("GET", c.url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("ERROR setting up request to add resource: %s", err)
	}
	return c.clientset.do(request)

}

func (t *clientset) do(request *http.Request) (*Result, error) {
	if t.config.Token != "" {
		request.Header.Set("Token", t.config.Token)
	}
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	response, err := t.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized {
		err := t.authenticate()
		if err != nil {
			return nil, err
		}
		return t.do(request)
	}

	if response.StatusCode == http.StatusNoContent {
		return &Result{IsSuccess: true}, nil
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
		status200 = false
		errMsg += fmt.Sprintf(" with the following response in the body: %s", string(responseBody))
	} else {
		data = structuredResponse
		if !status200 {
			errMsg += fmt.Sprintf(": %s", structuredResponse.StatusInfo.Message)
		}
	}

	return &Result{Data: data, IsSuccess: status200 && hasData, ErrMsg: fmt.Sprint(errMsg)}, nil
}

func (c clientset) getClientID(clientName string) (string, error) {

	jsonData := []byte(fmt.Sprintf(`{"cluster_name":"%s"}`, clientName))
	url := fmt.Sprintf("%s/api/v1/cluster", c.config.Host)
	request, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("ERROR setting up request to get client id: %s", err)
	}

	result, err := c.do(request)
	if err != nil {
		return "", err
	}

	if !result.IsSuccess {
		return "", fmt.Errorf(result.ErrMsg)
	}

	var clusterIDUInt uint
	apiResponse := result.Data.(api.Response)
	for _, i := range apiResponse.Data.([]interface{}) {
		cluster, err := assertClusterModel(i)
		if err != nil {
			return "", err
		}
		clusterIDUInt = cluster.ID
	}

	return strconv.FormatUint(uint64(clusterIDUInt), 10), nil
}

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
