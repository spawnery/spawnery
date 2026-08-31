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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

// The reserved-prefix rule is a CEL expression on the CRD, so the only thing
// that can prove it works is an API server holding the generated CRD and
// refusing the object. internal/podspec/env_test.go checks that the rule still
// spells the prefix the Go constant does; this checks that the rule the API
// server compiles actually denies anything.
//
// Both halves are needed. A rule with a typo in the CEL -- an unbalanced
// paren, `startWith` for `startsWith` -- fails to compile, and a CRD whose
// validation does not compile is accepted with the rule inert on some
// versions. Then every group is admitted, a group shadowing
// SPAWNERY_OPERATOR_ENDPOINT points its agents at an address of its choosing,
// and the marker test goes on passing because the literal in the rule is
// still right.
func TestTheAPIServerRefusesAReservedEnvName(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10},
			Env: []corev1.EnvVar{
				{Name: spawneryv1alpha1.ReservedEnvPrefix + "OPERATOR_ENDPOINT", Value: "elsewhere:9443"},
			},
		},
	}
	err := c.Create(ctx, group)
	if err == nil {
		t.Fatal("a group naming SPAWNERY_OPERATOR_ENDPOINT was admitted: it would reach the pod " +
			"twice, and which value the agent reads is not something the object says")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("refusal did not mention the reservation, so nobody learns why from it: %v", err)
	}

	proxy := &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: ns},
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Replicas:   2,
			Image:      "ghcr.io/spawnery/velocity:3.4.0-0.2.0",
			Expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
			},
			Routing: spawneryv1alpha1.RoutingSpec{FallbackGroups: []string{"lobby"}},
			Env: []corev1.EnvVar{
				{Name: spawneryv1alpha1.ReservedEnvPrefix + "PROXY", Value: "somebody-else"},
			},
		},
	}
	if err := c.Create(ctx, proxy); err == nil {
		t.Fatal("a proxy group naming SPAWNERY_PROXY was admitted")
	}
}

func TestTheAPIServerAdmitsAnOrdinaryEnvName(t *testing.T) {
	// The other half of the rule: it has to let through the thing the field
	// exists for. A CEL expression that refuses everything would pass the test
	// above and leave the feature unusable.
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "bingo-solo", Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10},
			Env: []corev1.EnvVar{
				{Name: "JAVA_TOOL_OPTIONS", Value: "-Dgame.amountOfTeams=0"},
			},
		},
	}
	if err := c.Create(ctx, group); err != nil {
		t.Fatalf("an ordinary env name was refused: %v", err)
	}

	// And the list is a map by name, so the same name twice is refused before
	// two entries can reach a pod and leave the winner unstated.
	dup := group.DeepCopy()
	dup.ObjectMeta = metav1.ObjectMeta{Name: "bingo-team", Namespace: ns}
	dup.Spec.Env = []corev1.EnvVar{
		{Name: "JAVA_TOOL_OPTIONS", Value: "-Dgame.teamSize=4"},
		{Name: "JAVA_TOOL_OPTIONS", Value: "-Dgame.teamSize=3"},
	}
	if err := c.Create(ctx, dup); err == nil {
		t.Error("a duplicate env name was admitted; the pod would carry both and say nothing")
	}
}
