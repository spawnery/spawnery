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
	"crypto/rand"
	"fmt"
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

	// Agents is the runtime state reported by the in-game agents.
	Agents *agent.Registry
	// Clock is injectable so the time rules are testable.
	Clock func() time.Time
	// Expectations reserves the creates and deletes this reconciler has issued
	// and the cache has not shown yet. One instance is shared across groups.
	Expectations *expectations
	// DrainTaintKeys is Options.DrainTaintKeys. Nil means only cordoned nodes
	// count.
	DrainTaintKeys []string
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups,verbs=get;list;watch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups/status,verbs=update
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups/finalizers,verbs=update
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile sizes the group and updates its status.
func (r *ServerGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		if apierrors.IsNotFound(err) {
			// No ServerGroup finalizer exists, so a deleted group is gone from
			// the API server before any reconcile can observe its deletion
			// timestamp. This is the only path most deletions take, and without
			// it a group's reservations outlive it for the life of the process:
			// expectationTTL is applied inside observe, and observe is only
			// ever called for a group that still exists.
			r.Expectations.forget(req.Namespace + "/" + req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !group.DeletionTimestamp.IsZero() {
		// The Server objects are owned by the group; Kubernetes garbage
		// collection cascades, and each Server drains through its finalizer.
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
	// A Network that exists is not automatically usable: it also has to have
	// won the Network controller's one-per-namespace contest. A Network that
	// lost that contest, or one that simply has not been reconciled yet,
	// reads the same way here — Accepted is not set to true — and both are
	// self-healing through the requeue below, so there is no need to tell
	// them apart.
	networkUsable := networkFound && meta.IsStatusConditionTrue(network.Status.Conditions, spawneryv1alpha1.ConditionAccepted)

	requeue := resyncInterval
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
	default:
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionTrue,
			Reason:  spawneryv1alpha1.ReasonAccepted,
			Message: fmt.Sprintf("managed as part of network %q", network.Name),
		})
	}

	// Rendered before anything that can create a Server, and unconditionally —
	// not gated on networkUsable, because the ConfigMap has nothing to do with
	// the Network, and a spec.maxPlayers edit has to reach it even on a
	// resync that creates no new server. A pod's projected volume names this
	// ConfigMap by group (podspec.GroupConfigMapName), so it must exist
	// before the first pod; failing here returns before createServer runs,
	// which is what makes that a guarantee and not just where the calls
	// happen to be written.
	if err := r.reconcileConfigMap(ctx, group); err != nil {
		return ctrl.Result{}, err
	}

	views, servers, err := r.collectViews(ctx, group)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Carries the node names collectViews already resolved (ServerView.NodeName)
	// for every view it found condemned, rather than asking nodeDeparting about
	// any pod again here. No event accompanies this: condemn(), reached from
	// size() below, already emits one NodeDraining event per server it
	// condemns, and a second event tied to this condition's own transition
	// would report the same occasion twice.
	//
	// This condition and that condemnation are computed from the same
	// Condemned flags on the same views, and size() is now called whether or
	// not the group's Network is usable — so a node named here is one whose
	// servers this group has actually condemned, on this pass or on an
	// earlier one whose reservation is still standing. That was not true
	// before: a group with a broken Network published this condition naming
	// the node and condemned nobody, ever, which left kubectl drain hanging on
	// the strength of a status saying the operator was on it.
	drainingNodes := make([]string, 0, len(views))
	for _, v := range views {
		if v.Condemned {
			drainingNodes = append(drainingNodes, v.NodeName)
		}
	}
	meta.SetStatusCondition(&group.Status.Conditions, drainingCondition(drainingNodes))

	// The counter and the two conditions belong to the spec that produced the
	// failures. A generation change is the operator's answer to whatever broke,
	// so the streak it caused is over and the next attempt is immediate.
	if group.Generation != group.Status.ObservedGeneration {
		group.Status.ConsecutiveFailures = 0
		group.Status.LastFailureAt = nil
		// The two conditions need no explicit removal: the BackingOff/Degraded
		// switch below (the one with the "!sized" case) republishes both with
		// meta.SetStatusCondition unconditionally on every pass, for a group
		// of either type, including the case where nothing was decided — so a
		// Remove here would be overwritten before it could ever be observed.
		//
		// "Either type" is newer than the sentence above it and is what makes
		// it true. While the two conditions were published only inside an
		// `if group.IsEphemeral()` block, the sentence held for a persistent
		// group only by accident: nothing published them there, so this reset
		// had nothing to overwrite and equally nothing to remove. Now they are
		// published for both, and the republishing below is what carries the
		// reset onto the status of either — so if either condition is ever put
		// back behind a type test, this reset needs a Remove of its own.
	}

	var lastFailure time.Time
	if group.Status.LastFailureAt != nil {
		lastFailure = group.Status.LastFailureAt.Time
	}
	// countViews and requiredOrdinals are chosen by type rather than read off
	// group.DesiredReplicas(): that accessor returns spec.scaling.minReplicas
	// for an ephemeral group, not the zero CountFailures needs to select its
	// maximum-of-all-views rule.
	countViews, requiredOrdinals := views, group.DesiredReplicas()
	if group.IsEphemeral() {
		countViews, requiredOrdinals = ofGeneration(views, group.Generation), 0
	}
	failures, newestFailure := CountFailures(countViews,
		group.Status.ConsecutiveFailures, lastFailure, requiredOrdinals)
	group.Status.ConsecutiveFailures = failures
	// Only written when this pass actually counted something. CountFailures
	// returns the timestamp it counted from unchanged when it counted nothing,
	// including when a success reset the streak to zero — so what stays on the
	// status is the watermark of the newest failure already counted, which is
	// what keeps the count idempotent across a five-second resync. Clearing it
	// on a reset would be the opposite of durable: the next pass would count
	// from the zero time and every retained corpse again with it. Moving it
	// forward to the success instead would be durable, but it would write a
	// time at which nothing failed into a status field an operator reads as
	// lastFailureAt. The residual edge — a corpse older than a success that has
	// since left the views being counted once more — is bounded by how many
	// corpses the group can be holding at all, and that number differs by
	// type. For an ephemeral group it is what pruneFailed retains, which is
	// one. pruneFailed does not run for a persistent group: there each corpse
	// keeps its ordinal until its own failed retention elapses and the Server
	// controller removes it, so the bound is one per ordinal, up to
	// spec.replicas.
	if !newestFailure.IsZero() {
		stamped := metav1.NewTime(newestFailure)
		group.Status.LastFailureAt = &stamped
	}
	backoff := DecideBackoff(BackoffInputs{
		ConsecutiveFailures: failures,
		LastFailureAt:       newestFailure,
		Now:                 r.Clock(),
	})

	// The Network gate, and the rule for deciding which side of it a step
	// belongs on. The next person adding a step here needs the rule, not just
	// the list.
	//
	// Sizing needs a usable Network: a Server created without one could never
	// get a pod, would run into its startup deadline and would be replaced
	// over and over. That holds whether the Network is missing entirely or
	// merely not accepted (lost the one-per-namespace contest, or has not been
	// reconciled yet). The deletes and retirements are the other half of the
	// same arithmetic and wait with it.
	//
	// The rule for everything else: a step that keeps the eviction API off an
	// occupied pod, or that moves players off a node that is going away, does
	// not depend on the Network. Both are about players who are already
	// connected, and a rejected group holding players is still holding
	// players. Two steps answer to that rule, and both run whatever the
	// Network is doing: the PodDisruptionBudget below, and the condemnation of
	// the servers on a departing node — which is why size() now runs on every
	// pass regardless of this flag, branching internally on the mayResize it
	// is given rather than being skipped from the call site. The published
	// status is below the line too, but for the plainer reason that it reports
	// what those two did. Design §3.3 calls condemnation "unconditional. The
	// node is leaving with or without our consent", and a group that published
	// NodeDraining: True, condemned nothing and hung kubectl drain forever
	// would be the hang §1 of that design opens by describing.
	//
	// The counter-argument is real, and is answered rather than left implicit:
	// a group whose Network is broken cannot build replacements, so condemning
	// means running below capacity. That is accepted. The group was already in
	// that state; its players are evicted off that node regardless of what the
	// group thinks; and moving them to a fallback group beats holding them on
	// a node that is going away. It is the same direction already settled for
	// the create-backoff case, which docs/known-issues.md documents under "a
	// group in create-backoff condemns without replacing".
	//
	// The group's type is not part of this flag, and that is the second half of
	// the same rule. The Network gates sizing of either kind, because neither
	// kind can render a pod without one; the type selects *which* rule sizes
	// the group — the spare-slot rule for an ephemeral group, spec.replicas for
	// a persistent one — and size() branches on it internally. A persistent
	// group's servers are condemned on the way through either way, which is
	// what known-issues already describes: a departing node takes a server
	// regardless of what kind of server it is.
	mayResize := networkUsable

	// Gated the same as sizing, on mayResize rather than a check of its own:
	// BuildServerPod reads net.Spec.Defaults, and when the Network was never
	// found net is the zero value, so a hash computed from it would not
	// describe what the group renders once a real Network is usable. The
	// found-but-not-accepted case does not strictly need this -- that net
	// already carries the real spec -- but splitting it out would need its own
	// branch on networkFound for a case sizing itself does not distinguish
	// either, and reusing mayResize costs nothing: it self-heals the same way
	// sizing does, on the next pass.
	//
	// Computed once per pass rather than once per server either way: a create
	// loop that asked BuildServerPod once per server would render the same pod
	// repeatedly for one identical answer. serverConfigValues is the one
	// construction of the config document (see its own comment);
	// DesiredServerHash is the digest of it plus the pod.
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

		// Adoption runs before sizing, on a persistent group only: a server
		// whose spec.podHash is still empty predates the field (ServerSpec.
		// PodHash's doc comment is what says an empty value means adopt rather
		// than stale), and this is the pass that gives it the current hash so
		// a later comparison has one to compare against. It orders no takedown
		// of its own -- nothing reads podHash to decide a removal yet, which
		// is Task 4's rule -- so the stamp is the whole effect here.
		if err := r.adoptServers(ctx, group, servers, podHash); err != nil {
			return ctrl.Result{}, err
		}
	}

	decision, err := r.size(ctx, group, views, servers, backoff, mayResize, podHash)
	if err != nil {
		return ctrl.Result{}, err
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
				// The cold-start-refused case: Wanted and Create are both 0,
				// so the ordinary-shortfall message below would tell the
				// operator nothing is needed — the opposite of the truth.
				// The group is at its ceiling and the changeover cannot
				// begin; raising maxReplicas by one is the way out.
				limited.Message = fmt.Sprintf(
					"changeover cannot begin: the group is already at maxReplicas %d; raise it by at least 1 to start the new generation",
					group.Spec.Scaling.MaxReplicas)
			} else {
				limited.Message = fmt.Sprintf(
					"%d more server(s) needed to cover spareSlots %d; maxReplicas %d allows %d now",
					decision.Wanted, group.Spec.Scaling.SpareSlots,
					group.Spec.Scaling.MaxReplicas, decision.Create)
			}
		}
		if !sized {
			// Nothing was decided this pass, so the False above is the absence
			// of a verdict rather than one. Saying "free slots cover the spare"
			// here would assert something no code checked, and an all-clear
			// event would announce it.
			limited.Message = "scaling is not being decided: the group's network is not usable"
		}
		// The event goes on the flank only. SetStatusCondition moves
		// lastTransitionTime just on a change of status, so comparing across
		// the call is what tells a transition from a resync.
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

	// StorageResize is the persistent-only counterpart to ScalingLimited
	// above: each condition belongs to the group type whose question it
	// answers, and an ephemeral group has no claim for this one to be about
	// -- group.Spec.Storage is a persistent group's field, and growClaim in
	// the Server controller is skipped outright for an ephemeral one, so
	// every view's ResizeError here would read "" for an ephemeral group
	// regardless.
	//
	// Nothing here talks to a claim directly: storageResizeCondition only
	// reads ResizeError off the views collectViews already built, the same
	// separation ResizePending's own comment describes. growClaim and its
	// neighbour resizeConditionError, in the Server controller, are the ones
	// that decided what each server's ResizeError says.
	if !group.IsEphemeral() {
		resize := storageResizeCondition(views)
		// The event goes on the flank only, the rule ScalingLimited above
		// and BackingOff/Degraded below both follow. What differs here is
		// which value is the interesting one to flank on: True is the
		// healthy default for this condition, the reverse of those three, so
		// what is compared is refusal (Status == False) rather than health —
		// a group publishing this condition for the first time while every
		// claim already matches its spec stays quiet instead of announcing
		// an all-clear nobody asked about.
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

	// BackingOff and Degraded belong to a group of either type, and the line
	// between them and ScalingLimited above is what kind of question each
	// answers. ScalingLimited is about capacity the players need: free slots
	// against spec.scaling.spareSlots, and the maxReplicas ceiling that can
	// stop the group covering them. A persistent group has no such question --
	// its size is spec.replicas, a number nobody's play session moves -- so
	// the condition stays inside the block above, where a reader looking for
	// it will find it beside the rule it reports on.
	//
	// These two are about failures, and either kind of group can have those.
	// The backoff already gated a persistent group's creates -- size() runs
	// its CreateOrdinals loop under the same backoff.MayCreate as the
	// ephemeral count -- so until this was lifted out, a persistent group was
	// backing off and giving up in silence, with nothing on its status saying
	// why nothing was happening.
	//
	// Its likeliest failure is the one this milestone introduces: a claim that
	// never binds, which no replacement can fix, because the world is on that
	// volume and a rebuilt ordinal would only run at the same one. It does
	// rebuild, slowly: nothing on the group's side removes a persistent server
	// for having failed -- pruneFailed is ephemeral-only and
	// DecidePersistentSize holds an ordinal in any phase, so the removals that
	// do exist here are about other things, a lower spec.replicas or a
	// departing node -- but the Server controller's failed-retention path
	// takes the corpse away eventually, and the ordinal is created again on
	// the next pass. So the attempts are spaced by spec.failedRetentionSeconds,
	// an hour at the CRD default, against backoff windows of at most 160s
	// before the threshold: at that default the waiting half of the backoff
	// never delays an attempt, and what it contributes is the end of them --
	// GaveUp at the threshold, after which the group stalls and waits for a
	// human. Without these two conditions the verdict would be the part nobody
	// could see: status.consecutiveFailures is counted for either type and
	// would climb, but a number is not a verdict, and the phase would read
	// Pending throughout, exactly like a slow start. With them the group says
	// which of the two it is doing, and the give-up reaches derivePhase as
	// Degraded -- which is the whole of what they do for a persistent group,
	// and the whole of why they were lifted out.
	//
	// Both are built false-by-default and flipped, like ScalingLimited above.
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
	// Before !sized, deliberately, and docs/known-issues.md's milestone 4d
	// entry asked for the choice to be a ruling with a test rather than an
	// accident of how the switch was written.
	//
	// Both messages are true of a group that has given up while its Network is
	// also dead. They are not equally useful. The Network's unusability is
	// transient and already has a condition of its own — Accepted: False says
	// it, which is what the !sized case's own comment observes — so putting it
	// here as well spends the only two conditions that can carry the give-up
	// on a fact that is reported elsewhere anyway. And giving up is terminal:
	// it needs a spec edit, so an operator who reads only "the group's network
	// is not usable", fixes the Network and walks away has been told something
	// true and left with a group that will still create nothing.
	//
	// The count that produces GaveUp is computed from the views before sized
	// is known and does not depend on the Network at all, so saying it here is
	// not a guess about a pass that decided nothing.
	case backoff.GaveUp:
		// No pending retry, so BackingOff is false — but an all-clear
		// reason here would be a lie, so it carries the real one.
		backingOff.Reason = spawneryv1alpha1.ReasonCrashLoopBackoff
		backingOff.Message = fmt.Sprintf(
			"not retrying: %d rounds of server starts failed in a row; change the group's spec to try again",
			group.Status.ConsecutiveFailures)
		degraded.Status = metav1.ConditionTrue
		degraded.Reason = spawneryv1alpha1.ReasonCrashLoopBackoff
		degraded.Message = backingOff.Message
	case !sized:
		// Nothing was decided this pass, so the False above is the
		// absence of a verdict rather than one — the same reasoning
		// ScalingLimited's own !sized case gives above. The counting two
		// blocks up runs whether or not the Network is usable, so
		// without this case a group with a dead Network would advertise
		// a wait it is not serving, beside an Accepted: False that
		// already explains the same standstill differently.
		backingOff.Message = "backoff is not being decided: the group's network is not usable"
		degraded.Message = "backoff is not being decided: the group's network is not usable"
	case backoff.RetryAfter > 0:
		backingOff.Status = metav1.ConditionTrue
		backingOff.Reason = spawneryv1alpha1.ReasonCrashLoopBackoff
		backingOff.Message = fmt.Sprintf(
			"%d round(s) of server starts failed in a row; next attempt in %s",
			group.Status.ConsecutiveFailures, backoff.RetryAfter.Round(time.Second))
	default:
		// Nothing has failed this streak. Only reachable with a non-nil
		// LastFailureAt when a past failure's watermark is still stuck
		// on the status (the residual edge Task 1 and Task 4 both
		// accepted) — a genuinely clean history leaves it nil, which the
		// !newestFailure.IsZero() guard above is what keeps true: drop
		// that guard and this renders the zero time instead.
		if group.Status.LastFailureAt != nil {
			backingOff.Message = fmt.Sprintf(
				"no server has failed to start recently (last failure at %s)",
				group.Status.LastFailureAt.Format(time.RFC3339))
		}
	}

	// The event goes on the flank only, for the reason the
	// ScalingLimited block gives: a five-second resync would otherwise
	// announce the same wait over and over for its whole duration.
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

	if group.IsEphemeral() {
		if err := r.pruneFailed(ctx, group, views, servers); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.reconcilePDB(ctx, group, views); err != nil {
		return ctrl.Result{}, err
	}

	totals := AggregateGroup(views, group.Generation)
	group.Status.Replicas = totals.Replicas
	group.Status.ReadyReplicas = totals.ReadyReplicas
	group.Status.OnlinePlayers = totals.OnlinePlayers
	group.Status.FreeSlots = totals.FreeSlots
	group.Status.ObservedGeneration = group.Generation
	group.Status.Phase = derivePhase(group, totals)

	return ctrl.Result{RequeueAfter: requeue}, r.Status().Update(ctx, group)
}

