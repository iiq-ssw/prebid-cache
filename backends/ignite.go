package backends

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/prebid/prebid-cache/config"
	"github.com/prebid/prebid-cache/utils"
	log "github.com/sirupsen/logrus"
)

// httpClientWrapper is an interface that will let us mock the Ignite server responses
type httpClientWrapper interface {
	Do(req *http.Request) (*http.Response, error)
}

// clientWrapper implements the httpClientWrapper interface
type clientWrapper struct {
	client *http.Client
}

// Do simply calls the underlying *http.Client's Do(req *http.Request) (*http.Response, error) implementation
func (wrapper *clientWrapper) Do(req *http.Request) (*http.Response, error) {
	return wrapper.client.Do(req)
}

// igniteClient is an interface who's DoRequest method will let us send the request to the Ignite
// server and handle it's response and error. Other implementations of it will let us mock error scenarios.
type igniteClient interface {
	DoRequest(ctx context.Context, url *url.URL, headers http.Header) ([]byte, error)
}

// igClient implements the igniteClient interface
type igClient struct {
	client httpClientWrapper
}

// DoRequest will hit the Ignite server specified in the url parameter and handle error responses
func (c *igClient) DoRequest(ctx context.Context, url *url.URL, headers http.Header) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", url.String(), nil)
	if err != nil {
		return nil, err
	}

	if len(headers) > 0 {
		httpReq.Header = headers
	}

	httpResp, httpErr := c.client.Do(httpReq)
	if httpErr != nil {
		return nil, httpErr
	}

	if httpResp.StatusCode != http.StatusOK {
		httpErr = fmt.Errorf("Ignite error. Unexpected status code: %d", httpResp.StatusCode)
	}

	if httpResp.Body == nil {
		errMsg := "Received empty httpResp.Body"
		if httpErr == nil {
			return nil, fmt.Errorf("Ignite error. %s", errMsg)
		}
		return nil, fmt.Errorf("%s; %s", httpErr.Error(), errMsg)
	}
	defer httpResp.Body.Close()

	responseBody, ioErr := io.ReadAll(httpResp.Body)
	if ioErr != nil {
		errMsg := fmt.Sprintf("IO reader error: %s", ioErr)
		if httpErr == nil {
			return nil, fmt.Errorf("Ignite error. %s", errMsg)
		}
		return nil, fmt.Errorf("%s; %s", httpErr.Error(), errMsg)
	}

	return responseBody, httpErr
}

// IgniteBackend implements Backend interface and communicates with the Apache Ignite storage
// via its REST API as documented in https://ignite.apache.org/docs/2.11.1/restapi#rest-api-reference
type IgniteBackend struct {
	client    igniteClient
	serverURL *url.URL
	headers   http.Header
	cacheName string
}

// NewIgniteBackend expects a valid config.IgniteBackend object and will create an Apache Ignite cache in the
// Ignite server if the config.Ignite.Cache.CreateOnStart flag is set to true
func NewIgniteBackend(cfg config.Ignite) *IgniteBackend {

	if len(cfg.Scheme) == 0 || len(cfg.Host) == 0 || cfg.Port == 0 || len(cfg.Cache.Name) == 0 {
		errMsg := "Error creating Ignite backend: configuration is missing ignite.schema, ignite.host, ignite.port or ignite.cache.name"
		log.Fatalf(errMsg)
		panic(errMsg)
	}
	completeHost := fmt.Sprintf("%s://%s:%d/ignite", cfg.Scheme, cfg.Host, cfg.Port)

	url, err := url.Parse(fmt.Sprintf("%s?cacheName=%s", completeHost, cfg.Cache.Name))
	if err != nil {
		errMsg := fmt.Sprintf("Error creating Ignite backend: error parsing Ignite host URL %s", err.Error())
		log.Fatalf(errMsg)
		panic(errMsg)
	}

	igb := &IgniteBackend{serverURL: url}
	if cfg.Secure {
		igb.client = &igClient{
			client: http.DefaultClient,
		}
	} else {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		igb.client = &igClient{
			client: &http.Client{Transport: tr},
		}
	}

	if len(cfg.Headers) > 0 {
		for k, v := range cfg.Headers {
			igb.headers.Add(k, v)
		}
	}

	if cfg.Cache.CreateOnStart {
		igb.cacheName = cfg.Cache.Name
		if err := createCache(igb); err != nil {
			errMsg := fmt.Sprintf("Error creating Ignite backend: %s", err.Error())
			log.Fatalf(errMsg)
			panic(errMsg)
		}
	}
	log.Infof("Prebid Cache will write to Ignite cache name: %s", cfg.Cache.Name)

	return igb
}

