package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/vatesfr/xenorchestra-go-sdk/internal/common/core"
	"github.com/vatesfr/xenorchestra-go-sdk/pkg/config"
)

type Token string

func (t Token) String() string {
	return string(t)
}

const (
	webSocketScheme       = "ws"
	secureWebSocketScheme = "wss"
	httpScheme            = "http"
	httpsScheme           = "https"
	authCookieName        = "authenticationToken"
)

// Client handles communication with the Xen Orchestra REST API.
type Client struct {
	/*
		The Client has an HttpClient that is constructed with the provided config.
		I'd prefer to unexport this field and hide the implementation details.
		However, for backward compatibility with the older version using jsonrpc,
		it needs to remain exported so it can be used to make requests for missing endpoints.
		Once we have the final fully implemented v2 REST API, we won't need to export
		the HttpClient field anymore and can remove it.
	*/
	HttpClient *http.Client
	BaseURL    *url.URL
	AuthToken  Token

	RetryMode    core.RetryMode
	RetryMaxTime time.Duration
}

// New creates an authenticated client with the provided configuration.
func New(config *config.Config) (*Client, error) {
	if config.Url == "" {
		return nil, errors.New("url is required")
	}

	baseURL, err := url.Parse(config.Url)
	if err != nil {
		return nil, fmt.Errorf("failed to parse url: %w", err)
	}

	switch baseURL.Scheme {
	case webSocketScheme:
		baseURL.Scheme = httpScheme
	case secureWebSocketScheme:
		baseURL.Scheme = httpsScheme
	}

	baseURL.Path = path.Join(baseURL.Path, core.RestV0Path)

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		// #nosec G402 -- Allow self-signed certificates when configured by user
		InsecureSkipVerify: config.InsecureSkipVerify,
	}

	clientTimeout := config.ClientTimeout
	if clientTimeout == 0 {
		clientTimeout = 30 * time.Second
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   clientTimeout,
	}

	client := &Client{
		HttpClient:   httpClient,
		BaseURL:      baseURL,
		RetryMode:    config.RetryMode,
		RetryMaxTime: config.RetryMaxTime,
	}

	if config.Token != "" {
		client.AuthToken = Token(config.Token)
	} else if config.Username != "" && config.Password != "" {
		token, err := client.authenticate(config.Username, config.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to authenticate: %w", err)
		}
		client.AuthToken = token
	} else {
		return nil, errors.New("either token or username/password are required for authentication")
	}

	return client, nil
}

func (c *Client) authenticate(username, password string) (Token, error) {
	authURL := *c.BaseURL
	authURL.Path = path.Join(strings.TrimSuffix(c.BaseURL.Path, core.RestV0Path), "auth/login")

	payload := map[string]string{
		"username": username,
		"password": password,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, authURL.String(), bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// #nosec G704 -- The URL is provided by the SDK user via configuration, this is not an SSRF vulnerability
	resp, err := c.HttpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to authenticate: %s", string(body))
	}

	for _, cookie := range resp.Cookies() {
		if cookie.Name == authCookieName {
			return Token(cookie.Value), nil
		}
	}

	return "", fmt.Errorf("no auth token found")
}

func (c *Client) buildURL(endpoint string) url.URL {
	reqURL := *c.BaseURL

	if strings.HasPrefix(endpoint, "api/") {
		reqURL.Path = strings.TrimSuffix(reqURL.Path, core.RestV0Path)
	} else {
		reqURL.Path = path.Join(reqURL.Path, endpoint)
	}

	return reqURL
}

// doRequest performs an HTTP request and returns the raw response.
// The caller is responsible for closing the response body when finished reading it.
// For error responses (non-2xx), the body is read, closed, and included in the error message.
func (c *Client) doRequest(req *http.Request) (*http.Response, error) {
	// #nosec G124 -- Outbound request cookie for SDK auth; Secure/HttpOnly/SameSite do not apply here.
	req.AddCookie(&http.Cookie{
		Name:  authCookieName,
		Value: c.AuthToken.String(),
	})

	// #nosec G704 -- The URL is provided by the SDK user via configuration, this is not an SSRF vulnerability
	resp, err := c.HttpClient.Do(req)
	if err != nil {
		return nil, core.ErrFailedToDoRequest.WithArgs(err, req.URL.String())
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		slog.DebugContext(req.Context(), "Received HTTP response",
			"method", req.Method,
			"url", req.URL.String(),
			"status", resp.StatusCode,
			"body", string(bodyBytes),
			"read_error", readErr,
		)
		if readErr != nil {
			return nil, core.ErrFailedToReadResponse.WithArgs(readErr, "")
		}
		return nil, fmt.Errorf("API error: %s - %s", resp.Status, string(bodyBytes))
	}

	return resp, nil
}

