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
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/env/internal/internaltest/testcerts"
	"github.com/agent-substrate/env/internal/portproxy"
	"github.com/agent-substrate/env/internal/routertls"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// recordingRouter wraps the fake router and records each CONNECT's actor
// reference and authority.
type recordingRouter struct {
	*fakerouter.Router
	mu       sync.Mutex
	connects []string
}

func (r *recordingRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		r.mu.Lock()
		r.connects = append(r.connects, req.Header.Get(ate.TargetActorHeader)+" "+req.Host)
		r.mu.Unlock()
	}
	r.Router.ServeHTTP(w, req)
}

func (r *recordingRouter) connectCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.connects)
}

type setup struct {
	router     *recordingRouter
	routerAddr string
	proxy      *httptest.Server
}

func newSetup(t *testing.T) *setup {
	t.Helper()
	rr := &recordingRouter{Router: fakerouter.New()}
	srv := httptest.NewServer(rr)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	p := mustNew(t, portproxy.Options{
		ConnectAddr:      addr,
		DefaultAtespace:  "ate-env",
		HandshakeTimeout: 5 * time.Second,
		WakeBudget:       2 * time.Second,
	})
	proxy := httptest.NewServer(p)
	t.Cleanup(proxy.Close)
	return &setup{router: rr, routerAddr: addr, proxy: proxy}
}

// mustNew builds a proxy and drops its idle tunnels when the test ends, so
// the fake router's tunnel goroutines end with the test.
func mustNew(t *testing.T, opts portproxy.Options) *portproxy.Proxy {
	t.Helper()
	p, err := portproxy.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func (s *setup) do(t *testing.T, method, path, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, s.proxy.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func target(id string, port int) map[string]string {
	return map[string]string{portproxy.EnvIDHeader: id, portproxy.EnvPortHeader: fmt.Sprint(port)}
}

func TestForwardsToEnvironmentPort(t *testing.T) {
	s := newSetup(t)
	var seen *http.Request
	var seenBody string
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("X-Daemon", "yes")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "daemon says hi")
	}))

	h := target("env1", 8000)
	h["X-Custom"] = "kept"
	resp, body := s.do(t, http.MethodPost, "/execute?lang=python", `{"code":"1+1"}`, h)

	if resp.StatusCode != http.StatusCreated || body != "daemon says hi" || resp.Header.Get("X-Daemon") != "yes" {
		t.Fatalf("got %d %q (X-Daemon %q), want 201 from the daemon", resp.StatusCode, body, resp.Header.Get("X-Daemon"))
	}
	if seen.Method != http.MethodPost || seen.URL.RequestURI() != "/execute?lang=python" || seenBody != `{"code":"1+1"}` {
		t.Errorf("daemon saw %s %s %q", seen.Method, seen.URL.RequestURI(), seenBody)
	}
	if seen.Host != "127.0.0.1:8000" {
		t.Errorf("daemon saw Host %q, want 127.0.0.1:8000 (what an in-sandbox client sends)", seen.Host)
	}
	for _, hdr := range []string{portproxy.EnvIDHeader, portproxy.EnvAtespaceHeader, portproxy.EnvPortHeader} {
		if v := seen.Header.Get(hdr); v != "" {
			t.Errorf("daemon saw routing header %s=%q; the proxy must strip it", hdr, v)
		}
	}
	if seen.Header.Get("X-Custom") != "kept" {
		t.Error("other request headers must reach the daemon")
	}
	if got := s.router.connects; len(got) != 1 || got[0] != "ate-env/env1 env1."+ate.DefaultHostSuffix+":8000" {
		t.Errorf("router saw CONNECTs %q, want one for ate-env/env1 on port 8000", got)
	}
}

func TestAtespaceHeaderNamesTheActor(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("env1", 9000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h := target("env1", 9000)
	h[portproxy.EnvAtespaceHeader] = "team-a"
	if resp, body := s.do(t, http.MethodGet, "/", "", h); resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if got := s.router.connects; len(got) != 1 || !strings.HasPrefix(got[0], "team-a/env1 ") {
		t.Errorf("router saw %q, want the actor team-a/env1", got)
	}
}

func TestStreamsResponses(t *testing.T) {
	s := newSetup(t)
	release := make(chan struct{})
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "second\n")
	}))
	req, _ := http.NewRequest(http.MethodGet, s.proxy.URL+"/events", nil)
	for k, v := range target("env1", 8000) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, len("first\n"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "first\n" {
		t.Fatalf("first chunk %q, %v: the proxy must flush before the daemon finishes", buf, err)
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "second\n" {
		t.Errorf("rest %q", rest)
	}
}

func TestRejectsBadTargets(t *testing.T) {
	s := newSetup(t)
	for name, h := range map[string]map[string]string{
		"no id":           {portproxy.EnvPortHeader: "8000"},
		"port not number": {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "http"},
		"port zero":       {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "0"},
		"port too large":  {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "70000"},
		"id uppercase":    {portproxy.EnvIDHeader: "Env1", portproxy.EnvPortHeader: "8000"},
		"id with slash":   {portproxy.EnvIDHeader: "a/b", portproxy.EnvPortHeader: "8000"},
		"port with sign":  {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "+8000"},
		"port zero-led":   {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "08000"},
		"bad atespace":    {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "8000", portproxy.EnvAtespaceHeader: "x y"},
		"guest port":      {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "80"},
	} {
		if resp, body := s.do(t, http.MethodGet, "/", "", h); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: got %d %q, want 400", name, resp.StatusCode, body)
		}
	}
	// A routing header sent twice is ambiguous.
	req, _ := http.NewRequest(http.MethodGet, s.proxy.URL+"/", nil)
	req.Header.Add(portproxy.EnvIDHeader, "env1")
	req.Header.Add(portproxy.EnvIDHeader, "env2")
	req.Header.Set(portproxy.EnvPortHeader, "8000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("duplicate %s: got %d, want 400", portproxy.EnvIDHeader, resp.StatusCode)
	}
	if n := s.router.connectCount(); n != 0 {
		t.Errorf("router saw %d CONNECTs for rejected requests, want 0", n)
	}
}

