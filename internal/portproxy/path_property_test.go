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
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// pathRestGen draws an escaped path suffix ("/seg/seg...") made of
// characters an HTTP client may send in a path: unreserved, sub-delims,
// ':' '@', and percent-escapes.
func pathRestGen() *rapid.Generator[string] {
	atom := rapid.OneOf(
		rapid.SampledFrom([]string{"a", "b", "z", "0", "-", "_", ".", "~"}),
		rapid.SampledFrom([]string{"!", "$", "&", "'", "(", ")", "*", "+", ",", ";", "=", ":", "@"}),
		rapid.SampledFrom([]string{"%20", "%2F", "%41", "%21", "%25", "%3F"}),
	)
	seg := rapid.Custom(func(t *rapid.T) string {
		return strings.Join(rapid.SliceOfN(atom, 0, 6).Draw(t, "atoms"), "")
	})
	return rapid.Custom(func(t *rapid.T) string {
		return "/" + strings.Join(rapid.SliceOfN(seg, 1, 4).Draw(t, "segs"), "/")
	})
}

// PP-1 (docs/plans/port-proxy/acceptance-criteria.md). Rule (Dispatch doc;
// TestPathFormKeepsPathAsSent):
// the daemon gets the path after the routing prefix exactly as sent. As a
// differential: the path form delivers the same request-target as the
// header form, which forwards r.URL untouched.
func TestPathFormDeliversPathAsSentProperty(t *testing.T) {
	s, base := pathSetup(t)
	var got string
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.RequestURI
		io.WriteString(w, "daemon")
	}))
	send := func(t *rapid.T, url string, hdr map[string]string) string {
		got = ""
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "daemon" {
			t.Fatalf("GET %s: %d %q", url, resp.StatusCode, b)
		}
		return got
	}
	rapid.Check(t, func(rt *rapid.T) {
		rest := pathRestGen().Draw(rt, "rest")
		viaPath := send(rt, base+"/v1alpha/envs/env1/ports/8000"+rest, nil)
		viaHeader := send(rt, base+rest, map[string]string{"X-Env-Id": "env1", "X-Env-Port": "8000"})
		if viaPath != rest || viaHeader != rest {
			rt.Fatalf("sent %q: path form delivered %q, header form %q", rest, viaPath, viaHeader)
		}
	})
}

// Deterministic reduction of the counterexample.
func TestPathFormKeepsSubDelimsAsSent(t *testing.T) {
	s, base := pathSetup(t)
	var got string
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.RequestURI
	}))
	for _, rest := range []string{"/!", "/files/report(1).txt", "/a*b", "/it's"} {
		resp, err := http.Get(base + "/v1alpha/envs/env1/ports/8000" + rest)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got != rest {
			t.Errorf("sent %q after the prefix, daemon got %q", rest, got)
		}
	}
}

// sendRaw writes a GET for target on a fresh TCP connection, so no client
// escapes or cleans the request-target, and returns the status and body.
func sendRaw(t testing.TB, base, target, extra string) (int, string) {
	t.Helper()
	host := strings.TrimPrefix(base, "http://")
	c, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET "+target+" HTTP/1.1\r\nHost: "+host+"\r\n"+extra+"Connection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

const headerForm = "X-Env-Id: env1\r\nX-Env-Port: 8000\r\n"

// PP-1: bytes Go's server accepts in a request-target but url.EscapedPath
// escapes ({ } | ^, raw UTF-8) reach the daemon as sent in both forms, and an
// escaped slash next to them stays escaped. A path that starts with "//"
// cannot be written as is (it would read as an authority): only those bytes
// are escaped, and the segments stay.
func TestRawBytesKeptAsSent(t *testing.T) {
	s, base := pathSetup(t)
	var got, host string
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, host = r.RequestURI, r.Host
		io.WriteString(w, "daemon")
	}))
	for _, c := range []struct{ rest, want string }{
		{"/a{b}", "/a{b}"},
		{"/a|b", "/a|b"},
		{"/a^b", "/a^b"},
		{"/caf\xc3\xa9", "/caf\xc3\xa9"},
		{"/a{b%2Fc", "/a{b%2Fc"},
		{"/a\"b<c>d\\e`f", "/a\"b<c>d\\e`f"},
		{"/a{b?x={y}", "/a{b?x={y}"},
		{"//a{b%2Fc", "//a%7Bb%2Fc"},
		{"//evil.example/x", "//evil.example/x"},
	} {
		for _, form := range []struct{ name, target, extra string }{
			{"path form", "/v1alpha/envs/env1/ports/8000" + c.rest, ""},
			{"header form", c.rest, headerForm},
		} {
			got, host = "", ""
			code, body := sendRaw(t, base, form.target, form.extra)
			if code != http.StatusOK || body != "daemon" || got != c.want || host != "127.0.0.1:8000" {
				t.Errorf("%s: sent %q, got %d %q, daemon got %q (Host %q), want %q", form.name, c.rest, code, body, got, host, c.want)
			}
		}
	}
}

// The routing prefix is matched on the path as sent: an escaped slash or a
// stray byte inside it does not make a path form, and does not reach the
// daemon.
func TestRawBytesDoNotMoveRoutingPrefix(t *testing.T) {
	s, base := pathSetup(t)
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "daemon")
	}))
	for _, c := range []struct {
		target string
		code   int
		body   string
	}{
		{"/v1alpha%2Fenvs%2Fenv1%2Fports%2F8000/a{", http.StatusOK, "api"},
		{"/v1alpha/envs%2Fenv1/ports/8000/a{", http.StatusOK, "api"},
		{"/v1alpha/envs/env1%2Fports%2F8000/ports/1/a{", http.StatusBadRequest, ""},
		{"/v1alpha/envs/env1{/ports/8000/a", http.StatusBadRequest, ""},
		{"/v1alpha/envs/env1/ports/8000{/a", http.StatusBadRequest, ""},
		{"/v1alpha/envs/env1/ports/8000%2F{", http.StatusBadRequest, ""},
		{"/v1alpha/envs/caf\xc3\xa9/ports/8000/a", http.StatusBadRequest, ""},
	} {
		code, body := sendRaw(t, base, c.target, "")
		if code != c.code || (c.body != "" && body != c.body) || body == "daemon" {
			t.Errorf("GET %q: got %d %q, want %d %q", c.target, code, body, c.code, c.body)
		}
	}
}