// networkNotAcceptedMessage explains why a Network that exists is still not
// usable, quoting its own Accepted condition when one has been published so
// an operator reading the group does not also have to go look at the Network.
func networkNotAcceptedMessage(network *spawneryv1alpha1.Network) string {
	if cond := meta.FindStatusCondition(network.Status.Conditions, spawneryv1alpha1.ConditionAccepted); cond != nil {
		return fmt.Sprintf("network %q is not accepted (%s): %s", network.Name, cond.Reason, cond.Message)
	}
	return fmt.Sprintf("network %q has not been accepted yet", network.Name)
}

// ofGeneration narrows the views to the servers the group's current spec
// produced. The call site below uses it for an ephemeral group's failure
// count only: a persistent group's ordinals outlive a generation change by
// design, so filtering here would empty every view CountFailures sees on the
// very next spec edit and freeze status.consecutiveFailures wherever it
// stood. requiredOrdinals is what keeps a persistent group's count
// generation-blind instead, and it does so a different way — by ordinal, not
// by spec — which is covered where that parameter is declared.
//
// A streak belongs to the spec that caused it, for the ephemeral group this
// still governs. The previous generation's corpse says nothing about the new
// image — selectFailedForPruning already keeps the newest generation's
// failure for exactly that reason — and a previous generation's server going
// Ready says nothing about it either.
//
// It is also what makes the clear above mean anything. Without it the reset
// would be undone on the very pass that performs it: the counter goes to zero
// and lastFailureAt to nil, and the retained corpse of the spec just replaced,
// now newer than a zero watermark, is counted straight back into it. A group
// that had given up would come back with one failure already against it and a
// window it did not earn, and the "the next attempt is immediate" this whole
// branch exists for would be false.
//
// A later reader will arrive believing the generation is forbidden anywhere
// near this code, so: the standing constraint is that the *capacity
// arithmetic* stays generation-blind. ScalingInputs carries the reason — a
// generation filter in provisionalCapacity or readyFree makes every running
// server stop counting the instant any field of the spec changes, so the group
// orders a full replacement set up to maxReplicas: runaway creates, which is
// the failure that disconnects players.
//
// This is a filter on *counting failures*, and it runs in the other direction.
// The strictest thing it can do is hold a create back. It cannot order one, and
// it never reaches DecideSize at all. Different direction, different rule.
func ofGeneration(views []ServerView, generation int64) []ServerView {
	out := make([]ServerView, 0, len(views))
	for _, v := range views {
		if v.Generation == generation {
			out = append(out, v)
		}
	}
	return out
}

