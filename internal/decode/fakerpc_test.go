package decode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// fakeRPCServer is a minimal in-process stand-in for Qogecoin Core's
// JSON-RPC endpoint, used only to test CoreAddressResolver's request
// shape/memoization without a running qogecoind. It is NOT a general RPC
// test harness — internal/rpc's own client behavior is out of scope here.
type fakeRPCServer struct {
	*httptest.Server
	host string
	port int
}

type fakeRPCHandler func(method string, params []json.RawMessage) (any, error)

func newFakeRPCServer(t *testing.T, handler fakeRPCHandler) *fakeRPCServer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     string            `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		result, handlerErr := handler(req.Method, req.Params)

		resp := struct {
			Result any `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			ID string `json:"id"`
		}{ID: req.ID}
		if handlerErr != nil {
			resp.Error = &struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{Code: -1, Message: handlerErr.Error()}
		} else {
			resp.Result = result
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode fake rpc response: %v", err)
		}
	}))

	u, err := url.Parse(srv.URL)
	if err != nil {
		srv.Close()
		t.Fatalf("parse fake rpc server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		srv.Close()
		t.Fatalf("parse fake rpc server port: %v", err)
	}
	return &fakeRPCServer{Server: srv, host: u.Hostname(), port: port}
}
