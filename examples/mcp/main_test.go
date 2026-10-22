// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// authedMCPServer serves an MCP server with no tools behind ate-env-api's
// bearer check and records the Authorization header of each request.
func authedMCPServer(t *testing.T, token string) (*httptest.Server, func() []string) {
	t.Helper()
	auth, err := apiservice.NewBearerAuth(token)
	if err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		auth.WrapHTTP(handler).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func connect(ctx context.Context, endpoint, token string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient(token),
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
}

func TestMCPTransportSendsTheToken(t *testing.T) {
	srv, seen := authedMCPServer(t, "s3cret")
	ctx := context.Background()

	session, err := connect(ctx, srv.URL, "s3cret")
	if err != nil {
		t.Fatalf("connect with the token: %v", err)
	}
	if _, err := session.ListTools(ctx, nil); err != nil {
		t.Fatalf("list tools with the token: %v", err)
	}
	_ = session.Close()
	if n := len(seen()); n < 2 {
		t.Fatalf("%d MCP requests recorded, want initialize and tools/list at least", n)
	}
	for i, h := range seen() {
		if h != "Bearer s3cret" {
			t.Errorf("request %d: Authorization = %q, want %q", i, h, "Bearer s3cret")
		}
	}

	if _, err := connect(ctx, srv.URL, ""); err == nil {
		t.Fatal("connect without the token succeeded against an API that requires one")
	}
}

func TestHTTPClientWithoutTokenSendsNoHeader(t *testing.T) {
	if httpClient("") != nil {
		t.Fatal("httpClient(\"\") must return nil so the transport keeps its default client")
	}
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Values("Authorization")
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer old")
	resp, err := httpClient("new").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(got) != 1 || got[0] != "Bearer new" {
		t.Fatalf("Authorization = %q, want exactly %q", got, "Bearer new")
	}
	if req.Header.Get("Authorization") != "Bearer old" {
		t.Fatal("RoundTrip modified the caller's request")
	}
}