// The router accepts the CONNECT and answers the request inside the tunnel
// with its own error; without a state reader the proxy passes it unchanged.
func TestPassesRouterRefusals(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("asleep", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.Running = func(id string) bool { return id != "asleep" }

	if resp, body := s.do(t, http.MethodGet, "/", "", target("missing", 8000)); resp.StatusCode != http.StatusNotFound || body != "actor ate-env/missing not found" {
		t.Errorf("unknown environment: got %d %q, want the router's 404", resp.StatusCode, body)
	}
	if resp, body := s.do(t, http.MethodGet, "/", "", target("asleep", 8000)); resp.StatusCode != http.StatusServiceUnavailable || body != "actor ate-env/asleep unavailable" {
		t.Errorf("not running: got %d %q, want the router's 503", resp.StatusCode, body)
	}
}

// Substrate v0.6.0's router refuses a CONNECT only at the Envoy level (for
// example overload), never for an actor state. Such a 503 is retried within
// the wake budget instead of passed on at once.
func TestRetriesARefusedConnect(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("waking", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "awake") }))
	var mu sync.Mutex
	refusals := 0
	s.router.RefuseConnect = func(string) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if refusals < 2 {
			refusals++
			return http.StatusServiceUnavailable, "upstream overflow"
		}
		return 0, ""
	}
	resp, body := s.do(t, http.MethodGet, "/", "", target("waking", 8000))
	if resp.StatusCode != http.StatusOK || body != "awake" {
		t.Fatalf("got %d %q, want the daemon's answer after the retries", resp.StatusCode, body)
	}
	if n := s.router.connectCount(); n != 3 {
		t.Errorf("router saw %d CONNECTs, want 3 (two refused, one accepted)", n)
	}
}

func TestWakeBudgetBoundsTheWait(t *testing.T) {
	s := newSetup(t)
	p := mustNew(t, portproxy.Options{ConnectAddr: s.routerAddr, DefaultAtespace: "ate-env", HandshakeTimeout: 5 * time.Second, WakeBudget: 300 * time.Millisecond})
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	s.router.RegisterPort("asleep", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.RefuseConnect = func(string) (int, string) { return http.StatusServiceUnavailable, "upstream overflow" }
	start := time.Now()
	code, body, _ := get(t, srv.URL, target("asleep", 8000))
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "not available") {
		t.Errorf("got %d %q, want 503 once the wake budget is spent", code, body)
	}
	if took := time.Since(start); took < 300*time.Millisecond || took > 3*time.Second {
		t.Errorf("waited %s, want about the 300ms budget", took)
	}
	if n := s.router.connectCount(); n < 2 {
		t.Errorf("router saw %d CONNECTs, want at least 2 (the budget allows retries)", n)
	}
}

// stateProxy serves a proxy with a 30 s wake budget whose state reader
// answers with state(n), n counting the reads from 1.
func stateProxy(t *testing.T, s *setup, state func(n int) (ateapipb.ActorState, error)) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	reads := 0
	p := mustNew(t, portproxy.Options{
		ConnectAddr:      s.routerAddr,
		DefaultAtespace:  "ate-env",
		HandshakeTimeout: 5 * time.Second,
		WakeBudget:       30 * time.Second,
		ActorState: func(_ context.Context, atespace, id string) (ateapipb.ActorState, error) {
			mu.Lock()
			reads++
			n := reads
			mu.Unlock()
			return state(n)
		},
	})
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, func() int { mu.Lock(); defer mu.Unlock(); return reads }
}

func get(t *testing.T, base string, headers map[string]string) (int, string, time.Duration) {
	t.Helper()
	code, _, body, took := getWithHeader(t, base, headers, "")
	return code, body, took
}

// getWithHeader is get that also returns the answer's header named want.
func getWithHeader(t *testing.T, base string, headers map[string]string, want string) (int, string, string, time.Duration) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get(want), string(b), time.Since(start)
}

// routerBody is the router's 503 text for an actor that cannot resume from
// state (workflow_resume.go and ingress/errors.go in Substrate v0.6.0).
func routerBody(id, state string) string {
	return "actor ate-env/" + id + " unavailable: AssignWorker prerequisite not met for Actor: ate-env/" + id +
		" (got: ACTOR_STATE_" + state + ", want ACTOR_STATE_SUSPENDED)"
}

