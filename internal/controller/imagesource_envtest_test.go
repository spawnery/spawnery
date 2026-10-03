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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

func imageSourceGroup(ns string, mutate func(*spawneryv1alpha1.ServerGroup)) *spawneryv1alpha1.ServerGroup {
	g := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10},
		},
	}
	mutate(g)
	return g
}

func TestTheAPIServerTakesExactlyOnePluginSource(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	for _, bad := range []struct {
		why string
		ep  spawneryv1alpha1.ExtraPlugins
	}{
		{"both sources", spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins", Image: "registry.example.net/lobby-plugins:1"}},
		{"neither source", spawneryv1alpha1.ExtraPlugins{}},
		{"a pull policy without an image", spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins", PullPolicy: corev1.PullAlways}},
	} {
		ep := bad.ep
		if err := c.Create(ctx, imageSourceGroup(ns, func(g *spawneryv1alpha1.ServerGroup) { g.Spec.ExtraPlugins = &ep })); err == nil {
			t.Errorf("%s was admitted", bad.why)
		}
	}
	ok := imageSourceGroup(ns, func(g *spawneryv1alpha1.ServerGroup) {
		g.Spec.ExtraPlugins = &spawneryv1alpha1.ExtraPlugins{Image: "registry.example.net/lobby-plugins@sha256:" + strings.Repeat("a", 64)}
		g.Spec.ExtraFiles = &spawneryv1alpha1.ExtraFiles{Image: "registry.example.net/lobby-files:1", PullPolicy: corev1.PullAlways}
		g.Spec.Substitution = &spawneryv1alpha1.Substitution{Prefix: "SECRET_"}
	})
	if err := c.Create(ctx, ok); err != nil {
		t.Fatalf("image sources with a substitution were refused: %v", err)
	}
}

func TestTheAPIServerRefusesABadSubstitutionPrefix(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	for i, prefix := range []string{"", "secret_", "1X", "SECRET-"} {
		p, name := prefix, fmt.Sprintf("lobby-%d", i)
		if err := c.Create(ctx, imageSourceGroup(ns, func(g *spawneryv1alpha1.ServerGroup) {
			g.Name = name
			g.Spec.Substitution = &spawneryv1alpha1.Substitution{Prefix: p}
		})); err == nil {
			t.Errorf("prefix %q was admitted", p)
		}
	}
}

func TestTheAPIServerRefusesAnEmptyImage(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	// Unstructured, because the typed client drops image: "" (omitempty); a
	// templated manifest sends the empty string itself.
	for i, field := range []string{"extraPlugins", "extraFiles"} {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "spawnery.cloud/v1alpha1",
			"kind":       "ServerGroup",
			"metadata":   map[string]any{"name": fmt.Sprintf("lobby-%d", i), "namespace": ns},
			"spec": map[string]any{
				"networkRef": map[string]any{"name": "production"},
				"type":       "Ephemeral",
				"image":      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
				"maxPlayers": int64(100),
				"scaling":    map[string]any{"minReplicas": int64(1), "maxReplicas": int64(2), "spareSlots": int64(10)},
				field:        map[string]any{"image": ""},
			},
		}}
		if err := c.Create(ctx, u); err == nil {
			t.Errorf("%s.image \"\" was admitted", field)
		}
	}
}
