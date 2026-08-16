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

package v1alpha1_test

import (
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = testenv.Stop()
	os.Exit(code)
}

func TestNetworkRoundTrip(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	net := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
			Defaults: &spawneryv1alpha1.Defaults{
				MinecraftVersion: "1.21.4",
			},
		},
	}
	if err := c.Create(ctx, net); err != nil {
		t.Fatalf("create Network: %v", err)
	}

	got := &spawneryv1alpha1.Network{}
	if err := c.Get(ctx, types.NamespacedName{Name: "production", Namespace: ns}, got); err != nil {
		t.Fatalf("get Network: %v", err)
	}
	if got.Spec.ForwardingSecretRef.Name != "velocity-forwarding-secret" {
		t.Errorf("forwardingSecretRef = %q, want velocity-forwarding-secret", got.Spec.ForwardingSecretRef.Name)
	}
	if got.Spec.Defaults.MinecraftVersion != "1.21.4" {
		t.Errorf("minecraftVersion = %q, want 1.21.4", got.Spec.Defaults.MinecraftVersion)
	}
}

func TestNetworkRequiresForwardingSecretRef(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	net := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: ns},
	}
	if err := c.Create(ctx, net); err == nil {
		t.Fatal("create without forwardingSecretRef succeeded, want rejection")
	}
}

// The status field has to survive a round trip through a real API server, not
// only through the Go type: a field missing from the generated CRD schema is
// pruned on write and the operator would re-detect the same rotation forever.
func TestNetworkStatusCarriesTheForwardingSecretHash(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	net := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "fwd-hash", Namespace: ns},
		Spec:       spawneryv1alpha1.NetworkSpec{ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "fwd"}},
	}
	if err := c.Create(ctx, net); err != nil {
		t.Fatalf("create network: %v", err)
	}

	net.Status.ForwardingSecretHash = "0123456789abcdef"
	if err := c.Status().Update(ctx, net); err != nil {
		t.Fatalf("update status: %v", err)
	}

	got := &spawneryv1alpha1.Network{}
	if err := c.Get(ctx, types.NamespacedName{Name: "fwd-hash", Namespace: ns}, got); err != nil {
		t.Fatalf("get network: %v", err)
	}
	if got.Status.ForwardingSecretHash != "0123456789abcdef" {
		t.Errorf("status.forwardingSecretHash = %q, want %q — the field is missing from the generated CRD schema",
			got.Status.ForwardingSecretHash, "0123456789abcdef")
	}
}

// The recorded digest reaches a pod label unread (internal/podspec/server.go,
// internal/podspec/proxy.go), and a label value the API server rejects fails
// every pod Create for the network. The schema is where that is stopped, so
// the check belongs against a real API server rather than against the Go type.
func TestNetworkStatusRejectsAMalformedForwardingSecretHash(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	for _, tc := range []struct {
		name       string
		hash       string
		wantReject bool
	}{
		{name: "too long", hash: "0123456789abcdef0", wantReject: true},
		{name: "illegal in a label value", hash: "not a label value", wantReject: true},
		{name: "uppercase is not what ForwardingHash emits", hash: "0123456789ABCDEF", wantReject: true},
		{name: "a digest", hash: "0123456789abcdef"},
		// The operator's own writes are covered by omitempty, but another
		// client may send an explicit empty string, and clearing the field is
		// not what the pattern is there to stop.
		{name: "explicitly empty", hash: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			net := &spawneryv1alpha1.Network{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "fwd-hash-", Namespace: ns},
				Spec:       spawneryv1alpha1.NetworkSpec{ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "fwd"}},
			}
			if err := c.Create(ctx, net); err != nil {
				t.Fatalf("create network: %v", err)
			}

			net.Status.ForwardingSecretHash = tc.hash
			err := c.Status().Update(ctx, net)
			switch {
			case tc.wantReject && err == nil:
				t.Errorf("the API server accepted status.forwardingSecretHash = %q; it reaches a pod label "+
					"unvalidated, so every pod Create for this network would fail 422", tc.hash)
			case !tc.wantReject && err != nil:
				t.Errorf("the API server rejected status.forwardingSecretHash = %q: %v", tc.hash, err)
			}
		})
	}
}