// The router answers a crashed or deleting actor with the same 503 as a
// passing state, inside an accepted CONNECT. The proxy reads the state for
// that answer and makes it final: 410 for CRASHED, 404 for DELETING, each
// with X-Env-State, and 404 when the environment is gone.
func TestRouterErrorForEnvironmentThatWillNotWake(t *testing.T) {
	for _, tc := range []struct {
		name       string
		denyCode   int
		denyBody   string
		state      ateapipb.ActorState
		err        error
		status     int
		header     string
		bodyNaming string
	}{
		{"crashed", 503, routerBody("broken", "CRASHED"), ateapipb.ActorState_ACTOR_STATE_CRASHED, nil, http.StatusGone, "CRASHED", "actor state CRASHED"},
		{"deleting", 503, routerBody("broken", "DELETING"), ateapipb.ActorState_ACTOR_STATE_DELETING, nil, http.StatusNotFound, "DELETING", "actor state DELETING"},
		{"deleted after the 503", 503, routerBody("broken", "DELETING"), 0, status.Error(codes.NotFound, "environment not found"), http.StatusNotFound, "", "was not found"},
		{"deleted", 404, "actor ate-env/broken not found", 0, status.Error(codes.NotFound, "environment not found"), http.StatusNotFound, "", "actor ate-env/broken not found"},
	} {
		s := newSetup(t)
		s.router.RegisterPort("broken", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		s.router.Deny = func(string) (int, string) { return tc.denyCode, tc.denyBody }
		srv, reads := stateProxy(t, s, func(int) (ateapipb.ActorState, error) { return tc.state, tc.err })
		code, header, body, took := getWithHeader(t, srv.URL, target("broken", 8000), portproxy.EnvStateHeader)
		if code != tc.status || header != tc.header || !strings.Contains(body, tc.bodyNaming) {
			t.Errorf("%s: got %d %s=%q %q, want %d %q naming %q", tc.name, code, portproxy.EnvStateHeader, header, body, tc.status, tc.header, tc.bodyNaming)
		}
		if n := reads(); n != 1 {
			t.Errorf("%s: %d state reads, want 1", tc.name, n)
		}
		if took > time.Second {
			t.Errorf("%s: answered after %s, want at once", tc.name, took)
		}
	}
}

// A router 503 for an actor in a passing state (resuming, suspending) is the
// caller's to retry: it passes unchanged after the state read.
func TestRouterErrorForPassingStatePassesUnchanged(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("waking", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.Running = func(string) bool { return false }
	srv, reads := stateProxy(t, s, func(int) (ateapipb.ActorState, error) { return ateapipb.ActorState_ACTOR_STATE_RESUMING, nil })
	code, header, body, _ := getWithHeader(t, srv.URL, target("waking", 8000), portproxy.EnvStateHeader)
	if code != http.StatusServiceUnavailable || header != "" || body != "actor ate-env/waking unavailable" {
		t.Errorf("got %d %s=%q %q, want the router's 503 unchanged", code, portproxy.EnvStateHeader, header, body)
	}
	if n := reads(); n != 1 {
		t.Errorf("%d state reads, want 1", n)
	}
}

// The daemon's own 503 and 404 pass unchanged and cost no state read; a
// daemon error that looks like the router's costs one read and passes
// unchanged while the actor runs.
func TestDaemonErrorsPassUnchanged(t *testing.T) {
	s := newSetup(t)
	var answer atomic.Value
	answer.Store([2]string{"503", "busy"})
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := answer.Load().([2]string)
		code, _ := strconv.Atoi(a[0])
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(code)
		io.WriteString(w, a[1])
	}))
	srv, reads := stateProxy(t, s, func(int) (ateapipb.ActorState, error) { return ateapipb.ActorState_ACTOR_STATE_RUNNING, nil })
	for _, a := range [][2]string{{"503", "busy"}, {"404", "no such page"}} {
		answer.Store(a)
		for range 5 {
			code, body, _ := get(t, srv.URL, target("env1", 8000))
			if strconv.Itoa(code) != a[0] || body != a[1] {
				t.Errorf("daemon %s %q: got %d %q, want it unchanged", a[0], a[1], code, body)
			}
		}
	}
	if n := reads(); n != 0 {
		t.Errorf("%d state reads for the daemon's own errors, want 0", n)
	}
	answer.Store([2]string{"503", "actor ate-env/env1 unavailable"})
	if code, body, _ := get(t, srv.URL, target("env1", 8000)); code != 503 || body != "actor ate-env/env1 unavailable" {
		t.Errorf("router-like daemon error: got %d %q, want it unchanged", code, body)
	}
	if n := reads(); n != 1 {
		t.Errorf("%d state reads for a router-like error, want 1", n)
	}
}

// A burst of router errors for one environment costs one state read, also
// when the requests run at once.
func TestStateReadIsSharedAndCached(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("broken", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.Deny = func(string) (int, string) { return 503, routerBody("broken", "CRASHED") }
	srv, reads := stateProxy(t, s, func(int) (ateapipb.ActorState, error) {
		time.Sleep(50 * time.Millisecond) // concurrent answers wait on this read
		return ateapipb.ActorState_ACTOR_STATE_CRASHED, nil
	})
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _, _ := get(t, srv.URL, target("broken", 8000)); code != http.StatusGone {
				t.Errorf("concurrent: got %d, want 410", code)
			}
		}()
	}
	wg.Wait()
	for range 10 {
		if code, _, _ := get(t, srv.URL, target("broken", 8000)); code != http.StatusGone {
			t.Errorf("sequential: got %d, want 410", code)
		}
	}
	if n := reads(); n != 1 {
		t.Errorf("%d state reads for 20 answers within the cache time, want 1", n)
	}
}

