/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package certs issues and renews the operator's own serving certificate. The
// operator is its own CA on purpose: it creates the agent pods anyway, so it
// can pin its CA into them, and "one helm install is enough" survives without
// cert-manager.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"slices"
	"time"
)

const (
	// CALifetime is long because rotating the CA needs an overlap phase that
	// milestone 2a does not build.
	CALifetime = 10 * 365 * 24 * time.Hour
	// ServingLifetime is short because renewing it is automatic and costs no
	// connection.
	ServingLifetime = 90 * 24 * time.Hour

	// backdate absorbs clock skew between the operator and the agents' nodes.
	backdate = time.Minute
)

// Bundle is everything the operator keeps in its TLS secret.
type Bundle struct {
	CACertPEM      []byte
	CAKeyPEM       []byte
	ServingCertPEM []byte
	ServingKeyPEM  []byte

	// NextCACertPEM and NextCAKeyPEM hold the incoming CA while a rotation is
	// distributing, and are empty at every other time. The CA that signs the
	// serving certificate stays CACertPEM/CAKeyPEM throughout that phase --
	// still the outgoing one -- and that is what makes the phase safe: the new
	// CA is published for agents to trust well before anything is signed with
	// it.
	NextCACertPEM []byte
	NextCAKeyPEM  []byte

	// PreviousCACertPEM and PreviousCAKeyPEM hold the outgoing CA between the
	// switch and drop-old. The key is kept and not only the certificate,
	// because signing with it again is the whole content of a rollback; a
	// certificate on its own would be trust nobody can act on.
	PreviousCACertPEM []byte
	PreviousCAKeyPEM  []byte
}

// ServingDNSNames are the names an agent may use to reach the service.
func ServingDNSNames(service, namespace string) []string {
	return []string{
		service,
		service + "." + namespace,
		service + "." + namespace + ".svc",
		service + "." + namespace + ".svc.cluster.local",
	}
}

// IssueCA mints a self-signed CA. Issue calls it for the first one; a rotation
// calls it for the incoming one, which is why it exists separately: at that
// point there is no serving certificate to sign, and signing one would be
// exactly the thing the overlap window has to postpone.
func IssueCA(now time.Time) (certPEM, keyPEM []byte, err error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "spawnery-agent-ca"},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(CALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("self-sign CA: %w", err)
	}
	caKeyPEM, err := encodeKey(caKey)
	if err != nil {
		return nil, nil, err
	}
	return encodeCert(caDER), caKeyPEM, nil
}

// Issue creates a fresh CA and a serving certificate signed by it.
func Issue(now time.Time, dnsNames []string) (*Bundle, error) {
	caCertPEM, caKeyPEM, err := IssueCA(now)
	if err != nil {
		return nil, err
	}
	b := &Bundle{
		CACertPEM: caCertPEM,
		CAKeyPEM:  caKeyPEM,
	}
	return Reissue(now, b, dnsNames)
}

// Reissue replaces only the serving certificate. The CA stays, because every
// running agent has it pinned.
func Reissue(now time.Time, b *Bundle, dnsNames []string) (*Bundle, error) {
	caCert, caKey, err := b.parseCA()
	if err != nil {
		return nil, err
	}
	servingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate serving key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     slices.Clone(dnsNames),
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(ServingLifetime - backdate),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &servingKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign serving certificate: %w", err)
	}
	keyPEM, err := encodeKey(servingKey)
	if err != nil {
		return nil, err
	}
	return &Bundle{
		CACertPEM:      b.CACertPEM,
		CAKeyPEM:       b.CAKeyPEM,
		ServingCertPEM: encodeCert(der),
		ServingKeyPEM:  keyPEM,
	}, nil
}

// PublishedCA is what the agents pin: the CA that signs the serving
// certificate, followed by whichever second CA the rotation is currently
// holding. Order does not matter to the agent -- OperatorChannel.trustManager
// loads every certificate in the stream -- but it is deterministic so that a
// phase which has not changed produces a ConfigMap write that is a no-op.
func (b *Bundle) PublishedCA() []byte {
	switch {
	case len(b.NextCACertPEM) > 0:
		return slices.Concat(b.CACertPEM, b.NextCACertPEM)
	case len(b.PreviousCACertPEM) > 0:
		return slices.Concat(b.CACertPEM, b.PreviousCACertPEM)
	}
	return b.CACertPEM
}

// NeedsRenewal is true once less than a third of the lifetime is left.
func (b *Bundle) NeedsRenewal(now time.Time) bool {
	cert, err := b.parseServing()
	if err != nil {
		return true
	}
	total := cert.NotAfter.Sub(cert.NotBefore)
	return now.After(cert.NotAfter.Add(-total / 3))
}

