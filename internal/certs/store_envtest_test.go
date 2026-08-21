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

package certs_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/spawnery/spawnery/internal/certs"
	"github.com/spawnery/spawnery/internal/testenv"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newStore(t *testing.T) (*certs.Store, *testClock, context.Context, string) {
	t.Helper()
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	clock := &testClock{now: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)}
	return &certs.Store{
		Client:    c,
		Namespace: ns,
		Name:      certs.SecretName,
		DNSNames:  certs.ServingDNSNames("spawnery-operator", ns),
		Clock:     clock.Now,
	}, clock, ctx, ns
}

func TestEnsureCreatesTheSecret(t *testing.T) {
	s, _, ctx, ns := newStore(t)

	b, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(b.CACertPEM) == 0 || len(b.ServingCertPEM) == 0 {
		t.Fatal("Ensure returned an empty bundle")
	}

	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: certs.SecretName, Namespace: ns}, secret); err != nil {
		t.Fatalf("the secret was not written: %v", err)
	}
	for _, key := range []string{"ca.crt", "ca.key", "tls.crt", "tls.key"} {
		if len(secret.Data[key]) == 0 {
			t.Errorf("secret key %q is empty", key)
		}
	}
	if secret.Type != corev1.SecretTypeTLS {
		t.Errorf("secret type = %q, want %q", secret.Type, corev1.SecretTypeTLS)
	}
}

// This is the restart: a second Ensure must not mint a new CA, or every agent
// pinned to the old one would stop trusting the operator.
func TestEnsureIsIdempotent(t *testing.T) {
	s, _, ctx, _ := newStore(t)

	first, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	second, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	if string(second.CACertPEM) != string(first.CACertPEM) {
		t.Error("the second Ensure replaced the CA")
	}
	if string(second.ServingCertPEM) != string(first.ServingCertPEM) {
		t.Error("the second Ensure replaced a still-valid serving certificate")
	}
}

func TestEnsureRenewsUnderTheThreshold(t *testing.T) {
	s, clock, ctx, _ := newStore(t)

	first, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}

	clock.Advance(certs.ServingLifetime*2/3 + time.Hour)
	second, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	if string(second.ServingCertPEM) == string(first.ServingCertPEM) {
		t.Error("Ensure did not renew below the threshold")
	}
	if string(second.CACertPEM) != string(first.CACertPEM) {
		t.Error("the renewal replaced the CA")
	}
}

func TestEnsureRepairsACorruptSecret(t *testing.T) {
	s, _, ctx, ns := newStore(t)

	before, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}

	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: certs.SecretName, Namespace: ns}, secret); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	secret.Data["tls.crt"] = []byte("kaputt")
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatalf("update secret: %v", err)
	}

	b, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure over a corrupt secret: %v", err)
	}
	if err := b.Validate(s.Clock(), s.DNSNames); err != nil {
		t.Errorf("the repaired bundle is still broken: %v", err)
	}
	// Only the serving certificate was damaged; the CA was still readable, so
	// the repair must have kept it rather than throwing it away.
	if string(b.CACertPEM) != string(before.CACertPEM) {
		t.Error("repairing a damaged serving certificate replaced a still-intact CA")
	}
}

// A CA cert and CA key that are each individually well-formed but do not
// belong together must be repaired once — and the repair must converge, not
// loop, since a Reissue against a mismatched pair would fail Validate again
// and Ensure would "fix" it forever under Task 9's hourly ticker.
func TestEnsureConvergesOnAMismatchedCAPair(t *testing.T) {
	s, _, ctx, _ := newStore(t)

	other, err := certs.Issue(s.Clock(), s.DNSNames)
	if err != nil {
		t.Fatalf("Issue (other): %v", err)
	}
	mine, err := certs.Issue(s.Clock(), s.DNSNames)
	if err != nil {
		t.Fatalf("Issue (mine): %v", err)
	}
	broken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"ca.crt":  mine.CACertPEM,
			"ca.key":  other.CAKeyPEM, // does not belong to ca.crt
			"tls.crt": mine.ServingCertPEM,
			"tls.key": mine.ServingKeyPEM,
		},
	}
	if err := s.Client.Create(ctx, broken); err != nil {
		t.Fatalf("create the mismatched secret: %v", err)
	}

	first, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("first Ensure over a mismatched CA pair: %v", err)
	}
	if err := first.Validate(s.Clock(), s.DNSNames); err != nil {
		t.Errorf("the repaired bundle is still broken: %v", err)
	}

	second, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if string(second.CACertPEM) != string(first.CACertPEM) {
		t.Error("the repair did not converge: the CA changed again on the second Ensure")
	}
	if string(second.ServingCertPEM) != string(first.ServingCertPEM) {
		t.Error("the repair did not converge: the serving certificate changed again on the second Ensure")
	}
}

