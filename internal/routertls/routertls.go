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

// Package routertls is the client side of ate-env-api's TLS connections to the
// atenet router. Since Substrate v0.6.0 the router serves only TLS listeners,
// HTTPS for the guest's gRPC and a TLS CONNECT listener for other ports, and
// each requires a client certificate whose SPIFFE ID is on the router's
// ingress allow-list. The certificate is the pod identity credential bundle
// the kubelet projects into the pod: the signer issues it for 24 hours and
// the kubelet renews it 30 minutes before expiry. The router's own serving
// certificate chains to the servicedns trust bundle. Both files are checked
// at every handshake and parsed again when they change, so a rotation needs
// no restart.
package routertls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc/credentials"
)

// Options name the files and the router.
type Options struct {
	// CredentialBundleFile is a PEM file holding the client certificate chain
	// and its private key: the credential bundle of the pod identity signer
	// (podidentity.podcert.ate.dev/identity). Required.
	CredentialBundleFile string
	// TrustBundleFile is a PEM file with the CAs that sign the router's
	// serving certificate: the servicedns trust bundle. Required.
	TrustBundleFile string
	// ServerName is the name the router's serving certificate must carry,
	// its Service DNS name, e.g. atenet-router.ate-system.svc. Required.
	ServerName string
}

// SPIFFEID is the identity the pod identity signer gives a pod: what the
// router's --ingress-allowed-spiffe-ids must list for ate-env-api.
func SPIFFEID(namespace, serviceAccount string) string {
	return fmt.Sprintf("spiffe://cluster.local/ns/%s/sa/%s", namespace, serviceAccount)
}

// Identity is the SPIFFE ID the credential bundle's certificate carries, its
// URI SAN: what the router's allow-list must hold.
func (o Options) Identity() (string, error) {
	cert, err := loadBundle(o.CredentialBundleFile)
	if err != nil {
		return "", err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return "", fmt.Errorf("routertls: parsing the client certificate: %w", err)
	}
	if len(leaf.URIs) == 0 {
		return "", fmt.Errorf("routertls: the client certificate in %s carries no SPIFFE ID (URI SAN)", o.CredentialBundleFile)
	}
	return leaf.URIs[0].String(), nil
}

// RejectedCertificate reports whether err is the router turning the client
// certificate away: its SPIFFE ID is not on the ingress allow-list, the
// router does not trust the CA that signed it, or it is not valid. The router
// sends a TLS alert, which Go reports on the first read or write after the
// handshake as "remote error: tls: ...", inside a gRPC Unavailable or a plain
// error, so the text identifies it; the alert names which. No wait or retry
// fixes it.
func RejectedCertificate(err error) bool {
	return err != nil && strings.Contains(err.Error(), "remote error: tls:")
}

// TransportCredentials wraps credentials.NewTLS(cfg) so gRPC client dials
// report post-handshake TLS 1.3 alerts. In TLS 1.3 the client finishes
// Handshake before the router verifies the client certificate. grpc-go then
// writes the HTTP/2 client preface and flushes the initial SETTINGS frame
// before awaiting its reader goroutine. If the router has already sent a TLS
// alert and closed the socket, the second write fails with EPIPE and hides
// the alert. When a write fails before any peer bytes arrive, alertConn
// performs a short-deadline read to return a pending TLS alert.
func TransportCredentials(cfg *tls.Config) credentials.TransportCredentials {
	return &alertCreds{TransportCredentials: credentials.NewTLS(cfg)}
}

type alertCreds struct {
	credentials.TransportCredentials
}

func (c *alertCreds) ClientHandshake(ctx context.Context, authority string, rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := c.TransportCredentials.ClientHandshake(ctx, authority, rawConn)
	if err != nil {
		return nil, nil, err
	}
	return wrapAlertConn(conn), info, nil
}

func (c *alertCreds) Clone() credentials.TransportCredentials {
	return &alertCreds{TransportCredentials: c.TransportCredentials.Clone()}
}

type sysConn = syscall.Conn

type alertConn struct {
	net.Conn
	probeDone atomic.Bool
}

type alertSyscallConn struct {
	*alertConn
	sysConn
}

func wrapAlertConn(conn net.Conn) net.Conn {
	ac := &alertConn{Conn: conn}
	if sc, ok := conn.(syscall.Conn); ok {
		return &alertSyscallConn{alertConn: ac, sysConn: sc}
	}
	return ac
}

