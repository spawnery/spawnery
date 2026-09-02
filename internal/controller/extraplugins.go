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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// checkExtraPlugins decides whether a group's spec.extraPlugins can be served.
//
// One function for both group kinds. A ServerGroup and a ProxyGroup ask the
// identical question of the identical field, and two copies would be two
// answers the day somebody improves one message.
//
// It returns the condition reason and the sentence for a person, or ok when
// there is nothing wrong. It reports rather than writes, so the caller decides
// where the answer lands -- which is what lets both controllers put it in
// their own Accepted chain, in the position each one thinks right.
func checkExtraPlugins(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	ep *spawneryv1alpha1.ExtraPlugins,
	allowed bool,
) (string, string, bool) {
	if ep == nil {
		return "", "", true
	}
	if !allowed {
		// Before the claim is read, so an installation with the feature off
		// never touches a PersistentVolumeClaim -- and so the message sends
		// somebody whose claim is perfectly good to the operator's arguments
		// rather than to their own storage.
		return spawneryv1alpha1.ReasonPluginVolumesDisabled,
			"spec.extraPlugins is set, and this operator was started without " +
				"--allow-plugin-volumes so it renders no plugin volume",
			false
	}

	if problem, ok := checkClaimMountable(ctx, reader, namespace, ep.ClaimName); !ok {
		return spawneryv1alpha1.ReasonPluginVolumeUnusable,
			fmt.Sprintf("spec.extraPlugins names claim %q, which %s", ep.ClaimName, problem),
			false
	}
	return "", "", true
}

// checkGroupVolumes is every storage question a group's spec asks, answered in
// one call so that both controllers keep one branch rather than two identical
// ones. spec.extraPlugins is asked first: it is the older field and the one
// more installations set, and when a group has both wrong there is no reason
// to prefer the other. spec.extraFiles is asked next, then spec.mounts.
//
// The reasons stay distinct -- see ReasonMountVolumeUnusable -- so the caller
// puts the answer on the object without having to know which field produced
// it.
func checkGroupVolumes(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	ep *spawneryv1alpha1.ExtraPlugins,
	ef *spawneryv1alpha1.ExtraFiles,
	mounts []spawneryv1alpha1.Mount,
	allowed bool,
	filesAllowed bool,
) (string, string, bool) {
	if reason, message, ok := checkExtraPlugins(ctx, reader, namespace, ep, allowed); !ok {
		return reason, message, false
	}
	if reason, message, ok := checkExtraFiles(ctx, reader, namespace, ef, filesAllowed); !ok {
		return reason, message, false
	}
	return checkMountClaims(ctx, reader, namespace, mounts, allowed)
}

// checkClaimMountable answers the one question both spec.extraPlugins and a
// spec.mounts claim ask of a PersistentVolumeClaim: can every pod of a group
// mount it. It returns a clause a caller puts after "claim %q, which ...", so
// that each field names itself in its own message while the rule stays in one
// place.
//
// Refused here rather than left to the scheduler. Kubernetes would leave every
// pod of the group Pending on a claim that does not exist, and the answer
// would be in a pod event rather than on the group somebody is looking at.
func checkClaimMountable(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	claimName string,
) (string, bool) {
	var pvc corev1.PersistentVolumeClaim
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: claimName}, &pvc)
	switch {
	case apierrors.IsNotFound(err):
		return "does not exist in this namespace", false
	case err != nil:
		return fmt.Sprintf("could not be read: %v", err), false
	}

	// The whole list, not the first entry: a claim may carry several modes,
	// and one that is both RWO and RWX is mountable by every node.
	for _, m := range pvc.Spec.AccessModes {
		if m == corev1.ReadWriteMany {
			return "", true
		}
	}
	return fmt.Sprintf("has access modes %v; every pod of a group mounts it, "+
		"which needs ReadWriteMany", pvc.Spec.AccessModes), false
}

// checkMountClaims decides whether the claims a group's spec.mounts names can
// be served. ConfigMap and Secret mounts are not its business: they need no
// storage class, no access mode and no flag, and a group made of nothing but
// those never reaches a single API call here.
//
// One function for both group kinds, for the reason checkExtraPlugins gives.
// It reports the first mount that fails rather than collecting every one: the
// condition holds a sentence, and a person fixing three broken claims fixes
// them one reconcile at a time either way.
func checkMountClaims(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	mounts []spawneryv1alpha1.Mount,
	allowed bool,
) (string, string, bool) {
	for _, m := range mounts {
		if m.PersistentVolumeClaim == nil {
			continue
		}
		if !allowed {
			// Before the claim is read, exactly as checkExtraPlugins does it,
			// so an installation with the feature off touches no
			// PersistentVolumeClaim at all -- and so somebody whose claim is
			// perfectly good is sent to the operator's arguments rather than
			// to their own storage.
			return spawneryv1alpha1.ReasonMountVolumesDisabled,
				fmt.Sprintf("mount %q names claim %q, and this operator was started without "+
					"--allow-plugin-volumes so it mounts no claim",
					m.Name, m.PersistentVolumeClaim.ClaimName),
				false
		}
		if problem, ok := checkClaimMountable(ctx, reader, namespace, m.PersistentVolumeClaim.ClaimName); !ok {
			return spawneryv1alpha1.ReasonMountVolumeUnusable,
				fmt.Sprintf("mount %q names claim %q, which %s",
					m.Name, m.PersistentVolumeClaim.ClaimName, problem),
				false
		}
	}
	return "", "", true
}