// size brings the group to the size its own rule asks for and reports that
// decision, so Reconcile can publish the part of it that belongs on the
// status. Which rule that is follows from the group's type: the spare-slot
// rule of DecideSize for an ephemeral group, the number in spec.replicas by
// way of DecidePersistentSize for a persistent one. It also condemns the
// servers a departing node has claimed, and that half runs whether or not the
// group may resize.
//
// mayResize is false when the group's Network is unusable, which stops the
// group deciding a size of either kind and does not stop a node leaving. See
// the rule at the call site for why those are different questions. A nil
// spec.scaling on an ephemeral group is a second way to have no size to
// decide, and it lands in the same place for the same reason.
//
// The returned decision carries nothing but Condemn whenever no rule decided a
// size, so a caller publishing ScalingLimited from it says "nothing was
// decided" rather than a verdict nothing computed.
func (r *ServerGroupReconciler) size(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	views []ServerView,
	servers map[string]*spawneryv1alpha1.Server,
	backoff BackoffDecision,
	mayResize bool,
	podHash string,
) (SizeDecision, error) {
	logger := log.FromContext(ctx)
	key := group.Namespace + "/" + group.Name

	// Before the switch below and outside every one of its branches: the
	// reservations are what keep condemned() from naming the same server twice
	// across two passes, so a group that only ever condemns still has to
	// observe them and still has to read them.
	r.Expectations.observe(key, views)
	pendingCreates, pendingDeletes, pendingRetires := r.Expectations.pending(key)

	var decision SizeDecision
	switch {
	case !mayResize:
		// No size is decided, and the condemnation attached below is the whole
		// of what this function does on this path: every loop under it is fed
		// by a field no rule filled in.
	case group.IsEphemeral():
		if group.Spec.Scaling != nil {
			decision = DecideSize(ScalingInputs{
				Views:         views,
				MinReplicas:   group.Spec.Scaling.MinReplicas,
				MaxReplicas:   group.Spec.Scaling.MaxReplicas,
				SpareSlots:    group.Spec.Scaling.SpareSlots,
				MaxPlayers:    group.Spec.MaxPlayers,
				Stabilization: time.Duration(group.Spec.Scaling.ScaleDownStabilizationSeconds) * time.Second,

				Generation:     group.Generation,
				MaxUnavailable: group.UpdateMaxUnavailable(),

				PendingCreates: int32(len(pendingCreates)),
				PendingDeletes: pendingDeletes,
				PendingRetires: pendingRetires,
			})
		}
	default:
		decision = DecidePersistentSize(PersistentInputs{
			Group:          group.Name,
			Replicas:       group.DesiredReplicas(),
			PodHash:        podHash,
			Views:          views,
			PendingCreates: pendingCreates,
			PendingDeletes: pendingDeletes,
		})
	}

	// Condemnation is attached here, once, for every path out of the switch
	// above rather than by each branch remembering to do it. DecideSize
	// attaches it too, and condemned() reads nothing but the two fields handed
	// to it here, so for that branch this assigns the same names a second
	// time; the persistent rule and the two paths that decide no size have no
	// attachment of their own, and this is theirs. Two neighbouring branches
	// that must each remember one step is the shape where one of them
	// eventually does not.
	decision.Condemn = condemned(ScalingInputs{Views: views, PendingDeletes: pendingDeletes})

	// The backoff gate is on execution, not on the decision. DecideSize keeps
	// computing what the group needs, so Limited and ColdStartBlocked go on
	// telling the truth about the shortfall while the backoff separately says
	// the group is waiting — two facts an operator needs to see apart. It also
	// means no create is reserved on a pass where the gate refused every
	// attempt. (The deletes, condemnations and retirements below reserve their
	// own removals whatever the gate said, because the gate does not reach
	// them.)
	//
	// That is narrower than "a reservation implies an object was created", and
	// the difference is worth stating because the sentence above is the one a
	// future reader would otherwise cite for the wider claim.
	// createPersistentServer returns the name on AlreadyExists as well, and the
	// loop below reserves it just the same: a reservation's job is to stop the
	// next pass asking for the same name again, not to record what this pass
	// made.
	//
	// Creation is the only thing the backoff gates. The deletes, condemnations
	// and retirements below run whatever it decided: they touch players, and
	// must not wait on an unrelated failure. The Network gate above is a
	// different question with a different answer, settled at the call site.
	//
	// Two loops under one gate, because the two rules ask for creation in the
	// two different currencies they decide in: a count for the ephemeral rule,
	// which builds interchangeable servers, and a list of ordinals for the
	// persistent one, which builds identities. At most one of them is ever
	// non-empty -- the switch above ran one branch, and neither rule fills the
	// other's field -- and neither loop needs to know that.
	if backoff.MayCreate {
		for i := int32(0); i < decision.Create; i++ {
			name, err := r.createServer(ctx, group, podHash)
			if err != nil {
				return decision, err
			}
			r.Expectations.expectCreated(key, name)
		}
		for _, ordinal := range decision.CreateOrdinals {
			name, err := r.createPersistentServer(ctx, group, ordinal, podHash)
			if err != nil {
				return decision, err
			}
			r.Expectations.expectCreated(key, name)
		}
	}
	if int32(len(decision.Delete)) < decision.Surplus {
		logger.Info("fewer free servers than the surplus, trying again later",
			"group", group.Name, "surplus", decision.Surplus, "free", len(decision.Delete))
	}
	// DeleteReason distinguishes the persistent rule's occasions for the
	// event trail; the ephemeral rule leaves it empty and falls back to the
	// reason it always used.
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
	// Ungated by the backoff, like the deletes and retirements around it and
	// for the same reason: this touches players and must not wait on an
	// unrelated failure. It is ungated by the Network too, because Condemn is
	// attached above whether or not any rule decided a size.
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

// condemn removes the named servers and reserves each removal, one delete per
// server on a node that is going away.
//
// It stayed a method of its own after the Network gate stopped needing a
// second call site for it, because the occasion it names is not the one the
// deletes beside it in size() name — a node leaving rather than a size
// decision — and the event it emits says so. Reserved after the delete,
// matching size()'s other removal loops.
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

// storageResizeCondition reports whether every persistent server's claim
// currently matches spec.storage.size. True is the ordinary case. False
// carries the message the offending server's own status already worked out
// -- growClaim's synchronous rejection, or resizeConditionError's read of
// the claim's ControllerResizeError/NodeResizeError condition -- taken from
// the lowest-ordinal view that has one, deterministically rather than by
// which one collectViews happens to have listed first: collectViews makes no
// ordering promise, so with two unhealthy claims "the first one found" could
// alternate between reconciles and leave an operator watching the condition
// flap between two messages instead of reading one steady signal. Lowest
// ordinal is not a claim that ordinal's cause is any more urgent than the
// other's -- it is only the tie-break that stays put.
//
// Deliberately not folded into Degraded. Design §4's point stands regardless
// of which of the two failure shapes produced the message: a storage class
// that refuses to grow and a group whose servers will not start are
// different problems with different remedies, and derivePhase must not read
// this condition as if it were.
func storageResizeCondition(views []ServerView) metav1.Condition {
	cond := metav1.Condition{
		Type:    spawneryv1alpha1.ConditionStorageResize,
		Status:  metav1.ConditionTrue,
		Reason:  spawneryv1alpha1.ReasonStorageResized,
		Message: "every claim matches spec.storage.size",
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

// ordinalBefore reports whether a's ordinal sorts before b's, for
// storageResizeCondition's tie-break. A nil ordinal does occur among a
// persistent group's views -- an adopted or hand-made object need not carry
// spec.ordinal, which is what DecidePersistentSize's nil-ordinal rule and
// known-issues' squatter entry are about -- and this has nothing to compare
// it against, so it sorts last rather than panicking on the dereference.
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
	// No `&& ReadyReplicas > 0`. A group asked for nothing and running nothing
	// has exactly what it was asked for, and the extra clause made that state
	// report Pending forever -- the truth about every field except the phase.
	//
	// It matters because zero is a deliberate operator action rather than an
	// edge: `spec.replicas: 0` is the accepted way to park a persistent group
	// and keep its claims, since deleting the group would leave the claims
	// behind but take the ordinals' Server objects with it.
	//
	// Dropping the clause changes exactly one state. A group short of its
	// target still fails `ReadyReplicas >= DesiredReplicas()` -- zero ready
	// against five wanted is not suddenly Ready -- so the only pair the clause
	// was deciding was zero against zero.
	if totals.ReadyReplicas >= group.DesiredReplicas() {
		return string(phase.Ready)
	}
	return string(phase.Pending)
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

		// The live count comes from the registry, not from the throttled
		// status: the control loop must decide on fresh data.
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
			Phase:    phase.Phase(srv.Status.Phase),
			Players:  players,
			Slots:    slots,
			EmptyFor: snap.EmptyFor,
			Stale:    snap.PlayersStale,
			// Read from the status, never guessed from the phase: a server that
			// lost its probe is in Starting with its players still connected.
			WasRegistered: srv.Status.WasRegistered,
			// A pod that once existed and is now gone took its sessions with it,
			// exactly like one that reached a terminal state.
			SessionsGone: srv.Status.PodName != "" && (!podFound || podTerminal(pod)),
			Generation:   srv.Spec.GroupGeneration,
			PodHash:      srv.Spec.PodHash,
			Retire:       srv.Spec.Retire,
			CreatedAt:    srv.CreationTimestamp.Time,
			// podFound is required, and it is what makes pod safe to
			// dereference here. It is false on all three of podFor's routes: no
			// status.podName, a Get that failed, and a pod already carrying a
			// deletion timestamp. None of the three is condemned, but not for
			// one reason — two of them and then a third.
			//
			// For two of them there is no pod to make the claim about, and
			// condemnation is a claim about the node a live pod is sitting on.
			// A server with no status.podName has no pod at all. One whose pod
			// already carries a deletion timestamp is worth stating plainly,
			// because a drain is the likeliest thing to have put that
			// timestamp there — the eviction may well be this very node's
			// doing — but the removal is under way either way, and
			// re-condemning it would only reserve a second delete for a server
			// that is going.
			//
			// The failed Get is the different one, and it is not the same
			// sentence at a discount: a live pod on a departing node may exist
			// and simply be unreadable, so this is a claim we decline to make
			// rather than one with no subject. Declining is the same choice
			// nodeDeparting makes when it cannot read a Node, for the same
			// reason it gives — a group must not be emptied on the strength of
			// a cache miss — and it costs at most a delay, because the next
			// pass asks again.
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
// whose registry key is empty and whose snapshot is "unknown, therefore stale"
// — the conservative answer. A pod already carrying a deletion timestamp counts
// as gone, the same rule the Server controller applies.
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

// newServer builds the Server object both create paths write. Everything a
// server of this group carries is here except the two things that tell the
// kinds apart: where the name came from, and spec.ordinal.
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

// createServer creates one interchangeable server of an ephemeral group, under
// a name with a random suffix because it has no identity to preserve.
func (r *ServerGroupReconciler) createServer(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	podHash string,
) (string, error) {
	srv, err := r.newServer(group, NewServerName(group.Name), podHash)
	if err != nil {
		return "", err
	}
	if err := r.Create(ctx, srv); err != nil {
		return "", err
	}
	r.Recorder.Eventf(group, nil, corev1.EventTypeNormal, "ServerCreated", actionCreateServer,
		"created server %s", srv.Name)
	return srv.Name, nil
}

// createPersistentServer creates the server holding one ordinal. Unlike
// createServer's random suffix, this name is derived and stable: it is what
// makes the claim name stable, which is what makes the world survive.
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
	if err := r.Create(ctx, srv); err != nil {
		// A name this reconciler derives can already be taken, which a random
		// one effectively cannot: the cache that DecidePersistentSize read may
		// simply not have shown the server this same reconciler created a
		// moment ago. The object is already what this call wanted, so the
		// caller reserves it as it would any other create -- returning the
		// error instead would fail the whole reconcile, status and PDB with
		// it, for as long as the collision lasts: a pass or two while a cache
		// catches up, and without end for an object that holds the name
		// without holding the ordinal. No event: nothing was created here.
		if !apierrors.IsAlreadyExists(err) {
			return "", err
		}
		return srv.Name, nil
	}
	r.Recorder.Eventf(group, nil, corev1.EventTypeNormal, "ServerCreated", actionCreateServer,
		"created server %s", srv.Name)
	return srv.Name, nil
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
	// Already asked for. A Server keeps its phase while it drains, so it can be
	// nominated again on the next pass; repeating the call would emit the same
	// event every resync for the whole drain.
	if !srv.DeletionTimestamp.IsZero() {
		return nil
	}
	// Deleting the object is the request; the Server controller's finalizer
	// runs the drain before the object actually goes away.
	if err := r.Delete(ctx, srv); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	r.Recorder.Eventf(group, nil, corev1.EventTypeNormal, reason, actionDeleteServer, message, name)
	return nil
}

// retireServer asks one server to enter soft drain.
//
// A patch rather than an update: the group holds a cached copy and the Server
// controller writes status on the same object, so a full update here would
// race it for no reason.
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
	// Already asked. The nomination reads the cache, which lags the patch by
	// a reconcile or two, so without this the same server collects the same
	// event every resync for the whole of its retirement.
	if srv.Spec.Retire {
		return nil
	}
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.Retire = true
	if err := r.Patch(ctx, srv, patch); err != nil {
		return err
	}
	r.Recorder.Eventf(group, nil, corev1.EventTypeNormal, "ServerRetiring", actionRetireServer,
		"retiring server %s for a rolling update", name)
	return nil
}

// adoptServers stamps the freshly computed render hash onto every server of a
// persistent group whose spec.podHash is still empty: one created before this
// field existed. Adoption applies to persistent groups only; an ephemeral
// group's servers are skipped outright.
func (r *ServerGroupReconciler) adoptServers(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	servers map[string]*spawneryv1alpha1.Server,
	podHash string,
) error {
	if group.IsEphemeral() {
		return nil
	}
	for _, srv := range servers {
		// Already adopted. Guards the same cache-lag window retireServer's own
		// guard does, above: without it, a server this pass (or an earlier one)
		// already patched, but not yet visible through the cache this List
		// read, would be patched again.
		if srv.Spec.PodHash != "" {
			continue
		}
		patch := client.MergeFrom(srv.DeepCopy())
		srv.Spec.PodHash = podHash
		if err := r.Patch(ctx, srv, patch); err != nil {
			return err
		}
	}
	return nil
}

// pruneFailed keeps the number of retained failures per group at
// maxRetainedFailures. It does not depend on the Network, so it runs even when
// that cannot be resolved: a group whose Network was deleted is exactly the one
// that will pile failures up.
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
		// The Server controller still drains it first if it turns out to have
		// players on it; this only asks for the removal.
		if err := r.deleteServer(ctx, group, servers, name, "FailedServerPruned",
			"removing failed server %s, only the newest generation's oldest failure is kept for diagnosis"); err != nil {
			return err
		}
	}
	return nil
}