// The gRPC server asks the provider on every handshake, so a renewal takes
// effect without a restart and without dropping a connection.
func TestProviderServesTheCurrentCertificate(t *testing.T) {
	s, _, ctx, _ := newStore(t)
	p := certs.NewProvider(s)

	first, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := p.Set(first); err != nil {
		t.Fatalf("Set: %v", err)
	}

	before, err := p.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}

	renewed, err := certs.Reissue(s.Clock(), first, s.DNSNames)
	if err != nil {
		t.Fatalf("Reissue: %v", err)
	}
	if err := p.Set(renewed); err != nil {
		t.Fatalf("Set after renewal: %v", err)
	}

	after, err := p.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate after renewal: %v", err)
	}
	if string(before.Certificate[0]) == string(after.Certificate[0]) {
		t.Error("the provider still serves the old certificate")
	}
	if string(p.CABundle()) != string(first.CACertPEM) {
		t.Error("the CA bundle changed on renewal")
	}
}

func TestGetCertificateBeforeSetFails(t *testing.T) {
	s, _, _, _ := newStore(t)
	p := certs.NewProvider(s)

	if _, err := p.GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Error("the provider handed out a certificate before it had one")
	}
}

// A rotation in progress survives the operator restarting.
//
// Everything about the sequence lives in the secret so that a new leader can
// pick it up, and this is the assertion that the storage layer actually keeps
// it. Ensure reads the secret back on every call; a rotation slot it did not
// know about would be dropped on the first renewal, silently, and the operator
// would go back to publishing one CA while agents were mid-window.
func TestEnsureCarriesARotationSlotThroughAReadBack(t *testing.T) {
	s, clock, ctx, ns := newStore(t)

	if _, err := s.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	nextCert, nextKey, err := certs.IssueCA(clock.Now())
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}
	// A previous slot alongside the next one: drop-old is the only other
	// consumer of ca-previous.*, so this is the one test standing between a
	// deleted write block and a rollback losing the CA it needs to sign with
	// again.
	prevCert, prevKey, err := certs.IssueCA(clock.Now())
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: certs.SecretName, Namespace: ns}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		t.Fatalf("get the secret: %v", err)
	}
	secret.Data["ca-next.crt"] = nextCert
	secret.Data["ca-next.key"] = nextKey
	secret.Data["ca-previous.crt"] = prevCert
	secret.Data["ca-previous.key"] = prevKey
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatalf("plant the incoming and outgoing CAs: %v", err)
	}

	// Far enough for the serving certificate to need renewing, so this goes
	// down Ensure's rewrite path rather than its no-op one.
	clock.Advance(80 * 24 * time.Hour)

	b, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure after the advance: %v", err)
	}
	if !bytes.Equal(b.NextCACertPEM, nextCert) {
		t.Error("Ensure did not read the incoming CA back out of the secret")
	}
	if !bytes.Equal(b.PreviousCACertPEM, prevCert) {
		t.Error("Ensure did not read the outgoing CA back out of the secret")
	}

	if err := s.Client.Get(ctx, key, secret); err != nil {
		t.Fatalf("get the secret again: %v", err)
	}
	if !bytes.Equal(secret.Data["ca-next.crt"], nextCert) {
		t.Error("a renewal dropped the incoming CA from the secret; a rotation " +
			"would silently lose its overlap on the next renewal")
	}
	if !bytes.Equal(secret.Data["ca-next.key"], nextKey) {
		t.Error("a renewal dropped the incoming CA's key from the secret")
	}
	if !bytes.Equal(secret.Data["ca-previous.crt"], prevCert) {
		t.Error("a renewal dropped the outgoing CA from the secret; a rollback " +
			"would have nothing left to switch back to")
	}
	if !bytes.Equal(secret.Data["ca-previous.key"], prevKey) {
		t.Error("a renewal dropped the outgoing CA's key from the secret")
	}
}

// When the stored CA is damaged beyond repair, Ensure has to mint a whole
// new one -- but a next CA already being distributed has nothing to do with
// why ca.key stopped parsing, and carrying it forward is a deliberate choice
// (see carryRotation): dropping it would leave the secret claiming a
// rotation is still distributing with no slot left to prove it, and Task 4's
// SwitchToNext would find nothing to switch to. This is the one test that
// exercises that fallback at all -- every other test's stored CA parses
// fine, so this is the only place a change to that branch would be caught.
func TestEnsureCarriesTheNextCAAcrossAFullCAReissue(t *testing.T) {
	s, clock, ctx, ns := newStore(t)

	before, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	nextCert, nextKey, err := certs.IssueCA(clock.Now())
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: certs.SecretName, Namespace: ns}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		t.Fatalf("get the secret: %v", err)
	}
	secret.Data["ca-next.crt"] = nextCert
	secret.Data["ca-next.key"] = nextKey
	// Not PEM at all, let alone an EC key -- parseCA has to fail outright,
	// not just fail to match a certificate, so Ensure takes the Issue
	// fallback rather than the Reissue-keeps-the-CA one.
	secret.Data["ca.key"] = []byte("not a key")
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatalf("plant the incoming CA and corrupt ca.key: %v", err)
	}

	after, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure after the corruption: %v", err)
	}
	if bytes.Equal(after.CACertPEM, before.CACertPEM) {
		t.Fatal("the signing CA is unchanged; the test did not reach the Issue fallback")
	}
	if !bytes.Equal(after.NextCACertPEM, nextCert) {
		t.Error("Ensure dropped the incoming CA while replacing an unparseable one")
	}
	if !bytes.Equal(after.NextCAKeyPEM, nextKey) {
		t.Error("Ensure dropped the incoming CA's key while replacing an unparseable one")
	}
}
