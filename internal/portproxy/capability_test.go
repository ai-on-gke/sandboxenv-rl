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

package portproxy_test

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/portproxy"
)

// The Python client computes the same capability; this vector keeps the two
// in step.
func TestCapabilityVector(t *testing.T) {
	got := portproxy.Capability("s3cret", "ate-env", "env1", 8000, time.Unix(1700000000, 0))
	want := "v1.1700000000.a2f01c6feececba10e18c15123f7f7124cd436d7ec370910c470f75325920f2f"
	if got != want {
		t.Errorf("Capability = %q, want %q", got, want)
	}
}

func TestVerifyCapability(t *testing.T) {
	now := time.Unix(1700000000, 0)
	good := portproxy.Capability("s3cret", "ate-env", "env1", 8000, now.Add(time.Hour))
	if err := portproxy.VerifyCapability("s3cret", good, "ate-env", "env1", 8000, now); err != nil {
		t.Fatalf("valid capability refused: %v", err)
	}
	tampered := good[:len(good)-1] + map[bool]string{true: "0", false: "1"}[good[len(good)-1] != '0']
	for name, tc := range map[string]struct {
		token, capability, atespace, id string
		port                            int
		now                             time.Time
	}{
		"another token":    {"other", good, "ate-env", "env1", 8000, now},
		"another env":      {"s3cret", good, "ate-env", "env2", 8000, now},
		"another atespace": {"s3cret", good, "team", "env1", 8000, now},
		"another port":     {"s3cret", good, "ate-env", "env1", 8001, now},
		"expired":          {"s3cret", good, "ate-env", "env1", 8000, now.Add(2 * time.Hour)},
		"too far ahead":    {"s3cret", portproxy.Capability("s3cret", "ate-env", "env1", 8000, now.Add(portproxy.MaxCapabilityTTL+time.Minute)), "ate-env", "env1", 8000, now},
		"tampered mac":     {"s3cret", tampered, "ate-env", "env1", 8000, now},
		"tampered expiry":  {"s3cret", strings.Replace(good, ".1700003600.", ".1700007200.", 1), "ate-env", "env1", 8000, now},
		"another version":  {"s3cret", "v2" + good[2:], "ate-env", "env1", 8000, now},
		"the token itself": {"s3cret", "s3cret", "ate-env", "env1", 8000, now},
		"empty":            {"s3cret", "", "ate-env", "env1", 8000, now},
		"not hex":          {"s3cret", "v1.1700003600.zz", "ate-env", "env1", 8000, now},
	} {
		if err := portproxy.VerifyCapability(tc.token, tc.capability, tc.atespace, tc.id, tc.port, tc.now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// capabilitySetup serves the proxy the way ate-env-api does with
// -auth-token-file, in front of a WebSocket-like daemon on env1:8000 that
// records each request it gets.
func capabilitySetup(t *testing.T) (*setup, string, func() []*http.Request) {
	t.Helper()
	s := newSetup(t)
	var mu sync.Mutex
	var seen []*http.Request
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(r.Context()))
		mu.Unlock()
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			io.WriteString(w, "daemon")
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		rw.Flush()
	}))
	auth, err := apiservice.NewBearerAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	p := mustNew(t, portproxy.Options{
		ConnectAddr: s.routerAddr, DefaultAtespace: "ate-env", HandshakeTimeout: 5 * time.Second,
		Authenticate: auth.Check, AuthenticateCapability: auth.CheckCapability,
	})
	srv := httptest.NewServer(portproxy.Dispatch(p, http.NotFoundHandler()))
	t.Cleanup(srv.Close)
	return s, strings.TrimPrefix(srv.URL, "http://"), func() []*http.Request {
		mu.Lock()
		defer mu.Unlock()
		out := seen
		seen = nil
		return out
	}
}

func basic(capability string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(portproxy.CapabilityUser+":"+capability))
}

