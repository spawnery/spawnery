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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

func groupWithMounts(ns, name string, mounts ...spawneryv1alpha1.Mount) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10},
			Mounts:     mounts,
		},
	}
}

// The rule went from "exactly one of two" to "exactly one of three" when the
// claim source arrived, and a CEL expression rewritten from != to exists_one
// is exactly the kind of change that compiles, installs, and quietly stops
// refusing anything. So all three of the shapes it exists to reject are driven
// against a real API server, not reasoned about.
func TestTheAPIServerRefusesAMountWithoutExactlyOneSource(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	none := groupWithMounts(ns, "no-source", spawneryv1alpha1.Mount{
		Name: "empty", MountPath: "/data/empty",
	})
	if err := c.Create(ctx, none); err == nil {
		t.Error("a mount naming no source at all was admitted; it would render an empty volume")
	}

	two := groupWithMounts(ns, "two-sources", spawneryv1alpha1.Mount{
		Name:                  "both",
		MountPath:             "/data/both",
		ConfigMap:             &corev1.ConfigMapVolumeSource{},
		PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{ClaimName: "worlds"},
	})
	if err := c.Create(ctx, two); err == nil {
		t.Error("a mount naming a ConfigMap and a claim was admitted; only one of them would reach the pod")
	}

	three := groupWithMounts(ns, "three-sources", spawneryv1alpha1.Mount{
		Name:                  "all",
		MountPath:             "/data/all",
		ConfigMap:             &corev1.ConfigMapVolumeSource{},
		Secret:                &corev1.SecretVolumeSource{},
		PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{ClaimName: "worlds"},
	})
	if err := c.Create(ctx, three); err == nil {
		t.Error("a mount naming all three sources was admitted")
	}
}

func TestTheAPIServerAdmitsEachMountSourceOnItsOwn(t *testing.T) {
	// The other half: the rule has to let all three through. An exists_one
	// written against the wrong operand would refuse everything and pass the
	// test above completely.
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	good := groupWithMounts(ns, "each",
		spawneryv1alpha1.Mount{
			Name: "cm", MountPath: "/data/cm",
			ConfigMap: &corev1.ConfigMapVolumeSource{},
		},
		spawneryv1alpha1.Mount{
			Name: "sec", MountPath: "/data/sec",
			Secret: &corev1.SecretVolumeSource{},
		},
		spawneryv1alpha1.Mount{
			Name: "pvc", MountPath: "/data/worlds",
			PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{ClaimName: "worlds", Writable: true},
		},
	)
	if err := c.Create(ctx, good); err != nil {
		t.Fatalf("a group with one mount of each source was refused: %v", err)
	}

	// Read back rather than trusting the round trip: writable is the field
	// with a default, and a bool that does not survive serialisation would
	// leave every claim read-only with the spec saying otherwise.
	var got spawneryv1alpha1.ServerGroup
	if err := c.Get(ctx, client.ObjectKeyFromObject(good), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	claim := got.Spec.Mounts[2].PersistentVolumeClaim
	if claim == nil || !claim.Writable {
		t.Errorf("mounts[2].persistentVolumeClaim = %+v, want writable", claim)
	}
}

func TestAProxyGroupAcceptsMounts(t *testing.T) {
	// spec.mounts is new on ProxyGroup. Without this, the CRD could ship
	// without the field and every manifest naming it would be accepted with
	// the mounts silently pruned -- which is what the API server does with an
	// unknown field on a structural schema.
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

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
			Mounts: []spawneryv1alpha1.Mount{{
				Name: "assets", MountPath: "/data/resources",
				PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{ClaimName: "assets"},
			}},
		},
	}
	if err := c.Create(ctx, proxy); err != nil {
		t.Fatalf("a proxy group with a mount was refused: %v", err)
	}

	var got spawneryv1alpha1.ProxyGroup
	if err := c.Get(ctx, client.ObjectKeyFromObject(proxy), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Spec.Mounts) != 1 || got.Spec.Mounts[0].PersistentVolumeClaim == nil {
		t.Fatalf("mounts = %+v, want the claim mount to survive the round trip", got.Spec.Mounts)
	}
}
