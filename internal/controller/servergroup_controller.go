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
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	boostpkg "github.com/spawnery/spawnery/internal/boost"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/render"
)

// nameSuffixAlphabet avoids characters that are easy to misread in a terminal.
const nameSuffixAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// networkRetryInterval is how long the group waits before looking for a
// missing Network again.
const networkRetryInterval = 30 * time.Second

// NewServerName builds a unique ephemeral server name below the group prefix.
func NewServerName(group string) string {
	buf := make([]byte, 4)
	// crypto/rand.Read never fails on the platforms we support.
	_, _ = rand.Read(buf)
	suffix := make([]byte, len(buf))
	for i, b := range buf {
		suffix[i] = nameSuffixAlphabet[int(b)%len(nameSuffixAlphabet)]
	}
	return fmt.Sprintf("%s-%s", group, suffix)
}

// ServerGroupReconciler keeps a group at its desired size and publishes its
// aggregated status.
type ServerGroupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	Agents *agent.Registry
	Clock  func() time.Time
	// Expectations reserves the creates and deletes this reconciler has issued
	// and the cache has not shown yet. One instance is shared across groups.
	Expectations *expectations
	// DrainTaintKeys is Options.DrainTaintKeys. Nil means only cordoned nodes
	// count.
	DrainTaintKeys []string

	// AllowPluginVolumes is Options.AllowPluginVolumes.
	AllowPluginVolumes bool

	// AllowFileVolumes is Options.AllowFileVolumes.
	AllowFileVolumes bool

	// AllowMountVolumes is Options.AllowMountVolumes.
	AllowMountVolumes bool

	// ClaimReader reads a group's spec.extraPlugins claim, and it must be
	// uncached: the manager's cache holds only claims carrying our managed-by
	// label (cmd/spawnery-operator/main.go), and an administrator's plugin claim
	// does not. envtest does not restrict its cache, so no test catches this.
	ClaimReader client.Reader
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups,verbs=get;list;watch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups/status,verbs=update
// No update on scaleboosts: an edit would change a group's floor without the
// expiry that makes a boost safe.
// +kubebuilder:rbac:groups=spawnery.cloud,resources=scaleboosts,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups/finalizers,verbs=update
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile sizes the group and updates its status.
func (r *ServerGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		if apierrors.IsNotFound(err) {
			// No ServerGroup finalizer exists, so most deletions are only seen as
			// NotFound, and observe never runs again to expire the reservations.
			r.Expectations.forget(req.Namespace + "/" + req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !group.DeletionTimestamp.IsZero() {
		// Owned Servers cascade and drain through their own finalizers.
		r.Expectations.forget(group.Namespace + "/" + group.Name)
		return ctrl.Result{}, nil
	}

	network := &spawneryv1alpha1.Network{}
	networkKey := types.NamespacedName{Name: group.Spec.NetworkRef.Name, Namespace: group.Namespace}
	networkFound := true
	if err := r.Get(ctx, networkKey, network); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		networkFound = false
	}
	// A Network must also have won the one-per-namespace contest; a loser and
	// one not yet reconciled read the same here and both heal on requeue.
	networkUsable := networkFound && meta.IsStatusConditionTrue(network.Status.Conditions, spawneryv1alpha1.ConditionAccepted)

	volumeReason, volumeMessage, volumesOK := checkGroupVolumes(
		ctx, r.ClaimReader, group.Namespace,
		group.Spec.ExtraPlugins, group.Spec.ExtraFiles, group.Spec.Mounts,
		r.AllowPluginVolumes, r.AllowFileVolumes, r.AllowMountVolumes)
	if volumeReason == reasonClaimUnreadable {
		logger.Info("a claim could not be read; keeping the group's last decision",
			"group", group.Name, "problem", volumeMessage)
		volumeReason, volumeMessage, volumesOK = keepLastVolumeDecision(
			group.Status.Conditions, volumeReason, volumeMessage, volumesOK)
	}

	schedulingMessage, schedulingOK := "", true
	if networkFound {
		schedulingMessage, schedulingOK = podspec.SchedulingRefusal(network,
			podspec.EffectiveScheduling(network, group.Spec.Scheduling), group.Namespace)
	}

	requeue := ResyncInterval
	switch {
	case !networkFound:
		logger.Info("network not found, no servers are created for this group",
			"group", group.Name, "network", group.Spec.NetworkRef.Name)
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonNetworkNotFound,
			Message: fmt.Sprintf("network %q does not exist in this namespace", group.Spec.NetworkRef.Name),
		})
		requeue = networkRetryInterval
	case !networkUsable:
		logger.Info("network not accepted, no servers are created for this group",
			"group", group.Name, "network", network.Name)
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonNetworkNotAccepted,
			Message: networkNotAcceptedMessage(network),
		})
		requeue = networkRetryInterval
	// After the network cases: a missing Network is the bigger problem.
	case !volumesOK:
		logger.Info("volume unusable, no servers are created for this group",
			"group", group.Name, "reason", volumeReason)
		// Announced on the transition only; this branch runs every pass.
		if !hasConditionReason(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted, volumeReason) {
			r.Recorder.Eventf(group, nil, corev1.EventTypeWarning, volumeReason, actionSyncStatus,
				"%s", volumeMessage)
		}
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionFalse,
			Reason:  volumeReason,
			Message: volumeMessage,
		})
		requeue = networkRetryInterval
	case !schedulingOK:
		logger.Info("scheduling not allowed by the network, no servers are created for this group",
			"group", group.Name, "reason", schedulingMessage)
		if !hasConditionReason(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted, spawneryv1alpha1.ReasonSchedulingNotAllowed) {
			r.Recorder.Eventf(group, nil, corev1.EventTypeWarning, spawneryv1alpha1.ReasonSchedulingNotAllowed, actionSyncStatus,
				"%s", schedulingMessage)
		}
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonSchedulingNotAllowed,
			Message: schedulingMessage,
		})
		requeue = networkRetryInterval
	default:
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionTrue,
			Reason:  spawneryv1alpha1.ReasonAccepted,
			Message: fmt.Sprintf("managed as part of network %q", network.Name),
		})
	}

	// Not gated on the Network: a spec.maxPlayers edit must reach the ConfigMap
	// even on a pass that creates nothing, and every pod mounts it by name, so it
	// must exist before the first one. A foreign ConfigMap at that name stops only
	// creation; the budget and condemnation go on. Requeued, since the colliding
	// object has no owner reference back here.
	foreignConfigMap := false
	if err := r.reconcileConfigMap(ctx, group); err != nil {
		if !errors.Is(err, errForeignConfigMap) {
			return ctrl.Result{}, err
		}
		foreignConfigMap = true
		requeue = networkRetryInterval
	}

	views, servers, err := r.collectViews(ctx, group)
	if err != nil {
		return ctrl.Result{}, err
	}

	// No event here: condemn() already emits one NodeDraining event per server.
	drainingNodes := make([]string, 0, len(views))
	for _, v := range views {
		if v.Condemned {
			drainingNodes = append(drainingNodes, v.NodeName)
		}
	}
	// Published below, once the backoff is known; see blockedReplacement.

	// Sizing needs a usable Network, mountable claims, allowed scheduling and our
	// own ConfigMap; a Server created without them would never get a working pod
	// and would be replaced over and over.
	//
	// The rule for other steps: whatever keeps the eviction API off an occupied
	// pod, or moves players off a departing node, does not depend on the Network.
	// So the PodDisruptionBudget and condemnation run on every pass, and size()
	// branches on mayResize internally. Condemning without being able to rebuild
	// means running below capacity; that is accepted, the node evicts the players
	// regardless.
	mayResize := networkUsable && volumesOK && schedulingOK && !foreignConfigMap

	// Gated on mayResize: with no Network found, net is the zero value and the
	// hash would not describe what the group renders later. Computed once per pass,
	// not per server.
	var podHash string
	if mayResize {
		configValues, err := serverConfigValues(group)
		if err != nil {
			return ctrl.Result{}, err
		}
		podHash, err = podspec.DesiredServerHash(network, group, configValues)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Adoption stamps the current hash on servers that predate spec.podHash;
		// staleSpec never nominates a hashless view, so this orders no takedown.
		if err := r.adoptServers(ctx, group, servers, podHash); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The streak belongs to the attempt (see attemptKey) and is reset only when
	// that moves. lastFailureAt survives the reset, so the previous attempt's
	// corpse is not counted into the new streak.
	var lastFailure time.Time
	if group.Status.LastFailureAt != nil {
		lastFailure = group.Status.LastFailureAt.Time
	}
	streakKey := group.Status.FailureStreakKey
	var currentKey string
	if podHash != "" && group.Status.ConsecutiveFailures > 0 {
		currentKey, err = r.attemptKey(ctx, group, podHash)
		if err != nil {
			return ctrl.Result{}, err
		}
		if streakKey != "" && streakKey != currentKey {
			group.Status.ConsecutiveFailures = 0
			streakKey = ""
		}
	}
	countViews, requiredOrdinals := views, group.DesiredReplicas()
	if group.IsEphemeral() {
		attemptHash := podHash
		if attemptHash == "" {
			attemptHash = hashOfAttempt(streakKey)
		}
		countViews, requiredOrdinals = ofAttempt(views, attemptHash), 0
	}
	failures, newestFailure := CountFailures(countViews,
		group.Status.ConsecutiveFailures, lastFailure, requiredOrdinals)
	group.Status.ConsecutiveFailures = failures
	switch {
	case failures == 0:
		streakKey = ""
	case streakKey == "" && podHash != "":
		if currentKey == "" {
			currentKey, err = r.attemptKey(ctx, group, podHash)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		streakKey = currentKey
	}
	group.Status.FailureStreakKey = streakKey
	// Only written when this pass counted something: it is the watermark that
	// keeps the count idempotent across resyncs. Clearing it on a reset would
	// recount every retained corpse, and moving it to the success would publish a
	// time at which nothing failed as lastFailureAt.
	if !newestFailure.IsZero() {
		stamped := metav1.NewTime(newestFailure)
		group.Status.LastFailureAt = &stamped
	}
	backoff := DecideBackoff(BackoffInputs{
		ConsecutiveFailures: failures,
		LastFailureAt:       newestFailure,
		Now:                 r.Clock(),
	})

	// The NodeDraining condition, with what stops this group rebuilding what it
	// condemns. The Network is named first: it is the unbounded wait.
	blocked := blockedReplacement{}
	switch {
	case !networkUsable:
		blocked = blockedReplacement{Reason: "its Network is missing or not accepted"}
	case !volumesOK:
		blocked = blockedReplacement{Reason: "a claim it mounts cannot be served; see its Accepted condition"}
	case !backoff.MayCreate:
		blocked = blockedReplacement{
			Reason:  "its servers are failing to start and it is backing off",
			Bounded: true,
		}
	}
	meta.SetStatusCondition(&group.Status.Conditions,
		drainingConditionBlocked(drainingNodes, blocked))

	// A failed boost list sizes on the declared floor alone rather than failing
	// the pass, so dead servers are still replaced.
	var boost int32
	boostList := &spawneryv1alpha1.ScaleBoostList{}
	if err := r.List(ctx, boostList, client.InNamespace(group.Namespace)); err != nil {
		log.FromContext(ctx).V(1).Info("could not read scale boosts; sizing on the declared floor alone",
			"group", group.Name, "reason", err.Error())
	} else {
		boost = boostpkg.Live(boostList.Items, group.Name, r.Clock())
	}

	var siblings []ChangeoverView
	budget := network.ChangeoverBudget()
	if !group.IsOnDemand() && mayResize {
		siblings, err = changeoverSiblings(ctx, r, group.Namespace, network.Name, "ServerGroup", group.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	decision, err := r.size(ctx, group, views, servers, backoff, mayResize, podHash, boost, siblings, budget)
	if err != nil {
		return ctrl.Result{}, err
	}
	var wait ChangeoverWait
	if decision.ChangeoverWaiting {
		wait = serverChangeoverWait(group, siblings, budget)
	}
	sized := mayResize

	if group.IsEphemeral() {
		limited := metav1.Condition{
			Type:    spawneryv1alpha1.ConditionScalingLimited,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonWithinLimits,
			Message: "free slots cover spec.scaling.spareSlots",
		}
		if decision.Limited {
			limited.Status = metav1.ConditionTrue
			limited.Reason = spawneryv1alpha1.ReasonMaxReplicasReached
			if decision.ColdStartBlocked {
				// Wanted and Create are both 0 here, so the shortfall message would say
				// nothing is needed.
				limited.Message = fmt.Sprintf(
					"changeover cannot begin: the group is already at maxReplicas %d; raise it by at least 1 to start the new generation",
					group.Spec.Scaling.MaxReplicas)
			} else if decision.FloorBlocked {
				limited.Message = fmt.Sprintf(
					"the changeover keeps minAvailable %d joinable and needs one extra server; the group is at maxReplicas %d",
					group.UpdateMinAvailable(), group.Spec.Scaling.MaxReplicas)
			} else {
				limited.Message = fmt.Sprintf(
					"%d more server(s) needed to cover spareSlots %d; maxReplicas %d allows %d now",
					decision.Wanted, group.Spec.Scaling.SpareSlots,
					group.Spec.Scaling.MaxReplicas, decision.Create)
			}
		}
		if !sized {
			// Nothing was decided, so False is not a verdict.
			limited.Message = "scaling is not being decided: the group's network is not usable"
		}
		// The event goes on the flank only.
		was := meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionScalingLimited)
		meta.SetStatusCondition(&group.Status.Conditions, limited)
		if sized && decision.Limited != was {
			eventType := corev1.EventTypeNormal
			if decision.Limited {
				eventType = corev1.EventTypeWarning
			}
			r.Recorder.Eventf(group, nil, eventType, limited.Reason, actionSyncStatus, "%s", limited.Message)
		}
	}

	// StorageResize only for group types that keep claims.
	if !group.IsEphemeral() {
		resize := storageResizeCondition(views)
		// Flanked on refusal rather than health: True is this condition's healthy
		// default, and a first publish with every claim in step stays quiet.
		wasRefused := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionStorageResize) != nil &&
			!meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionStorageResize)
		meta.SetStatusCondition(&group.Status.Conditions, resize)
		isRefused := resize.Status == metav1.ConditionFalse
		if isRefused != wasRefused {
			eventType := corev1.EventTypeNormal
			if isRefused {
				eventType = corev1.EventTypeWarning
			}
			r.Recorder.Eventf(group, nil, eventType, resize.Reason, actionSyncStatus, "%s",
				eventNote("%s", resize.Message))
		}
	}

	// BackingOff and Degraded apply to every group type, unlike ScalingLimited:
	// the backoff gates a persistent group's creates too, and without these it
	// would give up in silence, its phase reading Pending like a slow start.
	backingOff := metav1.Condition{
		Type:    spawneryv1alpha1.ConditionBackingOff,
		Status:  metav1.ConditionFalse,
		Reason:  spawneryv1alpha1.ReasonNoRecentFailures,
		Message: "no server has failed to start recently",
	}
	degraded := metav1.Condition{
		Type:    spawneryv1alpha1.ConditionDegraded,
		Status:  metav1.ConditionFalse,
		Reason:  spawneryv1alpha1.ReasonNoRecentFailures,
		Message: "servers are starting normally",
	}
	switch {
	case foreignConfigMap:
		name := podspec.GroupConfigMapName(group.Name, podspec.RoleServer)
		degraded.Status = metav1.ConditionTrue
		degraded.Reason = spawneryv1alpha1.ReasonConfigMapNotOurs
		degraded.Message = foreignConfigMapMessage(group.Namespace, name)
		backingOff.Message = "backoff is not being decided: the group cannot write its own configuration"
	// Before !sized: giving up needs a spec edit, the Network already has its
	// own Accepted condition, and GaveUp does not depend on the Network.
	case backoff.GaveUp:
		// BackingOff is false with no pending retry, but carries the real reason.
		backingOff.Reason = spawneryv1alpha1.ReasonCrashLoopBackoff
		backingOff.Message = fmt.Sprintf(
			"not retrying: %d rounds of server starts failed in a row; a change to what the servers start "+
				"with (the pod spec or the configOverlay ConfigMap) retries, and so does a new value on the "+
				"%s annotation once a cause outside the group is fixed",
			group.Status.ConsecutiveFailures, spawneryv1alpha1.AnnotationRetry)
		degraded.Status = metav1.ConditionTrue
		degraded.Reason = spawneryv1alpha1.ReasonCrashLoopBackoff
		degraded.Message = backingOff.Message
	case !sized:
		// Nothing was decided, as in ScalingLimited's !sized case; the failure
		// count above runs regardless of the Network.
		backingOff.Message = "backoff is not being decided: the group's network is not usable"
		degraded.Message = "backoff is not being decided: the group's network is not usable"
	case backoff.RetryAfter > 0:
		backingOff.Status = metav1.ConditionTrue
		backingOff.Reason = spawneryv1alpha1.ReasonCrashLoopBackoff
		backingOff.Message = fmt.Sprintf(
			"%d round(s) of server starts failed in a row; next attempt in %s",
			group.Status.ConsecutiveFailures, backoff.RetryAfter.Round(time.Second))
	default:
		// A non-nil LastFailureAt here is a past failure's watermark.
		if group.Status.LastFailureAt != nil {
			backingOff.Message = fmt.Sprintf(
				"no server has failed to start recently (last failure at %s)",
				group.Status.LastFailureAt.Format(time.RFC3339))
		}
	}

	// The events go on the flank only.
	wasBackingOff := meta.IsStatusConditionTrue(group.Status.Conditions,
		spawneryv1alpha1.ConditionBackingOff)
	wasDegraded := meta.IsStatusConditionTrue(group.Status.Conditions,
		spawneryv1alpha1.ConditionDegraded)
	meta.SetStatusCondition(&group.Status.Conditions, backingOff)
	meta.SetStatusCondition(&group.Status.Conditions, degraded)
	if isTrue := backingOff.Status == metav1.ConditionTrue; sized && isTrue != wasBackingOff {
		eventType := corev1.EventTypeNormal
		if isTrue {
			eventType = corev1.EventTypeWarning
		}
		r.Recorder.Eventf(group, nil, eventType, backingOff.Reason, actionSyncStatus, "%s", backingOff.Message)
	}
	if isTrue := degraded.Status == metav1.ConditionTrue; sized && isTrue != wasDegraded {
		eventType := corev1.EventTypeNormal
		if isTrue {
			eventType = corev1.EventTypeWarning
		}
		r.Recorder.Eventf(group, nil, eventType, degraded.Reason, actionSyncStatus, "%s", degraded.Message)
	}

	if group.IsEphemeral() || group.IsOnDemand() {
		if err := r.pruneFailed(ctx, group, views, servers); err != nil {
			return ctrl.Result{}, err
		}
	}
	if group.IsOnDemand() {
		if err := r.sweepOnDemand(ctx, group, views, servers); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.reconcilePDB(ctx, group, views); err != nil {
		return ctrl.Result{}, err
	}

	totals := AggregateGroup(views, podHash)
	group.Status.Replicas = totals.Replicas
	group.Status.ReadyReplicas = totals.ReadyReplicas
	group.Status.OnlinePlayers = totals.OnlinePlayers
	group.Status.FreeSlots = totals.FreeSlots
	group.Status.BoostedReplicas = boost
	group.Status.ObservedGeneration = group.Generation
	group.Status.Phase = derivePhase(group, totals)
	// Independent of derivePhase; see ConditionProgressing. The floor is only
	// reported while the extra server is not being built: the creating pass reads
	// views from before the create.
	var floor FloorReport
	if decision.FloorHeld && (decision.FloorBlocked || (decision.Create > 0 && !backoff.MayCreate)) {
		floor = FloorReport{Joinable: decision.Joinable, Min: group.UpdateMinAvailable()}
	}
	reportProgressing(group, views, podHash, wait, floor)
	// A failed list leaves the rotation condition as it was rather than failing
	// the pass.
	if pods, err := r.groupPods(ctx, group); err != nil {
		log.FromContext(ctx).Error(err, "listing this group's pods for the rotation report",
			"group", group.Name, "namespace", group.Namespace)
	} else {
		reportGroupRotation(&group.Status.Conditions, network.Status.ForwardingSecretHash, pods)
	}

	return ctrl.Result{RequeueAfter: requeue}, r.Status().Update(ctx, group)
}

// networkNotAcceptedMessage quotes the Network's own Accepted condition when
// one has been published.
func networkNotAcceptedMessage(network *spawneryv1alpha1.Network) string {
	if cond := meta.FindStatusCondition(network.Status.Conditions, spawneryv1alpha1.ConditionAccepted); cond != nil {
		return fmt.Sprintf("network %q is not accepted (%s): %s", network.Name, cond.Reason, cond.Message)
	}
	return fmt.Sprintf("network %q has not been accepted yet", network.Name)
}

// ofAttempt narrows the views to the servers started with podHash, for an
// ephemeral group's failure count only: a previous spec's server going Ready
// says nothing about this one. A hashless server is adopted on this pass, so
// it counts as current. Unlike a filter in the capacity arithmetic, this can
// only hold a create back.
func ofAttempt(views []ServerView, podHash string) []ServerView {
	if podHash == "" {
		return views
	}
	out := make([]ServerView, 0, len(views))
	for _, v := range views {
		if v.PodHash == podHash || v.PodHash == "" {
			out = append(out, v)
		}
	}
	return out
}

// attemptKey is what a group's servers start with: the desired pod hash, the
// configOverlay ConfigMap's resourceVersion, which reaches no hash because the
// operator mounts it unread, and the retry annotation. The overlay is read
// uncached, and only while a streak runs, because the manager's ConfigMap
// cache holds only the operator's own.
func (r *ServerGroupReconciler) attemptKey(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	podHash string,
) (string, error) {
	var overlay string
	if ref := group.Spec.ConfigOverlay; ref != nil {
		cm := &corev1.ConfigMap{}
		err := r.ClaimReader.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: ref.Name}, cm)
		switch {
		case apierrors.IsNotFound(err):
			overlay = "missing"
		case err != nil:
			return "", fmt.Errorf("read configOverlay %s: %w", ref.Name, err)
		default:
			overlay = cm.ResourceVersion
		}
	}
	return podHash + "/" + overlay + "/" + group.Annotations[spawneryv1alpha1.AnnotationRetry], nil
}

