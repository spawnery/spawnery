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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// BuildDataClaim renders the PersistentVolumeClaim a persistent server's world
// lives on.
//
// It carries no owner reference, and that is the load-bearing property rather
// than an omission. The claim outlives its server -- which is the whole point,
// since a recreated ordinal is meant to find its old world -- and it outlives
// its group, and the operator who deletes the wrong object. A StatefulSet
// retains its claims on both scale-down and deletion for the same reason. The
// cost is that claims accumulate and must be found and removed by hand; a
// later task in this milestone documents how.
//
// LabelManagedBy is not decoration: cmd/spawnery-operator restricts the
// manager's cache to that label for several kinds, claims among them as of the
// task that started creating these. Nothing reads a claim back yet, so an
// unlabelled one would go unnoticed today -- and be invisible to the very
// first Get anybody adds.
func BuildDataClaim(
	group *spawneryv1alpha1.ServerGroup,
	srv *spawneryv1alpha1.Server,
) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DataClaimName(srv.Name),
			Namespace: srv.Namespace,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelNetwork:   group.Spec.NetworkRef.Name,
				LabelGroup:     group.Name,
				LabelServer:    srv.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      group.Spec.Storage.AccessModes,
			StorageClassName: group.Spec.Storage.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: group.Spec.Storage.Size,
				},
			},
		},
	}
}
