/*
Copyright paul_wtf.

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

// Package certs issues and renews the operator's own serving certificate.
// The operator is its own CA so that one helm install works without
// cert-manager; it pins the CA into the agent pods it creates.
package certs

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"
)

const (
	// CALifetime is long because only a human starts a CA rotation.
	CALifetime = 10 * 365 * 24 * time.Hour
	// ServingLifetime is short because renewal is automatic and drops no
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
	// distributing. It is published for trust but signs nothing yet.
	NextCACertPEM []byte
	NextCAKeyPEM  []byte

	// PreviousCACertPEM and PreviousCAKeyPEM hold the outgoing CA between the
	// switch and drop-old. The key is kept because a rollback signs with it.
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

// IssueCA mints a self-signed CA without a serving certificate, which a
// rotation must not sign with the incoming CA yet.
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

// PublishedCA is what the agents pin: the signing CA, then whichever
// rotation slot is held. The order is fixed so an unchanged phase writes an
// identical ConfigMap. A slot failing parsableCert is treated as absent.
func (b *Bundle) PublishedCA() []byte {
	switch {
	case parsableCert(b.NextCACertPEM) == nil:
		return slices.Concat(b.CACertPEM, b.NextCACertPEM)
	case parsableCert(b.PreviousCACertPEM) == nil:
		return slices.Concat(b.CACertPEM, b.PreviousCACertPEM)
	}
	return b.CACertPEM
}

// errNotOnlyTheFirstBlock: the first PEM block is a certificate, but the slot
// holds more. parseCA reads only that block, so truncating to it loses
// nothing. The surplus must not be published: an invalid block makes the
// agent throw for the whole stream, a valid one silently widens its trust.
var errNotOnlyTheFirstBlock = errors.New("more than its first PEM block")

// parsableCert accepts exactly the PEM encoding of one certificate.
//
// OpenJDK's X509Factory steps over stray bytes without a five-hyphen line but
// throws for the whole stream on any such line that opens no decodable
// certificate. This check is stricter, because depending on that JDK detail
// breaks on a single pasted "-----".
//
// It compares against firstPEMBlock rather than inspecting pem.Decode's rest:
// Decode silently skips junk before the first block.
func parsableCert(pemBytes []byte) error {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return fmt.Errorf("not PEM")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return fmt.Errorf("parse certificate: %w", err)
	}
	// A slot the operator wrote itself re-encodes to the same bytes.
	if !bytes.Equal(pemBytes, firstPEMBlock(pemBytes)) {
		return errNotOnlyTheFirstBlock
	}
	return nil
}

// firstPEMBlock re-encodes the first PEM block of pemBytes and drops whatever
// surrounded it. Re-encoded because pem.Decode does not report the block's
// offsets.
func firstPEMBlock(pemBytes []byte) []byte {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil
	}
	return pem.EncodeToMemory(block)
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
// with the new CA. The only step that can strand an agent.
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