// rawRequest sends one request line and headers and returns the status code.
func rawRequest(t *testing.T, addr, path string, headers ...string) int {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	req := "GET " + path + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	for _, h := range headers {
		req += h + "\r\n"
	}
	fmt.Fprint(conn, req+"\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func TestWebSocketCapability(t *testing.T) {
	s, addr, seen := capabilitySetup(t)
	const events = "/v1alpha/envs/env1/ports/8000/sockets/events/c1"
	upgrade := []string{"Connection: Upgrade", "Upgrade: websocket"}
	valid := portproxy.Capability("s3cret", "ate-env", "env1", 8000, time.Now().Add(time.Hour))

	// A WebSocket upgrade with a valid capability reaches the daemon, which
	// never sees the capability.
	if code := rawRequest(t, addr, events, append(upgrade, "Authorization: "+basic(valid))...); code != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade with a valid capability: got %d, want 101", code)
	}
	got := seen()
	if len(got) != 1 || got[0].URL.Path != "/sockets/events/c1" {
		t.Fatalf("daemon got %v, want one request for /sockets/events/c1", got)
	}
	if v := got[0].Header.Get("Authorization"); v != "" {
		t.Errorf("daemon saw Authorization %q; the capability must not reach it", v)
	}

	// Refused before the proxy dials the router.
	connects := s.router.connectCount()
	other := portproxy.Capability("s3cret", "ate-env", "env2", 8000, time.Now().Add(time.Hour))
	expired := portproxy.Capability("s3cret", "ate-env", "env1", 8000, time.Now().Add(-time.Minute))
	foreign := portproxy.Capability("other-token", "ate-env", "env1", 8000, time.Now().Add(time.Hour))
	for name, headers := range map[string][]string{
		"upgrade without a credential":            upgrade,
		"upgrade with another env's capability":   append(upgrade, "Authorization: "+basic(other)),
		"upgrade with an expired capability":      append(upgrade, "Authorization: "+basic(expired)),
		"upgrade with another token's capability": append(upgrade, "Authorization: "+basic(foreign)),
		"upgrade with the token as a capability":  append(upgrade, "Authorization: "+basic("s3cret")),
		// A capability is for WebSocket upgrades only.
		"plain request with a valid capability": {"Authorization: " + basic(valid)},
		"other upgrade with a valid capability": {"Connection: Upgrade", "Upgrade: echo", "Authorization: " + basic(valid)},
	} {
		if code := rawRequest(t, addr, events, headers...); code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, code)
		}
	}
	if n := s.router.connectCount() - connects; n != 0 {
		t.Errorf("router saw %d CONNECTs for refused requests, want 0", n)
	}
	if got := seen(); len(got) != 0 {
		t.Errorf("refused requests reached the daemon: %d", len(got))
	}

	// With the token in X-Env-Authorization, a capability that rides along is
	// still dropped: it is never the daemon's credential.
	if code := rawRequest(t, addr, "/v1alpha/envs/env1/ports/8000/health",
		portproxy.EnvAuthorizationHeader+": Bearer s3cret", "Authorization: "+basic(valid)); code != http.StatusOK {
		t.Fatalf("token in %s with a capability: got %d, want 200", portproxy.EnvAuthorizationHeader, code)
	}
	if got := seen(); len(got) != 1 || got[0].Header.Get("Authorization") != "" {
		t.Errorf("daemon saw %v; the capability must not reach it", got)
	}
}

// Without Authenticate, the proxy still drops a capability, and leaves any
// other Authorization to the daemon.
func TestDropsCapabilityWithoutAuthenticate(t *testing.T) {
	s := newSetup(t)
	var seen *http.Request
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		io.WriteString(w, "daemon")
	}))
	for value, want := range map[string]string{
		basic("v1.1.00"): "",
		"Basic " + base64.StdEncoding.EncodeToString([]byte("user:daemon-password")): "Basic " + base64.StdEncoding.EncodeToString([]byte("user:daemon-password")),
	} {
		h := target("env1", 8000)
		h["Authorization"] = value
		if resp, body := s.do(t, http.MethodGet, "/", "", h); resp.StatusCode != http.StatusOK || body != "daemon" {
			t.Fatalf("got %d %q", resp.StatusCode, body)
		}
		if got := seen.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization %q: daemon saw %q, want %q", value, got, want)
		}
	}
}
