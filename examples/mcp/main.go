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

// Command mcp demonstrates connecting to an environment's MCP endpoint using the
// official github.com/modelcontextprotocol/go-sdk package: creating an environment,
// discovering built-in tools over MCP, and executing a tool call.
//
// It expects a port-forward to the ate-env-api service:
//
//	kubectl port-forward -n ate-env svc/ate-env-api 7777:7777
//
// When ate-env-api requires a bearer token, export it as
// SUBSTRATE_ENV_AUTH_TOKEN; the example sends it on the gRPC calls and on the
// MCP requests.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/agent-substrate/env/clients/go"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearerTransport adds "Authorization: Bearer <token>" to each request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req)
}

// httpClient returns the HTTP client for the MCP transport: nil (the
// transport's default) without a token, else one that sends the token.
func httpClient(token string) *http.Client {
	if token == "" {
		return nil
	}
	return &http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}}
}

func main() {
	ctx := context.Background()
	token := os.Getenv("SUBSTRATE_ENV_AUTH_TOKEN")

	// 1. Create an environment via the env Client.
	c, err := env.NewClient(env.ClientOptions{
		Endpoint:  "http://localhost:7777",
		AuthToken: token,
	})
	if err != nil {
		log.Fatalf("connecting to Substrate: %v", err)
	}
	defer c.Close()

	e, err := c.Create(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "mcp-demo"})
	if err != nil {
		log.Fatalf("creating environment: %v", err)
	}
	defer e.Delete(ctx)

	// 2. Initialize MCP Client using official go-sdk.
	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "mcp-demo-agent",
		Version: "1.0.0",
	}, nil)

	// 3. Connect to the environment's MCP endpoint.
	transport := &mcp.StreamableClientTransport{
		Endpoint:   "http://localhost:7777/v1alpha/envs/mcp-demo/mcp",
		HTTPClient: httpClient(token),
	}

	session, err := mcpClient.Connect(ctx, transport, nil)
	if err != nil {
		log.Fatalf("connecting to environment MCP endpoint: %v", err)
	}
	defer session.Close()

	// 4. Discover tools.
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		log.Fatalf("listing tools: %v", err)
	}

	fmt.Println("Discovered tools:")
	for _, tool := range tools.Tools {
		fmt.Printf("- %s: %s\n", tool.Name, tool.Description)
	}

	// 5. Execute a tool call (e.g. shell).
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "shell",
		Arguments: map[string]any{
			"command": "uname -a",
		},
	})
	if err != nil {
		log.Fatalf("calling tool: %v", err)
	}

	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			fmt.Println("\nTool output:\n" + text.Text)
		}
	}
}
