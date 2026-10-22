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

package hostcheck

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/portproxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestAllowed(t *testing.T) {
	c, err := New([]string{"ate-env-api", "ate-env-api.ate-env.svc.cluster.local.", "*.Internal.Example.COM"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"", true},
		{"127.0.0.1:7777", true},
		{"10.4.0.12", true},
		{"[::1]:7777", true},
		{"[::1]", true},
		{"[fe80::1%25eth0]:7777", true},
		{"localhost:7777", true},
		{"LOCALHOST", true},
		{"localhost.:7777", true},
		{"app.localhost:7777", true},
		{"ate-env-api:7777", true},
		{"ate-env-api.ate-env.svc.cluster.local:7777", true},
		{"lb.internal.example.com:7777", true},
		{"a.b.internal.example.com", true},
		{"internal.example.com:7777", false}, // the wildcard does not match the name itself
		{"attacker.example:7777", false},
		{"ate-env-api.attacker.example", false},
		{"notlocalhost:7777", false},
		{"localhost.attacker.example", false},
		{"127.0.0.1.attacker.example", false},
	} {
		if got := c.Allowed(tc.host); got != tc.want {
			t.Errorf("Allowed(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestNewRejectsBadEntries(t *testing.T) {
	for _, bad := range []string{"http://x.example", "x.example:7777", "x.example/path", "*.", "a.*.example", "*example.com", "*.*.example.com"} {
		if _, err := New([]string{bad}); err == nil {
			t.Errorf("New(%q): want error", bad)
		}
	}
	if _, err := New([]string{"", " ", "ok.example"}); err != nil {
		t.Errorf("blank entries must be skipped: %v", err)
	}
}

func TestParseList(t *testing.T) {
	got := ParseList(" a.example, ,*.b.example,")
	if strings.Join(got, "|") != "a.example|*.b.example" {
		t.Errorf("ParseList = %q", got)
	}
}

func TestWrapRefusesAndLogsOncePerHost(t *testing.T) {
	c, _ := New(nil)
	reached := 0
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++ }))
	for range 3 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://attacker.example:7777/healthz", nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "--allowed-hosts") {
			t.Fatalf("got %d %q, want 403 naming --allowed-hosts", rec.Code, rec.Body.String())
		}
	}
	if reached != 0 {
		t.Errorf("next handler reached %d times", reached)
	}
	if len(c.logged) != 1 {
		t.Errorf("logged %d hosts, want 1", len(c.logged))
	}
	for i := range maxLogged + 10 {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = "h" + strings.Repeat("x", i) + ".example"
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if len(c.logged) != maxLogged || !c.loggedMax {
		t.Errorf("logged %d hosts (max flag %v), want %d and the flag set", len(c.logged), c.loggedMax, maxLogged)
	}

	// HTTP/2 requests pass whatever their authority.
	req := httptest.NewRequest("GET", "http://attacker.example:7777/", nil)
	req.ProtoMajor, req.ProtoMinor = 2, 0
	h.ServeHTTP(httptest.NewRecorder(), req)
	if reached != 1 {
		t.Errorf("HTTP/2 request not passed through")
	}
}

// TestListenerStack serves the same composition as cmd/ate-env-api, the port
// proxy's Dispatch in front of a mux with /healthz, the MCP route and gRPC,
// behind the check, on one listener with HTTP/1.1 and h2c.
func TestListenerStack(t *testing.T) {
	grpcServer := grpc.NewServer()
	healthpb.RegisterHealthServer(grpcServer, health.NewServer())
	t.Cleanup(grpcServer.Stop)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/v1alpha/envs/{id}/mcp", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "mcp\n") })
	mux.Handle("/", grpcServer)

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	connectAddr := closed.Addr().String()
	closed.Close() // nothing listens: the proxy fails fast instead of reaching a router
	pp, err := portproxy.New(portproxy.Options{ConnectAddr: connectAddr, WakeBudget: 50 * time.Millisecond, HandshakeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c, err := New([]string{"ate-env-api.ate-env", "*.internal.example"})
	if err != nil {
		t.Fatal(err)
	}

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: c.Wrap(portproxy.Dispatch(pp, mux)), Protocols: &protocols}
	go srv.Serve(lis)
	t.Cleanup(func() { srv.Close() })
	addr := lis.Addr().String()

	get := func(host, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s %s: %v", host, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	refused := func(code int, body string) bool {
		return code == http.StatusForbidden && strings.Contains(body, "--allowed-hosts")
	}

	for _, path := range []string{"/healthz", "/v1alpha/envs/dev1/mcp", "/v1alpha/envs/dev1/ports/8000/"} {
		if code, body := get("attacker.example:7777", path); !refused(code, body) {
			t.Errorf("rebinding host on %s: got %d %q, want the hostcheck 403", path, code, body)
		}
	}
	for _, host := range []string{addr, "localhost:7777", "ate-env-api.ate-env:7777", "lb.internal.example"} {
		if code, body := get(host, "/healthz"); code != http.StatusOK {
			t.Errorf("host %s on /healthz: got %d %q, want 200", host, code, body)
		}
		if code, body := get(host, "/v1alpha/envs/dev1/mcp"); code != http.StatusOK || body != "mcp\n" {
			t.Errorf("host %s on the MCP route: got %d %q, want 200", host, code, body)
		}
		// The proxy is reached (and fails, nothing listens), not refused.
		if code, body := get(host, "/v1alpha/envs/dev1/ports/8000/"); refused(code, body) {
			t.Errorf("host %s on the path-form proxy: refused by the host check", host)
		}
	}

	// gRPC over h2c with a foreign :authority still works.
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithAuthority("attacker.example:7777"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("gRPC health over h2c with a foreign authority: %v, %v", resp, err)
	}
}
