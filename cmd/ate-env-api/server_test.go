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
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// advertisedMaxStreams opens a prior-knowledge h2c connection to a server
// built by newHTTPServer and returns the SETTINGS_MAX_CONCURRENT_STREAMS it
// advertises, which is the per-connection limit gRPC clients obey (0 when the
// server sends none).
func advertisedMaxStreams(t *testing.T, maxStreams int) uint32 {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPServer(lis.Addr().String(), http.NotFoundHandler(), maxStreams)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.DialTimeout("tcp", lis.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("reading the server's SETTINGS: %v", err)
		}
		sf, ok := f.(*http2.SettingsFrame)
		if !ok || sf.IsAck() {
			continue
		}
		v, _ := sf.Value(http2.SettingMaxConcurrentStreams)
		return v
	}
}

func TestNewHTTPServerAdvertisesConfiguredStreamLimit(t *testing.T) {
	if got := advertisedMaxStreams(t, 3); got != 3 {
		t.Errorf("max-concurrent-streams 3: server advertises %d", got)
	}
}

func TestNewHTTPServerDefaultIsAboveGoDefault(t *testing.T) {
	got := advertisedMaxStreams(t, DefaultMaxConcurrentStreams)
	if got != DefaultMaxConcurrentStreams || got <= 250 {
		t.Errorf("default: server advertises %d, want %d (above Go's 250)", got, DefaultMaxConcurrentStreams)
	}
}

func TestNewHTTPServerZeroKeepsGoDefault(t *testing.T) {
	if got := advertisedMaxStreams(t, 0); got != 250 {
		t.Errorf("max-concurrent-streams 0: server advertises %d, want Go's default 250", got)
	}
}
