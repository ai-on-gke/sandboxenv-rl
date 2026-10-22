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

// Package testcerts mints a throwaway certificate authority, server
// certificates and client credential bundles for tests of the TLS paths to
// the atenet router. Nothing here is used outside tests.
package testcerts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Authority is a test CA.
type Authority struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	// PEM is the CA certificate, the content of a trust bundle file.
	PEM []byte
}

// NewAuthority mints a CA named name.
func NewAuthority(t testing.TB, name string) *Authority {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("testcerts: CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &Authority{Cert: cert, Key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// Pool holds the authority alone, as a server's ClientCAs or a client's RootCAs.
func (a *Authority) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(a.Cert)
	return p
}

// Server mints a serving certificate for dnsNames.
func (a *Authority) Server(t testing.TB, dnsNames ...string) tls.Certificate {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.Cert, &key.PublicKey, a.Key)
	if err != nil {
		t.Fatalf("testcerts: server certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// ClientBundle mints a client certificate carrying spiffeID as its URI SAN
// and returns it with its key as one PEM bundle, the shape of the credential
// bundle the kubelet projects.
func (a *Authority) ClientBundle(t testing.TB, spiffeID string) []byte {
	t.Helper()
	id, err := url.Parse(spiffeID)
	if err != nil {
		t.Fatal(err)
	}
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		URIs:         []*url.URL{id},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.Cert, &key.PublicKey, a.Key)
	if err != nil {
		t.Fatalf("testcerts: client certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return append(bundle, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...)
}

// WriteFile writes data under dir and returns the path.
func WriteFile(t testing.TB, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatal(err)
	}
	return n
}
