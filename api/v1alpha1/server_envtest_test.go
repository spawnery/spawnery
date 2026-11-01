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

package v1alpha1_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestServerStatusIsASubresource(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-x7k2", Namespace: ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef:        spawneryv1alpha1.ObjectRef{Name: "lobby"},
			GroupGeneration: 7,
		},
		Status: spawneryv1alpha1.ServerStatus{Phase: "Ready"},
	}
	if err := c.Create(ctx, srv); err != nil {
		t.Fatalf("create Server: %v", err)
	}

	got := &spawneryv1alpha1.Server{}
	if err := c.Get(ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: ns}, got); err != nil {
		t.Fatalf("get Server: %v", err)
	}
	if got.Status.Phase != "" {
		t.Errorf("status survived create, want it dropped: %q", got.Status.Phase)
	}

	got.Status.Phase = "Starting"
	got.Status.Players = 3
	got.Status.Slots = 100
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatalf("status update: %v", err)
	}

	again := &spawneryv1alpha1.Server{}
	if err := c.Get(ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: ns}, again); err != nil {
		t.Fatalf("get after status update: %v", err)
	}
	if again.Status.Phase != "Starting" || again.Status.Players != 3 {
		t.Errorf("status = %+v, want phase Starting and 3 players", again.Status)
	}
}

func TestServerRequiresGroupRef(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: ns},
	}
	if err := c.Create(ctx, srv); err == nil {
		t.Fatal("create without groupRef succeeded, want rejection")
	}
}

func TestServerCarriesForceStop(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	if err := c.Create(ctx, &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-x7k2", Namespace: ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef:  spawneryv1alpha1.ObjectRef{Name: "lobby"},
			ForceStop: true,
		},
	}); err != nil {
		t.Fatalf("create Server: %v", err)
	}
	got := &spawneryv1alpha1.Server{}
	if err := c.Get(ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: ns}, got); err != nil {
		t.Fatalf("get Server: %v", err)
	}
	if !got.Spec.ForceStop {
		t.Error("spec.forceStop did not survive a write and a read")
	}
}

func TestServerResourcesAndHeapAreImmutable(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	key := types.NamespacedName{Name: "worlds-abc", Namespace: ns}
	if err := c.Create(ctx, &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "worlds"},
			Resources: &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
			},
			Heap: ptr.To(resource.MustParse("2Gi")),
		},
	}); err != nil {
		t.Fatalf("create Server: %v", err)
	}

	mutations := map[string]struct {
		change  func(*spawneryv1alpha1.Server)
		wantErr string
	}{
		"resources changed": {func(s *spawneryv1alpha1.Server) {
			s.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("8Gi")
		}, "spec.resources is immutable"},
		"resources removed": {func(s *spawneryv1alpha1.Server) { s.Spec.Resources = nil }, "spec.resources is immutable"},
		"heap changed": {func(s *spawneryv1alpha1.Server) {
			s.Spec.Heap = ptr.To(resource.MustParse("3Gi"))
		}, "spec.heap is immutable"},
		"heap removed": {func(s *spawneryv1alpha1.Server) { s.Spec.Heap = nil }, "spec.heap is immutable"},
	}
	for name, m := range mutations {
		t.Run(name, func(t *testing.T) {
			s := &spawneryv1alpha1.Server{}
			if err := c.Get(ctx, key, s); err != nil {
				t.Fatalf("get: %v", err)
			}
			m.change(s)
			err := c.Update(ctx, s)
			if err == nil || !strings.Contains(err.Error(), m.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, m.wantErr)
			}
		})
	}

	s := &spawneryv1alpha1.Server{}
	if err := c.Get(ctx, key, s); err != nil {
		t.Fatalf("get: %v", err)
	}
	s.Spec.Retire = true
	if err := c.Update(ctx, s); err != nil {
		t.Fatalf("updating spec.retire next to resources and heap was refused: %v", err)
	}
}

func TestServerWithoutResourcesCannotGainThem(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	key := types.NamespacedName{Name: "hub-abc", Namespace: ns}
	if err := c.Create(ctx, &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "hub"}},
	}); err != nil {
		t.Fatalf("create Server: %v", err)
	}
	s := &spawneryv1alpha1.Server{}
	if err := c.Get(ctx, key, s); err != nil {
		t.Fatalf("get: %v", err)
	}
	s.Spec.Heap = ptr.To(resource.MustParse("1Gi"))
	if err := c.Update(ctx, s); err == nil || !strings.Contains(err.Error(), "spec.heap is immutable") {
		t.Fatalf("adding spec.heap: err = %v, want 'spec.heap is immutable'", err)
	}
	s.Spec.Heap = nil
	s.Spec.Resources = &corev1.ResourceRequirements{}
	if err := c.Update(ctx, s); err == nil || !strings.Contains(err.Error(), "spec.resources is immutable") {
		t.Fatalf("adding spec.resources: err = %v, want 'spec.resources is immutable'", err)
	}
}