func hashOfAttempt(key string) string {
	hash, _, _ := strings.Cut(key, "/")
	return hash
}

// size brings the group to the size its rule asks for (DecideSize for an
// ephemeral group, DecidePersistentSize for a persistent one) and condemns the
// servers on departing nodes, whether or not mayResize allows sizing. Without
// a sizing decision the result carries only Condemn.
func (r *ServerGroupReconciler) size(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	views []ServerView,
	servers map[string]*spawneryv1alpha1.Server,
	backoff BackoffDecision,
	mayResize bool,
	podHash string,
	boost int32,
	siblings []ChangeoverView,
	budget int32,
) (SizeDecision, error) {
	logger := log.FromContext(ctx)
	key := group.Namespace + "/" + group.Name

	// Outside the switch: the reservations keep condemned() from naming a server
	// twice across passes, even when no size is decided.
	r.Expectations.observe(key, views)
	pendingCreates, pendingDeletes, pendingRetires := r.Expectations.pending(key)

	var decision SizeDecision
	was := group.Status.Changeover
	group.Status.Changeover = spawneryv1alpha1.ChangeoverNone
	self := serverChangeoverSelf(group, "")
	switch {
	case !mayResize:
		if was != spawneryv1alpha1.ChangeoverWaiting {
			group.Status.Changeover = was
		}
	case group.IsEphemeral():
		if group.Spec.Scaling != nil {
			own := ownServerChangeover(views, podHash, int32(len(pendingCreates)), group.UpdateWhenEmpty(), was)
			self.State = own
			decision = DecideSize(ScalingInputs{
				Views:         views,
				MinReplicas:   group.Spec.Scaling.MinReplicas,
				Boost:         boost,
				MaxReplicas:   group.Spec.Scaling.MaxReplicas,
				SpareSlots:    group.Spec.Scaling.SpareSlots,
				MaxPlayers:    group.Spec.MaxPlayers,
				PlayableSlots: playableSpec(group),
				Stabilization: time.Duration(group.Spec.Scaling.ScaleDownStabilizationSeconds) * time.Second,

				PodHash:        podHash,
				MaxUnavailable: group.UpdateMaxUnavailable(),
				MinAvailable:   group.UpdateMinAvailable(),
				WhenEmpty:      group.UpdateWhenEmpty(),

				PendingCreates: int32(len(pendingCreates)),
				PendingDeletes: pendingDeletes,
				PendingRetires: pendingRetires,

				ChangeoverRefused: changeoverRefused(siblings, budget, self),
			})
			switch {
			// The ceiling, not the budget, holds this group; a place would
			// only keep a sibling waiting.
			case decision.ColdStartBlocked:
			case own == spawneryv1alpha1.ChangeoverWaiting && decision.Create > 0 && backoff.MayCreate:
				group.Status.Changeover = spawneryv1alpha1.ChangeoverBegun
			default:
				group.Status.Changeover = own
			}
		}
	case group.IsOnDemand():
		// No size can be decided: members exist because somebody asked for them
		// by name. The persistent path would read the nil spec.replicas as zero.
	default:
		own := ownPersistentChangeover(views, podHash, pendingDeletes, group.DesiredReplicas(), was)
		self.State = own
		refused := changeoverRefused(siblings, budget, self)
		decision = DecidePersistentSize(PersistentInputs{
			Group:             group.Name,
			Replicas:          group.DesiredReplicas(),
			PodHash:           podHash,
			Views:             views,
			PendingCreates:    pendingCreates,
			PendingDeletes:    pendingDeletes,
			ChangeoverRefused: refused,
		})
		decision.ChangeoverWaiting = refused
		group.Status.Changeover = own
		if own == spawneryv1alpha1.ChangeoverWaiting && decision.DeleteReason == "StaleSpec" {
			group.Status.Changeover = spawneryv1alpha1.ChangeoverBegun
		}
	}

	// Attached here for every path out of the switch, rather than by each
	// branch; DecideSize's own attachment yields the same names.
	decision.Condemn = condemned(ScalingInputs{Views: views, PendingDeletes: pendingDeletes})

	// The backoff gates execution, not the decision, so Limited and
	// ColdStartBlocked still report the shortfall while it waits. It gates only
	// creation: deletes, condemnations and retirements touch players and must not
	// wait on an unrelated failure. createPersistentServer also returns the name
	// on AlreadyExists, and that is reserved too: a reservation stops the next
	// pass asking for the same name.
	clearOrdinalBlocked(group)
	if backoff.MayCreate {
		taken := takenNumbers(views, r.Expectations.pendingNumbers(key))
		for i := int32(0); i < decision.Create; i++ {
			// Added to the set so the next create of this pass gets a new number.
			number := NextNumber(taken)
			taken[number] = true
			name, err := r.createServer(ctx, group, podHash, number)
			if err != nil {
				return decision, err
			}
			r.Expectations.expectCreated(key, name, number)
		}
		for _, ordinal := range decision.CreateOrdinals {
			name, err := r.createPersistentServer(ctx, group, ordinal, podHash)
			if err != nil {
				return decision, err
			}
			// Zero, not the ordinal: zero means "no number" in that set, and
			// DecidePersistentSize reads ordinals off the views.
			r.Expectations.expectCreated(key, name, 0)
		}
	}
	// After the creates, overriding reportSquatter's condition: a duplicated
	// ordinal may already be two pods contending for one ReadWriteOnce volume.
	reportDuplicateOrdinals(group, decision.Conflicts)

	if int32(len(decision.Delete)) < decision.Surplus {
		logger.Info("fewer free servers than the surplus, trying again later",
			"group", group.Name, "surplus", decision.Surplus, "free", len(decision.Delete))
	}
	deleteReason := decision.DeleteReason
	if deleteReason == "" {
		deleteReason = "ServerRemoved"
	}
	for _, name := range decision.Delete {
		if err := r.deleteServer(ctx, group, servers, name,
			deleteReason, "removing server %s"); err != nil {
			return decision, err
		}
		r.Expectations.expectDeleted(key, name)
	}
	if err := r.condemn(ctx, group, servers, key, decision.Condemn); err != nil {
		return decision, err
	}
	for _, name := range decision.Retire {
		if err := r.retireServer(ctx, group, servers, name); err != nil {
			return decision, err
		}
		r.Expectations.expectRetired(key, name)
	}
	return decision, nil
}

