package tfoapiclient

import (
	"bytes"
	"context"
	"crypto/tls"
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
	Clientset
	url string
}

type ClusterClient struct {
	Clientset
	clientName string
}

type ResourceClient struct {
	Clientset
	resourceUID string
}

type result struct {
	Data      api.Response
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

type Clientset struct {
	http.Client
	config config
}

func NewClientset(host, username, password string, insecureSkipVerify bool) (*Clientset, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureSkipVerify},
	}
	client := http.Client{Transport: tr}
	tfoapiClientset := Clientset{
		Client: client,
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

func (c Clientset) AccessToken() crudResource {
	return newCRUDResource(c, fmt.Sprintf("%s/login", c.config.Host))
}

func (c *Clientset) UnauthenticatedClient() *Clientset {
	c.config.Token = ""
	return c
}

func (c *Clientset) Cluster(clientName string) *ClusterClient {
	return &ClusterClient{
		Clientset:  *c,
		clientName: clientName,
	}
}

func (c *Clientset) Resource(resourceUID string) *ResourceClient {
	return &ResourceClient{
		Clientset:   *c,
		resourceUID: resourceUID,
	}
}

func (c *Clientset) do(method, url string, bodyData any) (*result, error) {
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
		if !status200 {
			errMsg += fmt.Sprintf(": %s", structuredResponse.StatusInfo.Message)
		}
		if status200 && structuredResponse.Data == nil {
			hasData = false
			errMsg += fmt.Sprintf("response has no data. %s", structuredResponse.StatusInfo.Message)
		}

	}

	return &result{Data: structuredResponse, IsSuccess: status200 && hasData, ErrMsg: fmt.Sprint(errMsg)}, nil
}

func (c *Clientset) authenticate() error {
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

	data := result.Data.Data.([]any)
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

func (c ClusterClient) Register() error {
	result, err := c.Clientset.do("POST", fmt.Sprintf("%s/api/v1/cluster", c.config.Host), struct {
		ClusterName string `json:"cluster_name"`
	}{
		ClusterName: c.clientName,
	})
	if err != nil {
		return err
	}
	if !result.IsSuccess {
		return fmt.Errorf(result.ErrMsg)
	}
	return nil
}

func (c ClusterClient) Event() crudResource {
	return newCRUDResource(c.Clientset, fmt.Sprintf("%s/api/v1/cluster/%s/event", c.config.Host, c.clientName))
}

func (c ClusterClient) Poll(namespace, name string) crudResource {
	return newCRUDResource(c.Clientset, fmt.Sprintf("%s/api/v1/cluster/%s/resource/%s/%s/poll", c.config.Host, c.clientName, namespace, name))
}

func (c ClusterClient) SyncDependencies() crudResource {
	return newCRUDResource(c.Clientset, fmt.Sprintf("%s/api/v1/cluster/%s/sync-dependencies", c.config.Host, c.clientName))
}

func newCRUDResource(c Clientset, url string) crudResource {
	return crudResource{Clientset: c, url: url}
}

func (c crudResource) Create(ctx context.Context, data any) (*result, error) {
	return c.Clientset.do("POST", c.url, data)
}

func (c crudResource) Read(ctx context.Context, data any) (*result, error) {
	return c.Clientset.do("GET", c.url, data)
}

func (c crudResource) Update(ctx context.Context, data any) (*result, error) {
	return c.Clientset.do("PUT", c.url, data)
}

func (c crudResource) Delete(ctx context.Context, data any) (*result, error) {
	return c.Clientset.do("DELETE", c.url, data)
}
