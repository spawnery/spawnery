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
	"context"
	"crypto/tls"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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

	if _, err := s.Ensure(ctx); err != nil {
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