// reconcilePDB keeps the group's PodDisruptionBudget in step with the number
// of occupied pods.
//
// For pods without a controller carrying a scale subresource, Kubernetes
// allows neither maxUnavailable nor percentages in a PDB. The absolute number
// of occupied pods is the only formulation that works — and it makes the
// eviction API refuse to evict any of them.
//
// Named through podspec.GroupPDBName, not the bare group.Name it used before
// -- see that function's doc comment for why a ServerGroup and a same-named
// ProxyGroup collide over it. This is the side whose object name actually
// changed when GroupPDBName was introduced, and an operator upgrading across
// that change strands whatever PodDisruptionBudget previously sat at the
// bare group.Name: it stays owned by this ServerGroup, keeps whatever
// minAvailable it last had, and goes on blocking evictions of whatever pods
// it still selects, because nothing here ever renames or deletes it.
//
// The selector pins podspec.LabelRole as well as the group, and that term is
// load-bearing rather than tidiness. minAvailable is counted from
// occupiedPods(views), which sees this group's *servers* and nothing else. A
// selector without the role term matches on managed-by, group and occupied,
// and a ProxyGroup may share this group's name in this namespace — the very
// case GroupPDBName exists for. From the milestone that put
// podspec.LabelOccupied on proxy pods too, such a selector matches the
// occupied proxies of the same-named ProxyGroup as well: currentHealthy
// counts the ready ones among them and minAvailable counts none of them, so
// disruptionsAllowed goes positive, and the eviction API can spend every one
// of those disruptions on an occupied *server* pod, disconnecting its
// players. The role term is what keeps the pods the selector matches and the
// pods minAvailable is counted over one and the same set. See isOccupied
// (candidates.go) for the two-sides-must-agree requirement this term is the
// third participant in.
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

