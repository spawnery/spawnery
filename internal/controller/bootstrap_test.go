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
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestEnsureCreatesConfigMapAndServiceAccount(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: testenv.RestrictedClient(t), Reader: testenv.RestrictedClient(t), CA: func() []byte { return []byte("PEM-A") }}

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
	b := &Bootstrapper{Client: testenv.RestrictedClient(t), Reader: testenv.RestrictedClient(t), CA: func() []byte { return []byte("PEM-A") }}

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
	b := &Bootstrapper{Client: testenv.RestrictedClient(t), Reader: testenv.RestrictedClient(t), CA: func() []byte { return []byte(ca) }}

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
	b := &Bootstrapper{Client: testenv.RestrictedClient(t), Reader: testenv.RestrictedClient(t), CA: func() []byte { return []byte("PEM-A") }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: ns}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	cm.Data[podspec.CAConfigMapKey] = "someone-tampered-with-this"
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
	b := &Bootstrapper{Client: testenv.RestrictedClient(t), Reader: testenv.RestrictedClient(t), CA: func() []byte { return []byte("PEM-A") }}

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

// restrictedCacheClient returns a cached client filtered exactly the way
// main.go filters the manager's cache: ConfigMaps and ServiceAccounts, and
// only those carrying the managed-by label. Everything else is invisible to
// it — which is the whole point, and the reason ensureConfigMap needs a repair
// path at all.
func restrictedCacheClient(t *testing.T, ctx context.Context) client.Client {
	t.Helper()
	managed := labels.SelectorFromSet(labels.Set{podspec.LabelManagedBy: podspec.ManagedByValue})
	mgr, err := ctrl.NewManager(testenv.Config(t), manager.Options{
		Scheme:         testenv.Scheme(t),
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.ConfigMap{}:      {Label: managed},
			&corev1.ServiceAccount{}: {Label: managed},
		}},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	mgrCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("the cache manager did not stop within 20s")
		}
	})

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("the restricted cache never synced")
	}
	return mgr.GetClient()
}

// The repair path in ensureConfigMap only exists because the manager's cache
// is filtered on the managed-by label: strip the label and the cached client
// stops seeing the object, so CreateOrUpdate's Get misses, its Create comes
// back AlreadyExists, and only a read through the uncached Reader can recover.
// Every other test in this file wires Client and Reader to the same unfiltered
// client, where that branch is unreachable. This one uses a real restricted
// cache, so the branch is the only way through.
func TestEnsureRepairsAConfigMapThatFellOutOfTheCache(t *testing.T) {
	direct, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, direct)
	cached := restrictedCacheClient(t, ctx)

	b := &Bootstrapper{Client: cached, Reader: direct, CA: func() []byte { return []byte("PEM-A") }}
	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	key := types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: ns}
	cm := &corev1.ConfigMap{}
	if err := direct.Get(ctx, key, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	delete(cm.Labels, podspec.LabelManagedBy)
	cm.Data[podspec.CAConfigMapKey] = "someone-tampered-with-this"
	if err := direct.Update(ctx, cm); err != nil {
		t.Fatalf("update ConfigMap: %v", err)
	}

	// The watch delivers the label removal asynchronously. Without waiting for
	// it the cached Get could still hit the old, labelled copy, and the test
	// would quietly exercise the ordinary update path instead of the repair.
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := cached.Get(ctx, key, &corev1.ConfigMap{})
		if apierrors.IsNotFound(err) {
			break
		}
		if err != nil {
			t.Fatalf("cached get: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the unlabelled ConfigMap never left the restricted cache")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure over a ConfigMap the cache cannot see: %v", err)
	}

	if err := direct.Get(ctx, key, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if cm.Data[podspec.CAConfigMapKey] != "PEM-A" {
		t.Errorf("ca.crt = %q, want the restored PEM-A", cm.Data[podspec.CAConfigMapKey])
	}
	if cm.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		t.Error("the label was not restored; the object would stay outside the cache forever")
	}
}

// The ServiceAccount half of the same situation, and the deliberate asymmetry:
// there is no repair, because restoring the label would need an update verb on
// every ServiceAccount in the cluster. Ensure has to succeed anyway — the pod
// only needs the account to exist under its name.
func TestEnsureToleratesAServiceAccountThatFellOutOfTheCache(t *testing.T) {
	direct, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, direct)
	cached := restrictedCacheClient(t, ctx)

	b := &Bootstrapper{Client: cached, Reader: direct, CA: func() []byte { return []byte("PEM-A") }}
	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	key := types.NamespacedName{Name: podspec.ServerServiceAccountName, Namespace: ns}
	sa := &corev1.ServiceAccount{}
	if err := direct.Get(ctx, key, sa); err != nil {
		t.Fatalf("get ServiceAccount: %v", err)
	}
	delete(sa.Labels, podspec.LabelManagedBy)
	if err := direct.Update(ctx, sa); err != nil {
		t.Fatalf("update ServiceAccount: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		err := cached.Get(ctx, key, &corev1.ServiceAccount{})
		if apierrors.IsNotFound(err) {
			break
		}
		if err != nil {
			t.Fatalf("cached get: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the unlabelled ServiceAccount never left the restricted cache")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The cached Get misses, the Create comes back AlreadyExists, and that is
	// swallowed on purpose: one wasted call per pod creation in a namespace
	// someone edited by hand, instead of a clusterwide write permission.
	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure over a ServiceAccount the cache cannot see: %v", err)
	}
	if err := direct.Get(ctx, key, sa); err != nil {
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
	b := &Bootstrapper{Client: testenv.RestrictedClient(t), Reader: testenv.RestrictedClient(t), CA: func() []byte { return nil }}

	if err := b.Ensure(ctx, ns); err == nil {
		t.Error("Ensure wrote an empty CA bundle")
	}
}

// Without this a proxy pod would have no identity to present at all: the token
// projection names a ServiceAccount, and the kubelet cannot mint a token for
// one that does not exist — the pod fails before it reaches the first TLS
// handshake, with an error about a volume rather than about credentials.
func TestEnsureCreatesBothServiceAccounts(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: testenv.RestrictedClient(t), Reader: testenv.RestrictedClient(t), CA: func() []byte { return []byte("PEM-A") }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	for _, name := range []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName} {
		sa := &corev1.ServiceAccount{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sa); err != nil {
			t.Errorf("get ServiceAccount %s: %v", name, err)
			continue
		}
		if sa.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
			t.Errorf("ServiceAccount %s is unlabelled", name)
		}
	}
}

// The no-write guarantee has to hold for both. It is what lets the operator
// keep get;list;watch;create on serviceaccounts and no update verb at all.
func TestEnsureLeavesExistingServiceAccountsAlone(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: testenv.RestrictedClient(t), Reader: testenv.RestrictedClient(t), CA: func() []byte { return []byte("PEM-A") }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}

	before := map[string]string{}
	for _, name := range []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName} {
		sa := &corev1.ServiceAccount{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sa); err != nil {
			t.Fatalf("get ServiceAccount %s: %v", name, err)
		}
		before[name] = sa.ResourceVersion
	}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	for name, was := range before {
		sa := &corev1.ServiceAccount{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sa); err != nil {
			t.Fatalf("get ServiceAccount %s: %v", name, err)
		}
		if sa.ResourceVersion != was {
			t.Errorf("ServiceAccount %s was written on the second Ensure — the update verb is not granted", name)
		}
	}
}