// condemn removes the named servers, one delete per server on a node that is
// going away, and reserves each removal.
//
// It is not throttled: every server on a departing node goes in one pass, so
// a persistent group can have more than one ordinal down at once. Draining
// one at a time would make kubectl drain wait drain.timeoutSeconds per
// occupied server, and the node takes its pods regardless. The
// PodDisruptionBudget does not bound these deletes, which bypass eviction.
func (r *ServerGroupReconciler) condemn(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	servers map[string]*spawneryv1alpha1.Server,
	key string,
	names []string,
) error {
	for _, name := range names {
		if err := r.deleteServer(ctx, group, servers, name,
			"NodeDraining", "draining server %s off a node that is going away"); err != nil {
			return err
		}
		r.Expectations.expectDeleted(key, name)
	}
	return nil
}

// storageResizeCondition reports whether any server's claim has a failed
// resize, with the message of the lowest-ordinal one so the condition does not
// flap between two. Kept out of Degraded: a storage class that refuses to grow
// is not a group whose servers will not start.
func storageResizeCondition(views []ServerView) metav1.Condition {
	cond := metav1.Condition{
		Type:    spawneryv1alpha1.ConditionStorageResize,
		Status:  metav1.ConditionTrue,
		Reason:  spawneryv1alpha1.ReasonStorageResized,
		Message: "no claim reports a failed resize",
	}
	var worst *ServerView
	for i := range views {
		v := &views[i]
		if v.ResizeError == "" {
			continue
		}
		if worst == nil || ordinalBefore(v, worst) {
			worst = v
		}
	}
	if worst == nil {
		return cond
	}
	cond.Status = metav1.ConditionFalse
	cond.Reason = spawneryv1alpha1.ReasonStorageResizeRefused
	cond.Message = worst.ResizeError
	return cond
}

