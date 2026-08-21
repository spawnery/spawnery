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

// namespacesMissingCA is unexported -- Task 4 calls it from within this
// package -- so this file is white-box (package certs, like bundle_test.go),
// not certs_test like the rest of the envtest suite: an external test
// package cannot reference an unexported method at all, regardless of what
// it does at runtime.
package certs

import (
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

// The gate is driven from the Network objects, not from the ConfigMaps.
//
// "A Network owns its namespace" is the one-per-namespace rule
// (pickNamespaceOwner), not a Kubernetes OwnerReference -- the operator never
// creates a namespace and never owns one -- and the CA ConfigMap deliberately
// carries no owner reference so that it outlives the operator. So a
// spawnery-ca ConfigMap whose Network was deleted stays in its namespace
// forever with whatever bundle it last received. A gate phrased as "every
// managed CA ConfigMap" would wait on that dead namespace until somebody
// cleaned it up by hand, which is to say: a rotation would never complete on
// any cluster where a Network had ever been deleted.
func TestTheGateIsDrivenFromNetworksNotConfigMaps(t *testing.T) {
	c, ctx := testenv.Client(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	target, _, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA (target): %v", err)
	}
	unrelated, _, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA (unrelated): %v", err)
	}
	stale, _, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA (stale): %v", err)
	}

	network := func(ns string) {
		t.Helper()
		n := &spawneryv1alpha1.Network{
			ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: ns},
			Spec: spawneryv1alpha1.NetworkSpec{
				ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
			},
		}
		if err := c.Create(ctx, n); err != nil {
			t.Fatalf("create Network in %s: %v", ns, err)
		}
	}
	configMap := func(ns string, caPEM ...[]byte) {
		t.Helper()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podspec.CAConfigMapName,
				Namespace: ns,
				Labels:    map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue},
			},
			Data: map[string]string{
				podspec.CAConfigMapKey: string(slices.Concat(caPEM...)),
			},
		}
		if err := c.Create(ctx, cm); err != nil {
			t.Fatalf("create ConfigMap in %s: %v", ns, err)
		}
	}

	// Has the target CA already: not missing.
	hasTarget := testenv.Namespace(t, ctx, c)
	network(hasTarget)
	configMap(hasTarget, target, unrelated)

	// Has a Network but not the target CA: missing.
	lacksTarget := testenv.Namespace(t, ctx, c)
	network(lacksTarget)
	configMap(lacksTarget, unrelated)

	// Has a stale ConfigMap but no Network -- the deleted-network case. Must
	// NOT appear in the result even though its ConfigMap lacks the target.
	orphaned := testenv.Namespace(t, ctx, c)
	configMap(orphaned, stale)

	// Has a Network but the bootstrapper has not written spawnery-ca there
	// yet: absent counts as missing, not as an error. If this branch were
	// mishandled as an error, namespacesMissingCA would return early with a
	// nil slice and no error for the whole call -- indistinguishable from
	// "everything is caught up" -- which is the one outcome a read failure
	// must never produce.
	noConfigMapYet := testenv.Namespace(t, ctx, c)
	network(noConfigMapYet)

	s := &Store{Client: c}
	got, err := s.namespacesMissingCA(ctx, target)
	if err != nil {
		t.Fatalf("namespacesMissingCA: %v", err)
	}
	want := []string{lacksTarget, noConfigMapYet}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("namespacesMissingCA = %v, want %v (the orphaned namespace %q must be excluded)",
			got, want, orphaned)
	}
}