// A WebSocket upgrade the router refuses for a crashed actor gets the same
// final answer.
func TestUpgradeToCrashedEnvironment(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("broken", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.Deny = func(string) (int, string) { return 503, routerBody("broken", "CRASHED") }
	srv, _ := stateProxy(t, s, func(int) (ateapipb.ActorState, error) { return ateapipb.ActorState_ACTOR_STATE_CRASHED, nil })
	headers := target("broken", 8000)
	headers["Connection"] = "Upgrade"
	headers["Upgrade"] = "websocket"
	headers["Sec-WebSocket-Version"] = "13"
	headers["Sec-WebSocket-Key"] = "dGhlIHNhbXBsZSBub25jZQ=="
	code, header, _, _ := getWithHeader(t, srv.URL, headers, portproxy.EnvStateHeader)
	if code != http.StatusGone || header != "CRASHED" {
		t.Errorf("upgrade: got %d %s=%q, want 410 CRASHED", code, portproxy.EnvStateHeader, header)
	}
}

// Requests that the daemon answers read no state.
func TestNoStateReadOnTheFastPath(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	srv, reads := stateProxy(t, s, func(int) (ateapipb.ActorState, error) { return ateapipb.ActorState_ACTOR_STATE_RUNNING, nil })
	for range 3 {
		if code, body, _ := get(t, srv.URL, target("env1", 8000)); code != http.StatusOK || body != "ok" {
			t.Fatalf("got %d %q", code, body)
		}
	}
	if n := reads(); n != 0 {
		t.Errorf("%d state reads, want 0", n)
	}
}

// A state read that fails for another reason than NotFound leaves the
// router's answer unchanged and is logged once, not once for each answer.
func TestFailedStateReadIsLoggedOnce(t *testing.T) {
	var buf safeBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	s := newSetup(t)
	s.router.RegisterPort("broken", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.Deny = func(string) (int, string) { return 503, routerBody("broken", "CRASHED") }
	srv, reads := stateProxy(t, s, func(int) (ateapipb.ActorState, error) {
		return 0, status.Error(codes.PermissionDenied, "no access")
	})
	for range 3 {
		if code, _, _ := get(t, srv.URL, target("broken", 8000)); code != http.StatusServiceUnavailable {
			t.Errorf("got %d, want the router's 503 unchanged", code)
		}
	}
	if n := reads(); n != 1 {
		t.Errorf("%d state reads, want 1 (cached)", n)
	}
	if got := strings.Count(buf.String(), "reading the state of environment ate-env/broken failed"); got != 1 {
		t.Errorf("logged the failed state read %d times, want 1:\n%s", got, buf.String())
	}
}

// A state read that stalls hits its own deadline, and the router's answer
// passes unchanged instead of hanging on the read.
func TestStalledStateReadHitsItsDeadline(t *testing.T) {
	portproxy.SetStateReadTimeout(t, 200*time.Millisecond)
	s := newSetup(t)
	s.router.RegisterPort("broken", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.Deny = func(string) (int, string) { return 503, routerBody("broken", "CRASHED") }
	p := mustNew(t, portproxy.Options{
		ConnectAddr: s.routerAddr, DefaultAtespace: "ate-env", HandshakeTimeout: 5 * time.Second,
		ActorState: func(ctx context.Context, _, _ string) (ateapipb.ActorState, error) {
			<-ctx.Done() // a control plane that never answers
			return 0, ctx.Err()
		},
	})
	stalled := httptest.NewServer(p)
	t.Cleanup(stalled.Close)
	req, _ := http.NewRequest(http.MethodGet, stalled.URL+"/", nil)
	for k, v := range target("broken", 8000) {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request did not end within 5s with a stalled state read: %v", err)
	}
	resp.Body.Close()
	took := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("got %d, want the router's 503 unchanged", resp.StatusCode)
	}
	if took < 200*time.Millisecond || took > 2*time.Second {
		t.Errorf("took %s, want about the 200ms read deadline (the read must run and end)", took)
	}
}

// A caller that leaves during a state read gets no log line for it.
func TestCallerLeavingDuringStateReadLogsNothing(t *testing.T) {
	var buf safeBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	portproxy.SetStateReadTimeout(t, 500*time.Millisecond)
	s := newSetup(t)
	s.router.RegisterPort("broken", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.Deny = func(string) (int, string) { return 503, routerBody("broken", "CRASHED") }
	var read atomic.Int32
	p := mustNew(t, portproxy.Options{
		ConnectAddr: s.routerAddr, DefaultAtespace: "ate-env", HandshakeTimeout: 5 * time.Second,
		ActorState: func(ctx context.Context, _, _ string) (ateapipb.ActorState, error) {
			read.Add(1)
			<-ctx.Done()
			return 0, ctx.Err()
		},
	})
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	for k, v := range target("broken", 8000) {
		req.Header.Set(k, v)
	}
	if resp, err := (&http.Client{Timeout: 200 * time.Millisecond}).Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("got %d, want the client's own timeout", resp.StatusCode)
	}
	time.Sleep(time.Second) // past the read deadline
	if read.Load() != 1 {
		t.Errorf("%d state reads, want 1", read.Load())
	}
	if strings.Contains(buf.String(), "reading the state") {
		t.Errorf("logged a state read for a caller that left:\n%s", buf.String())
	}
}

// A wake that outlasts Envoy's ext_proc timeout gets Envoy's empty 504 while
// the wake goes on. For an actor that is still waking the proxy answers 503
// with Retry-After and X-Env-State, so the client knows to retry.
func TestWakeTimeoutWhileWaking(t *testing.T) {
	for _, tc := range []struct {
		state ateapipb.ActorState
		name  string
	}{
		{ateapipb.ActorState_ACTOR_STATE_RESUMING, "RESUMING"},
		{ateapipb.ActorState_ACTOR_STATE_SUSPENDED, "SUSPENDED"},
	} {
		s := newSetup(t)
		s.router.RegisterPort("waking", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		s.router.Running = func(string) bool { return false }
		s.router.EnvoyTimeout = 200 * time.Millisecond
		srv, reads := stateProxy(t, s, func(int) (ateapipb.ActorState, error) { return tc.state, nil })
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
		for k, v := range target("waking", 8000) {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" ||
			resp.Header.Get(portproxy.EnvStateHeader) != tc.name || !strings.Contains(string(b), "still waking; retry") {
			t.Errorf("%s: got %d Retry-After=%q %s=%q %q, want 503, 1, %s and \"still waking; retry\"", tc.name, resp.StatusCode,
				resp.Header.Get("Retry-After"), portproxy.EnvStateHeader, resp.Header.Get(portproxy.EnvStateHeader), b, tc.name)
		}
		if n := reads(); n != 1 {
			t.Errorf("%s: %d state reads, want 1", tc.name, n)
		}
	}
}

// A wake that ends inside the router's hold is served, with no state read.
func TestWakeInsideTheHoldIsServed(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("waking", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "awake") }))
	start := time.Now()
	s.router.Running = func(string) bool { return time.Since(start) > 300*time.Millisecond }
	s.router.EnvoyTimeout = 2 * time.Second
	srv, reads := stateProxy(t, s, func(int) (ateapipb.ActorState, error) { return ateapipb.ActorState_ACTOR_STATE_RESUMING, nil })
	if code, body, _ := get(t, srv.URL, target("waking", 8000)); code != http.StatusOK || body != "awake" {
		t.Errorf("got %d %q, want the daemon's answer", code, body)
	}
	if n := reads(); n != 0 {
		t.Errorf("%d state reads, want 0", n)
	}
}

// The timeout path also reaches a crashed actor.
func TestWakeTimeoutForCrashedEnvironment(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("broken", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.Running = func(string) bool { return false }
	s.router.EnvoyTimeout = 200 * time.Millisecond
	srv, _ := stateProxy(t, s, func(int) (ateapipb.ActorState, error) { return ateapipb.ActorState_ACTOR_STATE_CRASHED, nil })
	code, header, _, _ := getWithHeader(t, srv.URL, target("broken", 8000), portproxy.EnvStateHeader)
	if code != http.StatusGone || header != "CRASHED" {
		t.Errorf("got %d %s=%q, want 410 CRASHED", code, portproxy.EnvStateHeader, header)
	}
}

// A running actor's daemon can answer 504 or 500 itself: that passes
// unchanged. An empty one costs a state read; one with a body does not.
func TestDaemonTimeoutsPassUnchanged(t *testing.T) {
	s := newSetup(t)
	var answer atomic.Value
	answer.Store([2]string{"504", ""})
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := answer.Load().([2]string)
		code, _ := strconv.Atoi(a[0])
		w.Header().Set("Content-Length", strconv.Itoa(len(a[1])))
		w.WriteHeader(code)
		io.WriteString(w, a[1])
	}))
	srv, reads := stateProxy(t, s, func(int) (ateapipb.ActorState, error) { return ateapipb.ActorState_ACTOR_STATE_RUNNING, nil })
	for _, a := range [][2]string{{"504", ""}, {"500", ""}, {"504", "upstream app timed out"}, {"500", "boom"}} {
		answer.Store(a)
		code, header, body, _ := getWithHeader(t, srv.URL, target("env1", 8000), portproxy.EnvStateHeader)
		if strconv.Itoa(code) != a[0] || body != a[1] || header != "" {
			t.Errorf("daemon %s %q: got %d %s=%q %q, want it unchanged", a[0], a[1], code, portproxy.EnvStateHeader, header, body)
		}
	}
	if n := reads(); n != 1 {
		t.Errorf("%d state reads, want 1 (the empty answers, cached)", n)
	}
}

