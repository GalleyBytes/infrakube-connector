package tfoapiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"

	"github.com/galleybytes/terraform-operator-api/pkg/api"
	"github.com/galleybytes/terraform-operator-api/pkg/common/models"
	tfv1alpha2 "github.com/isaaguilar/terraform-operator/pkg/apis/tf/v1alpha2"
)

type CRUDInterface interface {
	Create(context.Context, any) (*result, error)
	Read(context.Context, any) (*result, error)
	Update(context.Context, any) (*result, error)
	Delete(context.Context, any) (*result, error)
}

type crudResource struct {
	clientset
	url string
}

func (c clientset) AccessToken() crudResource {
	return newCRUDResource(c, fmt.Sprintf("%s/login", c.config.Host))
}

func (c ClusterClient) Event() crudResource {
	return newCRUDResource(c.clientset, fmt.Sprintf("%s/api/v1/cluster/%s/event", c.config.Host, c.clientName))
	// return c
}
func (c ClusterClient) ResourcePoll() crudResource {
	return newCRUDResource(c.clientset, fmt.Sprintf("%s/api/v1/cluster/%s/resource-poll", c.config.Host, c.clientName))
}

// type Client struct {
// 	clientset
// }

type ClusterClient struct {
	clientset
	clientName string
}

type result struct {
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

type clientset struct {
	http.Client
	config config
}

func newCRUDResource(c clientset, url string) crudResource {
	return crudResource{clientset: c, url: url}
}

func NewClientset(host, username, password string) (*clientset, error) {
	tfoapiClientset := clientset{
		Client: http.Client{},
		config: config{
			Host:     host,
			Username: username,
			Password: password,
		},
	}
	err := tfoapiClientset.authenticate()
	if err != nil {
		return nil, err
	}
	return &tfoapiClientset, nil
}

func (c *clientset) UnauthenticatedClient() *clientset {
	c.config.Token = ""
	return c
}

func (c *clientset) Cluster(clientName string) *ClusterClient {
	return &ClusterClient{
		clientset:  *c,
		clientName: clientName,
	}
}

func (c *clientset) do(request *http.Request) (*result, error) {
	if c.config.Token != "" {
		request.Header.Set("Token", c.config.Token)
	}
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	response, err := c.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized {
		err := c.authenticate()
		if err != nil {
			return nil, err
		}
		return c.do(request)
	}

	if response.StatusCode == http.StatusNoContent {
		return &result{IsSuccess: true}, nil
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

	return &result{Data: data, IsSuccess: status200 && hasData, ErrMsg: fmt.Sprint(errMsg)}, nil
}

func (c clientset) getClientID(clientName string) (string, error) {
	jsonData := []byte(fmt.Sprintf(`{"cluster_name":"%s"}`, clientName))
	url := fmt.Sprintf("%s/api/v1/cluster", c.config.Host)
	request, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("ERROR setting up request to get client id: %s", err)
	}

	result, err := c.do(request)
	// result, err := c.Cluster().Registration().Create(context.TODO(), struct {
	// 	ClusterName string `json:"cluster_name"`
	// }{
	// 	ClusterName: clientName,
	// })
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
	c.config.Token = token
	return nil
}

func (c crudResource) Create(ctx context.Context, data any) (*result, error) {
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

func (c crudResource) Read(ctx context.Context, data any) (*result, error) {
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

func (c crudResource) Update(ctx context.Context, data any) (*result, error) {

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

func (c crudResource) Delete(ctx context.Context, tf *tfv1alpha2.Terraform) (*result, error) {
	return nil, nil
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
