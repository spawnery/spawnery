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

	var pvc corev1.PersistentVolumeClaim
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ep.ClaimName}, &pvc)
	switch {
	case apierrors.IsNotFound(err):
		// Refused here rather than left to the scheduler. Kubernetes would
		// leave every pod of the group Pending on a claim that does not exist,
		// and the answer would be in a pod event rather than on the group
		// somebody is looking at.
		return spawneryv1alpha1.ReasonPluginVolumeUnusable,
			fmt.Sprintf("spec.extraPlugins names claim %q, which does not exist in this namespace",
				ep.ClaimName),
			false
	case err != nil:
		return spawneryv1alpha1.ReasonPluginVolumeUnusable,
			fmt.Sprintf("could not read claim %q: %v", ep.ClaimName, err),
			false
	}

	// The whole list, not the first entry: a claim may carry several modes,
	// and one that is both RWO and RWX is mountable by every node.
	for _, m := range pvc.Spec.AccessModes {
		if m == corev1.ReadWriteMany {
			return "", "", true
		}
	}
	return spawneryv1alpha1.ReasonPluginVolumeUnusable,
		fmt.Sprintf("spec.extraPlugins names claim %q, whose access modes are %v; "+
			"every server of a group must mount it, which needs ReadWriteMany",
			ep.ClaimName, pvc.Spec.AccessModes),
		false
}