func (c *Client) do(ctx context.Context, method, endpoint string, params map[string]any, result any) error {
	reqURL := c.buildURL(endpoint)

	var reqBody io.Reader
	var requestBody string
	if params != nil && (method == "POST" || method == "PUT" || method == "PATCH") {
		jsonData, err := json.Marshal(params)
		if err != nil {
			return core.ErrFailedToMarshalParams.WithArgs(err, string(jsonData))
		}
		reqBody = bytes.NewBuffer(jsonData)
		requestBody = string(jsonData)
	} else if params != nil {
		q := reqURL.Query()
		for k, v := range params {
			q.Add(k, fmt.Sprintf("%v", v))
		}
		reqURL.RawQuery = q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL.String(), reqBody)
	if err != nil {
		return core.ErrFailedToMakeRequest.WithArgs(err, reqURL.String())
	}

	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	slog.DebugContext(ctx, "Sending HTTP request",
		"method", req.Method,
		"url", req.URL.String(),
		"body", requestBody,
	)

	resp, err := c.doRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	slog.DebugContext(ctx, "Received HTTP response",
		"method", req.Method,
		"url", req.URL.String(),
		"status", resp.StatusCode,
		"body", string(bodyBytes),
		"read_error", err,
	)
	if err != nil {
		return core.ErrFailedToReadResponse.WithArgs(err, string(bodyBytes))
	}

	// Parse response as JSON first; if it fails and the expected result is a string, fall back to raw string.
	if result != nil && len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, result); err != nil {
			// If JSON unmarshaling fails but the caller expects a string, return the raw body.
			if strPtr, ok := result.(*string); ok {
				*strPtr = string(bodyBytes)
				return nil
			}
			return core.ErrFailedToUnmarshalResponse.WithArgs(err, string(bodyBytes))
		}
	}

	return nil
}

