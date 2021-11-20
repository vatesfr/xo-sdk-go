package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"testing"

	"github.com/pact-foundation/pact-go/dsl"
	"github.com/sourcegraph/jsonrpc2"
)

type JSONRPC2Pact struct {
	pact *dsl.Pact
}

// Call issues a standard request (http://www.jsonrpc.org/specification#request_object).
func (rpc *JSONRPC2Pact) Call(ctx context.Context, method string, params, result interface{}, opt ...jsonrpc2.CallOption) error {
	req := &jsonrpc2.Request{Method: method}
	if err := req.SetParams(params); err != nil {
		return err
	}

	u := fmt.Sprintf("http://localhost:%d/api", rpc.pact.Server.Port)
	message, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequest("POST", u, bytes.NewReader(message))

	// NOTE: by default, request bodies are expected to be sent with a Content-Type
	// of application/json. If you don't explicitly set the content-type, you
	// will get a mismatch during Verification.
	httpReq.Header.Set("Content-Type", "application/json")

	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	// if response.
	if result != nil {
		responseBody, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		err = json.Unmarshal(responseBody, result)
		if err != nil {
			return err
		}
	}

	return err
}

// Notify issues a notification request (http://www.jsonrpc.org/specification#notification).
func (rpc *JSONRPC2Pact) Notify(ctx context.Context, method string, params interface{}, opt ...jsonrpc2.CallOption) error {
	return nil
}

// Close closes the underlying connection, if it exists.
func (rpc *JSONRPC2Pact) Close() error {
	rpc.pact.Teardown()
	return nil
}

func TestCreateUser(t *testing.T) {
	// Create Client, wrapping a dummy JSONRPC2 client which talks to a local pact daemon
	var pact = &dsl.Pact{
		Consumer: "xo-sdk-go",
		Provider: "xenorchestra",
		Host:     "localhost",
	}
	var jsonRpcPact = &JSONRPC2Pact{
		pact: pact,
	}
	c := &Client{
		rpc: jsonRpcPact,
	}
	defer jsonRpcPact.Close()

	// Set up our expected interactions.
	pact.
		AddInteraction().
		Given("No user exists").
		UponReceiving("A request to create ddelnano").
		WithRequest(dsl.Request{
			Method:  "POST",
			Path:    dsl.String("/api"),
			Headers: dsl.MapMatcher{"Content-Type": dsl.String("application/json")},
			Body: map[string]interface{}{
				"method": "user.create",
				"params": map[string]string{
					"email":    "ddelnano",
					"password": "password",
				},
				"id":      0,
				"jsonrpc": "2.0",
			},
		}).
		WillRespondWith(dsl.Response{
			Status:  200,
			Headers: dsl.MapMatcher{"Content-Type": dsl.String("application/json")},
			Body:    `"a1234abcd"`,
		})
	pact.
		AddInteraction().
		Given("No user exists").
		UponReceiving("A request to get all users").
		WithRequest(dsl.Request{
			Method:  "POST",
			Path:    dsl.String("/api"),
			Headers: dsl.MapMatcher{"Content-Type": dsl.String("application/json")},
			Body: map[string]interface{}{
				"method": "user.getAll",
				"params": map[string]string{
					"dummy": "dummy",
				},
				"id":      0,
				"jsonrpc": "2.0",
			},
		}).
		WillRespondWith(dsl.Response{
			Status:  200,
			Headers: dsl.MapMatcher{"Content-Type": dsl.String("application/json")},
			Body: []User{
				{
					Id:       "a1234abcd",
					Email:    "ddelnano",
					Password: "password",
				},
			},
		})

	userToCreate := User{
		Email:    "ddelnano",
		Password: "password",
	}

	// Pass in test case
	var test = func() error {
		_, err := c.CreateUser(userToCreate)
		return err
	}

	// Verify
	if err := pact.Verify(test); err != nil {
		log.Fatalf("Error on Verify: %v", err)
	}

}

// func TestGetUser(t *testing.T) {
// 	c, err := NewClient(GetConfigFromEnv())

// 	expectedUser := User{
// 		Email:    "ddelnano",
// 		Password: "password",
// 	}

// 	if err != nil {
// 		t.Fatalf("failed to create client with error: %v", err)
// 	}

// 	user, err := c.CreateUser(expectedUser)
// 	defer c.DeleteUser(*user)

// 	if err != nil {
// 		t.Fatalf("failed to create user with error: %v", err)
// 	}

// 	if user == nil {
// 		t.Fatalf("expected to receive non-nil user")
// 	}

// 	if user.Id == "" {
// 		t.Errorf("expected user to have a non-empty Id")
// 	}

// 	_, err = c.GetUser(User{Id: user.Id})

// 	if err != nil {
// 		t.Errorf("failed to find user by id `%s` with error: %v", user.Id, err)
// 	}
// }

// func TestDeleteUser(t *testing.T) {
// 	c, err := NewClient(GetConfigFromEnv())

// 	expectedUser := User{
// 		Email:    "ddelnano",
// 		Password: "password",
// 	}

// 	if err != nil {
// 		t.Fatalf("failed to create client with error: %v", err)
// 	}

// 	user, err := c.CreateUser(expectedUser)
// 	defer c.DeleteUser(*user)

// 	if err != nil {
// 		t.Fatalf("failed to create user with error: %v", err)
// 	}

// 	if user == nil {
// 		t.Fatalf("expected to receive non-nil user")
// 	}

// 	if user.Id == "" {
// 		t.Errorf("expected user to have a non-empty Id")
// 	}

// 	_, err = c.GetUser(User{Id: user.Id})

// 	if err != nil {
// 		t.Errorf("failed to find user by id `%s` with error: %v", user.Id, err)
// 	}
// }