// reconcileConfigMap keeps the group's rendered ConfigMap — design section
// 5.4's one ConfigMap per group — in step with the fields spec.maxPlayers
// exposes to a user. It carries only that: online-mode, the forwarding mode
// and the ports are operationally critical and live in internal/render's
// critical layer and nowhere else, so there is exactly one place that can be
// wrong about any of them.
//
// It marshals a render.Values document under podspec.ConfigValuesKey, the
// same key BuildServerPod projects into ConfigDir, and it carries
// podspec.LabelManagedBy for the reason podspec.GroupConfigMapName and
// Bootstrapper.ensureConfigMap both document: cmd/spawnery-operator narrows
// the manager's cache for ConfigMaps to that label, so an unlabelled one this
// reconciler just wrote would be invisible to it on the very next Get.
// serverConfigValues is the config document a server group renders, and the
// single place it is built. reconcileConfigMap writes it to the ConfigMap and
// DesiredServerHash digests it; two constructions of the same document would
// drift, and the failure that drift produces is an update that never fires.
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

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podspec.GroupConfigMapName(group.Name, podspec.RoleServer),
			Namespace: group.Namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[podspec.LabelManagedBy] = podspec.ManagedByValue
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[podspec.ConfigValuesKey] = string(data)
		return controllerutil.SetControllerReference(group, cm, r.Scheme)
	})
	return err
}

