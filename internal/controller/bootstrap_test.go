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

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestEnsureCreatesConfigMapAndServiceAccount(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return []byte("PEM-A") }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: ns}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if cm.Data[podspec.CAConfigMapKey] != "PEM-A" {
		t.Errorf("ca.crt = %q, want PEM-A", cm.Data[podspec.CAConfigMapKey])
	}
	if cm.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		t.Error("the ConfigMap is unlabelled and would fall out of the restricted cache")
	}

	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.ServerServiceAccountName, Namespace: ns}, sa); err != nil {
		t.Fatalf("get ServiceAccount: %v", err)
	}
	if sa.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		t.Error("the ServiceAccount is unlabelled")
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return []byte("PEM-A") }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
}

// A rotation has to reach every namespace, or agents in the ones left behind
// would stop trusting the operator.
func TestEnsureUpdatesAChangedCA(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	ca := "PEM-A"
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return []byte(ca) }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	ca = "PEM-B"
	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure after rotation: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: ns}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if cm.Data[podspec.CAConfigMapKey] != "PEM-B" {
		t.Errorf("ca.crt = %q, want the rotated PEM-B", cm.Data[podspec.CAConfigMapKey])
	}
}

func TestEnsureRepairsAHandEditedConfigMap(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return []byte("PEM-A") }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: ns}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	cm.Data[podspec.CAConfigMapKey] = "jemand-hat-daran-gedreht"
	delete(cm.Labels, podspec.LabelManagedBy)
	if err := c.Update(ctx, cm); err != nil {
		t.Fatalf("update ConfigMap: %v", err)
	}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure over the edited ConfigMap: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: ns}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if cm.Data[podspec.CAConfigMapKey] != "PEM-A" {
		t.Errorf("ca.crt = %q, want the restored PEM-A", cm.Data[podspec.CAConfigMapKey])
	}
	if cm.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		t.Error("the label was not restored; the object would stay outside the cache")
	}
}

// Unlike the ConfigMap, a hand-edited ServiceAccount is deliberately NOT
// repaired: restoring the label would need Client.Update, and the
// serviceaccounts RBAC marker grants no update verb on purpose — a
// clusterwide write to every ServiceAccount's secrets is too big a grant for
// a cosmetic label. Ensure must still succeed; the pod only needs the
// ServiceAccount to exist under its name, not to carry the label. This
// asserts no write happened at all: the label must still be gone afterwards,
// not merely that Ensure tolerated either outcome.
func TestEnsureLeavesAnUnlabelledServiceAccountAlone(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return []byte("PEM-A") }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.ServerServiceAccountName, Namespace: ns}, sa); err != nil {
		t.Fatalf("get ServiceAccount: %v", err)
	}
	delete(sa.Labels, podspec.LabelManagedBy)
	if err := c.Update(ctx, sa); err != nil {
		t.Fatalf("update ServiceAccount: %v", err)
	}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure over the unlabelled ServiceAccount: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.ServerServiceAccountName, Namespace: ns}, sa); err != nil {
		t.Fatalf("get ServiceAccount: %v", err)
	}
	if _, ok := sa.Labels[podspec.LabelManagedBy]; ok {
		t.Error("Ensure wrote the label back; ensureServiceAccount must never Update")
	}
}

// Ensure must not run before the provider has a CA — an empty ca.crt would be
// worse than none, because the pod would start and fail the handshake.
func TestEnsureRefusesAnEmptyCA(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return nil }}

	if err := b.Ensure(ctx, ns); err == nil {
		t.Error("Ensure wrote an empty CA bundle")
	}
}
