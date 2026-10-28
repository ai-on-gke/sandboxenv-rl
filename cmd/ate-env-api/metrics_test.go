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
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// A bind failure is returned to main, which exits after its deferred
// functions, instead of ending the process from the serving goroutine.
func TestServeMetricsReturnsBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv, errc, err := serveMetrics(ln.Addr().String(), http.NotFoundHandler())
	if err == nil {
		srv.Close()
		t.Fatalf("serveMetrics on a taken address: no error")
	}
	if srv != nil || errc != nil {
		t.Errorf("serveMetrics returned a server or channel with the error")
	}
	if !strings.Contains(err.Error(), "metrics listener") {
		t.Errorf("error %q does not name the metrics listener", err)
	}
}

// The server serves GET /metrics only, bounds header reads, and closes its
// error channel when it stops cleanly.
func TestServeMetricsServesAndStops(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "families\n") })
	srv, errc, err := serveMetrics("127.0.0.1:0", h)
	if err != nil {
		t.Fatal(err)
	}
	if srv.ReadHeaderTimeout != metricsReadHeaderTimeout || metricsReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %s, want %s", srv.ReadHeaderTimeout, metricsReadHeaderTimeout)
	}
	get := func(path string) (int, string) {
		resp, err := http.Get("http://" + srv.Addr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/metrics"); code != http.StatusOK || body != "families\n" {
		t.Errorf("GET /metrics = %d %q", code, body)
	}
	if code, _ := get("/"); code != http.StatusNotFound {
		t.Errorf("GET / = %d, want 404", code)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err, ok := <-errc; ok {
		t.Errorf("error channel after a clean stop: %v, want closed", err)
	}
}
