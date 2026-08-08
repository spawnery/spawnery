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

package certs

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func testDNSNames() []string { return ServingDNSNames("spawnery-operator", "spawnery-system") }

func parseServing(t *testing.T, b *Bundle) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(b.ServingCertPEM)
	if block == nil {
		t.Fatal("serving certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse serving certificate: %v", err)
	}
	return cert
}

func TestServingDNSNamesCoverEveryWayToReachTheService(t *testing.T) {
	got := ServingDNSNames("spawnery-operator", "spawnery-system")
	want := []string{
		"spawnery-operator",
		"spawnery-operator.spawnery-system",
		"spawnery-operator.spawnery-system.svc",
		"spawnery-operator.spawnery-system.svc.cluster.local",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("name %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIssueSetsTheSANsAndLifetimes(t *testing.T) {
	b, err := Issue(testNow, testDNSNames())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	cert := parseServing(t, b)

	if len(cert.DNSNames) != 4 {
		t.Errorf("DNSNames = %v", cert.DNSNames)
	}
	if got := cert.NotAfter.Sub(cert.NotBefore); got != ServingLifetime {
		t.Errorf("serving lifetime = %v, want %v", got, ServingLifetime)
	}
	// A minute of backdating absorbs clock skew between nodes.
	if !cert.NotBefore.Before(testNow) {
		t.Errorf("NotBefore = %v, want it backdated before %v", cert.NotBefore, testNow)
	}
}

// The whole point of the pinned CA: an agent that trusts only ca.crt must
// accept the serving certificate.
func TestServingCertificateChainsToTheCA(t *testing.T) {
	b, err := Issue(testNow, testDNSNames())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CACertPEM) {
		t.Fatal("CA is not usable as a pool")
	}
	_, err = parseServing(t, b).Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: testNow.Add(24 * time.Hour),
		DNSName:     "spawnery-operator.spawnery-system.svc",
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		t.Errorf("verify against the pinned CA: %v", err)
	}
}

func TestNeedsRenewalAtOneThirdRemaining(t *testing.T) {
	b, err := Issue(testNow, testDNSNames())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"fresh", testNow, false},
		{"just above the threshold", testNow.Add(ServingLifetime*2/3 - time.Hour), false},
		{"just below the threshold", testNow.Add(ServingLifetime*2/3 + time.Hour), true},
		{"expired", testNow.Add(ServingLifetime + time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.NeedsRenewal(tc.at); got != tc.want {
				t.Errorf("NeedsRenewal(%v) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

// Renewal must not change the CA — every running agent has it pinned.
func TestReissueKeepsTheCA(t *testing.T) {
	first, err := Issue(testNow, testDNSNames())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	later := testNow.Add(80 * 24 * time.Hour)
	second, err := Reissue(later, first, testDNSNames())
	if err != nil {
		t.Fatalf("Reissue: %v", err)
	}

	if string(second.CACertPEM) != string(first.CACertPEM) {
		t.Error("Reissue replaced the CA; every connected agent would break")
	}
	if string(second.ServingCertPEM) == string(first.ServingCertPEM) {
		t.Error("Reissue did not replace the serving certificate")
	}
	if second.NeedsRenewal(later) {
		t.Error("the freshly reissued certificate already wants renewal")
	}
}

func TestValidateRejectsWhatMustLeadToReissue(t *testing.T) {
	good, err := Issue(testNow, testDNSNames())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(b *Bundle)
		at     time.Time
	}{
		{"garbage instead of a certificate", func(b *Bundle) { b.ServingCertPEM = []byte("nope") }, testNow},
		{"CA missing", func(b *Bundle) { b.CACertPEM = nil }, testNow},
		{"CA key missing", func(b *Bundle) { b.CAKeyPEM = nil }, testNow},
		{"expired", func(b *Bundle) {}, testNow.Add(ServingLifetime + time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bundle{
				CACertPEM: good.CACertPEM, CAKeyPEM: good.CAKeyPEM,
				ServingCertPEM: good.ServingCertPEM, ServingKeyPEM: good.ServingKeyPEM,
			}
			tc.mutate(b)
			if err := b.Validate(tc.at, testDNSNames()); err == nil {
				t.Error("Validate accepted a bundle that must be reissued")
			}
		})
	}

	if err := good.Validate(testNow, testDNSNames()); err != nil {
		t.Errorf("Validate rejected a good bundle: %v", err)
	}
}

// A service moved to another namespace changes the SANs; the old certificate
// is then useless even though it has not expired.
func TestValidateRejectsMissingSAN(t *testing.T) {
	b, err := Issue(testNow, ServingDNSNames("spawnery-operator", "spawnery-system"))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := b.Validate(testNow, ServingDNSNames("spawnery-operator", "andere-ns")); err == nil {
		t.Error("Validate accepted a certificate without the required SAN")
	}
}

func TestTLSCertificateIsUsable(t *testing.T) {
	b, err := Issue(testNow, testDNSNames())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	tlsCert, err := b.TLSCertificate()
	if err != nil {
		t.Fatalf("TLSCertificate: %v", err)
	}
	if len(tlsCert.Certificate) == 0 || tlsCert.PrivateKey == nil {
		t.Error("TLSCertificate returned an empty pair")
	}
}