// Validate reports why a bundle read back from the secret cannot be used.
func (b *Bundle) Validate(now time.Time, dnsNames []string) error {
	if len(b.CACertPEM) == 0 || len(b.CAKeyPEM) == 0 {
		return fmt.Errorf("bundle has no CA")
	}
	caCert, _, err := b.parseCA()
	if err != nil {
		return err
	}
	cert, err := b.parseServing()
	if err != nil {
		return err
	}
	if now.After(cert.NotAfter) || now.Before(cert.NotBefore) {
		return fmt.Errorf("serving certificate is not valid at %s", now)
	}
	for _, name := range dnsNames {
		if !slices.Contains(cert.DNSNames, name) {
			return fmt.Errorf("serving certificate lacks the SAN %q", name)
		}
	}
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		return fmt.Errorf("serving certificate was not signed by the stored CA: %w", err)
	}
	if _, err := b.TLSCertificate(); err != nil {
		return err
	}
	return nil
}

// TLSCertificate is the pair the gRPC server serves.
func (b *Bundle) TLSCertificate() (tls.Certificate, error) {
	return tls.X509KeyPair(b.ServingCertPEM, b.ServingKeyPEM)
}

// WithNextCA returns a bundle carrying an incoming CA. The serving certificate
// is untouched and still chains to CACertPEM.
func (b *Bundle) WithNextCA(certPEM, keyPEM []byte) *Bundle {
	return &Bundle{
		CACertPEM:         b.CACertPEM,
		CAKeyPEM:          b.CAKeyPEM,
		ServingCertPEM:    b.ServingCertPEM,
		ServingKeyPEM:     b.ServingKeyPEM,
		NextCACertPEM:     certPEM,
		NextCAKeyPEM:      keyPEM,
		PreviousCACertPEM: b.PreviousCACertPEM,
		PreviousCAKeyPEM:  b.PreviousCAKeyPEM,
	}
}

// SwitchToNext promotes the incoming CA to the signing one, demotes the
// outgoing one to the previous slot, and signs a fresh serving certificate
// with the new CA. This is the step the overlap window exists to protect, and
// the only one that can strand an agent.
func (b *Bundle) SwitchToNext(now time.Time, dnsNames []string) (*Bundle, error) {
	if len(b.NextCACertPEM) == 0 || len(b.NextCAKeyPEM) == 0 {
		return nil, fmt.Errorf("bundle has no next CA to switch to")
	}
	switched, err := Reissue(now, &Bundle{CACertPEM: b.NextCACertPEM, CAKeyPEM: b.NextCAKeyPEM}, dnsNames)
	if err != nil {
		return nil, err
	}
	switched.PreviousCACertPEM = b.CACertPEM
	switched.PreviousCAKeyPEM = b.CAKeyPEM
	return switched, nil
}

// RestorePrevious is SwitchToNext undone: the outgoing CA signs again and the
// incoming one is discarded. Meaningful only after a switch.
func (b *Bundle) RestorePrevious(now time.Time, dnsNames []string) (*Bundle, error) {
	if len(b.PreviousCACertPEM) == 0 || len(b.PreviousCAKeyPEM) == 0 {
		return nil, fmt.Errorf("bundle has no previous CA to restore")
	}
	return Reissue(now, &Bundle{CACertPEM: b.PreviousCACertPEM, CAKeyPEM: b.PreviousCAKeyPEM}, dnsNames)
}

// WithoutRotation empties both rotation slots, leaving the signing CA alone.
// It is what drop-old does, and what a rollback out of the distributing phase
// does.
func (b *Bundle) WithoutRotation() *Bundle {
	return &Bundle{
		CACertPEM:      b.CACertPEM,
		CAKeyPEM:       b.CAKeyPEM,
		ServingCertPEM: b.ServingCertPEM,
		ServingKeyPEM:  b.ServingKeyPEM,
	}
}

func (b *Bundle) parseCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(b.CACertPEM)
	keyBlock, _ := pem.Decode(b.CAKeyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, fmt.Errorf("CA is not PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	certPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !key.PublicKey.Equal(certPub) {
		return nil, nil, fmt.Errorf("CA key does not match the CA certificate")
	}
	return cert, key, nil
}

func (b *Bundle) parseServing() (*x509.Certificate, error) {
	block, _ := pem.Decode(b.ServingCertPEM)
	if block == nil {
		return nil, fmt.Errorf("serving certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse serving certificate: %w", err)
	}
	return cert, nil
}

func newSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("draw serial number: %w", err)
	}
	return serial, nil
}

func encodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}
