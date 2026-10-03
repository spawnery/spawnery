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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

const rotationRunbook = "docs/guides/rotating-the-forwarding-secret.md"

type forwardingRead struct {
	// Hash is empty unless the read succeeded and the value was usable.
	Hash    string
	Status  metav1.ConditionStatus
	Reason  string
	Message string
	// Err is the API server's error, for the caller to log: Message omits it,
	// and test/e2e's theOperatorWasNeverDenied greps the log for "is forbidden:".
	Err error
}

// The reader must be uncached: the operator holds no list or watch on Secrets.
func readForwardingSecret(ctx context.Context, reader client.Reader, net *spawneryv1alpha1.Network) forwardingRead {
	name := net.Spec.ForwardingSecretRef.Name
	secret := &corev1.Secret{}
	err := reader.Get(ctx, client.ObjectKey{Namespace: net.Namespace, Name: name}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return forwardingRead{
			Status: metav1.ConditionFalse,
			Reason: spawneryv1alpha1.ReasonSecretNotFound,
			Message: fmt.Sprintf("spec.forwardingSecretRef names secret %q, which does not exist in namespace %q",
				name, net.Namespace),
		}
	case apierrors.IsForbidden(err):
		return forwardingRead{
			Err:    err,
			Status: metav1.ConditionUnknown,
			Reason: spawneryv1alpha1.ReasonSecretReadForbidden,
			Message: fmt.Sprintf("the operator may not read secret %q in namespace %q; grant it with "+
				"kubectl apply -n %s -f config/rbac/forwarding-secret-reader.yaml",
				name, net.Namespace, net.Namespace),
		}
	case err != nil:
		return forwardingRead{
			Err:     err,
			Status:  metav1.ConditionUnknown,
			Reason:  spawneryv1alpha1.ReasonSecretReadFailed,
			Message: fmt.Sprintf("reading secret %q in namespace %q failed: %v", name, net.Namespace, err),
		}
	}

	value := secret.Data[podspec.ForwardingSecretKey]
	if len(value) == 0 {
		return forwardingRead{
			Status: metav1.ConditionFalse,
			Reason: spawneryv1alpha1.ReasonSecretKeyMissing,
			Message: fmt.Sprintf("secret %q carries no non-empty %q key, which is where the Velocity "+
				"modern forwarding secret belongs", name, podspec.ForwardingSecretKey),
		}
	}

	return forwardingRead{
		Hash:    podspec.ForwardingHash(net.UID, value),
		Status:  metav1.ConditionTrue,
		Reason:  spawneryv1alpha1.ReasonSecretResolved,
		Message: fmt.Sprintf("secret %q carries a %q key", name, podspec.ForwardingSecretKey),
	}
}

func resolvedCondition(read forwardingRead) metav1.Condition {
	return metav1.Condition{
		Type:    spawneryv1alpha1.ConditionForwardingSecretResolved,
		Status:  read.Status,
		Reason:  read.Reason,
		Message: read.Message,
	}
}

type forwardingStamp struct {
	Group string
	Role  string
	Hash  string
}

// forwardingStamps drops terminating pods and pods podTerminal calls finished:
// neither runs the process the stamp describes, and a failed pod kept for
// spec.failedRetentionSeconds would hold the report open for that long.
func forwardingStamps(pods []corev1.Pod) []forwardingStamp {
	stamps := make([]forwardingStamp, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		if !pod.DeletionTimestamp.IsZero() || podTerminal(pod) {
			continue
		}
		stamps = append(stamps, forwardingStamp{
			Group: pod.Labels[podspec.LabelGroup],
			Role:  pod.Labels[podspec.LabelRole],
			Hash:  pod.Labels[podspec.LabelForwardingHash],
		})
	}
	return stamps
}

