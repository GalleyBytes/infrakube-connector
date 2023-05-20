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

type Result struct {
	Data      interface{}
	IsSuccess *bool
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

type tfoApiClientset struct {
	httpHandler
}
type Client struct {
	httpHandler
	clientID string
	// httpClient *http.Client
	// request *http.Request
}
type eventClient struct {
	httpHandler
	url string
}
type eventOutput struct {
	Result
}
type resourcePollClient struct{}
type resourcePollOutput struct{}

func NewTfoApiClientset(host, username, password string) (*tfoApiClientset, error) {
	handler := httpHandler{
		Client: http.Client{},
		config: config{
			Host:     host,
			Username: username,
			Password: password,
		},
	}

	err := handler.requestAccessToken()
	if err != nil {
		return nil, err
	}

	return &tfoApiClientset{handler}, nil
}

func (c *tfoApiClientset) Cluster(clientName string) *Client {
	clientID, err := c.httpHandler.getClientID(clientName)
	if err != nil {
		log.Print(err)
	}

	return &Client{
		httpHandler: c.httpHandler,
		clientID:    clientID,
	}
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
func (c Client) Event() eventClient {
	return eventClient{
		httpHandler: c.httpHandler,
		url:         fmt.Sprintf("%s/api/v1/cluster/%s/event", c.httpHandler.config.Host, c.clientID),
	}
}

func (c eventClient) Put(ctx context.Context, tf *tfv1alpha2.Terraform) (*eventOutput, error) {

	jsonData, err := json.Marshal(tf)
	if err != nil {
		return nil, fmt.Errorf("ERROR marshaling added tf: %s", err)
	}
	// url := ""
	request, err := http.NewRequest("PUT", c.url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("ERROR setting up request to add resource: %s", err)
	}

	result, err := c.httpHandler.do(request)
	return &eventOutput{*result}, err
}
func (c eventClient) Post(ctx context.Context, tf *tfv1alpha2.Terraform) (*eventOutput, error) {
	jsonData, err := json.Marshal(tf)
	if err != nil {
		return nil, fmt.Errorf("ERROR marshaling added tf: %s", err)
	}
	// url := ""
	request, err := http.NewRequest("PUT", c.url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("ERROR setting up request to add resource: %s", err)
	}

	result, err := c.httpHandler.do(request)
	return &eventOutput{*result}, err

}
func (c eventClient) Delete() (any, error) { return nil, nil }
func (c eventClient) Get() (any, error)    { return nil, nil }

func (c Client) ResourcePoll() resourcePollClient {
	return resourcePollClient{}
}
func (c resourcePollClient) Get() (resourcePollOutput, error) {
	return resourcePollOutput{}, nil
}
func (c resourcePollClient) Put() (any, error)    { return nil, nil }
func (c resourcePollClient) Post() (any, error)   { return nil, nil }
func (c resourcePollClient) Delete() (any, error) { return nil, nil }

func testmakesomecalls(a, b, c string) {
	tfoclient, _ := NewTfoApiClientset(a, b, c)

	// tfoclient.Cluster("").Event().Post()
	tfoclient.Cluster("").ResourcePoll().Get()
}

func (h *httpHandler) do(request *http.Request) (*Result, error) {
	request.Header.Set("Token", h.config.Token)
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	response, err := h.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized {
		err := h.requestAccessToken()
		if err != nil {
			return nil, err
		}
		return h.do(request)
	}

	if response.StatusCode == http.StatusNoContent {
		return &Result{IsSuccess: boolp(true)}, nil
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

	return &Result{Data: data, IsSuccess: boolp(status200 && hasData), ErrMsg: fmt.Sprint(errMsg)}, nil
}

func boolp(b bool) *bool { return &b }

func (h *httpHandler) requestAccessToken() error {

	url := fmt.Sprintf("%s/login", h.config.Host)
	jsonData, err := json.Marshal(map[string]any{
		"user":     h.config.Username,
		"password": h.config.Password,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}

	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	response, err := h.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != 200 {
		log.Panicf("request to %s returned a %d", request.URL, response.StatusCode)
	}

	responseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}

	structuredResponse := api.Response{}
	err = json.Unmarshal(responseBody, &structuredResponse)
	if err != nil {
		return err
	}
	data := structuredResponse.Data.([]any)
	if len(data) != 1 {
		log.Panicf("Expected 1 result in response to api server but got %d", len(data))
	}

	token, ok := data[0].(string)
	if !ok {
		return fmt.Errorf("token expected as string but was %T", data[0])
	}
	h.config.Token = token
	return nil
}

func (h httpHandler) getClientID(clientName string) (string, error) {

	jsonData := []byte(fmt.Sprintf(`{"cluster_name":"%s"}`, clientName))
	url := fmt.Sprintf("%s/api/v1/cluster", h.config.Host)
	request, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("ERROR setting up request to get client id: %s", err)
	}

	result, err := h.do(request)
	if err != nil {
		return "", err
	}

	if result.IsSuccess == nil {
		return "", fmt.Errorf("Result of request is unknown")
	}

	if !*result.IsSuccess {
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