// groupsOfNetwork maps a Network event onto the ServerGroups in its namespace that
// name it.
//
// Namespace-scoped and filtered by NetworkRef rather than enqueueing every
// group: one-network-per-namespace means the filter is nearly always a
// formality, but a namespace holding a loser as well as a winner is exactly
// the state this repository's own duplicate-network rule creates, and a group
// pointed at the loser has no business being woken by the winner.
//
// A List error returns nothing rather than failing. This shortens a wait that
// the resync would end anyway, so losing it costs latency and never
// correctness.
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

// groupsOnNode maps a Node event onto the ServerGroups with pods on that node.
//
// The five-second resync would find a cordoned node on its own; the watch is
// what makes the answer immediate, and an eviction issued in the same second
// as the cordon is exactly the race this milestone is about.
//
// It lists this operator's pods and filters by node rather than asking for a
// spec.nodeName field index. An index would have to be registered once and
// shared by two controllers -- registering it twice fails at manager start --
// and the population it would serve is this operator's own pods, which is
// small. A label-scoped list over a warm cache is cheaper than that
// coordination is worth.
func (r *ServerGroupReconciler) groupsOnNode(ctx context.Context, obj client.Object) []reconcile.Request {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingLabels{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelRole:      podspec.RoleServer,
	}); err != nil {
		// Enqueue nothing rather than guess. A map function has no way to
		// return an error and no queue of its own to retry from, so dropping
		// the event is the only option here; the five-second resync is the
		// fallback, and it reaches the same conclusion from collectViews. That
		// costs this cordon its immediacy, which is the whole point of the
		// watch, so it is logged rather than swallowed — the same rule
		// nodeDeparting follows when it cannot read a node.
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
	// A construction site that forgets this would otherwise panic inside a
	// reconcile, in a goroutine, minutes after start — the same failure mode
	// SetupAll refuses a nil Bootstrapper for.
	if r.Expectations == nil {
		r.Expectations = newExpectations(r.Clock)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.ServerGroup{}).
		Owns(&spawneryv1alpha1.Server{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.groupsOnNode)).
		// A group refused because its Network is missing or unaccepted has no
		// way of hearing that the Network came back: it is not an owner, and
		// nothing about the group itself changes when the Network does. It
		// waited out the resync. This is the watch docs/known-issues.md asks
		// for under milestone 4b -- the recovery it measures at roughly ninety
		// seconds is two requeues stacked, and this removes the second.
		Watches(&spawneryv1alpha1.Network{},
			handler.EnqueueRequestsFromMapFunc(r.groupsOfNetwork)).
		Named("servergroup").
		Complete(r)
}
