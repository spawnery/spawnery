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

package podspec

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// persistentGroupFixture is a Persistent ServerGroup: 20Gi on storage class
// "longhorn", ReadWriteOnce. It builds on testGroup (server_test.go) rather
// than duplicating its network and image fields.
func persistentGroupFixture(t *testing.T) *spawneryv1alpha1.ServerGroup {
	t.Helper()
	group := testGroup()
	group.Name = "survival"
	group.Spec.Type = spawneryv1alpha1.ServerGroupPersistent
	group.Spec.Scaling = nil
	group.Spec.Replicas = ptr.To[int32](1)
	group.Spec.Storage = &spawneryv1alpha1.StorageSpec{
		Size:             resource.MustParse("20Gi"),
		StorageClassName: ptr.To("longhorn"),
		AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
	}
	return group
}

// serverFixture is a persistent server named name, otherwise built on
// testServer (server_test.go).
func serverFixture(t *testing.T, name string) *spawneryv1alpha1.Server {
	t.Helper()
	srv := testServer()
	srv.Name = name
	srv.Spec.GroupRef.Name = "survival"
	srv.Spec.Ordinal = ptr.To[int32](0)
	return srv
}

func TestBuildDataClaim(t *testing.T) {
	group := persistentGroupFixture(t) // 20Gi, storageClassName "longhorn", ReadWriteOnce
	srv := serverFixture(t, "survival-0")

	claim := BuildDataClaim(group, srv)

	if claim.Name != "survival-0-data" {
		t.Errorf("name = %q, want survival-0-data: the claim is named from the server, which is named from the ordinal", claim.Name)
	}
	if claim.Namespace != group.Namespace {
		t.Errorf("namespace = %q, want %q", claim.Namespace, group.Namespace)
	}
	if len(claim.OwnerReferences) != 0 {
		t.Errorf("owner references = %v, want none: the claim outlives the server, the group, and a mistaken delete", claim.OwnerReferences)
	}
	if claim.Labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("managed-by = %q, want %q: the operator's cache is restricted to this label", claim.Labels[LabelManagedBy], ManagedByValue)
	}
	if got := claim.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(group.Spec.Storage.Size) != 0 {
		t.Errorf("size = %v, want %v", got, group.Spec.Storage.Size)
	}
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "longhorn" {
		t.Errorf("storageClassName = %v, want longhorn", claim.Spec.StorageClassName)
	}
	if len(claim.Spec.AccessModes) != 1 || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("accessModes = %v, want [ReadWriteOnce]", claim.Spec.AccessModes)
	}
}

func TestBuildDataClaimWithoutAStorageClass(t *testing.T) {
	// storageClassName is optional: unset means the cluster's default class,
	// and a claim carrying an empty string instead would mean "no class at
	// all", which is a different and usually wrong thing.
	group := persistentGroupFixture(t)
	group.Spec.Storage.StorageClassName = nil
	claim := BuildDataClaim(group, serverFixture(t, "survival-0"))
	if claim.Spec.StorageClassName != nil {
		t.Fatalf("storageClassName = %v, want nil so the cluster default applies", claim.Spec.StorageClassName)
	}
}
