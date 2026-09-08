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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// allowScheduling rewrites the fixture network's policy and returns once the
// API server has it.
func (f *fixture) allowScheduling(t *testing.T, policy *spawneryv1alpha1.SchedulingPolicy) {
	t.Helper()
	net := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.network.Name, Namespace: f.ns}, net); err != nil {
		t.Fatalf("get network: %v", err)
	}
	net.Spec.Scheduling = policy
	if err := f.c.Update(f.ctx, net); err != nil {
		t.Fatalf("update network: %v", err)
	}
}

// The boundary docs/network-boundaries.md claims: a group author cannot put
// a pod where the Network's owner did not say it may go. Before this check a
// toleration on a control-plane taint was copied into the pod as written.
func TestAGroupIsRefusedTheSchedulingItsNetworkDoesNotAllow(t *testing.T) {
	f := newFixture(t)
	arena := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "arena", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: f.network.Name},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10},
			Scheduling: &spawneryv1alpha1.Scheduling{Tolerations: []corev1.Toleration{{
				Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists,
			}}},
		},
	}
	if err := f.c.Create(f.ctx, arena); err != nil {
		t.Fatalf("create arena: %v", err)
	}
	r := groupReconciler(f)
	reconcile := func() *spawneryv1alpha1.ServerGroup {
		t.Helper()
		if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Name: "arena", Namespace: f.ns},
		}); err != nil {
			t.Fatalf("reconcile arena: %v", err)
		}
		got := &spawneryv1alpha1.ServerGroup{}
		if err := f.c.Get(f.ctx, types.NamespacedName{Name: "arena", Namespace: f.ns}, got); err != nil {
			t.Fatalf("get arena: %v", err)
		}
		return got
	}

	got := reconcile()
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionFalse, spawneryv1alpha1.ReasonSchedulingNotAllowed) {
		t.Fatalf("conditions = %+v, want Accepted=False/SchedulingNotAllowed", got.Status.Conditions)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted); !strings.Contains(c.Message, "node-role.kubernetes.io/control-plane") {
		t.Errorf("message = %q, want it to name the toleration key", c.Message)
	}
	for _, s := range f.listServers(t) {
		if s.Spec.GroupRef.Name == "arena" {
			t.Errorf("arena created server %s although its scheduling was refused", s.Name)
		}
	}

	// The Network's owner allows the key, and the same spec is accepted.
	f.allowScheduling(t, &spawneryv1alpha1.SchedulingPolicy{
		AllowedTolerationKeys: []string{"node-role.kubernetes.io/control-plane"},
		HostPortRange:         &spawneryv1alpha1.PortRange{Min: 1, Max: 65535},
	})
	if got := reconcile(); !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
		t.Errorf("conditions = %+v, want Accepted=True once the network allows the key", got.Status.Conditions)
	}
}

func TestAHostPortProxyGroupIsRefusedAPortOutsideTheNetworksRange(t *testing.T) {
	f := newFixture(t)
	f.allowScheduling(t, &spawneryv1alpha1.SchedulingPolicy{
		HostPortRange: &spawneryv1alpha1.PortRange{Min: 30000, Max: 30100},
	})
	f.createProxyGroup("edge", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
		}
	})
	r := proxyGroupReconciler(f)
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "edge", Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile edge: %v", err)
	}
	got := f.proxyGroup("edge")
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionFalse, spawneryv1alpha1.ReasonHostPortNotAllowed) {
		t.Fatalf("conditions = %+v, want Accepted=False/HostPortNotAllowed", got.Status.Conditions)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted); !strings.Contains(c.Message, "30000-30100") {
		t.Errorf("message = %q, want it to name the range", c.Message)
	}
}
