// Package rpc implements a minimal Qogecoin Core JSON-RPC client.
//
// Qogecoin Core is a Bitcoin Core derivative and speaks the same JSON-RPC
// 1.0-style single-endpoint protocol (HTTP POST, one JSON object per
// request, HTTP Basic Auth). This client intentionally stays generic:
// Call() sends a raw method+params and returns the raw "result" bytes,
// and typed helpers on top decode the fields the explorer actually needs.
// It does not attempt to wrap every RPC Core exposes.
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a Qogecoin Core JSON-RPC client bound to a single node.
type Client struct {
	endpoint string
	user     string
	password string
	http     *http.Client
}

// Config holds the connection parameters needed to build a Client.
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	UseTLS   bool
	Timeout  time.Duration
}

// New builds a Client from Config. It does not perform any network I/O;
// use Ping (or any RPC call) to verify connectivity.
func New(cfg Config) *Client {
	scheme := "http"
	if cfg.UseTLS {
		scheme = "https"
	}
	return &Client{
		endpoint: fmt.Sprintf("%s://%s:%d/", scheme, cfg.Host, cfg.Port),
		user:     cfg.User,
		password: cfg.Password,
		http:     &http.Client{Timeout: cfg.Timeout},
	}
}

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      string        `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	ID     string          `json:"id"`
}

// Call invokes method with params against Core and returns the raw JSON
// "result" payload. Callers decode it into whatever shape they need.
//
// The RPC password is sent only in the HTTP Basic Auth header of the
// outgoing request; it is never included in any returned error message.
func (c *Client) Call(ctx context.Context, method string, params ...interface{}) (json.RawMessage, error) {
	if params == nil {
		params = []interface{}{}
	}

	reqBody, err := json.Marshal(rpcRequest{
		JSONRPC: "1.0",
		ID:      "qoge-explorer",
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("encode rpc request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build rpc request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.SetBasicAuth(c.user, c.password)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("rpc call %s: %w", method, err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read rpc response for %s: %w", method, err)
	}

	if httpResp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("rpc call %s: authentication rejected by node (check RPC user/password)", method)
	}

	var parsed rpcResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode rpc response for %s (http %d): %w", method, httpResp.StatusCode, err)
	}

	if parsed.Error != nil {
		return nil, fmt.Errorf("rpc call %s failed: %w", method, parsed.Error)
	}

	return parsed.Result, nil
}

// CallInto invokes method and decodes the result directly into out.
func (c *Client) CallInto(ctx context.Context, out interface{}, method string, params ...interface{}) error {
	raw, err := c.Call(ctx, method, params...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode result of %s: %w", method, err)
	}
	return nil
}
