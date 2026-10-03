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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

func onDemandGroup(ns, name string) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:   spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:         spawneryv1alpha1.ServerGroupOnDemand,
			Image:        "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:   10,
			MaxInstances: ptr.To[int32](50),
			Storage: &spawneryv1alpha1.StorageSpec{
				Size:             resource.MustParse("2Gi"),
				StorageClassName: ptr.To("longhorn"),
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		},
	}
}

func TestServerGroupOnDemandAccepted(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	if err := c.Create(ctx, onDemandGroup(ns, "private-servers")); err != nil {
		t.Fatalf("create on-demand group: %v", err)
	}
}

func TestServerGroupOnDemandRefusesSizingFields(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	tests := map[string]func(*spawneryv1alpha1.ServerGroup){
		"scaling": func(g *spawneryv1alpha1.ServerGroup) {
			g.Spec.Scaling = &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 1}
		},
		"replicas": func(g *spawneryv1alpha1.ServerGroup) {
			g.Spec.Replicas = ptr.To[int32](1)
		},
		"update": func(g *spawneryv1alpha1.ServerGroup) {
			g.Spec.Update = &spawneryv1alpha1.UpdateSpec{MaxUnavailable: 1}
		},
	}
	for field, mutate := range tests {
		t.Run(field, func(t *testing.T) {
			g := onDemandGroup(ns, "refuses-"+field)
			mutate(g)
			if err := c.Create(ctx, g); err == nil {
				t.Fatalf("spec.%s was accepted for type OnDemand", field)
			}
		})
	}
}

func TestServerGroupOnDemandRefusesChangeoverStage(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	g := onDemandGroup(ns, "od-stage")
	g.Spec.ChangeoverStage = 5
	err := c.Create(ctx, g)
	if err == nil || !strings.Contains(err.Error(), "spec.changeoverStage is not allowed for type OnDemand") {
		t.Fatalf("create = %v, want the changeoverStage refusal", err)
	}
}

func TestServerGroupOnDemandRequiresStorageAndCeiling(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	noStorage := onDemandGroup(ns, "no-storage")
	noStorage.Spec.Storage = nil
	if err := c.Create(ctx, noStorage); err == nil {
		t.Fatal("an on-demand group without spec.storage was accepted")
	}

	noCeiling := onDemandGroup(ns, "no-ceiling")
	noCeiling.Spec.MaxInstances = nil
	if err := c.Create(ctx, noCeiling); err == nil {
		t.Fatal("an on-demand group without spec.maxInstances was accepted")
	}
}

func TestMaxInstancesIsOnDemandOnly(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	g := ephemeralGroup(ns, "lobby-with-ceiling")
	g.Spec.MaxInstances = ptr.To[int32](5)
	if err := c.Create(ctx, g); err == nil {
		t.Fatal("spec.maxInstances was accepted on an ephemeral group")
	}
}

