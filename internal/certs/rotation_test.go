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
