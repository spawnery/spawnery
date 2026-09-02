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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

// The bound on a group's display name is the API server's, like the bounds on
// its attributes, so it holds against every writer rather than against the one
// path a reconciler happens to read it through.

func displayNamedGroup(ns, displayName string) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "bingo-team", Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:  spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:        spawneryv1alpha1.ServerGroupEphemeral,
			Image:       "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:  100,
			Scaling:     &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10},
			DisplayName: displayName,
		},
	}
}

func TestTheAPIServerAdmitsAGroupsDisplayName(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	// Mixed case and a hyphen: exactly what a metadata.name may not carry,
	// which is the whole reason a display name exists next to it.
	group := displayNamedGroup(ns, "Bingo-Team")
	if err := c.Create(ctx, group); err != nil {
		t.Fatalf("an ordinary display name was refused: %v", err)
	}

	var read spawneryv1alpha1.ServerGroup
	if err := c.Get(ctx, client.ObjectKeyFromObject(group), &read); err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.Spec.DisplayName != "Bingo-Team" {
		t.Errorf("displayName = %q, want what was written", read.Spec.DisplayName)
	}
}

func TestTheAPIServerRefusesAnOversizedDisplayName(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	err := c.Create(ctx, displayNamedGroup(ns, strings.Repeat("x", 65)))
	if err == nil {
		t.Fatal("a 65-character display name was admitted; this reaches every agent on every resync")
	}
	// The message has to name the bound, because the person who meets it is
	// editing a file and has nothing else to go on.
	if !strings.Contains(err.Error(), "64") {
		t.Errorf("refusal did not name the bound: %v", err)
	}
}

func TestAProxyGroupCarriesADisplayNameToo(t *testing.T) {
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
			Routing:     spawneryv1alpha1.RoutingSpec{FallbackGroups: []string{"lobby"}},
			DisplayName: "Gateway",
		},
	}
	if err := c.Create(ctx, proxy); err != nil {
		t.Fatalf("a proxy group's display name was refused: %v", err)
	}
}