// ordinalBefore sorts a nil ordinal (an adopted or hand-made object) last.
func ordinalBefore(a, b *ServerView) bool {
	switch {
	case a.Ordinal == nil:
		return false
	case b.Ordinal == nil:
		return true
	default:
		return *a.Ordinal < *b.Ordinal
	}
}

// derivePhase maps the totals and conditions onto the group phase.
func derivePhase(group *spawneryv1alpha1.ServerGroup, totals GroupTotals) string {
	if meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		return "Degraded"
	}
	// Zero against zero is Ready: spec.replicas: 0 is how a persistent group
	// is parked with its claims kept.
	if totals.ReadyReplicas >= group.DesiredReplicas() {
		return string(phase.Ready)
	}
	return string(phase.Pending)
}

// groupPods lists this group's server pods, terminating ones included:
// forwardingStamps, in the only caller, already drops them.
func (r *ServerGroupReconciler) groupPods(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(group.Namespace),
		client.MatchingLabels(podspec.ServerGroupSelector(group.Spec.NetworkRef.Name, group.Name))); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// reportProgressing says whether the group has arrived where it decided to be,
// which derivePhase does not: an ephemeral group runs above its floor, and
// ReadyReplicas counts old-spec servers. A current-spec server not yet up is
// the group starting; an old-spec one still present is the group replacing.
// One pass behind on a scale-up, like the rest of the status. Draining,
// Terminating and Failed current servers are not counted.
// FloorReport is a changeover held at spec.update.minAvailable; the zero value
// is none.
type FloorReport struct {
	Joinable, Min int32
}

