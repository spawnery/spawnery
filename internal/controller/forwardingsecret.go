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

// rotationRunbook is where the condition messages and the rotation event send
// an operator. Named once so the three cannot drift apart.
const rotationRunbook = "docs/runbook-milestone-5c-secret-rotation.md"

// forwardingRead is what one attempt at reading a Network's forwarding secret
// produced: the digest when it worked, and in every case the
// ForwardingSecretResolved condition it justifies.
type forwardingRead struct {
	// Hash is podspec.ForwardingHash over the secret's value, empty unless the
	// read succeeded and the value was usable. Callers test this rather than
	// Reason, because "is there a digest to compare against" is the question
	// every one of them is actually asking.
	Hash string
	// Status, Reason and Message make up ForwardingSecretResolved.
	Status  metav1.ConditionStatus
	Reason  string
	Message string
	// Err is the API server's own error on the two branches that have one, and
	// it is carried out rather than dropped for one reason: Message is written
	// for a person reading `kubectl describe network` and says what to do
	// about it, so it deliberately does not quote the API server. That means
	// it carries no `is forbidden:` substring, which is the exact string
	// test/e2e's theOperatorWasNeverDenied greps the operator's log for. A 403
	// on this read was therefore invisible to the one check in this repository
	// that exists to catch a denial the RBAC audit cannot -- not through the
	// cache, the way that check's other blind spot works, but through an error
	// the code handled instead of surfacing. The caller logs this.
	Err error
}

// readForwardingSecret fetches the Network's forwarding secret and digests it.
//
// The reader is an argument rather than a field of the reconciler because it
// has to be the uncached one: a cached Secret would need an informer over every
// Secret in scope, and this operator holds no list or watch on them.
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

// resolvedCondition turns a read into the condition it justifies.
func resolvedCondition(read forwardingRead) metav1.Condition {
	return metav1.Condition{
		Type:    spawneryv1alpha1.ConditionForwardingSecretResolved,
		Status:  read.Status,
		Reason:  read.Reason,
		Message: read.Message,
	}
}

// forwardingStamp is one running pod's contribution to the rotation report.
type forwardingStamp struct {
	Group string
	Role  string
	// Hash is podspec.LabelForwardingHash, empty when the pod carries none.
	Hash string
}

// forwardingStamps reduces a network's pods to what the rotation report needs.
// Two kinds of pod are dropped, neither of them running a process the stamp
// describes. One with a DeletionTimestamp is on its way out and must not hold
// the report open after the replacement that fixes it already exists. One
// podTerminal calls finished is down, and its Server keeps it for
// spec.failedRetentionSeconds — an hour by default — so counting it would hold
// the report open for that hour after a rotation that is otherwise complete.
// The crash-looping case reads worst of all: the container may have restarted
// since the rotation and read the projected secret again, in which case the
// stamp does not name what that pod last loaded.
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

// rotationCondition decides ForwardingSecretRotationPending, in this
// precedence: an unreadable secret, then a stale pod, then an unstamped one. A
// known problem outranks an unknown one.
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

// staleSummary renders the stale counts as role/group=count, every server entry
// before every proxy entry and each sorted by name. The order is the runbook's:
// whoever reads this message is about to do the work it lists.
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

// hasConditionReason reports whether the object already carries this condition
// with this reason — which is how the events tell entering a state from staying
// in it. At a five-second requeue the difference is one event against seven
// hundred an hour.
func hasConditionReason(conditions []metav1.Condition, condType, reason string) bool {
	cond := meta.FindStatusCondition(conditions, condType)
	return cond != nil && cond.Reason == reason
}