// A HEAD request has no body to match; status, content type and the state
// decide, so a crashed environment gets 410 also for HEAD.
func TestHeadToCrashedEnvironment(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("broken", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.Deny = func(string) (int, string) { return 503, routerBody("broken", "CRASHED") }
	srv, _ := stateProxy(t, s, func(int) (ateapipb.ActorState, error) { return ateapipb.ActorState_ACTOR_STATE_CRASHED, nil })
	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/", nil)
	for k, v := range target("broken", 8000) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone || resp.Header.Get(portproxy.EnvStateHeader) != "CRASHED" {
		t.Errorf("HEAD: got %d %s=%q, want 410 CRASHED", resp.StatusCode, portproxy.EnvStateHeader, resp.Header.Get(portproxy.EnvStateHeader))
	}
}

// A daemon's text/plain 503 without a Content-Length is not read for the
// router's text: its headers reach the caller at once while it streams.
func TestStreamedDaemonErrorPassesWithoutDelay(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "a")
		w.(http.Flusher).Flush()
		time.Sleep(1500 * time.Millisecond)
		io.WriteString(w, "b")
	}))
	srv, reads := stateProxy(t, s, func(int) (ateapipb.ActorState, error) { return ateapipb.ActorState_ACTOR_STATE_RUNNING, nil })
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	for k, v := range target("env1", 8000) {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	headers := time.Since(start)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || string(b) != "ab" {
		t.Errorf("got %d %q, want the daemon's 503 \"ab\"", resp.StatusCode, b)
	}
	if headers > time.Second {
		t.Errorf("headers after %s, want at once while the daemon streams", headers)
	}
	if n := reads(); n != 0 {
		t.Errorf("%d state reads, want 0", n)
	}
}

// A state read that panics fails like any failed read: the answer passes
// unchanged, and the next answer for the environment does not wait on a read
// left in flight. The panic is logged with its stack, also right after an
// ordinary failed read was logged.
func TestPanickingStateReadDoesNotHang(t *testing.T) {
	var buf safeBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	s := newSetup(t)
	s.router.RegisterPort("broken", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.Deny = func(string) (int, string) { return 503, routerBody("broken", "CRASHED") }
	srv, _ := stateProxy(t, s, func(n int) (ateapipb.ActorState, error) {
		if n == 1 {
			return 0, status.Error(codes.PermissionDenied, "no access")
		}
		panic("control plane client bug")
	})
	client := &http.Client{Timeout: 3 * time.Second}
	for i := range 3 {
		if i == 1 {
			time.Sleep(2100 * time.Millisecond) // past the 2 s cache of the failed read
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
		for k, v := range target("broken", 8000) {
			req.Header.Set(k, v)
		}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || time.Since(start) > time.Second {
			t.Errorf("request %d: got %d after %s, want the router's 503 at once", i+1, resp.StatusCode, time.Since(start))
		}
	}
	logs := buf.String()
	if !strings.Contains(logs, "reading the state of environment ate-env/broken failed") {
		t.Errorf("the ordinary failed read is not logged:\n%s", logs)
	}
	if !strings.Contains(logs, "reading the state of environment ate-env/broken panicked: control plane client bug") ||
		!strings.Contains(logs, "goroutine ") {
		t.Errorf("the panic is not logged with its stack:\n%s", logs)
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestPoolsConnectionsPerTarget(t *testing.T) {
	s := newSetup(t)
	for _, id := range []string{"env1", "env2"} {
		id := id
		s.router.RegisterPort(id, 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, id) }))
	}
	for i := 0; i < 3; i++ {
		for _, id := range []string{"env1", "env2"} {
			if _, body := s.do(t, http.MethodGet, "/", "", target(id, 8000)); body != id {
				t.Fatalf("request for %s answered by %q: tunnels must not be shared across environments", id, body)
			}
		}
	}
	if n := s.router.connectCount(); n != 2 {
		t.Errorf("router saw %d CONNECTs for 6 sequential requests to 2 targets, want 2 (idle tunnels reused)", n)
	}
}

func TestDispatch(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "proxied") }))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "api") })
	h := portproxy.Dispatch(mustNew(t, portproxy.Options{ConnectAddr: s.routerAddr, DefaultAtespace: "ate-env"}), next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Body.String() != "api" {
		t.Errorf("request without %s went to %q, want the API", portproxy.EnvPortHeader, rec.Body.String())
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	for k, v := range target("env1", 8000) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "proxied" {
		t.Errorf("request with %s got %q, want the daemon's answer", portproxy.EnvPortHeader, b)
	}

	// A gRPC call is never proxied, even with the header: the tunnel carries
	// HTTP/1.1 only.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/ateenv.v1alpha.EnvironmentService/Get", nil)
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set(portproxy.EnvPortHeader, "8000")
	h.ServeHTTP(rec, req)
	if rec.Body.String() != "api" {
		t.Errorf("gRPC request with %s went to %q, want the API", portproxy.EnvPortHeader, rec.Body.String())
	}
}

// pathSetup serves the proxy the way ate-env-api does: both forms dispatched
// in front of the API's mux.
func pathSetup(t *testing.T, origins ...string) (*setup, string) {
	t.Helper()
	s := newSetup(t)
	p := mustNew(t, portproxy.Options{ConnectAddr: s.routerAddr, DefaultAtespace: "ate-env", HandshakeTimeout: 5 * time.Second, TrustedOrigins: origins})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "api") })
	srv := httptest.NewServer(portproxy.Dispatch(p, mux))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func TestPathFormStripsPrefix(t *testing.T) {
	s, base := pathSetup(t)
	var seen *http.Request
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		io.WriteString(w, "daemon")
	}))
	resp, err := http.Get(base + "/v1alpha/envs/env1/ports/8000/api/file/download?path=/etc/os-release")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "daemon" {
		t.Fatalf("got %d %q", resp.StatusCode, b)
	}
	if got := seen.URL.RequestURI(); got != "/api/file/download?path=/etc/os-release" {
		t.Errorf("daemon saw %q, want the path after the routing prefix with its query", got)
	}
	if got := s.router.connects; len(got) != 1 || !strings.HasPrefix(got[0], "ate-env/env1 ") {
		t.Errorf("router saw %q", got)
	}

	// Escaping after the prefix is kept as sent.
	resp, err = http.Get(base + "/v1alpha/envs/env1/ports/8000/files/a%2Fb")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := seen.URL.EscapedPath(); got != "/files/a%2Fb" {
		t.Errorf("daemon saw escaped path %q, want /files/a%%2Fb", got)
	}

	// The bare prefix with a trailing slash is the daemon's root.
	resp, err = http.Get(base + "/v1alpha/envs/env1/ports/8000/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen.URL.Path != "/" {
		t.Errorf("daemon saw %q for the bare prefix, want /", seen.URL.Path)
	}
}