func reportProgressing(group *spawneryv1alpha1.ServerGroup, views []ServerView, podHash string, wait ChangeoverWait, floor FloorReport) {
	var starting, older int32
	// A failed server that carries spec.retire holds a maxUnavailable slot for
	// its whole failed retention, and the changeover stops looking finished.
	// Reached when a readiness loss beats the retirement in phase.Decide. Named,
	// because the remedy is deleting that server.
	var stuck []string
	for _, v := range views {
		if v.Retire && v.Phase == phase.Failed {
			stuck = append(stuck, v.Name)
		}
		// An on-demand member of an earlier spec is not a replacement in flight:
		// nothing rolls it, so counting it would hold the condition True forever.
		// A failed server was replaced when it failed.
		if !group.IsOnDemand() && staleSpec(v, podHash) && !v.Hold && v.Phase != phase.Failed {
			older++
			continue
		}
		if v.Phase == phase.Pending || v.Phase == phase.Starting {
			starting++
		}
	}
	sort.Strings(stuck)

	condition := metav1.Condition{Type: spawneryv1alpha1.ConditionProgressing}
	switch {
	case wait.Reason != "":
		condition.Status = metav1.ConditionTrue
		condition.Reason = wait.Reason
		condition.Message = wait.Message
	// Before the counts: older > 0 would say "still being replaced" while
	// nothing is.
	case len(stuck) > 0:
		condition.Status = metav1.ConditionTrue
		condition.Reason = spawneryv1alpha1.ReasonRetireeStuck
		condition.Message = fmt.Sprintf(
			"the update is held by %s, which carries spec.retire and has failed: a failed retiree holds an "+
				"update slot for its whole failedRetentionSeconds (%ds), so no further server retires until "+
				"then. Deleting it returns the slot immediately.",
			strings.Join(stuck, ", "), group.Spec.FailedRetentionSeconds)
	case floor.Min > 0 && starting == 0:
		condition.Status = metav1.ConditionTrue
		condition.Reason = spawneryv1alpha1.ReasonWaitingForMinAvailable
		condition.Message = fmt.Sprintf(
			"%d server(s) joinable, minAvailable %d: the next stale server waits for an extra server "+
				"that is not being built (see ScalingLimited and BackingOff)", floor.Joinable, floor.Min)
	case starting > 0:
		condition.Status = metav1.ConditionTrue
		condition.Reason = spawneryv1alpha1.ReasonServersStarting
		condition.Message = fmt.Sprintf("%d server(s) of the group's current spec are not ready yet",
			starting)
		if older > 0 {
			condition.Message += fmt.Sprintf("; %d of an earlier spec are still being replaced", older)
		}
	case older > 0:
		condition.Status = metav1.ConditionTrue
		condition.Reason = spawneryv1alpha1.ReasonReplacingServers
		condition.Message = fmt.Sprintf("%d server(s) of an earlier spec are still being replaced", older)
	default:
		condition.Status = metav1.ConditionFalse
		condition.Reason = spawneryv1alpha1.ReasonAtDesiredState
		condition.Message = "every server is of the group's current spec and ready"
		// The loop never asked whether on-demand members carry the current spec.
		if group.IsOnDemand() {
			condition.Message = "no member is starting; each carries the spec it started with"
		}
	}
	meta.SetStatusCondition(&group.Status.Conditions, condition)
}

