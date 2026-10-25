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
	"slices"
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

	// Wrap checks Host regardless of HTTP version or Content-Type.
	for _, tc := range []struct {
		proto       int
		contentType string
	}{
		{2, "application/grpc"},
		{2, "application/grpc+proto"},
		{2, ""},
		{2, "text/html"},
		{2, "application/json"},
		{1, "application/grpc"},
	} {
		reached = 0
		req := httptest.NewRequest("POST", "http://attacker.example:7777/", nil)
		req.ProtoMajor, req.ProtoMinor = tc.proto, 0
		if tc.contentType != "" {
			req.Header.Set("Content-Type", tc.contentType)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if reached != 0 || rec.Code != http.StatusForbidden {
			t.Errorf("HTTP/%d %q with a foreign host: reached=%d code=%d, want 403", tc.proto, tc.contentType, reached, rec.Code)
		}
	}
}

func TestClusterDNS(t *testing.T) {
	for _, tc := range []struct {
		name, conf, ns, domain string
	}{
		{"kubelet", "search team-env.svc.cluster.local svc.cluster.local cluster.local us-central1-c.c.proj.internal\nnameserver 10.0.0.10\noptions ndots:5\n", "team-env", "cluster.local"},
		{"custom domain", "nameserver 10.0.0.10\nsearch ate-env.svc.corp.k8s svc.corp.k8s corp.k8s\n", "ate-env", "corp.k8s"},
		{"trailing dot and case", "search Team-Env.svc.Cluster.Local.\n", "team-env", "cluster.local"},
		{"later entry", "search example.com ate-env.svc.cluster.local\n", "ate-env", "cluster.local"},
		{"not a pod", "search example.com\nnameserver 1.1.1.1\n", "", ""},
		{"svc without namespace", "search svc.cluster.local\n", "", ""},
		{"empty", "", "", ""},
	} {
		ns, domain := ClusterDNS(tc.conf)
		if ns != tc.ns || domain != tc.domain {
			t.Errorf("%s: ClusterDNS = %q, %q; want %q, %q", tc.name, ns, domain, tc.ns, tc.domain)
		}
	}
}

func TestServiceHosts(t *testing.T) {
	if got, want := ServiceHosts("ate-env-api", "team-env", "corp.k8s"), []string{"ate-env-api", "ate-env-api.team-env", "ate-env-api.team-env.svc", "ate-env-api.team-env.svc.corp.k8s"}; !slices.Equal(got, want) {
		t.Errorf("with a domain: %q, want %q", got, want)
	}
	if got, want := ServiceHosts("ate-env-api", "team-env", ""), []string{"ate-env-api", "ate-env-api.team-env", "ate-env-api.team-env.svc"}; !slices.Equal(got, want) {
		t.Errorf("without a domain: %q, want %q", got, want)
	}
	if got, want := ServiceHosts("ate-env-api", "", ""), []string{"ate-env-api"}; !slices.Equal(got, want) {
		t.Errorf("without a namespace: %q, want %q", got, want)
	}
	if got := ServiceHosts("", "team-env", "cluster.local"); got != nil {
		t.Errorf("without a name: %q, want none", got)
	}
}

// TestListenerStack serves the same composition as cmd/ate-env-api, the port
// proxy's Dispatch in front of a mux with /healthz, the MCP route and gRPC,
// behind the check, on one listener with HTTP/1.1 and h2c.
func TestListenerStack(t *testing.T) {
	grpcServer := grpc.NewServer()
	healthpb.RegisterHealthServer(grpcServer, health.NewServer())
	t.Cleanup(grpcServer.Stop)

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

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })))
	mux.Handle("/v1alpha/envs/{id}/mcp", c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "mcp\n") })))
	mux.Handle("/", grpcServer)

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: portproxy.Dispatch(pp, mux, c.Wrap), Protocols: &protocols}
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

	// A browser request that a front proxy relays over h2c carries a foreign
	// :authority: refused like HTTP/1.x, even if Content-Type is application/grpc.
	var h2c http.Protocols
	h2c.SetUnencryptedHTTP2(true)
	h2Client := &http.Client{Transport: &http.Transport{Protocols: &h2c}}
	for _, path := range []string{"/healthz", "/v1alpha/envs/dev1/mcp", "/v1alpha/envs/dev1/ports/8000/"} {
		for _, ct := range []string{"", "application/grpc"} {
			req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
			req.Host = "attacker.example:7777"
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			resp, err := h2Client.Do(req)
			if err != nil {
				t.Fatalf("h2c GET %s: %v", path, err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.ProtoMajor != 2 || !refused(resp.StatusCode, string(b)) {
				t.Errorf("h2c rebinding host on %s (Content-Type %q): got HTTP/%d %d %q, want the hostcheck 403 over HTTP/2", path, ct, resp.ProtoMajor, resp.StatusCode, b)
			}
		}
	}
	req, _ := http.NewRequest("GET", "http://"+addr+"/healthz", nil)
	req.Host = "ate-env-api.ate-env:7777"
	if resp, err := h2Client.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("h2c with an allowed host: %v, %v; want 200", resp, err)
	} else {
		resp.Body.Close()
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