// doRaw performs a raw HTTP request and returns the response.
// The caller is responsible for closing the response body when finished reading it.
// This is useful for endpoints that return binary data or where the caller needs direct control over body consumption.
func (c *Client) doRaw(ctx context.Context, method, endpoint string,
	body io.Reader, contentType string, contentLength ...int64) (*http.Response, error) {
	reqURL := c.buildURL(endpoint)
	req, err := http.NewRequestWithContext(ctx, method, reqURL.String(), body)
	if err != nil {
		return nil, core.ErrFailedToMakeRequest.WithArgs(err, reqURL.String())
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	// Set Content-Length if provided
	if len(contentLength) > 0 && contentLength[0] >= 0 {
		req.ContentLength = contentLength[0]
	}

	return c.doRequest(req)
}

func (c *Client) get(ctx context.Context, endpoint string, params map[string]any, result any) error {
	return c.do(ctx, "GET", endpoint, params, result)
}

// TypedGet performs a type-safe GET request to the API.
// It converts the params struct to a map and unmarshals the response into the result struct.
//
// Example:
//
//	var result MyResponseType
//	err := TypedGet(ctx, client, "vms/123", MyParamsType{Filter: "value"}, &result)
func TypedGet[P any, R any](ctx context.Context, c *Client, endpoint string, params P, result *R) error {
	var paramsMap map[string]any

	if !reflect.ValueOf(params).IsZero() {
		data, err := json.Marshal(params)
		if err != nil {
			return core.ErrFailedToMarshalParams.WithArgs(err, string(data))
		}
		if err := json.Unmarshal(data, &paramsMap); err != nil {
			return core.ErrFailedToUnmarshalParams.WithArgs(err, string(data))
		}
	}

	return c.get(ctx, endpoint, paramsMap, result)
}

func (c *Client) post(ctx context.Context, endpoint string, params map[string]any, result any) error {
	return c.do(ctx, "POST", endpoint, params, result)
}

// TypedPost performs a type-safe POST request to the API.
// It converts the params struct to a map and unmarshals the response into the result struct.
// If the API returns a string (like a URL) and the result type is a string, it will handle
// that case appropriately without attempting JSON unmarshaling.
//
// Example:
//
//	var result MyResponseType
//	err := TypedPost(ctx, client, "vms", MyParamsType{Name: "new-vm"}, &result)
//
//	// For string responses like URLs
//	var urlResult string
//	err := TypedPost(ctx, client, "generate-link", MyParamsType{ID: "123"}, &urlResult)
func TypedPost[P any, R any](ctx context.Context, c *Client, endpoint string, params P, result *R) error {
	var paramsMap map[string]any
	if v := reflect.ValueOf(params); v.IsValid() && !v.IsZero() {
		data, err := json.Marshal(params)
		if err != nil {
			return core.ErrFailedToMarshalParams.WithArgs(err, string(data))
		}
		if err := json.Unmarshal(data, &paramsMap); err != nil {
			return core.ErrFailedToUnmarshalParams.WithArgs(err, string(data))
		}
	}

	return c.post(ctx, endpoint, paramsMap, result)
}

func (c *Client) delete(ctx context.Context, endpoint string, params map[string]any, result any) error {
	return c.do(ctx, "DELETE", endpoint, params, result)
}

// TypedDelete performs a type-safe DELETE request to the API.
// It converts the params struct to a map and unmarshals the response into the result struct.
//
// Example:
//
//	var result MyResponseType
//	err := TypedDelete(ctx, client, "vms/123", struct{}{}, &result)
func TypedDelete[P any, R any](ctx context.Context, c *Client, endpoint string, params P, result *R) error {
	var paramsMap map[string]any
	if !reflect.ValueOf(params).IsZero() {
		data, err := json.Marshal(params)
		if err != nil {
			return core.ErrFailedToMarshalParams.WithArgs(err, string(data))
		}
		if err := json.Unmarshal(data, &paramsMap); err != nil {
			return core.ErrFailedToUnmarshalParams.WithArgs(err, string(data))
		}
	}
	return c.delete(ctx, endpoint, paramsMap, result)
}

func (c *Client) put(ctx context.Context, endpoint string, params map[string]any, result any) error {
	return c.do(ctx, "PUT", endpoint, params, result)
}

// TypedPut performs a type-safe PUT request to the API.
// It converts the params struct to a map and unmarshals the response into the result struct.
//
// Example:
//
//	var result MyResponseType
//	err := TypedPut(ctx, client, "vms/123", MyParamsType{Name: "new-vm"}, &result)
func TypedPut[P any, R any](ctx context.Context, c *Client, endpoint string, params P, result *R) error {
	var paramsMap map[string]any
	if !reflect.ValueOf(params).IsZero() {
		data, err := json.Marshal(params)
		if err != nil {
			return core.ErrFailedToMarshalParams.WithArgs(err, string(data))
		}
		if err := json.Unmarshal(data, &paramsMap); err != nil {
			return core.ErrFailedToUnmarshalParams.WithArgs(err, string(data))
		}
	}
	return c.put(ctx, endpoint, paramsMap, result)
}

func (c *Client) patch(ctx context.Context, endpoint string, params map[string]any, result any) error {
	return c.do(ctx, "PATCH", endpoint, params, result)
}

// TypedPatch performs a type-safe PATCH request to the API.
// It converts the params struct to a map and unmarshals the response into the result struct.
//
// Example:
//
//	var result MyResponseType
//	err := TypedPatch(ctx, client, "vms/123", MyParamsType{Name: "new-vm"}, &result)
func TypedPatch[P any, R any](ctx context.Context, c *Client, endpoint string, params P, result *R) error {
	var paramsMap map[string]any
	if !reflect.ValueOf(params).IsZero() {
		data, err := json.Marshal(params)
		if err != nil {
			return core.ErrFailedToMarshalParams.WithArgs(err, string(data))
		}
		if err := json.Unmarshal(data, &paramsMap); err != nil {
			return core.ErrFailedToUnmarshalParams.WithArgs(err, string(data))
		}
	}
	return c.patch(ctx, endpoint, paramsMap, result)
}

func RawGet(ctx context.Context, c *Client, endpoint string) (*http.Response, error) {
	return c.doRaw(ctx, "GET", endpoint, nil, "")
}

func RawPut(ctx context.Context, c *Client, endpoint string,
	body io.Reader, contentType string, contentLength ...int64) (*http.Response, error) {
	return c.doRaw(ctx, "PUT", endpoint, body, contentType, contentLength...)
}
