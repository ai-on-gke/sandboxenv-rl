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
	"io"
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

// Rule (Dispatch doc, portproxy.go:276-278; TestPathFormKeepsPathAsSent):
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