func playableSpec(group *spawneryv1alpha1.ServerGroup) int32 {
	if group.Spec.PlayableSlots == nil {
		return 0
	}
	return *group.Spec.PlayableSlots
}

// collectViews reads every Server of the group plus its live player count.
func (r *ServerGroupReconciler) collectViews(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
) ([]ServerView, map[string]*spawneryv1alpha1.Server, error) {
	list := &spawneryv1alpha1.ServerList{}
	if err := r.List(ctx, list, client.InNamespace(group.Namespace)); err != nil {
		return nil, nil, err
	}

	views := make([]ServerView, 0, len(list.Items))
	byName := make(map[string]*spawneryv1alpha1.Server, len(list.Items))

	for i := range list.Items {
		srv := &list.Items[i]
		if srv.Spec.GroupRef.Name != group.Name {
			continue
		}
		byName[srv.Name] = srv

		// The registry, not the throttled status.
		pod, podFound := r.podFor(ctx, srv)
		snap := r.Agents.Lookup(podUID(pod, podFound))
		players, slots := clampReport(snap.Players, snap.Slots, group.Spec.MaxPlayers)
		if players != snap.Players || slots != snap.Slots {
			log.FromContext(ctx).V(1).Info("agent report clamped to the group's capacity",
				"server", srv.Name, "reportedPlayers", snap.Players, "reportedSlots", snap.Slots,
				"maxPlayers", group.Spec.MaxPlayers)
		}
		v := ServerView{
			Name:     srv.Name,
			Ordinal:  srv.Spec.Ordinal,
			Number:   srv.Spec.Number,
			Phase:    phase.Phase(srv.Status.Phase),
			Players:  players,
			Slots:    slots,
			Playable: playableSeats(snap.PlayableSlots, group.Spec.PlayableSlots, slots),
			EmptyFor: snap.EmptyFor,
			Stale:    snap.PlayersStale,
			// From the status, never guessed from the phase.
			WasRegistered: srv.Status.WasRegistered,
			// What the proxies have now, which capacity depends on.
			Registered: srv.Status.Registered,
			// AcceptingJoins is true for a pod the registry has never heard from,
			// so after an operator restart every door reads open until restated.
			JoinsClosed:  !snap.AcceptingJoins,
			SessionsGone: srv.Status.PodName != "" && (!podFound || podTerminal(pod)),
			Generation:   srv.Spec.GroupGeneration,
			PodHash:      srv.Spec.PodHash,
			Retire:       srv.Spec.Retire,
			Hold:         srv.Spec.Hold,
			CreatedAt:    srv.CreationTimestamp.Time,
			// Without a readable live pod there is nothing to condemn: no pod, or one
			// already being removed. A failed Get declines too, as nodeDeparting does
			// for an unreadable Node; the next pass asks again.
			Condemned:     podFound && nodeDeparting(ctx, r.Client, pod.Spec.NodeName, r.DrainTaintKeys),
			ResizePending: srv.Status.StorageResizePending,
			ResizeError:   srv.Status.StorageResizeError,
		}
		if podFound {
			v.NodeName = pod.Spec.NodeName
		}
		if srv.Status.FailedAt != nil {
			v.FailedAt = srv.Status.FailedAt.Time
		}
		if srv.Status.ReadySince != nil {
			v.ReadySince = srv.Status.ReadySince.Time
		}
		if v.Phase == "" {
			v.Phase = phase.Pending
		}
		views = append(views, v)
	}
	return views, byName, nil
}