const postHandshakeAlertReadTimeout = 50 * time.Millisecond

func (c *alertConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.probeDone.Store(true)
	}
	return n, err
}

func (c *alertConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if err == nil || RejectedCertificate(err) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrDeadlineExceeded) || c.probeDone.Swap(true) {
		return n, err
	}
	_ = c.Conn.SetReadDeadline(time.Now().Add(postHandshakeAlertReadTimeout))
	var one [1]byte
	if _, rerr := c.Conn.Read(one[:]); RejectedCertificate(rerr) {
		return n, rerr
	}
	return n, err
}

// RejectionAdvice is what an operator must check when the router rejects
// the certificate, for error messages.
const RejectionAdvice = "the router must list the API's SPIFFE ID in --ingress-allowed-spiffe-ids (ATE_INGRESS_ALLOWED_SPIFFE_IDS for ate-setup) and trust the podidentity signer, and the certificate must be current"

// Config returns the TLS configuration for the router's listeners. It parses
// both files once, so a missing or unreadable file fails at startup rather
// than at the first connection, and again whenever they change.
func (o Options) Config() (*tls.Config, error) {
	switch {
	case o.CredentialBundleFile == "":
		return nil, errors.New("routertls: the credential bundle file is required")
	case o.TrustBundleFile == "":
		return nil, errors.New("routertls: the trust bundle file is required")
	case o.ServerName == "":
		return nil, errors.New("routertls: the server name is required")
	}
	bundle := &cached[*tls.Certificate]{path: o.CredentialBundleFile, what: "credential bundle", parse: parseBundle}
	roots := &cached[*x509.CertPool]{path: o.TrustBundleFile, what: "trust bundle", parse: parsePool}
	if _, err := bundle.get(); err != nil {
		return nil, err
	}
	if _, err := roots.get(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: o.ServerName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return bundle.get()
		},
		// The chain is verified in VerifyConnection against the trust bundle
		// as it is at that moment, so a rotated CA is honored without a
		// restart; InsecureSkipVerify only turns off the built-in check that
		// would use a pool fixed at startup. VerifyConnection also runs on a
		// resumed session, which VerifyPeerCertificate would skip.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			pool, err := roots.get()
			if err != nil {
				return err
			}
			return o.verify(cs, pool)
		},
	}, nil
}

// cached is a parsed PEM file, parsed again only when the file's size or
// modification time changed: the kubelet rewrites a projected file on
// rotation, and the handshake per environment must not reparse it otherwise.
type cached[T any] struct {
	path, what string
	parse      func(path string, b []byte) (T, error)

	mu      sync.Mutex
	modTime time.Time
	size    int64
	value   T
	ok      bool
}

func (c *cached[T]) get() (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero T
	st, err := os.Stat(c.path)
	if err != nil {
		return zero, fmt.Errorf("routertls: reading the %s: %w", c.what, err)
	}
	if c.ok && st.ModTime().Equal(c.modTime) && st.Size() == c.size {
		return c.value, nil
	}
	b, err := os.ReadFile(c.path)
	if err != nil {
		return zero, fmt.Errorf("routertls: reading the %s: %w", c.what, err)
	}
	v, err := c.parse(c.path, b)
	if err != nil {
		return zero, err
	}
	c.modTime, c.size, c.value, c.ok = st.ModTime(), st.Size(), v, true
	return v, nil
}

func (o Options) verify(cs tls.ConnectionState, roots *x509.CertPool) error {
	certs := cs.PeerCertificates
	if len(certs) == 0 {
		return errors.New("routertls: the router presented no certificate")
	}
	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	_, err := certs[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		DNSName:       o.ServerName,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("routertls: verifying the router's certificate for %s: %w", o.ServerName, err)
	}
	return nil
}

// loadBundle reads a PEM credential bundle once: the certificate chain and
// the key in one file, in either order.
func loadBundle(path string) (*tls.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("routertls: reading the credential bundle: %w", err)
	}
	return parseBundle(path, b)
}

func parseBundle(path string, b []byte) (*tls.Certificate, error) {
	cert, err := tls.X509KeyPair(b, b)
	if err != nil {
		return nil, fmt.Errorf("routertls: credential bundle %s: %w", path, err)
	}
	return &cert, nil
}

func parsePool(path string, b []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("routertls: trust bundle %s holds no certificate", path)
	}
	return pool, nil
}
