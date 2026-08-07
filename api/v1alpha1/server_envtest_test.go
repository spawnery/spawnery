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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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