func TestPathFormAtespaceAndValidation(t *testing.T) {
	s, base := pathSetup(t)
	s.router.RegisterPort("env1", 9000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.URL.Path) }))
	resp, err := http.Get(base + "/v1alpha/atespaces/team-a/envs/env1/ports/9000/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "/x" {
		t.Errorf("got %d %q, want the daemon's /x", resp.StatusCode, b)
	}
	if got := s.router.connects; len(got) != 1 || !strings.HasPrefix(got[0], "team-a/env1 ") {
		t.Errorf("router saw %q, want team-a/env1", got)
	}
	for _, path := range []string{
		"/v1alpha/envs/Env1/ports/9000/x",
		"/v1alpha/envs/env1/ports/0/x",
		"/v1alpha/envs/env1/ports/http/x",
		"/v1alpha/envs/env1/ports/09000/x",
		"/v1alpha/envs//ports/9000/x",
		"/v1alpha/envs/a%2Fb/ports/9000/x",
		"/v1alpha/atespaces/A_B/envs/env1/ports/9000/x",
	} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", path, resp.StatusCode)
		}
	}
	// Other paths still reach the API.
	resp, err = http.Get(base + "/v1alpha/envs/env1/mcp")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "api" {
		t.Errorf("/v1alpha/envs/env1/mcp went to %q, want the API", b)
	}
}

// An HTTP upgrade (what a WebSocket handshake is) passes through the proxy and
// the tunnel, after which bytes flow both ways.
func TestUpgradePassesThrough(t *testing.T) {
	s, base := pathSetup(t)
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "echo" {
			http.Error(w, "want Upgrade: echo", http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		rw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(rw, buf); err == nil {
			rw.Write(append([]byte("re:"), buf...))
			rw.Flush()
		}
	}))
	addr := strings.TrimPrefix(base, "http://")
	for name, req := range map[string]string{
		"path form":   "GET /v1alpha/envs/env1/ports/8000/sockets/events/c1 HTTP/1.1\r\nHost: " + addr + "\r\n",
		"header form": "GET /sockets/events/c1 HTTP/1.1\r\nHost: " + addr + "\r\nX-Env-Id: env1\r\nX-Env-Port: 8000\r\n",
	} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprint(conn, req+"Connection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("%s: upgrade answered %v, %v", name, resp, err)
		}
		conn.Write([]byte("ping"))
		got := make([]byte, 7)
		if _, err := io.ReadFull(br, got); err != nil || string(got) != "re:ping" {
			t.Errorf("%s: after upgrade got %q, %v; want re:ping", name, got, err)
		}
		conn.Close()
	}
}

// The path form reaches the daemon exactly as sent: an escaped routing segment
// still ends the prefix, and nothing is redirected or cleaned.
func TestPathFormKeepsPathAsSent(t *testing.T) {
	s, base := pathSetup(t)
	var seen *http.Request
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		io.WriteString(w, "daemon")
	}))
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, c := range []struct{ method, path, want string }{
		{http.MethodGet, "/v1alpha/envs/%65nv1/ports/8000/a", "/a"},
		{http.MethodGet, "/v1alpha/envs/env1/ports/%38000/a", "/a"},
		{http.MethodGet, "/v1alpha/envs/env1/ports/8000", "/"},
		{http.MethodPost, "/v1alpha/envs/env1/ports/8000", "/"},
		{http.MethodPost, "/v1alpha/atespaces/ate-env/envs/env1/ports/8000", "/"},
		{http.MethodGet, "/v1alpha/envs/env1/ports/8000/a//b/../c?x=1", "/a//b/../c?x=1"},
	} {
		seen = nil
		req, _ := http.NewRequest(c.method, base+c.path, nil)
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(b) != "daemon" || seen == nil {
			t.Errorf("%s %s: got %d %q, want the daemon's 200", c.method, c.path, resp.StatusCode, b)
			continue
		}
		if got := seen.URL.RequestURI(); got != c.want {
			t.Errorf("%s %s: daemon saw %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// A browser page from another origin cannot drive a daemon through the path
// form: no state-changing request, no WebSocket handshake. Reads, non-browser
// clients, the listener's own origin and trusted origins pass.
func TestPathFormRefusesCrossOrigin(t *testing.T) {
	s, base := pathSetup(t, "https://ui.example")
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Upgrade", "echo")
			w.WriteHeader(http.StatusSwitchingProtocols)
			return
		}
		io.WriteString(w, "daemon")
	}))
	self := base // the listener's own origin, as a browser on it sends
	path := base + "/v1alpha/envs/env1/ports/8000/run"
	for _, c := range []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"cross-site POST", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"foreign Origin POST", http.MethodPost, map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"foreign Origin upgrade", http.MethodGet, map[string]string{"Origin": "https://evil.example", "Connection": "Upgrade", "Upgrade": "echo"}, http.StatusForbidden},
		{"cross-site upgrade", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site", "Connection": "Upgrade", "Upgrade": "echo"}, http.StatusForbidden},
		{"cross-site GET", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusOK},
		{"no browser headers", http.MethodPost, nil, http.StatusOK},
		{"own origin POST", http.MethodPost, map[string]string{"Origin": self}, http.StatusOK},
		{"same-origin upgrade", http.MethodGet, map[string]string{"Sec-Fetch-Site": "same-origin", "Connection": "Upgrade", "Upgrade": "echo"}, http.StatusSwitchingProtocols},
		{"trusted origin POST", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://ui.example"}, http.StatusOK},
	} {
		req, _ := http.NewRequest(c.method, path, nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: got %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	if _, err := portproxy.New(portproxy.Options{ConnectAddr: s.routerAddr, TrustedOrigins: []string{"not an origin"}}); err == nil {
		t.Error("New accepted a malformed trusted origin")
	}
}

// An error body names the target and a short cause. The router's message and
// address stay in the log.
func TestErrorBodiesNameCauseOnly(t *testing.T) {
	refuser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "dial tcp 10.1.2.3:8000: connect: connection refused", http.StatusBadGateway)
	}))
	defer refuser.Close()
	p := mustNew(t, portproxy.Options{ConnectAddr: strings.TrimPrefix(refuser.URL, "http://"), DefaultAtespace: "ate-env"})
	srv := httptest.NewServer(p)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	for k, v := range target("env1", 8000) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "ate-env/env1 port 8000") || !strings.Contains(body, "0.0.0.0") {
		t.Errorf("got %d %q, want 502 naming the target and the listen rule", resp.StatusCode, body)
	}
	if strings.Contains(body, "10.1.2.3") || strings.Contains(body, refuser.URL[len("http://"):]) {
		t.Errorf("body %q leaks an internal address", body)
	}
}

