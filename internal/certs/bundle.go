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
//
// A slot that fails parsableCert is treated as absent rather than surfaced as
// an error: this function is pure and keeps no logger, because a mistyped
// annotation reaching here must not take the operator down, and the report is
// a later concern's, not this one's.
func (b *Bundle) PublishedCA() []byte {
	switch {
	case parsableCert(b.NextCACertPEM) == nil:
		return slices.Concat(b.CACertPEM, b.NextCACertPEM)
	case parsableCert(b.PreviousCACertPEM) == nil:
		return slices.Concat(b.CACertPEM, b.PreviousCACertPEM)
	}
	return b.CACertPEM
}

// errNotOnlyTheFirstBlock is the one parsableCert verdict a caller can repair
// instead of throwing the slot away, and it is a sentinel rather than a
// message so that the caller keys on the verdict rather than on this file's
// wording.
//
// It says: the first PEM block is a certificate, and the slot is more than
// that block alone. Which is exactly the case where parsableCert and parseCA
// disagree -- parseCA decodes the first block and ignores everything else, so
// the slot still signs perfectly well -- and therefore exactly the case where
// truncating to the first block costs nothing and settles the disagreement.
//
// What the agent does with the surplus depends on what the surplus is, and
// neither answer is one to publish on. A trailing five-hyphen run that opens
// no valid block throws for the whole stream, taking the signing CA with it.
// A trailing *valid* block does not throw at all: it is loaded as another CA,
// silently widening the fleet's trust store. Leading junk is the same two
// cases with the certificate after it rather than before.
var errNotOnlyTheFirstBlock = errors.New("more than its first PEM block")

// parsableCert reports whether pemBytes is something an agent's trust store
// will accept: exactly the PEM encoding of one certificate.
//
// **What the agent actually rejects, measured rather than assumed.**
// OperatorChannel.trustManager parses with CertificateFactory.generateCertificates.
// OpenJDK's X509Factory.readOneBlock skips everything before the first line
// beginning with a five-hyphen run and returns null at end of stream instead
// of throwing, so stray bytes carrying no such line are stepped over: a good
// ca.crt followed by "this is not a certificate" still yields the good CA.
// What kills the stream is a line that begins with a five-hyphen run and does
// not open a complete, decodable certificate block -- a PEM envelope around
// something that is not a certificate, a block whose base64 is malformed, a
// header with no matching footer, or a bare "-----x-----" line. Then nothing
// already parsed survives; the whole bundle throws.
//
// **This check is deliberately stricter than that.** "Stray bytes that happen
// to contain no five-hyphen run" is not a property worth depending on: it
// belongs to one JDK's block scanner rather than to the format, it is
// invisible in the bytes a human pastes, and a single "-----" anywhere in
// them flips it. So the rule is the narrow one -- exactly the PEM encoding of
// one certificate -- and everything else is repaired or cleared by
// AdvanceRotation rather than published on the strength of that property.
//
// **One rule, not a list of failure modes.** The slot must be byte-identical
// to firstPEMBlock of itself, and that block must be a certificate. Phrased
// as "decode, then look at what is left over" it would have three modes and a
// hole: pem.Decode skips whatever precedes the first block, so `rest` comes
// back empty for a slot with junk pasted in front of the certificate and the
// junk is invisible to the check -- while the bytes that reach the agent
// still carry it.
func parsableCert(pemBytes []byte) error {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return fmt.Errorf("not PEM")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return fmt.Errorf("parse certificate: %w", err)
	}
	// firstPEMBlock is DER-faithful and idempotent, so a slot the operator
	// wrote itself compares equal here and nothing happens to it. Anything
	// else -- leading junk, trailing junk, a second block, or merely a
	// different encoding of the same certificate -- is what a hand-edit
	// produces, and the caller can repair every one of them the same way.
	if !bytes.Equal(pemBytes, firstPEMBlock(pemBytes)) {
		return errNotOnlyTheFirstBlock
	}
	return nil
}

// firstPEMBlock re-encodes the first PEM block of pemBytes and drops whatever
// surrounded it.
//
// Re-encoded rather than sliced out, because pem.Decode reports neither where
// the block began nor where it ended, and slicing from what it does report
// would carry any preceding junk along. The DER inside is untouched, so this
// is the same certificate parseCA reads out of these bytes.
//
// nil for input with no PEM block at all, which is how parsableCert's
// comparison stays correct without a second decode of its own: pemBytes is
// never nil there, having already decoded.
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
