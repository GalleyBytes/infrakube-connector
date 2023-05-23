package tfoapiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/galleybytes/terraform-operator-api/pkg/api"
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

func (c *clientset) do(method, url string, bodyData any) (*result, error) {
	jsonData, err := json.Marshal(bodyData)
	if err != nil {
		return nil, fmt.Errorf("ERROR marshaling added tf: %s", err)
	}
	request, err := http.NewRequest(method, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("ERROR setting up request to add resource: %s", err)
	}
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	if c.config.Token != "" {
		request.Header.Set("Token", c.config.Token)
	}
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
		return c.do(method, url, bodyData)
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

func (c clientset) AccessToken() crudResource {
	return newCRUDResource(c, fmt.Sprintf("%s/login", c.config.Host))
}

func (c ClusterClient) Register() error {
	result, err := c.clientset.do("POST", fmt.Sprintf("%s/api/v1/cluster", c.config.Host), struct {
		ClusterName string `json:"cluster_name"`
	}{c.clientName})
	if err != nil {
		return err
	}
	if !result.IsSuccess {
		return fmt.Errorf(result.ErrMsg)
	}
	return nil
}

func (c ClusterClient) Event() crudResource {
	return newCRUDResource(c.clientset, fmt.Sprintf("%s/api/v1/cluster/%s/event", c.config.Host, c.clientName))
}

func (c ClusterClient) ResourcePoll() crudResource {
	return newCRUDResource(c.clientset, fmt.Sprintf("%s/api/v1/cluster/%s/resource-poll", c.config.Host, c.clientName))
}

func newCRUDResource(c clientset, url string) crudResource {
	return crudResource{clientset: c, url: url}
}

func (c crudResource) Create(ctx context.Context, data any) (*result, error) {
	return c.clientset.do("POST", c.url, data)
}

func (c crudResource) Read(ctx context.Context, data any) (*result, error) {
	return c.clientset.do("GET", c.url, data)
}

func (c crudResource) Update(ctx context.Context, data any) (*result, error) {
	return c.clientset.do("PUT", c.url, data)
}

func (c crudResource) Delete(ctx context.Context, data any) (*result, error) {
	return c.clientset.do("DELETE", c.url, data)
}
