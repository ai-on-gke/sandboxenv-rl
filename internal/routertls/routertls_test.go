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

package routertls_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/internaltest/testcerts"
	"github.com/agent-substrate/env/internal/routertls"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const routerName = "atenet-router.ate-system.svc"

// echoID answers with the URI SAN of the client certificate, so a test sees
// which identity the client presented.
func echoID(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.PeerCertificates[0].URIs) == 0 {
		http.Error(w, "no client identity", http.StatusForbidden)
		return
	}
	io.WriteString(w, r.TLS.PeerCertificates[0].URIs[0].String())
}

func newRouter(t *testing.T, ca *testcerts.Authority) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(echoID))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.Server(t, routerName)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.Pool(),
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, cfg *tls.Config, url string) (int, string, error) {
	t.Helper()
	tr := &http.Transport{TLSClientConfig: cfg}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func TestConfigPresentsTheBundleAndVerifiesTheRouter(t *testing.T) {
	ca := testcerts.NewAuthority(t, "test podcert")
	srv := newRouter(t, ca)
	dir := t.TempDir()
	bundle := testcerts.WriteFile(t, dir, "credential-bundle.pem", ca.ClientBundle(t, routertls.SPIFFEID("ate-env", "ate-env-api")))
	trust := testcerts.WriteFile(t, dir, "trust-bundle.pem", ca.PEM)

	cfg, err := routertls.Options{CredentialBundleFile: bundle, TrustBundleFile: trust, ServerName: routerName}.Config()
	if err != nil {
		t.Fatal(err)
	}
	code, body, err := get(t, cfg, srv.URL)
	if err != nil || code != http.StatusOK || body != "spiffe://cluster.local/ns/ate-env/sa/ate-env-api" {
		t.Fatalf("got %d %q %v", code, body, err)
	}

	// The kubelet rewrites the bundle on rotation; the next handshake
	// presents the new certificate without a restart.
	testcerts.WriteFile(t, dir, "credential-bundle.pem", ca.ClientBundle(t, routertls.SPIFFEID("ate-env", "rotated")))
	code, body, err = get(t, cfg, srv.URL)
	if err != nil || code != http.StatusOK || !strings.HasSuffix(body, "/sa/rotated") {
		t.Fatalf("after rotation: got %d %q %v", code, body, err)
	}
}

func TestConfigRefusesTheWrongRouter(t *testing.T) {
	ca := testcerts.NewAuthority(t, "test podcert")
	srv := newRouter(t, ca)
	dir := t.TempDir()
	bundle := testcerts.WriteFile(t, dir, "credential-bundle.pem", ca.ClientBundle(t, routertls.SPIFFEID("ate-env", "ate-env-api")))
	trust := testcerts.WriteFile(t, dir, "trust-bundle.pem", ca.PEM)

	// A certificate for another name.
	cfg, err := routertls.Options{CredentialBundleFile: bundle, TrustBundleFile: trust, ServerName: "other.ate-system.svc"}.Config()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := get(t, cfg, srv.URL); err == nil || !strings.Contains(err.Error(), "other.ate-system.svc") {
		t.Fatalf("wrong name: err = %v, want a verification error naming the expected name", err)
	}

	// A router signed by a CA the trust bundle does not hold.
	stranger := testcerts.NewAuthority(t, "someone else")
	otherTrust := testcerts.WriteFile(t, dir, "other-trust.pem", stranger.PEM)
	cfg, err = routertls.Options{CredentialBundleFile: bundle, TrustBundleFile: otherTrust, ServerName: routerName}.Config()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := get(t, cfg, srv.URL); err == nil {
		t.Fatal("unknown CA: want a verification error")
	}
}

// A certificate the router's allow-list does not hold is turned away after
// the handshake; the client sees a remote TLS alert, which
// RejectedCertificate recognizes, and Identity names the ID to add.
func TestRejectedCertificate(t *testing.T) {
	ca := testcerts.NewAuthority(t, "test podcert")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(echoID))
	srv.TLS = &tls.Config{
		Certificates:          []tls.Certificate{ca.Server(t, routerName)},
		ClientAuth:            tls.RequireAndVerifyClientCert,
		ClientCAs:             ca.Pool(),
		VerifyPeerCertificate: testcerts.Allow(routertls.SPIFFEID("ate-env", "someone-else")),
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	opts := routertls.Options{
		CredentialBundleFile: testcerts.WriteFile(t, dir, "credential-bundle.pem", ca.ClientBundle(t, routertls.SPIFFEID("ate-env", "ate-env-api"))),
		TrustBundleFile:      testcerts.WriteFile(t, dir, "trust-bundle.pem", ca.PEM),
		ServerName:           routerName,
	}
	cfg, err := opts.Config()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = get(t, cfg, srv.URL)
	if err == nil || !routertls.RejectedCertificate(err) {
		t.Fatalf("not allow-listed: err = %v, want a remote TLS alert that RejectedCertificate recognizes", err)
	}
	if id, err := opts.Identity(); err != nil || id != "spiffe://cluster.local/ns/ate-env/sa/ate-env-api" {
		t.Fatalf("Identity() = %q, %v", id, err)
	}
	for _, other := range []error{nil, errors.New("dial tcp 10.0.0.1:443: connection refused"), errors.New("routertls: verifying the router's certificate for x: expired")} {
		if routertls.RejectedCertificate(other) {
			t.Errorf("RejectedCertificate(%v) = true", other)
		}
	}
}

func TestConfigFailsFastOnBadFiles(t *testing.T) {
	ca := testcerts.NewAuthority(t, "test podcert")
	dir := t.TempDir()
	bundle := testcerts.WriteFile(t, dir, "credential-bundle.pem", ca.ClientBundle(t, routertls.SPIFFEID("ate-env", "ate-env-api")))
	trust := testcerts.WriteFile(t, dir, "trust-bundle.pem", ca.PEM)
	empty := testcerts.WriteFile(t, dir, "empty.pem", nil)
	for _, o := range []routertls.Options{
		{TrustBundleFile: trust, ServerName: routerName},
		{CredentialBundleFile: bundle, ServerName: routerName},
		{CredentialBundleFile: bundle, TrustBundleFile: trust},
		{CredentialBundleFile: dir + "/missing.pem", TrustBundleFile: trust, ServerName: routerName},
		{CredentialBundleFile: bundle, TrustBundleFile: empty, ServerName: routerName},
		{CredentialBundleFile: empty, TrustBundleFile: trust, ServerName: routerName},
	} {
		if _, err := o.Config(); err == nil {
			t.Errorf("%+v: want an error", o)
		}
	}
	if got := routertls.SPIFFEID("ate-env", "ate-env-api"); got != "spiffe://cluster.local/ns/ate-env/sa/ate-env-api" {
		t.Errorf("SPIFFEID = %q", got)
	}
}

type waitPeerCloseCreds struct {
	credentials.TransportCredentials
	peerClosed <-chan struct{}
}

func (w waitPeerCloseCreds) ClientHandshake(ctx context.Context, authority string, rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := w.TransportCredentials.ClientHandshake(ctx, authority, rawConn)
	if err != nil {
		return nil, nil, err
	}
	if _, ok := conn.(syscall.Conn); !ok {
		conn.Close()
		return nil, nil, errors.New("wrapped connection dropped syscall.Conn")
	}
	select {
	case <-w.peerClosed:
	case <-ctx.Done():
		conn.Close()
		return nil, nil, ctx.Err()
	}
	return conn, info, nil
}

func (w waitPeerCloseCreds) Clone() credentials.TransportCredentials {
	return waitPeerCloseCreds{
		TransportCredentials: w.TransportCredentials.Clone(),
		peerClosed:           w.peerClosed,
	}
}

// In TLS 1.3 the client finishes Handshake before the router verifies the
// client certificate. When the router sends a TLS alert and closes the TCP
// connection before grpc-go writes its HTTP/2 client preface and initial
// SETTINGS frame, TransportCredentials still reports the TLS alert so
// RejectedCertificate matches it.
func TestTransportCredentialsReadsPostHandshakeAlert(t *testing.T) {
	ca := testcerts.NewAuthority(t, "test podcert")
	peerClosed := make(chan struct{})
	var closeOnce sync.Once

	srv := httptest.NewUnstartedServer(http.HandlerFunc(echoID))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{
		Certificates:          []tls.Certificate{ca.Server(t, routerName)},
		ClientAuth:            tls.RequireAndVerifyClientCert,
		ClientCAs:             ca.Pool(),
		VerifyPeerCertificate: testcerts.Allow(routertls.SPIFFEID("ate-env", "someone-else")),
	}
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closeOnce.Do(func() { close(peerClosed) })
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg, err := routertls.Options{
		CredentialBundleFile: testcerts.WriteFile(t, dir, "credential-bundle.pem", ca.ClientBundle(t, routertls.SPIFFEID("ate-env", "ate-env-api"))),
		TrustBundleFile:      testcerts.WriteFile(t, dir, "trust-bundle.pem", ca.PEM),
		ServerName:           routerName,
	}.Config()
	if err != nil {
		t.Fatal(err)
	}

	creds := waitPeerCloseCreds{
		TransportCredentials: routertls.TransportCredentials(cfg),
		peerClosed:           peerClosed,
	}
	conn, err := grpc.NewClient(strings.TrimPrefix(srv.URL, "https://"), grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = conn.Invoke(ctx, "/ateenv.test.Nothing/Ping", &emptypb.Empty{}, &emptypb.Empty{})
	_ = conn.Close()
	if !routertls.RejectedCertificate(err) {
		t.Fatalf("Invoke() = %v, want a remote TLS alert recognized by RejectedCertificate", err)
	}

	// An allow-listed client certificate completes RPCs normally through
	// TransportCredentials.
	lis, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates:          []tls.Certificate{ca.Server(t, routerName)},
		ClientAuth:            tls.RequireAndVerifyClientCert,
		ClientCAs:             ca.Pool(),
		NextProtos:            []string{"h2"},
		VerifyPeerCertificate: testcerts.Allow(routertls.SPIFFEID("ate-env", "ate-env-api")),
	})
	if err != nil {
		t.Fatal(err)
	}
	gsrv := grpc.NewServer()
	t.Cleanup(gsrv.Stop)
	go gsrv.Serve(lis)

	okConn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(routertls.TransportCredentials(cfg).Clone()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = okConn.Close() })
	if err := okConn.Invoke(ctx, "/ateenv.test.Nothing/Ping", &emptypb.Empty{}, &emptypb.Empty{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("Invoke() with allow-listed certificate = %v, want Unimplemented", err)
	}
}
