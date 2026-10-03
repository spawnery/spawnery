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

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// checkExtraPlugins returns the condition reason and message, or ok. It reports
// rather than writes, so each controller places it in its own Accepted chain.
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
	if ep.Image != "" {
		return "", "", true
	}
	if !allowed {
		// Before the claim is read, so a disabled feature never touches a PVC.
		return spawneryv1alpha1.ReasonPluginVolumesDisabled,
			"spec.extraPlugins is set, and this operator was started without " +
				"--allow-plugin-volumes so it renders no plugin volume",
			false
	}

	problem, ok, err := checkClaimMountable(ctx, reader, namespace, ep.ClaimName)
	if err != nil {
		return reasonClaimUnreadable,
			fmt.Sprintf("spec.extraPlugins names claim %q, which could not be read: %v", ep.ClaimName, err),
			false
	}
	if !ok {
		return spawneryv1alpha1.ReasonPluginVolumeUnusable,
			fmt.Sprintf("spec.extraPlugins names claim %q, which %s", ep.ClaimName, problem),
			false
	}
	return "", "", true
}

// checkGroupVolumes asks spec.extraPlugins, then spec.extraFiles, then
// spec.mounts. Each has its own flag because "runs no third-party plugins" is
// not "mounts no administrator claim".
func checkGroupVolumes(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	ep *spawneryv1alpha1.ExtraPlugins,
	ef *spawneryv1alpha1.ExtraFiles,
	mounts []spawneryv1alpha1.Mount,
	pluginsAllowed bool,
	filesAllowed bool,
	mountsAllowed bool,
) (string, string, bool) {
	if reason, message, ok := checkExtraPlugins(ctx, reader, namespace, ep, pluginsAllowed); !ok {
		return reason, message, false
	}
	if reason, message, ok := checkExtraFiles(ctx, reader, namespace, ef, filesAllowed); !ok {
		return reason, message, false
	}
	return checkMountClaims(ctx, reader, namespace, mounts, mountsAllowed)
}

// checkClaimMountable returns a clause to follow "claim %q, which ...". It
// refuses here rather than leaving every pod Pending, where the answer would
// be in a pod event instead of on the group.
func checkClaimMountable(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	claimName string,
) (string, bool, error) {
	var pvc corev1.PersistentVolumeClaim
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: claimName}, &pvc)
	switch {
	case apierrors.IsNotFound(err):
		return "does not exist in this namespace", false, nil
	case err != nil:
		// An API server that did not answer is no verdict: one missed round trip
		// must not empty a group.
		return "", false, err
	}

	// A claim that is both RWO and RWX is mountable by every node.
	for _, m := range pvc.Spec.AccessModes {
		if m == corev1.ReadWriteMany {
			return "", true, nil
		}
	}
	return fmt.Sprintf("has access modes %v; every pod of a group mounts it, "+
		"which needs ReadWriteMany", pvc.Spec.AccessModes), false, nil
}

// checkMountClaims checks only claim-backed mounts; ConfigMap and Secret mounts
// need no storage class, access mode or flag. It reports the first failure only.
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
			// Before the claim is read, as in checkExtraPlugins.
			return spawneryv1alpha1.ReasonMountVolumesDisabled,
				fmt.Sprintf("mount %q names claim %q, and this operator was started without "+
					"--allow-mount-volumes so it mounts no claim",
					m.Name, m.PersistentVolumeClaim.ClaimName),
				false
		}
		problem, ok, err := checkClaimMountable(ctx, reader, namespace, m.PersistentVolumeClaim.ClaimName)
		if err != nil {
			return reasonClaimUnreadable,
				fmt.Sprintf("mount %q names claim %q, which could not be read: %v",
					m.Name, m.PersistentVolumeClaim.ClaimName, err),
				false
		}
		if !ok {
			return spawneryv1alpha1.ReasonMountVolumeUnusable,
				fmt.Sprintf("mount %q names claim %q, which %s",
					m.Name, m.PersistentVolumeClaim.ClaimName, problem),
				false
		}
	}
	return "", "", true
}

// reasonClaimUnreadable reaches no condition: keepLastVolumeDecision turns it
// into whatever the group was last told.
const reasonClaimUnreadable = "ClaimUnreadable"

// keepLastVolumeDecision replaces a "could not read" verdict with the group's
// previous one, so an outage neither grants nor lifts a refusal.
func keepLastVolumeDecision(
	conditions []metav1.Condition, reason, message string, ok bool,
) (string, string, bool) {
	if reason != reasonClaimUnreadable {
		return reason, message, ok
	}
	c := meta.FindStatusCondition(conditions, spawneryv1alpha1.ConditionAccepted)
	if c != nil && c.Status == metav1.ConditionFalse && isVolumeReason(c.Reason) {
		return c.Reason, c.Message, false
	}
	return "", "", true
}

func isVolumeReason(reason string) bool {
	switch reason {
	case spawneryv1alpha1.ReasonPluginVolumeUnusable, spawneryv1alpha1.ReasonPluginVolumesDisabled,
		spawneryv1alpha1.ReasonFileVolumeUnusable, spawneryv1alpha1.ReasonFileVolumesDisabled,
		spawneryv1alpha1.ReasonMountVolumeUnusable, spawneryv1alpha1.ReasonMountVolumesDisabled:
		return true
	}
	return false
}