func TestMaxInstancesZeroIsLegalAndNegativeIsNot(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	closed := onDemandGroup(ns, "closed")
	closed.Spec.MaxInstances = ptr.To[int32](0)
	if err := c.Create(ctx, closed); err != nil {
		t.Fatalf("maxInstances: 0 was refused, so a group cannot be closed without deleting it: %v", err)
	}

	negative := onDemandGroup(ns, "negative")
	negative.Spec.MaxInstances = ptr.To[int32](-1)
	if err := c.Create(ctx, negative); err == nil {
		t.Fatal("maxInstances: -1 was accepted")
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
		{
			name: "minAvailable equal to maxReplicas",
			base: ephemeralGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Update = &spawneryv1alpha1.UpdateSpec{MinAvailable: ptr.To[int32](10)}
			},
		},
		{
			name: "minAvailable zero",
			base: ephemeralGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Update = &spawneryv1alpha1.UpdateSpec{MinAvailable: ptr.To[int32](0)}
			},
		},
		{
			name: "WhenEmpty with maxStaleSeconds",
			base: ephemeralGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Update = &spawneryv1alpha1.UpdateSpec{
					Strategy: spawneryv1alpha1.UpdateWhenEmpty, MaxStaleSeconds: 60,
				}
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

func TestServerGroupAcceptsAFloorAndWhenEmpty(t *testing.T) {
	c, ctx := testenv.Client(t)
	for _, tc := range []struct {
		name   string
		update *spawneryv1alpha1.UpdateSpec
	}{
		{"a floor one below the ceiling", &spawneryv1alpha1.UpdateSpec{MinAvailable: ptr.To[int32](9)}},
		{"WhenEmpty", &spawneryv1alpha1.UpdateSpec{Strategy: spawneryv1alpha1.UpdateWhenEmpty}},
		{"WhenEmpty with a floor", &spawneryv1alpha1.UpdateSpec{
			Strategy: spawneryv1alpha1.UpdateWhenEmpty, MinAvailable: ptr.To[int32](2),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := testenv.Namespace(t, ctx, c)
			g := ephemeralGroup(ns, "group")
			g.Spec.Update = tc.update
			if err := c.Create(ctx, g); err != nil {
				t.Fatalf("create: %v", err)
			}
			if g.Spec.Update.Strategy == "" {
				t.Errorf("strategy was not defaulted")
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

	t.Run("storage size may be lowered", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := persistentGroup(ns, "survival")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Storage.Size = resource.MustParse("10Gi")
		if err := c.Update(ctx, g); err != nil {
			t.Fatalf("lowering storage.size rejected: %v", err)
		}
	})

	t.Run("an on-demand storage size may be lowered", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := onDemandGroup(ns, "private-servers")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Storage.Size = resource.MustParse("1Gi")
		if err := c.Update(ctx, g); err != nil {
			t.Fatalf("lowering storage.size rejected: %v", err)
		}
	})

	t.Run("storage annotations round-trip", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := persistentGroup(ns, "survival")
		g.Spec.Storage.Annotations = map[string]string{"resize.topolvm.io/storage_limit": "20Gi"}
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		var got spawneryv1alpha1.ServerGroup
		if err := c.Get(ctx, client.ObjectKeyFromObject(g), &got); err != nil {
			t.Fatalf("get: %v", err)
		}
		if v := got.Spec.Storage.Annotations["resize.topolvm.io/storage_limit"]; v != "20Gi" {
			t.Fatalf("annotations = %v, want the limit kept", got.Spec.Storage.Annotations)
		}
	})

	t.Run("annotation keys must be valid annotation keys", func(t *testing.T) {
		long := strings.Repeat("a", 64)
		for name, key := range map[string]string{
			"space":                "bad key",
			"empty name":           "example.com/",
			"name too long":        long,
			"underscore in prefix": "ex_ample.com/key",
		} {
			ns := testenv.Namespace(t, ctx, c)
			g := persistentGroup(ns, "survival")
			g.Spec.Storage.Annotations = map[string]string{key: "x"}
			if err := c.Create(ctx, g); err == nil {
				t.Errorf("%s: key %q accepted, want rejection", name, key)
			}
		}
	})

	t.Run("annotations are capped", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := persistentGroup(ns, "survival")
		g.Spec.Storage.Annotations = map[string]string{}
		for i := range 65 {
			g.Spec.Storage.Annotations[fmt.Sprintf("key-%d", i)] = "x"
		}
		if err := c.Create(ctx, g); err == nil {
			t.Fatal("65 annotations accepted, want rejection")
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

const playableSlotsRule = "spec.playableSlots must be between 1 and spec.maxPlayers"

func TestPlayableSlotsIsBoundedByMaxPlayers(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	for name, tc := range map[string]struct {
		playable *int32
		ok       bool
	}{
		"absent":              {nil, true},
		"one":                 {ptr.To[int32](1), true},
		"equal to maxPlayers": {ptr.To[int32](100), true},
		"zero":                {ptr.To[int32](0), false},
		"above maxPlayers":    {ptr.To[int32](101), false},
	} {
		t.Run(name, func(t *testing.T) {
			g := ephemeralGroup(ns, "playable-"+strings.ToLower(strings.ReplaceAll(name, " ", "-")))
			g.Spec.PlayableSlots = tc.playable
			err := c.Create(ctx, g)
			if tc.ok && err != nil {
				t.Fatalf("playableSlots %v was refused: %v", tc.playable, err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), playableSlotsRule)) {
				t.Fatalf("playableSlots %v: err = %v, want the playableSlots rule", ptr.Deref(tc.playable, -1), err)
			}
		})
	}
}

func TestLoweringMaxPlayersBelowPlayableSlotsIsRefused(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	g := ephemeralGroup(ns, "duels")
	g.Spec.PlayableSlots = ptr.To[int32](12)
	if err := c.Create(ctx, g); err != nil {
		t.Fatalf("create: %v", err)
	}
	g.Spec.MaxPlayers = 10
	if err := c.Update(ctx, g); err == nil || !strings.Contains(err.Error(), playableSlotsRule) {
		t.Fatalf("maxPlayers 10 below playableSlots 12: err = %v, want the playableSlots rule", err)
	}
}

func TestPlayableSlotsIsAllowedOnEveryType(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	p := persistentGroup(ns, "persistent-playable")
	p.Spec.PlayableSlots = ptr.To[int32](10)
	if err := c.Create(ctx, p); err != nil {
		t.Fatalf("persistent: %v", err)
	}
	o := onDemandGroup(ns, "ondemand-playable")
	o.Spec.PlayableSlots = ptr.To[int32](5)
	if err := c.Create(ctx, o); err != nil {
		t.Fatalf("on-demand: %v", err)
	}
}

func TestServerGroupStorageKeepAccepted(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	keep := []string{"world", "plugins/ExampleGame/state", "world/level.dat*", "a?"}

	od := onDemandGroup(ns, "keeps-on-demand")
	od.Spec.Storage.Keep = keep
	if err := c.Create(ctx, od); err != nil {
		t.Fatalf("create on-demand group with keep: %v", err)
	}
	p := persistentGroup(ns, "keeps-persistent")
	p.Spec.Storage.Keep = keep
	if err := c.Create(ctx, p); err != nil {
		t.Fatalf("create persistent group with keep: %v", err)
	}
}

func TestServerGroupStorageKeepRefusesBadEntries(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	tests := map[string][]string{
		"absolute":       {"/x"},
		"parent segment": {"a/../b"},
		"dot segment":    {"a/./b"},
		"empty segment":  {"a//b"},
		"trailing slash": {"a/"},
		"bracket":        {"a[b"},
		"closing":        {"a]b"},
		"backslash":      {`a\b`},
		"too long":       {strings.Repeat("a", 257)},
		"newline":        {"a\nb"},
		"carriage":       {"a\rb"},
		"one bad entry":  {"world", "a//b"},
	}
	for name, keep := range tests {
		t.Run(name, func(t *testing.T) {
			g := onDemandGroup(ns, "keep-"+strings.ReplaceAll(name, " ", "-"))
			g.Spec.Storage.Keep = keep
			err := c.Create(ctx, g)
			if err == nil {
				t.Fatalf("keep %q was accepted", keep)
			}
			if name != "too long" && !strings.Contains(err.Error(), "a keep entry is a relative path") {
				t.Errorf("err = %v, want the keep entry message", err)
			}
		})
	}
}

// keep is omitempty, so an empty list cannot be sent through the typed client.
func TestServerGroupStorageKeepRefusesAnEmptyList(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(onDemandGroup(ns, "keeps-nothing"))
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: raw}
	u.SetGroupVersionKind(spawneryv1alpha1.GroupVersion.WithKind("ServerGroup"))
	if err := unstructured.SetNestedSlice(u.Object, []any{}, "spec", "storage", "keep"); err != nil {
		t.Fatal(err)
	}
	err = c.Create(ctx, u)
	if err == nil {
		t.Fatal("an empty keep list was accepted")
	}
	if !strings.Contains(err.Error(), "spec.storage.keep") {
		t.Errorf("err = %v, want it to name spec.storage.keep", err)
	}
}
