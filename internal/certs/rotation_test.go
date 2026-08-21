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
	"encoding/pem"
	"slices"
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/certs"
)

// The switch pairs each certificate with its own key, and both pairs stay
// usable afterwards.
//
// Pairing the incoming certificate with the outgoing key, or the reverse,
// does not produce a bundle that looks complete: SwitchToNext hands a
// two-field {CACertPEM, CAKeyPEM} bundle to Reissue before any serving
// certificate exists, and Reissue's own parseCA already checks that a key
// matches its certificate, so the mismatch surfaces as an error out of
// SwitchToNext itself, before Validate ever runs. What still needs asserting
// here is that both pairs -- the signing one and the one a rollback would
// sign with -- are each individually correct, since parseCA only ever checks
// one pair at a time.
func TestSwitchingToTheNextCAPairsEveryCertificateWithItsOwnKey(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	dnsNames := certs.ServingDNSNames("spawnery-operator", "spawnery-system")

	first, err := certs.Issue(now, dnsNames)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	nextCert, nextKey, err := certs.IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}

	switched, err := first.WithNextCA(nextCert, nextKey).SwitchToNext(now, dnsNames)
	if err != nil {
		t.Fatalf("SwitchToNext: %v", err)
	}

	if err := switched.Validate(now, dnsNames); err != nil {
		t.Fatalf("the switched bundle does not validate: %v", err)
	}
	if !bytes.Equal(switched.CACertPEM, nextCert) {
		t.Error("the signing CA after the switch is not the incoming one")
	}
	if !bytes.Equal(switched.PreviousCACertPEM, first.CACertPEM) {
		t.Error("the outgoing CA was not kept as the previous one")
	}
	if len(switched.NextCACertPEM) != 0 || len(switched.NextCAKeyPEM) != 0 {
		t.Error("the next slot is still occupied after the switch")
	}

	// The previous pair has to be usable, not merely present: it is what a
	// rollback signs with.
	rolledBack, err := switched.RestorePrevious(now, dnsNames)
	if err != nil {
		t.Fatalf("RestorePrevious: %v", err)
	}
	if err := rolledBack.Validate(now, dnsNames); err != nil {
		t.Fatalf("the rolled-back bundle does not validate: %v", err)
	}
	if !bytes.Equal(rolledBack.CACertPEM, first.CACertPEM) {
		t.Error("the rollback did not restore the original CA")
	}
	if len(rolledBack.NextCACertPEM) != 0 || len(rolledBack.PreviousCACertPEM) != 0 {
		t.Error("the rollback left a rotation slot occupied")
	}
}

// What the agents pin, in each phase. Two PEMs, not one, and the signing CA
// first so that an unchanged phase produces an unchanged ConfigMap write.
func TestThePublishedBundleCarriesBothCAsWhileRotating(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	dnsNames := certs.ServingDNSNames("spawnery-operator", "spawnery-system")

	atRest, err := certs.Issue(now, dnsNames)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	nextCert, nextKey, err := certs.IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}

	count := func(b *certs.Bundle) int {
		t.Helper()
		n := 0
		for rest := b.PublishedCA(); len(rest) > 0; {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			n++
		}
		return n
	}

	if got := count(atRest); got != 1 {
		t.Errorf("a bundle at rest publishes %d certificates, want 1", got)
	}
	distributing := atRest.WithNextCA(nextCert, nextKey)
	if got := count(distributing); got != 2 {
		t.Errorf("a distributing bundle publishes %d certificates, want 2 — "+
			"an agent that never sees the incoming CA cannot survive the switch", got)
	}
	switched, err := distributing.SwitchToNext(now, dnsNames)
	if err != nil {
		t.Fatalf("SwitchToNext: %v", err)
	}
	if got := count(switched); got != 2 {
		t.Errorf("a switched bundle publishes %d certificates, want 2 — "+
			"the outgoing CA stays trusted until drop-old", got)
	}
	if got := count(switched.WithoutRotation()); got != 1 {
		t.Errorf("after drop-old the bundle publishes %d certificates, want 1", got)
	}
}

