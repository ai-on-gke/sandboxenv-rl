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

// Package hostcheck refuses HTTP/1.x requests whose Host header names a host
// the listener does not serve.
//
// This blocks DNS rebinding. A web page on a name the attacker controls can
// make that name resolve to the listener, for example to a kubectl
// port-forward on 127.0.0.1. The browser then treats the listener as the
// page's own origin: it sends the attacker's name as the Host and lets the
// page read the answers. Refusing names the listener does not serve stops
// that, for the MCP endpoint and the port proxy alike.
//
// Only HTTP/1.x is checked. Browsers speak HTTP/1.1 to http:// origins and
// cannot open the prior-knowledge cleartext HTTP/2 (h2c) that gRPC clients
// use, so h2c requests pass whatever their :authority is.
//
// IP addresses are always allowed: a browser sends an IP address as the Host
// only when it connected to that address, so no other name is involved.
// localhost and names under .localhost are allowed because they resolve to
// the loopback interface only.
package hostcheck

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

// maxLogged bounds how many distinct refused hosts are logged, so a client
// that sends many names cannot flood the log.
const maxLogged = 64

// Checker decides whether a Host header names a host the listener serves.
type Checker struct {
	exact    map[string]struct{}
	suffixes []string // ".example.com" for the entry "*.example.com"

	mu        sync.Mutex
	logged    map[string]struct{}
	loggedMax bool
}

// New returns a Checker that allows IP addresses, localhost, names under
// .localhost and the given host names. A name that starts with "*." allows
// any name below it, not the name itself. Names carry no port or scheme.
func New(hosts []string) (*Checker, error) {
	c := &Checker{exact: map[string]struct{}{}, logged: map[string]struct{}{}}
	for _, raw := range hosts {
		h := normalize(raw)
		if h == "" {
			continue
		}
		if strings.ContainsAny(h, ":/@ ") {
			return nil, fmt.Errorf("%q: give a host name without scheme, port or path", raw)
		}
		if rest, ok := strings.CutPrefix(h, "*."); ok {
			if rest == "" || strings.Contains(rest, "*") {
				return nil, fmt.Errorf("%q: a wildcard must be a leading *. before a name", raw)
			}
			c.suffixes = append(c.suffixes, "."+rest)
			continue
		}
		if strings.Contains(h, "*") {
			return nil, fmt.Errorf("%q: a wildcard must be a leading *. before a name", raw)
		}
		c.exact[h] = struct{}{}
	}
	return c, nil
}

// ParseList splits a comma-separated flag value into host names.
func ParseList(s string) []string {
	var out []string
	for h := range strings.SplitSeq(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func normalize(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// Allowed reports whether hostport, the value of a Host header, names a host
// the listener serves. An empty Host (HTTP/1.0) is allowed: browsers always
// send one.
func (c *Checker) Allowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	host = normalize(host)
	if host == "" {
		return true
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if _, ok := c.exact[host]; ok {
		return true
	}
	for _, s := range c.suffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}

// Wrap refuses an HTTP/1.x request whose Host is not allowed with 403 and
// passes all other requests to next.
func (c *Checker) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor >= 2 || c.Allowed(r.Host) {
			next.ServeHTTP(w, r)
			return
		}
		c.logRefused(r.Host)
		http.Error(w, fmt.Sprintf("host %q is not served here: ate-env-api answers HTTP/1.x requests only for IP addresses, localhost and the names in --allowed-hosts", r.Host), http.StatusForbidden)
	})
}

func (c *Checker) logRefused(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.logged[host]; ok || c.loggedMax {
		return
	}
	if len(c.logged) >= maxLogged {
		c.loggedMax = true
		log.Printf("hostcheck: refused %d distinct hosts; not logging more", maxLogged)
		return
	}
	c.logged[host] = struct{}{}
	log.Printf("hostcheck: refused an HTTP/1.x request for host %q; add it to --allowed-hosts if ate-env-api serves it", host)
}