// createCache uses the Apache Ignite REST API "getorcreate" command to create a cache
func createCache(igb *IgniteBackend) error {

	urlCopy := *igb.serverURL
	q := urlCopy.Query()
	q.Set("cmd", "getorcreate")
	urlCopy.RawQuery = q.Encode()

	responseBytes, err := igb.client.DoRequest(context.Background(), &urlCopy, nil)
	if err != nil {
		return err
	}

	//igniteResponse := getResponse{}
	igniteResponse := struct {
		Error string `json:"error"`
		//Response string `json:"response"`
		Status int `json:"successStatus"`
	}{}

	if unmarshalErr := json.Unmarshal(responseBytes, &igniteResponse); unmarshalErr != nil {
		return fmt.Errorf("Unmarshal response error: %s; Response body: %s", unmarshalErr.Error(), string(responseBytes))
	}

	if igniteResponse.Status > 0 {
		if len(igniteResponse.Error) > 0 {
			return fmt.Errorf("Ignite error. %s", igniteResponse.Error)
		}
		return fmt.Errorf("Ignite error. successStatus does not equal 0 %v", igniteResponse)
	}

	return nil
}

//// getResponse is used to unmarshal the Ignite server's response to a GET request with
//// the "cmd" URL query field set to "get"
//type getResponse struct {
//	Error    string `json:"error"`
//	Response string `json:"response"`
//	Status   int    `json:"successStatus"`
//}

// Get implements the Backend interface. Makes the Ignite storage client retrieve the value that has
// been previously stored under 'key' if its TTL is still current. We can tell when a key is not found
// when Ignite doesn't return an error, nor a 'Status' different than zero, but the 'Response' field is
// empty. Get can also return Ignite server-side errors
func (back *IgniteBackend) Get(ctx context.Context, key string) (string, error) {

	urlCopy := *back.serverURL
	q := urlCopy.Query()
	q.Set("cmd", "get")
	q.Set("key", key)

	urlCopy.RawQuery = q.Encode()

	responseBytes, err := back.client.DoRequest(ctx, &urlCopy, back.headers)
	if err != nil {
		return "", err
	}

	// Unmarshall response
	//igniteResponse := getResponse{}
	igniteResponse := struct {
		Error    string `json:"error"`
		Response string `json:"response"`
		Status   int    `json:"successStatus"`
	}{}

	if unmarshalErr := json.Unmarshal(responseBytes, &igniteResponse); unmarshalErr != nil {
		return "", fmt.Errorf("Unmarshal response error: %s; Response body: %s", unmarshalErr.Error(), string(responseBytes))
	}

	// Validate response
	if len(igniteResponse.Error) > 0 {
		return "", utils.NewPBCError(utils.GET_INTERNAL_SERVER, igniteResponse.Error)
	} else if igniteResponse.Status > 0 {
		return "", utils.NewPBCError(utils.GET_INTERNAL_SERVER, "Ignite response.Status not zero")
	} else if len(igniteResponse.Response) == 0 { // both igniteResponse.Status == 0 && len(igniteResponse.Error) == 0
		return "", utils.NewPBCError(utils.KEY_NOT_FOUND)
	}

	return igniteResponse.Response, nil
}

// // getResponse is used to unmarshal the Ignite server's response to a GET request with
// // the "cmd" URL query field set to "putifabs"
// type putResponse struct {
// 	Error    string `json:"error"`
// 	Response bool   `json:"response"`
// 	Status   int    `json:"successStatus"`
// }

// Put implements the Backend interface to comunicates with the Ignite storage service to perform
// a "putifabs" command in order to store the "value" parameter only if the "key" doesn't exist in
// the storage already. Returns RecordExistsError or whatever PUT_INTERNAL_SERVER error we might
// find in the storage side
func (back *IgniteBackend) Put(ctx context.Context, key string, value string, ttlSeconds int) error {

	urlCopy := *back.serverURL
	q := urlCopy.Query()
	q.Set("cmd", "putifabs")
	q.Set("key", key)
	q.Set("val", value)
	q.Set("exp", fmt.Sprintf("%d", ttlSeconds*1000))

	urlCopy.RawQuery = q.Encode()

	responseBytes, err := back.client.DoRequest(ctx, &urlCopy, back.headers)
	if err != nil {
		return err
	}

	// Unmarshall response
	//igniteResponse := putResponse{}
	igniteResponse := struct {
		Error    string `json:"error"`
		Response bool   `json:"response"`
		Status   int    `json:"successStatus"`
	}{}
	if unmarshalErr := json.Unmarshal(responseBytes, &igniteResponse); unmarshalErr != nil {
		return fmt.Errorf("Unmarshal response error: %s; Response body: %s", unmarshalErr.Error(), string(responseBytes))
	}

	// Validate response
	if igniteResponse.Status > 0 || len(igniteResponse.Error) > 0 {
		return utils.NewPBCError(utils.PUT_INTERNAL_SERVER, igniteResponse.Error)
	}

	if !igniteResponse.Response {
		return utils.NewPBCError(utils.RECORD_EXISTS)
	}

	return nil
}
