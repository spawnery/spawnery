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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

func ephemeralGroup(ns, name string) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling: &spawneryv1alpha1.ScalingSpec{
				MinReplicas: 1,
				MaxReplicas: 10,
				SpareSlots:  40,
			},
		},
	}
}

func persistentGroup(ns, name string) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupPersistent,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 40,
			Replicas:   ptr.To[int32](1),
			Storage: &spawneryv1alpha1.StorageSpec{
				Size:             resource.MustParse("20Gi"),
				StorageClassName: ptr.To("longhorn"),
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		},
	}
}

func persistentGroupNoStorageClass(ns, name string) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupPersistent,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 40,
			Replicas:   ptr.To[int32](1),
			Storage: &spawneryv1alpha1.StorageSpec{
				Size:        resource.MustParse("20Gi"),
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		},
	}
}

func TestServerGroupEphemeralAccepted(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	if err := c.Create(ctx, ephemeralGroup(ns, "lobby")); err != nil {
		t.Fatalf("create ephemeral group: %v", err)
	}
}

func TestServerGroupPersistentAccepted(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	if err := c.Create(ctx, persistentGroup(ns, "survival")); err != nil {
		t.Fatalf("create persistent group: %v", err)
	}
}

func TestServerGroupCELRejections(t *testing.T) {
	c, ctx := testenv.Client(t)

	cases := []struct {
		name   string
		mutate func(*spawneryv1alpha1.ServerGroup)
		base   func(ns, name string) *spawneryv1alpha1.ServerGroup
	}{
		{
			name: "ephemeral with storage",
			base: ephemeralGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Storage = &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
			},
		},
		{
			name: "ephemeral with replicas",
			base: ephemeralGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Replicas = ptr.To[int32](3)
			},
		},
		{
			name: "persistent with scaling",
			base: persistentGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Scaling = &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10}
			},
		},
		{
			name: "persistent with update",
			base: persistentGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Update = &spawneryv1alpha1.UpdateSpec{MaxUnavailable: 1}
			},
		},
		{
			name: "persistent without storage",
			base: persistentGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Storage = nil
			},
		},
		{
			// The one CRD change this milestone makes. Without the rule such a
			// group is accepted and runs zero servers for good:
			// DesiredReplicas() returns 0 for a nil spec.replicas,
			// DecidePersistentSize is asked for no ordinals, and derivePhase
			// reports Pending against a group nobody asked anything of.
			name: "persistent without replicas",
			base: persistentGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Replicas = nil
			},
		},
		{
			name: "ephemeral without scaling",
			base: ephemeralGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Scaling = nil
			},
		},
		{
			name: "scaling minReplicas exceeds maxReplicas",
			base: ephemeralGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Scaling = &spawneryv1alpha1.ScalingSpec{MinReplicas: 5, MaxReplicas: 2}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := testenv.Namespace(t, ctx, c)
			g := tc.base(ns, "group")
			tc.mutate(g)
			if err := c.Create(ctx, g); err == nil {
				t.Fatalf("create succeeded, want CEL rejection")
			}
		})
	}
}

func TestServerGroupImmutableFields(t *testing.T) {
	c, ctx := testenv.Client(t)

	t.Run("type is immutable", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := ephemeralGroup(ns, "lobby")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Type = spawneryv1alpha1.ServerGroupPersistent
		g.Spec.Scaling = nil
		g.Spec.Storage = &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
		if err := c.Update(ctx, g); err == nil {
			t.Fatal("update changed spec.type, want rejection")
		}
	})

	t.Run("storageClassName is immutable", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := persistentGroup(ns, "survival")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Storage.StorageClassName = ptr.To("ceph")
		if err := c.Update(ctx, g); err == nil {
			t.Fatal("update changed storageClassName, want rejection")
		}
	})

	t.Run("storage size may grow", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := persistentGroup(ns, "survival")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Storage.Size = resource.MustParse("30Gi")
		if err := c.Update(ctx, g); err != nil {
			t.Fatalf("growing storage.size rejected: %v", err)
		}
	})

	t.Run("storage size may not shrink", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := persistentGroup(ns, "survival")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Storage.Size = resource.MustParse("10Gi")
		if err := c.Update(ctx, g); err == nil {
			t.Fatal("update shrank storage.size, want rejection")
		}
	})

	t.Run("accessModes is immutable", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := persistentGroup(ns, "survival")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Storage.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
		if err := c.Update(ctx, g); err == nil {
			t.Fatal("update changed accessModes, want rejection")
		}
	})

	t.Run("storageClassName unset may still grow storage.size", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := persistentGroupNoStorageClass(ns, "survival")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Storage.Size = resource.MustParse("30Gi")
		if err := c.Update(ctx, g); err != nil {
			t.Fatalf("growing storage.size rejected: %v", err)
		}
	})

	t.Run("storageClassName may not be set after creation without one", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := persistentGroupNoStorageClass(ns, "survival")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Storage.StorageClassName = ptr.To("longhorn")
		if err := c.Update(ctx, g); err == nil {
			t.Fatal("update set storageClassName, want rejection")
		}
	})
}
