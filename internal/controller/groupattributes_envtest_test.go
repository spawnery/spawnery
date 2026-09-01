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
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

// The bounds on a group's attributes are the API server's, so they hold
// against every writer -- kubectl, a GitOps controller, anything -- rather than
// against the one path a reconciler happens to read them through.
//
// One test per bound, each asserting the refusal names it: a single "it was
// refused" test passes when the wrong bound fired.

func attributedGroup(ns string, attributes map[string]string) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10},
			Attributes: attributes,
		},
	}
}

func TestTheAPIServerAdmitsAGroupsAttributes(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	group := attributedGroup(ns, map[string]string{"permission": "task.build", "game": "bingo"})
	if err := c.Create(ctx, group); err != nil {
		t.Fatalf("an ordinary pair of attributes was refused: %v", err)
	}

	// And they are readable back as written, which is the whole of what the
	// operator promises to do with them.
	var read spawneryv1alpha1.ServerGroup
	if err := c.Get(ctx, client.ObjectKeyFromObject(group), &read); err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.Spec.Attributes["permission"] != "task.build" {
		t.Errorf("attributes = %v, want what was written", read.Spec.Attributes)
	}
}

func TestTheAPIServerRefusesMoreAttributesThanItCarries(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	attributes := make(map[string]string)
	for i := 0; i < 17; i++ {
		attributes[fmt.Sprintf("key-%d", i)] = "v"
	}

	if err := c.Create(ctx, attributedGroup(ns, attributes)); err == nil {
		t.Fatal("seventeen attributes were admitted; this reaches every agent on every resync")
	}
}

func TestTheAPIServerRefusesAnOversizedAttribute(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	err := c.Create(ctx, attributedGroup(ns, map[string]string{"game": strings.Repeat("x", 257)}))
	if err == nil {
		t.Fatal("an oversized attribute value was admitted")
	}
	// The message has to name the bound, because the person who meets it is
	// editing a file and has nothing else to go on.
	if !strings.Contains(err.Error(), "256") {
		t.Errorf("refusal did not name the bound: %v", err)
	}

	long := strings.Repeat("k", 65)
	if err := c.Create(ctx, attributedGroup(ns, map[string]string{long: "v"})); err == nil {
		t.Fatal("an oversized attribute name was admitted")
	}
}

func TestAProxyGroupCarriesAttributesToo(t *testing.T) {
	// Both group kinds appear in the same picture, and a plugin reading one
	// list should not find that half of it can be described and half cannot.
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	proxy := &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: ns},
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Image:      "ghcr.io/spawnery/velocity:3.4.0-0.2.0",
			Replicas:   1,
			Expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
			},
			Routing:    spawneryv1alpha1.RoutingSpec{FallbackGroups: []string{"lobby"}},
			Attributes: map[string]string{"region": "eu"},
		},
	}
	if err := c.Create(ctx, proxy); err != nil {
		t.Fatalf("a proxy group's attributes were refused: %v", err)
	}
}