// rotationCondition's precedence: an unreadable secret, then a stale pod, then
// an unstamped one. A known problem outranks an unknown one.
func rotationCondition(read forwardingRead, stamps []forwardingStamp) metav1.Condition {
	cond := metav1.Condition{Type: spawneryv1alpha1.ConditionForwardingSecretRotationPending}

	if read.Hash == "" {
		cond.Status = metav1.ConditionUnknown
		cond.Reason = spawneryv1alpha1.ReasonSecretUnresolved
		cond.Message = "the forwarding secret could not be read, so whether a rotation is pending " +
			"cannot be told: " + read.Message
		return cond
	}

	stale := map[string]int{}
	untracked := 0
	for _, s := range stamps {
		switch {
		case s.Hash == "":
			untracked++
		case s.Hash != read.Hash:
			stale[s.Role+"/"+s.Group]++
		}
	}

	switch {
	case len(stale) > 0:
		cond.Status = metav1.ConditionTrue
		cond.Reason = spawneryv1alpha1.ReasonRotationPending
		cond.Message = fmt.Sprintf("still on the previous forwarding secret: %s; roll the server "+
			"groups first, then the proxy groups — see %s", staleSummary(stale), rotationRunbook)
	case untracked > 0:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = spawneryv1alpha1.ReasonPodsPredateTracking
		cond.Message = fmt.Sprintf("%d pod(s) carry no forwarding stamp, so whether they run on the "+
			"current secret cannot be told; they were created before this operator stamped it and "+
			"clear as pods turn over", untracked)
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = spawneryv1alpha1.ReasonForwardingSecretInSync
		cond.Message = "every pod of this network runs on the current forwarding secret"
	}
	return cond
}

// groupRotationCondition is one group's own ForwardingSecretRotationPending,
// the same condition type as the Network's at group scope, with
// rotationCondition's precedence.
//
// It reads the digest off Network.status rather than the secret: the group
// controllers have no grant on secrets outside the operator's namespace.
func groupRotationCondition(networkHash string, stamps []forwardingStamp) metav1.Condition {
	cond := metav1.Condition{Type: spawneryv1alpha1.ConditionForwardingSecretRotationPending}

	if networkHash == "" {
		cond.Status = metav1.ConditionUnknown
		cond.Reason = spawneryv1alpha1.ReasonSecretUnresolved
		cond.Message = "this group's network has published no forwarding-secret digest, so " +
			"whether a rotation is pending here cannot be told; the network's own " +
			"ForwardingSecretResolved condition says why"
		return cond
	}

	stale, untracked := 0, 0
	for _, s := range stamps {
		switch {
		case s.Hash == "":
			untracked++
		case s.Hash != networkHash:
			stale++
		}
	}

	switch {
	case stale > 0:
		cond.Status = metav1.ConditionTrue
		cond.Reason = spawneryv1alpha1.ReasonRotationPending
		cond.Message = fmt.Sprintf("%d pod(s) of this group still run on the previous forwarding "+
			"secret; roll the server groups first, then the proxy groups — see %s",
			stale, rotationRunbook)
	case untracked > 0:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = spawneryv1alpha1.ReasonPodsPredateTracking
		cond.Message = fmt.Sprintf("%d pod(s) of this group carry no forwarding stamp, so whether "+
			"they run on the current secret cannot be told; they were created before this "+
			"operator stamped it and clear as pods turn over", untracked)
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = spawneryv1alpha1.ReasonForwardingSecretInSync
		cond.Message = "every pod of this group runs on the current forwarding secret"
	}
	return cond
}

func reportGroupRotation(conditions *[]metav1.Condition, networkHash string, pods []corev1.Pod) {
	meta.SetStatusCondition(conditions, groupRotationCondition(networkHash, forwardingStamps(pods)))
}

// staleSummary lists every server entry before every proxy entry, each sorted
// by name: the runbook's order.
func staleSummary(stale map[string]int) string {
	keys := make([]string, 0, len(stale))
	for k := range stale {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		iServer := strings.HasPrefix(keys[i], podspec.RoleServer+"/")
		jServer := strings.HasPrefix(keys[j], podspec.RoleServer+"/")
		if iServer != jServer {
			return iServer
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, stale[k]))
	}
	return strings.Join(parts, ", ")
}

// hasConditionReason tells entering a state from staying in it, so events fire
// once rather than on every five-second requeue.
func hasConditionReason(conditions []metav1.Condition, condType, reason string) bool {
	cond := meta.FindStatusCondition(conditions, condType)
	return cond != nil && cond.Reason == reason
}
