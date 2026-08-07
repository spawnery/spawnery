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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// ServerFinalizer keeps the Server object around until its players are safe
// and its pod is gone.
const ServerFinalizer = "spawnery.cloud/drain"

// MaxContainerRestarts is how often the Paper container may restart before the
// server counts as broken rather than flaky.
const MaxContainerRestarts int32 = 3

// resyncInterval is how often a Server is re-examined even without an event.
// The state machine has time-driven transitions (startup deadline, drain
// deadline, stream grace period) that no watch reports.
const resyncInterval = 5 * time.Second

// ServerReconciler drives one Server through the state machine.
type ServerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Agents is the runtime state reported by the in-game agents.
	Agents *agent.Registry
	// Clock is injectable so the time rules are testable.
	Clock func() time.Time
	// StartupDeadline is how long a server may take to reach Ready.
	StartupDeadline time.Duration
	// PlayerStatusInterval throttles player-count writes into etcd.
	PlayerStatusInterval time.Duration
	// Registrar reaches the proxies.
	Registrar Registrar
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=servers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servers/finalizers,verbs=update
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups,verbs=get;list;watch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=networks,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile collects the inputs, asks the state machine and executes the
// decision. It contains no rule of its own about deleting an occupied pod.
func (r *ServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	srv := &spawneryv1alpha1.Server{}
	if err := r.Get(ctx, req.NamespacedName, srv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	group := &spawneryv1alpha1.ServerGroup{}
	groupKey := types.NamespacedName{Name: srv.Spec.GroupRef.Name, Namespace: srv.Namespace}
	if err := r.Get(ctx, groupKey, group); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// The group is gone. The orphan reconciler removes the Server; there
		// is nothing sensible to reconcile against in the meantime.
		logger.Info("server group not found, waiting for the orphan sweep", "group", srv.Spec.GroupRef.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if !group.IsEphemeral() {
		// Persistent groups need a PVC and an ordered shutdown; that is
		// milestone 5. Say so instead of building half a pod.
		logger.Info("persistent groups are not implemented yet", "group", group.Name)
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	network := &spawneryv1alpha1.Network{}
	networkKey := types.NamespacedName{Name: group.Spec.NetworkRef.Name, Namespace: srv.Namespace}
	if err := r.Get(ctx, networkKey, network); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		logger.Info("network not found", "network", group.Spec.NetworkRef.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// The finalizer must sit on the object before the pod exists, otherwise a
	// deletion between creation and the next reconcile skips the drain.
	if srv.DeletionTimestamp.IsZero() && !slices.Contains(srv.Finalizers, ServerFinalizer) {
		srv.Finalizers = append(srv.Finalizers, ServerFinalizer)
		if err := r.Update(ctx, srv); err != nil {
			return ctrl.Result{}, err
		}
	}

	pod, podFound, err := r.fetchPod(ctx, srv)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Create the pod once, and only for a server that has not been asked to go
	// away. status.podName is the record that a pod once existed; it is never
	// reused for a different pod, which is what makes PodLost detectable.
	if !podFound && srv.Status.PodName == "" && srv.DeletionTimestamp.IsZero() {
		built, err := podspec.BuildServerPod(network, group, srv)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, built); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(srv, corev1.EventTypeNormal, "PodCreated", "created pod %s", built.Name)

		srv.Status.PodName = built.Name
		now := metav1.NewTime(r.Clock())
		srv.Status.StartedAt = &now
		// A fresh pod has never been registered and carries no flap history.
		srv.Status.WasRegistered = false
		srv.Status.ReadinessLosses = 0
		if srv.Status.Phase == "" {
			srv.Status.Phase = string(phase.Pending)
		}
		if err := r.Status().Update(ctx, srv); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: resyncInterval}, nil
	}

	in := r.collectInputs(srv, group, pod, podFound)
	current := phase.Phase(srv.Status.Phase)
	if current == "" {
		current = phase.Pending
	}
	decision := phase.Decide(current, in)

	if err := r.applyDecision(ctx, srv, group, pod, podFound, current, decision); err != nil {
		return ctrl.Result{}, err
	}

	// Once the pod is gone and deletion was requested, let the object go.
	if decision.Next == phase.Terminating && !podFound {
		if !srv.DeletionTimestamp.IsZero() {
			srv.Finalizers = slices.DeleteFunc(srv.Finalizers, func(f string) bool { return f == ServerFinalizer })
			if err := r.Update(ctx, srv); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		// Terminating without a deletion request means the state machine
		// decided the server is finished. Remove the object so the group
		// creates a replacement.
		if err := r.Delete(ctx, srv); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// fetchPod returns the pod of a server. A pod carrying a deletion timestamp
// counts as gone: it is on its way out, its players are leaving with it, and
// nothing we could decide would bring it back. Without this rule the Server
// object would wait for a pod that only the kubelet can finally remove — and
// in envtest, where no kubelet runs, it would wait forever.
func (r *ServerReconciler) fetchPod(ctx context.Context, srv *spawneryv1alpha1.Server) (*corev1.Pod, bool, error) {
	name := srv.Status.PodName
	if name == "" {
		name = srv.Name
	}
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: srv.Namespace}, pod)
	switch {
	case err == nil:
		if !pod.DeletionTimestamp.IsZero() {
			return pod, false, nil
		}
		return pod, true, nil
	case apierrors.IsNotFound(err):
		return nil, false, nil
	default:
		return nil, false, err
	}
}

// collectInputs is the only place that reads Kubernetes state into the pure
// state machine.
func (r *ServerReconciler) collectInputs(
	srv *spawneryv1alpha1.Server,
	group *spawneryv1alpha1.ServerGroup,
	pod *corev1.Pod,
	podFound bool,
) phase.Inputs {
	now := r.Clock()

	in := phase.Inputs{
		DeletionRequested: !srv.DeletionTimestamp.IsZero(),
		PodExists:         podFound,
		PodLost:           !podFound && srv.Status.PodName != "",
		ReadinessLosses:   srv.Status.ReadinessLosses,
		// Whether the server was ever registered is recorded state, not
		// something to re-derive here: a Starting server that fell out of Ready
		// may still have players connected from before the readiness loss (the
		// fallback deregisters to stop new joins, it does not move anyone off),
		// and only status.wasRegistered still knows that. The controller writes
		// it wherever it registers and resets it when it creates a fresh pod.
		WasRegistered: srv.Status.WasRegistered,
	}

	if podFound {
		in.PodRunning = pod.Status.Phase == corev1.PodRunning
		in.PodTerminal = pod.Status.Phase == corev1.PodFailed ||
			pod.Status.Phase == corev1.PodSucceeded ||
			crashLooping(pod)
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady {
				in.PodReady = c.Status == corev1.ConditionTrue
			}
		}
	}

	snap := r.Agents.Lookup(podUID(pod, podFound))
	in.AgentReady = snap.Ready
	in.AgentConnected = snap.Connected
	in.AgentStreamDownFor = snap.StreamDownFor
	in.PlayersOnline = snap.Players
	in.PlayersStale = snap.PlayersStale
	in.Slots = snap.Slots

	if srv.Status.StartedAt != nil {
		in.StartupDeadlineReached = now.Sub(srv.Status.StartedAt.Time) > r.StartupDeadline
	}
	if srv.Status.ReadySince != nil {
		in.ReadyFor = now.Sub(srv.Status.ReadySince.Time)
	}
	if srv.Status.DrainStartedAt != nil {
		in.DrainDeadlineReached = now.Sub(srv.Status.DrainStartedAt.Time) >= group.DrainTimeout()
	}
	if srv.Status.FailedAt != nil {
		in.FailedRetentionElapsed = now.Sub(srv.Status.FailedAt.Time) >= group.FailedRetention()
	}

	return in
}

func podUID(pod *corev1.Pod, found bool) string {
	if !found {
		return ""
	}
	return string(pod.UID)
}

// crashLooping reports whether the Minecraft container is stuck restarting.
// The check is deliberately scoped to that one container: PodTerminal aborts a
// running drain, so a crash-looping sidecar must never be able to cut short the
// drain of a healthy server that still has players on it.
func crashLooping(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != podspec.ContainerName {
			continue
		}
		if cs.RestartCount >= MaxContainerRestarts &&
			cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}
	return false
}

// applyDecision executes the decision and writes the status.
func (r *ServerReconciler) applyDecision(
	ctx context.Context,
	srv *spawneryv1alpha1.Server,
	group *spawneryv1alpha1.ServerGroup,
	pod *corev1.Pod,
	podFound bool,
	current phase.Phase,
	d phase.Decision,
) error {
	now := metav1.NewTime(r.Clock())

	if d.Deregister {
		if err := r.Registrar.Deregister(ctx, srv); err != nil {
			return fmt.Errorf("deregister %s: %w", srv.Name, err)
		}
		srv.Status.Registered = false
	}
	if d.Register {
		if err := r.Registrar.Register(ctx, srv); err != nil {
			return fmt.Errorf("register %s: %w", srv.Name, err)
		}
		srv.Status.Registered = true
		// Remembered for the life of this pod: from here on a deletion has to
		// drain, even if the server falls back out of Ready first.
		srv.Status.WasRegistered = true
	}
	if d.StartDrain {
		if err := r.Registrar.Drain(ctx, srv); err != nil {
			return fmt.Errorf("drain %s: %w", srv.Name, err)
		}
	}

	if d.CountReadinessLoss {
		srv.Status.ReadinessLosses++
		r.Recorder.Eventf(srv, corev1.EventTypeWarning, phase.ReasonReadinessLost,
			"%s (loss %d of %d)", d.Message, srv.Status.ReadinessLosses, phase.MaxReadinessLosses)
	}
	if d.ResetReadinessLosses {
		srv.Status.ReadinessLosses = 0
	}

	// Phase bookkeeping. These timestamps are what the time-driven inputs are
	// derived from, so they have to survive an operator restart.
	if d.Next != current {
		r.Recorder.Eventf(srv, corev1.EventTypeNormal, d.Reason,
			"phase %s -> %s: %s", current, d.Next, d.Message)
	}
	switch d.Next {
	case phase.Ready:
		if current != phase.Ready || srv.Status.ReadySince == nil {
			srv.Status.ReadySince = &now
		}
	case phase.Draining:
		if srv.Status.DrainStartedAt == nil {
			srv.Status.DrainStartedAt = &now
		}
		srv.Status.ReadySince = nil
	case phase.Failed:
		if srv.Status.FailedAt == nil {
			srv.Status.FailedAt = &now
		}
		srv.Status.ReadySince = nil
	default:
		srv.Status.ReadySince = nil
	}
	srv.Status.Phase = string(d.Next)

	if podFound {
		srv.Status.Address = ""
		if pod.Status.PodIP != "" {
			srv.Status.Address = fmt.Sprintf("%s:%d", pod.Status.PodIP, podspec.MinecraftPort)
		}
	}

	snap := r.Agents.Lookup(podUID(pod, podFound))
	r.mirrorPlayerCount(srv, snap, now)

	if podFound {
		if err := r.syncOccupiedLabel(ctx, pod, snap); err != nil {
			return err
		}
	}

	if d.DeletePod && podFound && pod.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		r.Recorder.Eventf(srv, corev1.EventTypeNormal, "PodDeleted",
			"deleted pod %s: %s", pod.Name, d.Message)
	}

	meta.SetStatusCondition(&srv.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionReady,
		Status:  conditionStatus(d.Next == phase.Ready),
		Reason:  d.Reason,
		Message: d.Message,
	})

	return r.Status().Update(ctx, srv)
}