func TestResponseHeaderTimeout(t *testing.T) {
	rr := &recordingRouter{Router: fakerouter.New()}
	router := httptest.NewServer(rr)
	defer router.Close()
	release := make(chan struct{})
	defer close(release)
	rr.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	p := mustNew(t, portproxy.Options{
		ConnectAddr:           strings.TrimPrefix(router.URL, "http://"),
		DefaultAtespace:       "ate-env",
		ResponseHeaderTimeout: 200 * time.Millisecond,
	})
	srv := httptest.NewServer(p)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	for k, v := range target("env1", 8000) {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 10 * time.Second} // fail, not hang, without the limit
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("silent daemon: got %d, want 504", resp.StatusCode)
	}
}

// TestAuthenticate serves the proxy the way ate-env-api does with
// -auth-token-file: both forms require the API's bearer token, the token does
// not reach the daemon, and the API's own routes are left to their handlers.
func TestAuthenticate(t *testing.T) {
	s := newSetup(t)
	var seen *http.Request
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		io.WriteString(w, "daemon")
	}))
	auth, err := apiservice.NewBearerAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	p := mustNew(t, portproxy.Options{ConnectAddr: s.routerAddr, DefaultAtespace: "ate-env", HandshakeTimeout: 5 * time.Second, Authenticate: auth.Check})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "api") })
	srv := httptest.NewServer(portproxy.Dispatch(p, mux))
	t.Cleanup(srv.Close)

	get := func(path string, headers map[string]string) (*http.Response, string) {
		t.Helper()
		seen = nil
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	with := func(h map[string]string, kv ...string) map[string]string {
		out := map[string]string{}
		for k, v := range h {
			out[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	header := target("env1", 8000)
	const pathForm = "/v1alpha/envs/env1/ports/8000/health"

	for _, tc := range []struct {
		name    string
		path    string
		headers map[string]string
	}{
		{"header form without a token", "/health", header},
		{"header form with a wrong token", "/health", with(header, "Authorization", "Bearer wrong")},
		{"header form with a wrong token in " + portproxy.EnvAuthorizationHeader, "/health", with(header, portproxy.EnvAuthorizationHeader, "Bearer wrong", "Authorization", "Bearer s3cret")},
		{"path form without a token", pathForm, nil},
		{"path form with a wrong token", pathForm, map[string]string{"Authorization": "Bearer wrong"}},
	} {
		resp, body := get(tc.path, tc.headers)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: got %d %q, want 401 with WWW-Authenticate: Bearer", tc.name, resp.StatusCode, body)
		}
		if seen != nil {
			t.Errorf("%s: the request reached the daemon", tc.name)
		}
	}
	if n := s.router.connectCount(); n != 0 {
		t.Errorf("router saw %d CONNECTs for refused requests, want 0", n)
	}

	// The token in Authorization is the API's: the daemon does not get it.
	for _, path := range []string{"/health", pathForm} {
		h := map[string]string{"Authorization": "Bearer s3cret"}
		if path == "/health" {
			h = with(header, "Authorization", "Bearer s3cret")
		}
		resp, body := get(path, h)
		if resp.StatusCode != http.StatusOK || body != "daemon" {
			t.Fatalf("%s with the token in Authorization: got %d %q, want the daemon's answer", path, resp.StatusCode, body)
		}
		if v := seen.Header.Get("Authorization"); v != "" {
			t.Errorf("%s: daemon saw Authorization %q; the API's token must not reach it", path, v)
		}
	}

	// The token in X-Env-Authorization leaves Authorization to the daemon.
	resp, body := get("/health", with(header, portproxy.EnvAuthorizationHeader, "Bearer s3cret", "Authorization", "Bearer daemon-key"))
	if resp.StatusCode != http.StatusOK || body != "daemon" {
		t.Fatalf("token in %s: got %d %q, want the daemon's answer", portproxy.EnvAuthorizationHeader, resp.StatusCode, body)
	}
	if v := seen.Header.Get("Authorization"); v != "Bearer daemon-key" {
		t.Errorf("daemon saw Authorization %q, want the caller's daemon credential", v)
	}
	if v := seen.Header.Get(portproxy.EnvAuthorizationHeader); v != "" {
		t.Errorf("daemon saw %s %q; the proxy must strip it", portproxy.EnvAuthorizationHeader, v)
	}

	// The API's own routes are not the proxy's to check.
	if resp, body := get("/healthz", nil); resp.StatusCode != http.StatusOK || body != "api" {
		t.Errorf("/healthz: got %d %q, want the API's answer", resp.StatusCode, body)
	}
}

// Without Authenticate, the proxy still strips X-Env-Authorization and leaves
// Authorization to the daemon.
func TestStripsEnvAuthorizationWithoutAuthenticate(t *testing.T) {
	s := newSetup(t)
	var seen *http.Request
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		io.WriteString(w, "daemon")
	}))
	h := target("env1", 8000)
	h[portproxy.EnvAuthorizationHeader] = "Bearer api-token"
	h["Authorization"] = "Bearer daemon-key"
	if resp, body := s.do(t, http.MethodGet, "/", "", h); resp.StatusCode != http.StatusOK || body != "daemon" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if v := seen.Header.Get(portproxy.EnvAuthorizationHeader); v != "" {
		t.Errorf("daemon saw %s %q; the proxy must strip it", portproxy.EnvAuthorizationHeader, v)
	}
	if v := seen.Header.Get("Authorization"); v != "Bearer daemon-key" {
		t.Errorf("daemon saw Authorization %q, want it untouched", v)
	}
}

