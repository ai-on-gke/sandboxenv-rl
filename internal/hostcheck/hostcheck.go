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

// Package hostcheck refuses requests whose Host header names a host the
// listener does not serve.
//
// This blocks DNS rebinding. A web page on a name the attacker controls can
// make that name resolve to the listener, for example to a kubectl
// port-forward on 127.0.0.1. The browser then treats the listener as the
// page's own origin: it sends the attacker's name as the Host and lets the
// page read the answers. Refusing names the listener does not serve stops
// that, for the MCP endpoint and the port proxy alike.
//
// Wrap wraps the HTTP routes (/healthz, MCP and the port proxy), while the
// gRPC route on the mux stays unwrapped: gRPC clients send many different
// authorities, the dial target among them. Every wrapped route checks r.Host
// on both HTTP/1.x and HTTP/2 (where Go fills r.Host from :authority),
// whatever Content-Type the request carries.
//
// The check is a browser defense, not an authentication layer. Browsers
// always send a Host header, which the check reads; a client that sets its
// own Host, such as curl -H 'Host: x', passes by design. The bearer token of
// ate-env-api is the control for callers that are not browsers, gRPC included.
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

// Wrap refuses a request whose Host is not allowed with 403 and passes all
// other requests to next.
func (c *Checker) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.Allowed(r.Host) {
			next.ServeHTTP(w, r)
			return
		}
		c.logRefused(r.Host)
		http.Error(w, fmt.Sprintf("host %q is not served here: ate-env-api answers HTTP requests only for IP addresses, localhost, its Service names and the names in --allowed-hosts", r.Host), http.StatusForbidden)
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
	log.Printf("hostcheck: refused a request for host %q; add it to --allowed-hosts if ate-env-api serves it", host)
}

// ServiceHosts returns the in-cluster names of the Kubernetes Service name in
// namespace: name, name.namespace and name.namespace.svc, then
// name.namespace.svc.clusterDomain when the cluster domain is known. Without
// a namespace it returns name alone; without a name, nothing.
func ServiceHosts(name, namespace, clusterDomain string) []string {
	if name == "" {
		return nil
	}
	if namespace == "" {
		return []string{name}
	}
	svc := name + "." + namespace + ".svc"
	hosts := []string{name, name + "." + namespace, svc}
	if clusterDomain != "" {
		hosts = append(hosts, svc+"."+clusterDomain)
	}
	return hosts
}

// ClusterDNS reads the pod's namespace and the cluster domain from the
// search list of a resolv.conf. For a pod with the ClusterFirst DNS policy,
// kubelet writes "<namespace>.svc.<domain>" first, whatever the cluster
// domain is. It returns empty strings when no search entry has that form.
func ClusterDNS(resolvConf string) (namespace, clusterDomain string) {
	for line := range strings.SplitSeq(resolvConf, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "search" {
			continue
		}
		for _, d := range fields[1:] {
			ns, domain, ok := strings.Cut(normalize(d), ".svc.")
			if ok && ns != "" && !strings.Contains(ns, ".") && domain != "" {
				return ns, domain
			}
		}
	}
	return "", ""
}