// mirrorPlayerCount writes the in-memory count into the status, throttled.
// The control loop reads memory; the CR status is for observers.
func (r *ServerReconciler) mirrorPlayerCount(
	srv *spawneryv1alpha1.Server,
	snap agent.Snapshot,
	now metav1.Time,
) {
	if !snap.Known {
		return
	}
	significant := snap.Players != srv.Status.Players || snap.Slots != srv.Status.Slots
	overdue := srv.Status.PlayersUpdatedAt == nil ||
		now.Time.Sub(srv.Status.PlayersUpdatedAt.Time) >= r.PlayerStatusInterval
	if !significant && !overdue {
		return
	}
	srv.Status.Players = snap.Players
	srv.Status.Slots = snap.Slots
	srv.Status.PlayersUpdatedAt = &now
}

// syncOccupiedLabel keeps the label the group's PodDisruptionBudget selects on
// in step with reality. A stale count counts as occupied.
func (r *ServerReconciler) syncOccupiedLabel(ctx context.Context, pod *corev1.Pod, snap agent.Snapshot) error {
	occupied := snap.PlayersStale || snap.Players > 0
	_, labelled := pod.Labels[podspec.LabelOccupied]
	if occupied == labelled {
		return nil
	}

	patched := pod.DeepCopy()
	if occupied {
		if patched.Labels == nil {
			patched.Labels = map[string]string{}
		}
		patched.Labels[podspec.LabelOccupied] = "true"
	} else {
		delete(patched.Labels, podspec.LabelOccupied)
	}
	return r.Patch(ctx, patched, client.MergeFrom(pod))
}

func conditionStatus(ok bool) metav1.ConditionStatus {
	if ok {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// SetupWithManager registers the controller.
func (r *ServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.Server{}).
		Owns(&corev1.Pod{}).
		Named("server").
		Complete(r)
}
