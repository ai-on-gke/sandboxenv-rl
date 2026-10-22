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
// the kubelet projects into the pod and rotates about hourly; the router's own
// serving certificate chains to the servicedns trust bundle. Both files are
// read at every handshake, so a rotation needs no restart.
package routertls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
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

// Config returns the TLS configuration for the router's listeners. It reads
// both files once, so a missing or unreadable file fails at startup rather
// than at the first connection, and again at every handshake.
func (o Options) Config() (*tls.Config, error) {
	switch {
	case o.CredentialBundleFile == "":
		return nil, errors.New("routertls: the credential bundle file is required")
	case o.TrustBundleFile == "":
		return nil, errors.New("routertls: the trust bundle file is required")
	case o.ServerName == "":
		return nil, errors.New("routertls: the server name is required")
	}
	if _, err := loadBundle(o.CredentialBundleFile); err != nil {
		return nil, err
	}
	if _, err := loadPool(o.TrustBundleFile); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: o.ServerName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return loadBundle(o.CredentialBundleFile)
		},
		// The chain is verified in VerifyPeerCertificate against a trust
		// bundle read at that moment, so a rotated CA is honored without a
		// restart; InsecureSkipVerify only turns off the built-in check that
		// would use a pool fixed at startup.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: o.verify,
	}, nil
}

func (o Options) verify(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return errors.New("routertls: the router presented no certificate")
	}
	roots, err := loadPool(o.TrustBundleFile)
	if err != nil {
		return err
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("routertls: parsing the router's certificate: %w", err)
		}
		certs = append(certs, c)
	}
	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	_, err = certs[0].Verify(x509.VerifyOptions{
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

// loadBundle reads a PEM credential bundle: the certificate chain and the
// key in one file, in either order.
func loadBundle(path string) (*tls.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("routertls: reading the credential bundle: %w", err)
	}
	cert, err := tls.X509KeyPair(b, b)
	if err != nil {
		return nil, fmt.Errorf("routertls: credential bundle %s: %w", path, err)
	}
	return &cert, nil
}

func loadPool(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("routertls: reading the trust bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("routertls: trust bundle %s holds no certificate", path)
	}
	return pool, nil
}