// The router's CONNECT listener is TLS-only since Substrate v0.6.0: the proxy
// presents the pod identity certificate and verifies the router.
func TestConnectOverTLS(t *testing.T) {
	ca := testcerts.NewAuthority(t, "test podcert")
	rr := fakerouter.New()
	addr, stop := rr.ServeTLS(&tls.Config{
		Certificates: []tls.Certificate{ca.Server(t, "atenet-router.ate-system.svc")},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.Pool(),
	})
	t.Cleanup(stop)
	rr.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello over tls")
	}))
	dir := t.TempDir()
	cfg, err := routertls.Options{
		CredentialBundleFile: testcerts.WriteFile(t, dir, "bundle.pem", ca.ClientBundle(t, routertls.SPIFFEID("ate-env", "ate-env-api"))),
		TrustBundleFile:      testcerts.WriteFile(t, dir, "trust.pem", ca.PEM),
		ServerName:           "atenet-router.ate-system.svc",
	}.Config()
	if err != nil {
		t.Fatal(err)
	}
	p := mustNew(t, portproxy.Options{ConnectAddr: addr, TLS: cfg, DefaultAtespace: "ate-env", HandshakeTimeout: 5 * time.Second})
	proxy := httptest.NewServer(portproxy.Dispatch(p, http.NotFoundHandler()))
	t.Cleanup(proxy.Close)

	resp, err := http.Get(proxy.URL + "/v1alpha/envs/env1/ports/8000/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "hello over tls" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}

	// Without TLS the proxy sends a plaintext CONNECT to the TLS listener,
	// which Go's TLS server answers with HTTP 400; the proxy maps that to
	// 502, not a hang.
	plain := mustNew(t, portproxy.Options{ConnectAddr: addr, DefaultAtespace: "ate-env", HandshakeTimeout: 5 * time.Second})
	proxy2 := httptest.NewServer(portproxy.Dispatch(plain, http.NotFoundHandler()))
	t.Cleanup(proxy2.Close)
	resp, err = http.Get(proxy2.URL + "/v1alpha/envs/env1/ports/8000/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 500 {
		t.Fatalf("plaintext to a TLS router: got %d, want a 5xx", resp.StatusCode)
	}
}

// A router whose allow-list lacks the API's identity turns the certificate
// away after the handshake; the proxy answers 502 and names the setting.
func TestConnectRejectedCertificate(t *testing.T) {
	ca := testcerts.NewAuthority(t, "test podcert")
	rr := fakerouter.New()
	addr, stop := rr.ServeTLS(&tls.Config{
		Certificates:          []tls.Certificate{ca.Server(t, "atenet-router.ate-system.svc")},
		ClientAuth:            tls.RequireAndVerifyClientCert,
		ClientCAs:             ca.Pool(),
		VerifyPeerCertificate: testcerts.Allow(routertls.SPIFFEID("ate-env", "someone-else")),
	})
	t.Cleanup(stop)
	rr.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "unreachable")
	}))
	dir := t.TempDir()
	cfg, err := routertls.Options{
		CredentialBundleFile: testcerts.WriteFile(t, dir, "bundle.pem", ca.ClientBundle(t, routertls.SPIFFEID("ate-env", "ate-env-api"))),
		TrustBundleFile:      testcerts.WriteFile(t, dir, "trust.pem", ca.PEM),
		ServerName:           "atenet-router.ate-system.svc",
	}.Config()
	if err != nil {
		t.Fatal(err)
	}
	p := mustNew(t, portproxy.Options{ConnectAddr: addr, TLS: cfg, DefaultAtespace: "ate-env", HandshakeTimeout: 5 * time.Second, WakeBudget: 30 * time.Second})
	proxy := httptest.NewServer(portproxy.Dispatch(p, http.NotFoundHandler()))
	t.Cleanup(proxy.Close)

	start := time.Now()
	resp, err := http.Get(proxy.URL + "/v1alpha/envs/env1/ports/8000/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "ATE_INGRESS_ALLOWED_SPIFFE_IDS") {
		t.Fatalf("got %d %q, want 502 naming the allow-list setting", resp.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s; the wake budget must not apply to a rejected certificate", elapsed)
	}
}

// OnConnected fires once per accepted CONNECT, after the wake, with the
// environment's atespace and id; refused CONNECTs do not fire it.
func TestOnConnectedFiresAfterTheWake(t *testing.T) {
	rr := &recordingRouter{Router: fakerouter.New()}
	srv := httptest.NewServer(rr)
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	var got []string
	p := mustNew(t, portproxy.Options{
		ConnectAddr:      strings.TrimPrefix(srv.URL, "http://"),
		DefaultAtespace:  "ate-env",
		HandshakeTimeout: 5 * time.Second,
		WakeBudget:       2 * time.Second,
		OnConnected: func(atespace, id string) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, atespace+"/"+id)
		},
	})
	proxy := httptest.NewServer(p)
	t.Cleanup(proxy.Close)
	s := &setup{router: rr, proxy: proxy}

	s.router.RegisterPort("waking", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "awake") }))
	refusals := 0
	s.router.RefuseConnect = func(string) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if refusals < 2 {
			refusals++
			return http.StatusServiceUnavailable, "upstream overflow"
		}
		return 0, ""
	}
	if resp, _ := s.do(t, http.MethodGet, "/", "", target("waking", 8000)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 after the wake", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"ate-env/waking"}; !slices.Equal(got, want) {
		t.Errorf("OnConnected calls = %v, want %v (once, after two refused CONNECTs)", got, want)
	}
}