// podFor resolves the pod of a server. An unresolvable pod yields found=false,
// whose snapshot is "unknown, therefore stale". A terminating pod counts as
// gone, as in the Server controller.
func (r *ServerGroupReconciler) podFor(ctx context.Context, srv *spawneryv1alpha1.Server) (*corev1.Pod, bool) {
	if srv.Status.PodName == "" {
		return nil, false
	}
	pod := &corev1.Pod{}
	key := types.NamespacedName{Name: srv.Status.PodName, Namespace: srv.Namespace}
	if err := r.Get(ctx, key, pod); err != nil {
		return nil, false
	}
	if !pod.DeletionTimestamp.IsZero() {
		return pod, false
	}
	return pod, true
}

// newServer builds the Server object both create paths write, short of the
// name and spec.ordinal.
func (r *ServerGroupReconciler) newServer(
	group *spawneryv1alpha1.ServerGroup,
	name string,
	podHash string,
) (*spawneryv1alpha1.Server, error) {
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: group.Namespace,
			Labels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelNetwork:   group.Spec.NetworkRef.Name,
				podspec.LabelGroup:     group.Name,
			},
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef:        spawneryv1alpha1.ObjectRef{Name: group.Name},
			GroupGeneration: group.Generation,
			PodHash:         podHash,
		},
	}
	if err := controllerutil.SetControllerReference(group, srv, r.Scheme); err != nil {
		return nil, err
	}
	return srv, nil
}

// createServer creates one interchangeable server of an ephemeral group.
func (r *ServerGroupReconciler) createServer(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	podHash string,
	number int32,
) (string, error) {
	srv, err := r.newServer(group, NewServerName(group.Name), podHash)
	if err != nil {
		return "", err
	}
	srv.Spec.Number = number
	if err := r.Create(ctx, srv); err != nil {
		return "", err
	}
	r.Recorder.Eventf(group, nil, corev1.EventTypeNormal, "ServerCreated", actionCreateServer,
		"created server %s", srv.Name)
	return srv.Name, nil
}

// createPersistentServer creates the server holding one ordinal, under a
// derived name: a stable name makes the claim name stable, which keeps the
// world.
func (r *ServerGroupReconciler) createPersistentServer(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	ordinal int32,
	podHash string,
) (string, error) {
	srv, err := r.newServer(group, PersistentServerName(group.Name, ordinal), podHash)
	if err != nil {
		return "", err
	}
	srv.Spec.Ordinal = &ordinal
	// Agreeing with the name (survival-0) is worth persistent numbers starting
	// at 0 where ephemeral ones start at 1.
	srv.Spec.Number = ordinal
	if err := r.Create(ctx, srv); err != nil {
		// A derived name can already be taken, usually by this reconciler's own
		// create that the cache has not shown yet. Failing would stop the status
		// and PDB too.
		if !apierrors.IsAlreadyExists(err) {
			return "", err
		}
		return srv.Name, r.reportSquatter(ctx, group, srv.Name, ordinal)
	}
	r.Recorder.Eventf(group, nil, corev1.EventTypeNormal, "ServerCreated", actionCreateServer,
		"created server %s", srv.Name)
	return srv.Name, nil
}

// reportSquatter decides whether an AlreadyExists on a derived ordinal name is
// a cache catching up (the object carries the wanted spec.ordinal) or an object
// that will hold the name until a person removes it, and says so on the group
// in the second case. The collision itself is no error: one blocked ordinal
// must not stop the status and PDB.
func (r *ServerGroupReconciler) reportSquatter(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	name string,
	ordinal int32,
) error {
	existing := &spawneryv1alpha1.Server{}
	err := r.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case existing.Spec.Ordinal != nil && *existing.Spec.Ordinal == ordinal:
		// This reconciler's own object, one cache generation behind.
		return nil
	}

	held := "no spec.ordinal at all"
	if existing.Spec.Ordinal != nil {
		held = fmt.Sprintf("spec.ordinal %d", *existing.Spec.Ordinal)
	}
	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:   spawneryv1alpha1.ConditionOrdinalBlocked,
		Status: metav1.ConditionTrue,
		Reason: spawneryv1alpha1.ReasonOrdinalNameTaken,
		Message: fmt.Sprintf(
			"ordinal %d cannot be created: %q already exists with %s, so it is not a member "+
				"of this group and the ordinal stays missing until that object is removed",
			ordinal, name, held),
	})
	return nil
}

// reportDuplicateOrdinals publishes what DecidePersistentSize refused to act
// on, and does nothing without a duplicate. It names every colliding server
// but suggests none to delete: the operator cannot tell which claim holds the
// world anybody cares about.
func reportDuplicateOrdinals(group *spawneryv1alpha1.ServerGroup, conflicts []OrdinalConflict) {
	if len(conflicts) == 0 {
		return
	}
	parts := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		parts = append(parts, fmt.Sprintf("%d (%s)", c.Ordinal, strings.Join(c.Servers, ", ")))
	}
	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:   spawneryv1alpha1.ConditionOrdinalBlocked,
		Status: metav1.ConditionTrue,
		Reason: spawneryv1alpha1.ReasonOrdinalDuplicated,
		Message: fmt.Sprintf(
			"more than one server carries the same spec.ordinal: %s. This group will not remove, "+
				"replace or resize those ordinals until one server per ordinal remains, because it "+
				"cannot tell which of them holds the world. Check each server's claim before deleting "+
				"either.",
			strings.Join(parts, "; ")),
	})
}

// clearOrdinalBlocked runs every pass before the creates, so a True is this
// pass's own finding rather than one that latched.
func clearOrdinalBlocked(group *spawneryv1alpha1.ServerGroup) {
	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionOrdinalBlocked,
		Status:  metav1.ConditionFalse,
		Reason:  spawneryv1alpha1.ReasonOrdinalsAvailable,
		Message: "every ordinal is free to create and none is carried by two servers",
	})
}

func (r *ServerGroupReconciler) deleteServer(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	servers map[string]*spawneryv1alpha1.Server,
	name, reason, message string,
) error {
	srv, ok := servers[name]
	if !ok {
		return nil
	}
	// Already asked for; repeating would emit the event every resync.
	if !srv.DeletionTimestamp.IsZero() {
		return nil
	}
	if err := r.Delete(ctx, srv); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	r.Recorder.Eventf(group, nil, corev1.EventTypeNormal, reason, actionDeleteServer, message, name)
	return nil
}

