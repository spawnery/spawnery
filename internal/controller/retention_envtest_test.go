/*
Copyright paul_wtf.

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
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
	"github.com/spawnery/spawnery/internal/worldsync"
)

type recordingRetention struct {
	mu     sync.Mutex
	synced map[string]worldsync.Retention
}

func (r *recordingRetention) Sync(_ context.Context, namespace, group string, p worldsync.Retention) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.synced == nil {
		r.synced = map[string]worldsync.Retention{}
	}
	r.synced[namespace+"/"+group] = p
	return nil
}

func (r *recordingRetention) get(key string) (worldsync.Retention, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.synced[key]
	return p, ok
}

func reconcileGroupNamed(t *testing.T, f *fixture, r *ServerGroupReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: f.ns, Name: name}}); err != nil {
		t.Fatalf("reconcile group %s: %v", name, err)
	}
}

func TestTheGroupsRetentionIsPublishedAndWithdrawn(t *testing.T) {
	f := newFixture(t)
	published := &recordingRetention{}
	r := groupReconciler(f)
	r.Retention = published
	key := f.ns + "/worlds"

	g := f.createObjectStoreGroup(t, "worlds")
	// The reconciler writes the status, so every edit starts from a fresh read.
	setRetention := func(policy *spawneryv1alpha1.RetentionSpec) {
		t.Helper()
		if err := f.c.Get(f.ctx, types.NamespacedName{Namespace: f.ns, Name: g.Name}, g); err != nil {
			t.Fatal(err)
		}
		g.Spec.Storage.Retention = policy
		if err := f.c.Update(f.ctx, g); err != nil {
			t.Fatal(err)
		}
	}

	setRetention(&spawneryv1alpha1.RetentionSpec{Last: 12, Daily: 7})
	reconcileGroupNamed(t, f, r, g.Name)
	if got, ok := published.get(key); !ok || got != (worldsync.Retention{Last: 12, Daily: 7}) {
		t.Fatalf("published %+v (%v), want last 12, daily 7", got, ok)
	}

	setRetention(nil)
	reconcileGroupNamed(t, f, r, g.Name)
	if got, ok := published.get(key); !ok || got != (worldsync.Retention{}) {
		t.Fatalf("published %+v (%v) after retention was dropped, want the zero policy", got, ok)
	}

	setRetention(&spawneryv1alpha1.RetentionSpec{Last: 3})
	reconcileGroupNamed(t, f, r, g.Name)
	if got, ok := published.get(key); !ok || got != (worldsync.Retention{Last: 3}) {
		t.Fatalf("published %+v (%v), want last 3", got, ok)
	}
	if err := f.c.Delete(f.ctx, g); err != nil {
		t.Fatal(err)
	}
	reconcileGroupNamed(t, f, r, g.Name)
	if got, ok := published.get(key); !ok || got != (worldsync.Retention{}) {
		t.Fatalf("published %+v (%v) after the group was deleted, want the zero policy", got, ok)
	}
}

func TestAGroupOnClaimsPublishesNoRetention(t *testing.T) {
	f := newFixture(t)
	published := &recordingRetention{}
	r := groupReconciler(f)
	r.Retention = published
	g := f.createOnDemandGroup(t, "claims", 5)
	reconcileGroupNamed(t, f, r, g.Name)
	if got, ok := published.get(f.ns + "/claims"); !ok || got != (worldsync.Retention{}) {
		t.Fatalf("published %+v (%v), want the zero policy", got, ok)
	}
}

func TestNewServerGroupReconcilerCarriesTheRetentionPublisher(t *testing.T) {
	mgr, err := ctrl.NewManager(testenv.Config(t), manager.Options{
		Scheme:         testenv.Scheme(t),
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	published := &recordingRetention{}
	got := newServerGroupReconciler(mgr, Options{Clock: time.Now, Retention: published}).Retention
	if got != RetentionPublisher(published) {
		t.Fatalf("Retention = %v; the operator's policy writer never reaches the reconciler", got)
	}
}
