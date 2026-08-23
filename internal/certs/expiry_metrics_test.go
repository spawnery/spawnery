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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestPublishingABundleReportsBothExpiries goes through Provider.Set rather
// than calling observeExpiry directly, and that is the whole point of it. A
// gauge that is correct but never set is the failure this file exists to
// prevent: the two rotation gauges beside it were reachable by nothing for
// months because the chart's Service exposed no metrics port, and a gauge the
// operator declares but never writes is the same defect one layer in.
func TestPublishingABundleReportsBothExpiries(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	bundle, err := Issue(now, []string{"spawnery-operator.spawnery-system.svc"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if err := NewProvider(nil).Set(bundle); err != nil {
		t.Fatalf("Set: %v", err)
	}

	caCert, _, err := bundle.parseCA()
	if err != nil {
		t.Fatalf("parseCA: %v", err)
	}
	servingCert, err := bundle.parseServing()
	if err != nil {
		t.Fatalf("parseServing: %v", err)
	}

	if got, want := testutil.ToFloat64(CAExpiry), float64(caCert.NotAfter.Unix()); got != want {
		t.Errorf("spawnery_ca_expiry_timestamp_seconds = %v, want %v — nothing else in the "+
			"operator says how much of the CA's life is left, so this gauge is the only "+
			"warning a rotation is ever going to get", got, want)
	}
	if got, want := testutil.ToFloat64(ServingCertExpiry), float64(servingCert.NotAfter.Unix()); got != want {
		t.Errorf("spawnery_serving_cert_expiry_timestamp_seconds = %v, want %v", got, want)
	}

	// The two certificates must not report the same instant, or a mutation
	// swapping one for the other would leave both assertions above green.
	// CALifetime is ten years and ServingLifetime is far shorter, so this
	// holds by construction rather than by luck.
	if caCert.NotAfter.Equal(servingCert.NotAfter) {
		t.Fatal("the CA and the serving certificate expire at the same instant, so the two " +
			"assertions above cannot tell the gauges apart")
	}
}

// TestABundleThatWillNotParseLeavesTheGaugesAlone pins the deliberate choice
// not to zero a gauge on a parse failure. Zero is a timestamp in 1970, and an
// alert written the obvious way -- expiry minus now -- would read it as fifty
// years expired and wake somebody for a bundle that is fine.
func TestABundleThatWillNotParseLeavesTheGaugesAlone(t *testing.T) {
	good, err := Issue(time.Now(), []string{"spawnery-operator.spawnery-system.svc"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	observeExpiry(good)
	before := testutil.ToFloat64(CAExpiry)
	if before == 0 {
		t.Fatal("the gauge is zero before the unparseable bundle, so this test cannot show it surviving one")
	}

	observeExpiry(&Bundle{CACertPEM: []byte("not a certificate"), CAKeyPEM: []byte("nor this")})

	if got := testutil.ToFloat64(CAExpiry); got != before {
		t.Errorf("spawnery_ca_expiry_timestamp_seconds = %v after an unparseable bundle, want it "+
			"left at %v: a stale timestamp is the honest reading, a zero one is a page at "+
			"four in the morning", got, before)
	}
}
