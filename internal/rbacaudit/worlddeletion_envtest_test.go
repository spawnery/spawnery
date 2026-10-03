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

package rbacaudit_test

import (
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

// RBAC cannot narrow claim deletion to the claims the operator minted; the
// chart's admission policy limits it to on-demand worlds.
func TestTheOperatorMayDeleteOnlyAnOnDemandWorld(t *testing.T) {
	subject := applyDeploymentAndDeriveSubject(t)
	var policy admissionregistrationv1.ValidatingAdmissionPolicy
	var policyBinding admissionregistrationv1.ValidatingAdmissionPolicyBinding
	renderedManifest(t, "ValidatingAdmissionPolicy/spawnery-world-deletion", &policy)
	renderedManifest(t, "ValidatingAdmissionPolicyBinding/spawnery-world-deletion", &policyBinding)
	apply(t, &policy, &policyBinding)

	c, ctx := testenv.Client(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "worlds"}}
	apply(t, ns)
	claim := func(name string, labels map[string]string) *corev1.PersistentVolumeClaim {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Labels: labels},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		if err := c.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create claim %s: %v", name, err)
		}
		return pvc
	}
	managed := func(group string) map[string]string {
		return map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue, podspec.LabelGroup: group}
	}
	world := managed("private-servers")
	world[podspec.LabelKey] = "c0ffee"
	onDemand := claim("private-servers-c0ffee-data", world)
	persistent := claim("survival-0-data", managed("survival"))
	renamed := claim("database-data", world)
	foreign := claim("vault-data", nil)

	cfg := rest.CopyConfig(testenv.Config(t))
	cfg.Impersonate = rest.ImpersonationConfig{UserName: subject}
	asOperator, err := client.New(cfg, client.Options{Scheme: c.Scheme()})
	if err != nil {
		t.Fatalf("impersonating client: %v", err)
	}
	tryDelete := func(pvc *corev1.PersistentVolumeClaim) error {
		return asOperator.Delete(ctx, pvc.DeepCopy(), client.DryRunAll)
	}

	// The policy is compiled and picked up asynchronously after its creation.
	deadline := time.Now().Add(30 * time.Second)
	for tryDelete(persistent) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the operator may delete a persistent group's claim: the policy never took effect")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// With patch on claims the operator could otherwise label any claim into
	// reach.
	forge := func(pvc *corev1.PersistentVolumeClaim, label, value string) error {
		var live corev1.PersistentVolumeClaim
		if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), &live); err != nil {
			t.Fatalf("get %s: %v", pvc.Name, err)
		}
		patched := live.DeepCopy()
		if patched.Labels == nil {
			patched.Labels = map[string]string{}
		}
		patched.Labels[label] = value
		return asOperator.Patch(ctx, patched, client.MergeFrom(&live), client.DryRunAll)
	}
	for _, tc := range []struct {
		pvc          *corev1.PersistentVolumeClaim
		label, value string
	}{
		{persistent, podspec.LabelKey, "0"},
		{foreign, podspec.LabelManagedBy, podspec.ManagedByValue},
		{onDemand, podspec.LabelGroup, "survival"},
		{onDemand, podspec.LabelKey, "decaf"},
	} {
		if err := forge(tc.pvc, tc.label, tc.value); !apierrors.IsInvalid(err) && !apierrors.IsForbidden(err) {
			t.Errorf("the operator set %s=%s on %s: err = %v, want a policy denial",
				tc.label, tc.value, tc.pvc.Name, err)
		}
	}
	if err := forge(onDemand, "example.com/unrelated", "yes"); err != nil {
		t.Errorf("an update that leaves the three labels alone was refused: %v", err)
	}

	if err := tryDelete(onDemand); err != nil {
		t.Errorf("an on-demand world was refused: %v", err)
	}
	for _, pvc := range []*corev1.PersistentVolumeClaim{persistent, renamed, foreign} {
		if err := tryDelete(pvc); !apierrors.IsInvalid(err) && !apierrors.IsForbidden(err) {
			t.Errorf("deleting %s as the operator: err = %v, want a policy denial", pvc.Name, err)
		}
	}
}