// retireServer asks one server to enter soft drain, by patch: the Server
// controller writes status on the same object.
func (r *ServerGroupReconciler) retireServer(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	servers map[string]*spawneryv1alpha1.Server,
	name string,
) error {
	srv, ok := servers[name]
	if !ok {
		return nil
	}
	// Already asked; the cache lags the patch by a reconcile or two.
	if srv.Spec.Retire {
		return nil
	}
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.Retire = true
	if err := r.Patch(ctx, srv, patch); err != nil {
		return err
	}
	r.Recorder.Eventf(group, nil, corev1.EventTypeNormal, "ServerRetiring", actionRetireServer,
		"retiring server %s", name)
	return nil
}

// adoptServers stamps the current render hash onto every server whose
// spec.podHash is still empty. A hashless view is never stale, so an unstamped
// server would never be replaced.
func (r *ServerGroupReconciler) adoptServers(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	servers map[string]*spawneryv1alpha1.Server,
	podHash string,
) error {
	for _, srv := range servers {
		if srv.Spec.PodHash != "" {
			continue
		}
		// Adopted rather than stale, which would restart every persistent world
		// on upgrade. The cost: a spec edit landing in this same reconcile is
		// adopted too, until the next edit.
		patch := client.MergeFrom(srv.DeepCopy())
		srv.Spec.PodHash = podHash
		if err := r.Patch(ctx, srv, patch); err != nil {
			return err
		}
	}
	return nil
}

// pruneFailed keeps the number of retained failures per group at
// maxRetainedFailures. Not gated on the Network: a group whose Network was
// deleted is exactly the one that piles failures up.
func (r *ServerGroupReconciler) pruneFailed(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	views []ServerView,
	servers map[string]*spawneryv1alpha1.Server,
) error {
	names := selectFailedForPruning(views, maxRetainedFailures)
	if len(names) == 0 {
		return nil
	}
	log.FromContext(ctx).Info("pruning retained failures past the cap",
		"group", group.Name, "pruned", len(names), "kept", maxRetainedFailures)
	for _, name := range names {
		if err := r.deleteServer(ctx, group, servers, name, "FailedServerPruned",
			"removing failed server %s, only the newest generation's oldest failure is kept for diagnosis"); err != nil {
			return err
		}
	}
	return nil
}

// reconcilePDB keeps the group's PodDisruptionBudget in step with the number
// of occupied pods. Without a scale subresource, Kubernetes allows neither
// maxUnavailable nor percentages, so minAvailable is absolute.
//
// A PDB left at the bare group.Name by an older installation is neither
// renamed nor deleted here.
//
// The LabelRole term matters: proxy pods of a same-named ProxyGroup carry the
// occupied label too, and matching them would let the eviction API spend their
// disruptions on occupied server pods. See isOccupied.
func (r *ServerGroupReconciler) reconcilePDB(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	views []ServerView,
) error {
	minAvailable := intstr.FromInt32(occupiedPods(views))

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podspec.GroupPDBName(group.Name, podspec.RoleServer),
			Namespace: group.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Spec.MinAvailable = &minAvailable
		pdb.Spec.MaxUnavailable = nil
		pdb.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelGroup:     group.Name,
				podspec.LabelRole:      podspec.RoleServer,
				podspec.LabelOccupied:  "true",
			},
		}
		return controllerutil.SetControllerReference(group, pdb, r.Scheme)
	})
	return err
}

// serverConfigValues is the config document a server group renders, built in
// one place for reconcileConfigMap and DesiredServerHash: two constructions
// would drift, and an update would never fire. It carries only spec.maxPlayers;
// the critical settings live in internal/render's critical layer.
func serverConfigValues(group *spawneryv1alpha1.ServerGroup) ([]byte, error) {
	maxPlayers := group.Spec.MaxPlayers
	data, err := yaml.Marshal(render.Values{MaxPlayers: &maxPlayers})
	if err != nil {
		return nil, fmt.Errorf("marshal config.yaml for group %s: %w", group.Name, err)
	}
	return data, nil
}

func (r *ServerGroupReconciler) reconcileConfigMap(ctx context.Context, group *spawneryv1alpha1.ServerGroup) error {
	data, err := serverConfigValues(group)
	if err != nil {
		return err
	}
	return reconcileGroupConfigMap(ctx, r.Client, r.Scheme, group,
		podspec.GroupConfigMapName(group.Name, podspec.RoleServer), data)
}

// groupsOfNetwork maps a Network event onto the ServerGroups in its namespace
// that name it; a group pointed at a losing duplicate Network is not woken by
// the winner. A List error only costs latency, the resync follows.
func (r *ServerGroupReconciler) groupsOfNetwork(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &spawneryv1alpha1.ServerGroupList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.NetworkRef.Name != obj.GetName() {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].Namespace,
			Name:      list.Items[i].Name,
		}})
	}
	return out
}

// groupsOnNode maps a Node event onto the ServerGroups with pods on that node,
// so a cordon is answered before an eviction in the same second.
//
// A label-scoped list instead of a spec.nodeName index: an index would have to
// be registered once and shared by two controllers, for a small population.
func (r *ServerGroupReconciler) groupsOnNode(ctx context.Context, obj client.Object) []reconcile.Request {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingLabels{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelRole:      podspec.RoleServer,
	}); err != nil {
		// A map function cannot return an error; the resync is the fallback.
		log.FromContext(ctx).V(1).Info("listing server pods for a node event failed, "+
			"leaving this cordon to the resync", "node", obj.GetName(), "error", err)
		return nil
	}
	seen := map[types.NamespacedName]bool{}
	var out []reconcile.Request
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != obj.GetName() {
			continue
		}
		group := pod.Labels[podspec.LabelGroup]
		if group == "" {
			continue
		}
		key := types.NamespacedName{Name: group, Namespace: pod.Namespace}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, reconcile.Request{NamespacedName: key})
	}
	return out
}

// SetupWithManager registers the controller.
func (r *ServerGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Expectations == nil {
		r.Expectations = newExpectations(r.Clock)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.ServerGroup{}).
		Owns(&spawneryv1alpha1.Server{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		// A boost carries an owner reference to its group.
		Owns(&spawneryv1alpha1.ScaleBoost{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.groupsOnNode)).
		// A group refused for its Network is no owner of it and would otherwise
		// wait out the resync.
		Watches(&spawneryv1alpha1.Network{},
			handler.EnqueueRequestsFromMapFunc(r.groupsOfNetwork)).
		Named("servergroup").
		Complete(r)
}