// A rotation slot the agent could not parse is never published.
//
// PublishedCA's output travels Provider.Set -> Provider.CABundle ->
// Bootstrapper.CA -> the spawnery-ca ConfigMap of every namespace, and the
// consumer is OperatorChannel.trustManager, which parses the whole bundle with
// CertificateFactory.generateCertificates and throws on anything that is not a
// certificate. So a slot that does not parse does not cost a rotation; it
// costs every agent in every namespace its entire trust store. Only a
// hand-edited secret produces one, which is why this is a guard rather than a
// repair -- the repair is AdvanceRotation's, and it runs a tick later.
//
// The guard is here, at the one function whose output reaches an agent, rather
// than at a call site: a later path that publishes the bundle from somewhere
// else is exactly how this would come back.
func TestAnUnparseableSlotIsNotPublished(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	dnsNames := certs.ServingDNSNames("spawnery-operator", "spawnery-system")

	signing, err := certs.Issue(now, dnsNames)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	good, _, err := certs.IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}

	// Three shapes, because the agent throws on all of them and pem.Decode
	// alone only catches the first two: bytes that are not PEM at all, a PEM
	// envelope around something that is not a certificate, and a well-formed
	// certificate with stray bytes trailing it -- exactly what a hand-edit
	// that appends rather than replaces produces. parsableCert's contract is
	// "exactly one PEM block", not "at least one", precisely for this shape.
	notPEM := []byte("-- this is not a certificate --\n")
	pemButNotACert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("nonsense")})
	pemPlusTrailingJunk := slices.Concat(good, []byte("-- trailing, not PEM --\n"))

	for _, tc := range []struct {
		name string
		bad  []byte
	}{
		{"not PEM at all", notPEM},
		{"a PEM envelope around nonsense", pemButNotACert},
		{"a valid certificate followed by trailing bytes", pemPlusTrailingJunk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := &certs.Bundle{
				CACertPEM:      signing.CACertPEM,
				CAKeyPEM:       signing.CAKeyPEM,
				ServingCertPEM: signing.ServingCertPEM,
				ServingKeyPEM:  signing.ServingKeyPEM,
				NextCACertPEM:  tc.bad,
			}
			if got := next.PublishedCA(); !bytes.Equal(got, signing.CACertPEM) {
				t.Errorf("an unparseable ca-next was published; the bundle every agent "+
					"pins would have %d bytes of it, and trustManager throws on the whole "+
					"stream rather than skipping the bad block", len(tc.bad))
			}

			prev := &certs.Bundle{
				CACertPEM:         signing.CACertPEM,
				CAKeyPEM:          signing.CAKeyPEM,
				ServingCertPEM:    signing.ServingCertPEM,
				ServingKeyPEM:     signing.ServingKeyPEM,
				PreviousCACertPEM: tc.bad,
			}
			if got := prev.PublishedCA(); !bytes.Equal(got, signing.CACertPEM) {
				t.Error("an unparseable ca-previous was published")
			}
		})
	}

	// And the good case still publishes two, so the guard has not simply
	// disabled the feature.
	ok := &certs.Bundle{
		CACertPEM:      signing.CACertPEM,
		CAKeyPEM:       signing.CAKeyPEM,
		ServingCertPEM: signing.ServingCertPEM,
		ServingKeyPEM:  signing.ServingKeyPEM,
		NextCACertPEM:  good,
	}
	if got := ok.PublishedCA(); !bytes.Equal(got, slices.Concat(signing.CACertPEM, good)) {
		t.Error("a well-formed incoming CA was dropped from the published bundle")
	}

	// A bad Next does not suppress a good Previous. Next and Previous are
	// never both populated in a legitimate rotation -- they belong to
	// sequential phases, not concurrent ones -- so this shape only arises
	// from a corrupted bundle. But PublishedCA already documents a bad slot
	// as "treated as absent", and the switch's own case order falls through
	// from Next to Previous, so the safer reading -- the one that keeps
	// publishing a CA agents may still need for a rollback, rather than
	// silently narrowing to just the signing CA -- is that the fallback
	// still fires.
	badNextGoodPrevious := &certs.Bundle{
		CACertPEM:         signing.CACertPEM,
		CAKeyPEM:          signing.CAKeyPEM,
		ServingCertPEM:    signing.ServingCertPEM,
		ServingKeyPEM:     signing.ServingKeyPEM,
		NextCACertPEM:     notPEM,
		PreviousCACertPEM: good,
	}
	if got := badNextGoodPrevious.PublishedCA(); !bytes.Equal(got, slices.Concat(signing.CACertPEM, good)) {
		t.Error("a bad Next suppressed a well-formed Previous instead of falling back to it")
	}
}
