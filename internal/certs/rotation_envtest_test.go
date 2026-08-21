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
	"context"
	"encoding/pem"
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
		// testenv runs one apiserver+etcd per test binary with no
		// kube-controller-manager, so nothing ever garbage-collects a
		// Namespace (it would sit in Terminating forever) or the objects in
		// it. A Network left behind here stays visible, cluster-wide, to
		// every later test in this package that calls namespacesMissingCA
		// -- including Task 4's, which each issue their own fresh target CA
		// that this leftover namespace would never be seen holding. Deleting
		// the object (as opposed to the namespace) works fine against a bare
		// apiserver, and Network carries no finalizer (only ServerFinalizer
		// exists), so this completes immediately.
		//
		// Registered here, after testenv.Client's own t.Cleanup(cancel): Go
		// runs cleanups LIFO, so this one fires first, while ctx is still
		// live -- confirmed with a standalone experiment reproducing that
		// registration order before relying on it.
		t.Cleanup(func() {
			if err := c.Delete(ctx, n); err != nil {
				t.Errorf("cleanup: delete Network in %s: %v", ns, err)
			}
		})
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

	// Has the target CA already: not missing. Written re-encoded -- same DER,
	// different bytes (an added header) -- so this namespace only comes out
	// "not missing" if the comparison actually decodes PEM and hashes the
	// DER. A plain bytes.Contains(data, target) substring check would not
	// find the literal target bytes in here and would misreport this
	// namespace as missing -- confirmed by temporarily mutating
	// namespaceHasCA to do exactly that (see task-3-report.md).
	hasTarget := testenv.Namespace(t, ctx, c)
	network(hasTarget)
	configMap(hasTarget, reencodePEM(t, target), unrelated)

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

	// Belt to the t.Cleanup's braces: the cleanup above is what keeps this
	// namespace out of some *other* test's result (and out of the annotation
	// production code writes), but this call itself can still observe a
	// still-running or previously-failed-to-clean-up test's namespaces
	// because the control plane and its Networks are shared package-wide.
	// Filtering to this test's own namespaces keeps the assertion below
	// meaningful regardless of what else exists in the cluster; it is not a
	// substitute for the cleanup, which is what stops the leak in the first
	// place.
	own := map[string]bool{hasTarget: true, lacksTarget: true, orphaned: true, noConfigMapYet: true}
	got = slices.DeleteFunc(slices.Clone(got), func(ns string) bool { return !own[ns] })

	want := []string{lacksTarget, noConfigMapYet}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("namespacesMissingCA = %v, want %v (the orphaned namespace %q must be excluded)",
			got, want, orphaned)
	}
}

// reencodePEM decodes a certificate and re-encodes it with an added header,
// so the returned bytes differ from the input while the DER -- and so the
// SHA-256 this package actually compares -- stays identical. Exists to make
// TestTheGateIsDrivenFromNetworksNotConfigMaps catch a comparison that
// matches on PEM bytes instead of on the certificate.
func reencodePEM(t *testing.T, certPEM []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("reencodePEM: not PEM")
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:    block.Type,
		Headers: map[string]string{"X-Reencoded-By": "rotation_envtest_test.go"},
		Bytes:   block.Bytes,
	})
}

// A namespace whose ConfigMap cannot be read at all -- as opposed to one that
// is simply absent -- must come back as an error, not silently as "caught
// up". A nil slice with a nil error is indistinguishable from a rotation
// that is actually clear to proceed, and this is the one outcome
// namespacesMissingCA must never produce.
//
// This needs a client that can be told to fail a Get with something other
// than NotFound, which envtest's real apiserver has no simple way to do
// (that would need a caller with restricted RBAC). A fake client wrapped in
// an interceptor does it directly: intercept Get only for ConfigMap, return
// Forbidden, and leave the Network List path alone.
func TestTheGatePropagatesAnUnreadableConfigMapAsAnError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	net := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: "broken"},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
		},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(net).Build()
	broken := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				return apierrors.NewForbidden(corev1.Resource("configmaps"), key.Name, fmt.Errorf("no rbac"))
			}
			return inner.Get(ctx, key, obj, opts...)
		},
	})

	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	target, _, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}

	s := &Store{Client: broken}
	got, err := s.namespacesMissingCA(context.Background(), target)
	if err == nil {
		t.Fatalf("namespacesMissingCA returned (%v, nil); a Get failure that is not NotFound must surface as an error", got)
	}
	if got != nil {
		t.Errorf("namespacesMissingCA returned a non-nil result (%v) alongside an error", got)
	}
}
