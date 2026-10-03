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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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

// ReasonPodNameConflict marks a Server whose pod name is taken by a pod it does
// not control.
const ReasonPodNameConflict = "PodNameConflict"

// ReasonNamespaceNotBootstrapped says a namespace does not yet hold the CA
// bundle and the agent ServiceAccounts its pods mount. The Server controller
// sets it as a condition, the Network controller as an event.
const ReasonNamespaceNotBootstrapped = "NamespaceNotBootstrapped"

// ReasonPodNameTerminating marks a Server whose pod name is still held by its
// predecessor's terminating pod.
const ReasonPodNameTerminating = "PodNameTerminating"

// ReasonServerPodRejected marks a Server whose pod the API server refused; the
// remedy is the namespace's policy or quota, not a retry.
const ReasonServerPodRejected = "ServerPodRejected"

// ReasonServerClaimRejected marks a Server whose data claim the API server
// refused.
const ReasonServerClaimRejected = "ServerClaimRejected"

// These mirror the kubebuilder defaults on ServerGroupSpec, for a Server whose
// group is gone. TestTheFallbackGroupCarriesEveryCrdDefault keeps them in step.
const (
	defaultDrainTimeoutSeconds      int32 = 60
	defaultFailedRetentionSeconds   int32 = 3600
	defaultFinishedRetentionSeconds int32 = 300
)

// ResyncInterval is how often a Server is re-examined even without an event:
// the state machine has time-driven transitions no watch reports. A deadline
// the operator races has to be more than one of these away;
// cmd/spawnery-operator checks the rescue window against it.
const ResyncInterval = 5 * time.Second

// ServerReconciler drives one Server through the state machine.
type ServerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	Agents *agent.Registry
	Clock  func() time.Time
	// StartupDeadline is how long a server may take to reach Ready.
	StartupDeadline time.Duration
	// PlayerStatusInterval throttles player-count writes into etcd.
	PlayerStatusInterval time.Duration
	Registrar            Registrar
	// Bootstrap puts the CA bundle and the agent ServiceAccount into a
	// namespace before the first pod is created there. Required.
	Bootstrap *Bootstrapper
	// AgentEndpoint is the address the in-game agent dials, e.g.
	// "spawnery-operator.spawnery-system.svc:9443".
	AgentEndpoint string
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=servers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servers/status,verbs=update
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servers/finalizers,verbs=update
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups,verbs=get;list;watch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=networks,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;patch;delete

// Core events are only for leader election's recorder, which regards a Lease
// in the operator's own namespace, so that grant is namespaced. spawnery-system
// is the literal controller-gen needs to emit a Role; hack/chart-templates.sh
// rewrites it to the release namespace.
// +kubebuilder:rbac:groups="",namespace=spawnery-system,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile collects the inputs, asks the state machine and executes the
// decision. It contains no rule of its own about deleting an occupied pod.
func (r *ServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	srv := &spawneryv1alpha1.Server{}
	if err := r.Get(ctx, req.NamespacedName, srv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only pod creation needs the group and the network. The finalizer, the
	// drain and the occupied label keep running without them, or the orphan
	// sweep would deadlock on the finalizer.
	group := &spawneryv1alpha1.ServerGroup{}
	groupKey := types.NamespacedName{Name: srv.Spec.GroupRef.Name, Namespace: srv.Namespace}
	groupFound := true
	if err := r.Get(ctx, groupKey, group); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		groupFound = false
	}

	network := &spawneryv1alpha1.Network{}
	networkFound := false
	if groupFound {
		networkKey := types.NamespacedName{Name: group.Spec.NetworkRef.Name, Namespace: srv.Namespace}
		if err := r.Get(ctx, networkKey, network); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		} else {
			networkFound = true
		}
	}

	// Before anything writes to srv.Status; see ensureFinalizer.
	if err := r.ensureFinalizer(ctx, srv); err != nil {
		return ctrl.Result{}, err
	}

	// Stamped on acceptance, not only beside the pod, so a Server whose pod is
	// refused can still fail. Pod creation below re-stamps it.
	if srv.Status.StartedAt == nil && srv.DeletionTimestamp.IsZero() {
		started := metav1.NewTime(r.Clock())
		srv.Status.StartedAt = &started
	}

	switch {
	case !groupFound:
		logger.Info("server group not found, running on the CRD defaults", "group", srv.Spec.GroupRef.Name)
		setAccepted(srv, false, spawneryv1alpha1.ReasonGroupNotFound,
			fmt.Sprintf("server group %q not found; draining and cleanup continue on the default timings", srv.Spec.GroupRef.Name))
		group = fallbackGroup(srv)
	case !networkFound:
		logger.Info("network not found, running on the CRD defaults", "network", group.Spec.NetworkRef.Name)
		setAccepted(srv, false, spawneryv1alpha1.ReasonNetworkNotFound,
			fmt.Sprintf("network %q not found; no pod can be created for this server", group.Spec.NetworkRef.Name))
	default:
		setAccepted(srv, true, spawneryv1alpha1.ReasonAccepted, "group and network resolved")
	}

	pod, podFound, err := r.fetchPod(ctx, srv)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Recover from a status write lost between Create(pod) and Status().Update.
	nameConflict := false
	if podFound && srv.Status.PodName == "" {
		if metav1.IsControlledBy(pod, srv) {
			srv.Status.PodName = pod.Name
			if srv.Status.StartedAt == nil {
				started := pod.CreationTimestamp
				srv.Status.StartedAt = &started
			}
			r.Recorder.Eventf(srv, nil, corev1.EventTypeNormal, "PodAdopted", actionAdoptPod,
				"adopted existing pod %s after a lost status write", pod.Name)
		} else {
			// Someone else's pod holds this name: neither adopt nor delete it.
			r.Recorder.Eventf(srv, nil, corev1.EventTypeWarning, "PodNameConflict", actionAdoptPod,
				"pod %s exists but is not controlled by this Server", pod.Name)
			setAccepted(srv, false, ReasonPodNameConflict,
				fmt.Sprintf("pod %q exists but is not controlled by this Server", pod.Name))
			pod, podFound, nameConflict = nil, false, true
		}
	}

	// fetchPod reports a terminating pod as gone, but its name is still taken
	// and a Create would return AlreadyExists, which is tolerated below and would
	// record a pod this controller did not create. A recreated persistent ordinal
	// reuses its predecessor's name, so this is the ordinary case there.
	nameStillHeld := !podFound && pod != nil

	// status.podName is never reused for a different pod, which is what makes
	// PodLost detectable.
	createPod := groupFound && networkFound && !nameConflict && !nameStillHeld &&
		!podFound && srv.Status.PodName == "" && srv.DeletionTimestamp.IsZero()

	// Every pass: a persistent server's pod usually already exists when its
	// claim needs to grow.
	claimExists := false
	if !group.IsEphemeral() {
		var err error
		if claimExists, err = r.growClaim(ctx, group, srv); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.readResizePending(ctx, srv); err != nil {
			return ctrl.Result{}, err
		}
	}

	// A condition and no event: an event would announce an ordinary pod
	// replacement as a warning on every pass, and the wait is unbounded when the
	// old pod cannot finish terminating.
	if nameStillHeld && groupFound && networkFound && !nameConflict &&
		srv.Status.PodName == "" && srv.DeletionTimestamp.IsZero() {
		setAccepted(srv, false, ReasonPodNameTerminating,
			fmt.Sprintf("pod %q is still terminating; this server's own pod is created "+
				"once that name is free", pod.Name))
	}

	// The kubelet mounts the CA and the ServiceAccount at container start; a pod
	// that comes up without them fails its TLS handshake and burns the startup
	// deadline. Ensure also fails while no CA is published yet, so the requeue is
	// the retry; a refused write does not pass on its own and gets a condition.
	if createPod {
		if err := r.Bootstrap.Ensure(ctx, srv.Namespace); err != nil {
			logger.Info("waiting to bootstrap the namespace before creating the pod",
				"namespace", srv.Namespace, "reason", err.Error())
			r.Recorder.Eventf(srv, nil, corev1.EventTypeWarning, ReasonNamespaceNotBootstrapped, actionCreatePod, "%s",
				eventNote("cannot bootstrap namespace %s: %v", srv.Namespace, err))
			setAccepted(srv, false, ReasonNamespaceNotBootstrapped,
				fmt.Sprintf("namespace %q does not hold the CA bundle and the agent ServiceAccount yet (%v); "+
					"no pod is created until it does", srv.Namespace, err))
			createPod = false
		}
	}

	// Not waiting for Bound: under WaitForFirstConsumer a volume binds only once
	// a pod demands it. An existing claim is never created again; growClaim grows
	// it.
	if createPod && !group.IsEphemeral() && !claimExists {
		claim := podspec.BuildDataClaim(group, srv)
		err := r.Create(ctx, claim)
		switch {
		case err == nil, apierrors.IsAlreadyExists(err):
		case apierrors.IsForbidden(err), apierrors.IsInvalid(err):
			// A quota, a webhook, or storage.annotations over the API server's total
			// annotation size.
			r.Recorder.Eventf(srv, nil, corev1.EventTypeWarning, ReasonServerClaimRejected,
				actionCreatePod, "%s",
				eventNote("the API server refused this server's data claim: %v", err))
			setAccepted(srv, false, ReasonServerClaimRejected,
				fmt.Sprintf("the API server refused this server's data claim: %v; "+
					"the remedy is the namespace's quota or the group's spec.storage, not a retry", err))
			createPod = false
		default:
			return ctrl.Result{}, err
		}
	}

	if createPod {
		built, err := podspec.BuildServerPod(network, group, srv, r.AgentEndpoint)
		if err != nil {
			return ctrl.Result{}, err
		}
		err = r.Create(ctx, built)
		switch {
		case err == nil, apierrors.IsAlreadyExists(err):
			r.Recorder.Eventf(srv, nil, corev1.EventTypeNormal, "PodCreated", actionCreatePod,
				"created pod %s", built.Name)

			srv.Status.PodName = built.Name
			now := metav1.NewTime(r.Clock())
			srv.Status.StartedAt = &now
			srv.Status.WasRegistered = false
			srv.Status.ReadinessLosses = 0
			if srv.Status.Phase == "" {
				srv.Status.Phase = string(phase.Pending)
			}
			if err := persistedServer(r.Status().Update(ctx, srv)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: ResyncInterval}, nil

		case apierrors.IsForbidden(err), apierrors.IsInvalid(err):
			// Pod Security, RBAC, quota or a webhook. Without a condition the Server
			// would sit in Pending silently, since no deadline runs without a pod. Falls
			// through so the tail writes the status and requeues.
			r.Recorder.Eventf(srv, nil, corev1.EventTypeWarning, ReasonServerPodRejected,
				actionCreatePod, "%s",
				eventNote("the API server refused this server's pod: %v", err))
			setAccepted(srv, false, ReasonServerPodRejected,
				fmt.Sprintf("the API server refused this server's pod: %v; "+
					"the remedy is the namespace's policy or quota, not a retry", err))

		default:
			return ctrl.Result{}, err
		}
	}

	in := r.collectInputs(srv, group, pod, podFound, nameStillHeld || nameConflict)
	current := phase.Phase(srv.Status.Phase)
	if current == "" {
		current = phase.Pending
	}
	if current == phase.Failed && podFound {
		ready, err := r.groupHasReadyServer(ctx, srv)
		if err != nil {
			return ctrl.Result{}, err
		}
		in.GroupHasReadyServer = ready
	}
	decision := phase.Decide(current, in)

	if err := r.applyDecision(ctx, srv, group, pod, podFound, current, decision); err != nil {
		return ctrl.Result{}, err
	}

	// Once the pod is gone and deletion was requested, let the object go.
	if decision.Next == phase.Terminating && !podFound {
		if !srv.DeletionTimestamp.IsZero() {
			srv.Finalizers = slices.DeleteFunc(srv.Finalizers, func(f string) bool { return f == ServerFinalizer })
			if err := persistedServer(r.Update(ctx, srv)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		// Terminating without a deletion request: the state machine decided the
		// server is finished, and the group creates a replacement.
		if err := r.Delete(ctx, srv); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: ResyncInterval}, nil
}

// persistedServer treats the Server's disappearance during a write as done:
// the recreate path deletes the Server itself, and a NotFound from a cached
// read would log as a false error that `make e2e` reads for real refusals.
// Only for writes to srv; a NotFound on the group must not be swallowed.
func persistedServer(err error) error {
	return client.IgnoreNotFound(err)
}

// growClaim reports whether the claim exists and raises its storage request
// to spec.storage.size, never lowering it; a claim grown by hand is left alone.
// It is the operator's only write to an existing claim, by patch.
//
// A synchronous refusal is usually allowVolumeExpansion: false but has other
// causes of the same error kind (an unbound, class-less claim), so the message
// names the class as the first thing to check, not as the cause. A driver that
// fails the resize later says so only in the claim's conditions, read by
// resizeConditionError. Both land on status.storageResizeError.
func (r *ServerReconciler) growClaim(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	srv *spawneryv1alpha1.Server,
) (exists bool, err error) {
	if group.Spec.Storage == nil {
		return false, nil
	}
	claim := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: podspec.DataClaimName(srv.Name), Namespace: srv.Namespace}
	if err := r.Get(ctx, key, claim); err != nil {
		if apierrors.IsNotFound(err) {
			srv.Status.StorageResizeError = ""
		}
		return false, client.IgnoreNotFound(err)
	}
	want := group.Spec.Storage.Size
	have := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if want.Cmp(have) <= 0 {
		srv.Status.StorageResizeError = resizeConditionError(claim)
		return true, nil
	}
	patched := claim.DeepCopy()
	patched.Spec.Resources.Requests[corev1.ResourceStorage] = want
	if err := r.Patch(ctx, patched, client.MergeFrom(claim)); err != nil {
		if !apierrors.IsInvalid(err) && !apierrors.IsForbidden(err) {
			return true, err
		}
		// Recorded rather than returned: retrying an admission rejection changes
		// nothing, and returning would skip readResizePending.
		className := "(the cluster default)"
		if claim.Spec.StorageClassName != nil {
			className = *claim.Spec.StorageClassName
		}
		srv.Status.StorageResizeError = fmt.Sprintf(
			"claim %s: the patch growing it to %s was refused by the API server: %v; "+
				"check storage class %q first, in particular whether it sets allowVolumeExpansion: true",
			claim.Name, want.String(), err, className)
		return true, nil
	}
	srv.Status.StorageResizeError = resizeConditionError(claim)
	return true, nil
}

// resizeConditionError reads the resize-error conditions a CSI driver sets
// after admission let a resize through, which can come well after the pass
// that asked for it. "" when neither is True.
func resizeConditionError(claim *corev1.PersistentVolumeClaim) string {
	for _, c := range claim.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case corev1.PersistentVolumeClaimControllerResizeError, corev1.PersistentVolumeClaimNodeResizeError:
			return fmt.Sprintf("claim %s: %s: %s", claim.Name, c.Reason, c.Message)
		}
	}
	return ""
}

// readResizePending mirrors the claim's FileSystemResizePending condition onto
// status.storageResizePending, for DecidePersistentSize. A missing claim clears
// it.
func (r *ServerReconciler) readResizePending(
	ctx context.Context,
	srv *spawneryv1alpha1.Server,
) error {
	claim := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: podspec.DataClaimName(srv.Name), Namespace: srv.Namespace}
	if err := r.Get(ctx, key, claim); err != nil {
		if apierrors.IsNotFound(err) {
			srv.Status.StorageResizePending = false
			return nil
		}
		return err
	}
	pending := false
	for _, c := range claim.Status.Conditions {
		if c.Type == corev1.PersistentVolumeClaimFileSystemResizePending && c.Status == corev1.ConditionTrue {
			pending = true
			break
		}
	}
	srv.Status.StorageResizePending = pending
	return nil
}

// ensureFinalizer puts the drain finalizer on the object before the pod
// exists, or a deletion in between skips the drain.
//
// It must run before anything writes to srv.Status: Update writes the
// persisted status (empty on a first reconcile) back over srv, because status
// is a subresource, and every condition set earlier would be lost.
func (r *ServerReconciler) ensureFinalizer(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	if !srv.DeletionTimestamp.IsZero() || slices.Contains(srv.Finalizers, ServerFinalizer) {
		return nil
	}
	srv.Finalizers = append(srv.Finalizers, ServerFinalizer)
	return persistedServer(r.Update(ctx, srv))
}

// fetchPod returns the pod of a server. A pod carrying a deletion timestamp
// counts as gone: its players are leaving with it, and in envtest no kubelet
// would ever remove it. The pod is still returned, because nameStillHeld asks
// whether the name is free.
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

// groupHasReadyServer reports whether another server of srv's group is Ready.
func (r *ServerReconciler) groupHasReadyServer(ctx context.Context, srv *spawneryv1alpha1.Server) (bool, error) {
	var list spawneryv1alpha1.ServerList
	if err := r.List(ctx, &list, client.InNamespace(srv.Namespace)); err != nil {
		return false, err
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name != srv.Name && other.Spec.GroupRef.Name == srv.Spec.GroupRef.Name &&
			other.Status.Phase == string(phase.Ready) {
			return true, nil
		}
	}
	return false, nil
}

// collectInputs is the only place that reads Kubernetes state into the pure
// state machine.
func (r *ServerReconciler) collectInputs(
	srv *spawneryv1alpha1.Server,
	group *spawneryv1alpha1.ServerGroup,
	pod *corev1.Pod,
	podFound bool,
	nameTaken bool,
) phase.Inputs {
	now := r.Clock()

	in := phase.Inputs{
		DeletionRequested: !srv.DeletionTimestamp.IsZero(),
		PodExists:         podFound,
		PodLost:           !podFound && srv.Status.PodName != "",
		ReadinessLosses:   srv.Status.ReadinessLosses,
		// Recorded state: a Starting server that fell out of Ready may still
		// have players from before, and only status.wasRegistered knows.
		WasRegistered:       srv.Status.WasRegistered,
		RetirementRequested: srv.Spec.Retire,
		Registered:          srv.Status.Registered,
	}

	if podFound {
		in.PodRunning = pod.Status.Phase == corev1.PodRunning
		in.PodTerminal = podTerminal(pod)
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
	in.AgentUnheard = !snap.Known
	// Never-reported is excluded: a server just Ready may not have sent its
	// first count yet.
	in.AgentSilent = snap.Connected && !snap.PlayersReportedAt.IsZero() && snap.PlayersStale
	in.PlayersOnline = snap.Players
	in.PlayersStale = snap.PlayersStale
	in.Slots = snap.Slots
	stampRoundEnd(srv, snap, now)
	// The object, not the snapshot, so it holds across an operator restart.
	in.RoundEnded = srv.Status.RoundEndedAt != nil

	// The proxies count a player still in the configuration phase, which
	// neither the backend nor the proxy's own player list does. During a drain
	// both sources must report from after its start, or a pre-drain zero would
	// read as empty. Zero outside a drain, where any fresh report answers.
	var since time.Time
	if srv.Status.DrainStartedAt != nil {
		// metav1.Time truncates to whole seconds, so the stamp reads up to a
		// second early.
		since = srv.Status.DrainStartedAt.Add(time.Second)
	}
	in.ProxyAttached, in.ProxyAttachStale = r.Agents.AttachedTo(srv.Namespace, srv.Name, since)
	// A tie counts as predating: the reading that keeps a pod.
	in.CountPredatesDrain = !since.IsZero() && !snap.PlayersReportedAt.After(since)

	// Only once a pod has existed; a server that never had a pod gets the
	// creation deadline below instead.
	if srv.Status.StartedAt != nil && (podFound || srv.Status.PodName != "") {
		in.StartupDeadlineReached = now.Sub(srv.Status.StartedAt.Time) > r.StartupDeadline
	}
	// The wait a Server with no pod at all is allowed. Not while its name is
	// held by another pod: a Failed persistent server holds its ordinal for the
	// whole failed retention, and its replacement would hit the same name anyway.
	// PodNameTerminating and PodNameConflict already say so. The bound is the
	// drain timeout (a predecessor's termination) plus the startup deadline.
	if srv.Status.StartedAt != nil && !podFound && !nameTaken && srv.Status.PodName == "" {
		in.PodCreationDeadlineReached =
			now.Sub(srv.Status.StartedAt.Time) >= group.DrainTimeout()+r.StartupDeadline
	}
	if srv.Status.ReadySince != nil {
		in.ReadyFor = now.Sub(srv.Status.ReadySince.Time)
	}
	if srv.Status.DrainStartedAt != nil {
		in.DrainDeadlineReached = now.Sub(srv.Status.DrainStartedAt.Time) >= group.DrainTimeout()
	}
	// Measured from the soft drain, not the spec change: a server queued behind
	// maxUnavailable has not been asked yet. Zero means never.
	if srv.Status.RetiringSince != nil {
		if window := group.UpdateMaxStale(); window > 0 {
			in.MaxStaleReached = now.Sub(srv.Status.RetiringSince.Time) >= window
		}
	}
	if srv.Status.FailedAt != nil {
		in.FailedRetentionElapsed = now.Sub(srv.Status.FailedAt.Time) >= group.FailedRetention()
	}
	if srv.Status.RoundEndedAt != nil {
		in.FinishedRetentionElapsed = now.Sub(srv.Status.RoundEndedAt.Time) >= group.FinishedRetention()
	}

	return in
}

// stampRoundEnd records the first time a server said its round was over, and
// reports whether it wrote anything. Never moved afterwards.
func stampRoundEnd(srv *spawneryv1alpha1.Server, snap agent.Snapshot, now time.Time) bool {
	if !snap.RoundEnded || srv.Status.RoundEndedAt != nil {
		return false
	}
	srv.Status.RoundEndedAt = &metav1.Time{Time: now}
	return true
}

// fallbackGroup stands in for a ServerGroup that is gone. It carries the CRD
// defaults, so a Server that outlives its group still drains and cleans up on
// sane timings instead of freezing. It is never used to build a pod.
func fallbackGroup(srv *spawneryv1alpha1.Server) *spawneryv1alpha1.ServerGroup {
	// spec.ordinal marks a persistent server and spec.key an on-demand one;
	// an on-demand member read as ephemeral would lose its claim handling.
	groupType := spawneryv1alpha1.ServerGroupEphemeral
	switch {
	case srv.Spec.Ordinal != nil:
		groupType = spawneryv1alpha1.ServerGroupPersistent
	case srv.Spec.Key != "":
		groupType = spawneryv1alpha1.ServerGroupOnDemand
	}
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      srv.Spec.GroupRef.Name,
			Namespace: srv.Namespace,
		},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Type:                     groupType,
			Drain:                    &spawneryv1alpha1.DrainSpec{TimeoutSeconds: defaultDrainTimeoutSeconds},
			FailedRetentionSeconds:   defaultFailedRetentionSeconds,
			FinishedRetentionSeconds: defaultFinishedRetentionSeconds,
			Update:                   &spawneryv1alpha1.UpdateSpec{MaxUnavailable: 1, MaxStaleSeconds: 0},
		},
	}
}

// setAccepted records whether the operator can fully manage this Server;
// applyDecision persists it with the rest of the status.
func setAccepted(srv *spawneryv1alpha1.Server, ok bool, reason, message string) {
	meta.SetStatusCondition(&srv.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionAccepted,
		Status:  conditionStatus(ok),
		Reason:  reason,
		Message: message,
	})
}

func podUID(pod *corev1.Pod, found bool) string {
	if !found {
		return ""
	}
	return string(pod.UID)
}

// podTerminal reports whether the pod is finished for good, its sessions gone
// with it. Both the state machine and the occupied label read it.
func podTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodFailed ||
		pod.Status.Phase == corev1.PodSucceeded ||
		crashLooping(pod)
}

// crashLooping reports whether the Minecraft container is stuck restarting.
// Only that container: PodTerminal aborts a drain, and a crash-looping sidecar
// must not cut short the drain of a server with players.
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
		// Persisted before Register: a lost status write afterwards would let a
		// later deletion skip the drain with players already on. The cost is that a
		// failed Register leaves a drain that runs out its deadline.
		if !srv.Status.WasRegistered {
			srv.Status.WasRegistered = true
			if err := persistedServer(r.Status().Update(ctx, srv)); err != nil {
				return fmt.Errorf("persist the registration intent for %s: %w", srv.Name, err)
			}
		}
		if err := r.Registrar.Register(ctx, srv); err != nil {
			return fmt.Errorf("register %s: %w", srv.Name, err)
		}
		srv.Status.Registered = true
	}
	// The drain clock starts with the drain, not with phase Draining: a Failed
	// server is drained while staying Failed. The broadcast happens once; a
	// reconnecting proxy is re-synced from the phase.
	if d.StartDrain && srv.Status.DrainStartedAt == nil {
		if err := r.Registrar.Drain(ctx, srv); err != nil {
			return fmt.Errorf("drain %s: %w", srv.Name, err)
		}
		srv.Status.DrainStartedAt = &now
	}

	if d.CountReadinessLoss {
		srv.Status.ReadinessLosses++
		r.Recorder.Eventf(srv, nil, corev1.EventTypeWarning, phase.ReasonReadinessLost, actionSyncStatus,
			"%s (loss %d of %d)", d.Message, srv.Status.ReadinessLosses, phase.MaxReadinessLosses)
	}
	if d.ResetReadinessLosses {
		srv.Status.ReadinessLosses = 0
	}

	// These timestamps feed the time-driven inputs and must survive an operator
	// restart.
	if d.Next != current {
		r.Recorder.Eventf(srv, nil, corev1.EventTypeNormal, d.Reason, actionSyncStatus,
			"phase %s -> %s: %s", current, d.Next, d.Message)
	}
	switch d.Next {
	case phase.Ready:
		if current != phase.Ready || srv.Status.ReadySince == nil {
			srv.Status.ReadySince = &now
		}
		srv.Status.RetiringSince = nil
	case phase.Starting:
		// The startup deadline bounds the current attempt, not the pod's age, so
		// entering Starting from Ready re-arms it.
		if current != phase.Starting {
			srv.Status.StartedAt = &now
		}
		srv.Status.ReadySince = nil
	case phase.Draining:
		if srv.Status.DrainStartedAt == nil {
			srv.Status.DrainStartedAt = &now
		}
		srv.Status.ReadySince = nil
	case phase.Retiring:
		if srv.Status.RetiringSince == nil {
			srv.Status.RetiringSince = &now
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
	r.mirrorPlayerCount(srv, snap, group.Spec.MaxPlayers, group.Spec.PlayableSlots, now)

	// Written wherever the pod is seen; see ServerStatus.PodUID.
	if podFound {
		srv.Status.PodUID = string(pod.UID)
	}

	if podFound {
		if err := r.syncOccupiedLabel(ctx, srv, pod, snap); err != nil {
			return err
		}
	}

	if d.DeletePod && podFound && pod.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		r.Recorder.Eventf(srv, nil, corev1.EventTypeNormal, "PodDeleted", actionDeletePod,
			"deleted pod %s: %s", pod.Name, d.Message)
	}

	meta.SetStatusCondition(&srv.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionReady,
		Status:  conditionStatus(d.Next == phase.Ready),
		Reason:  d.Reason,
		Message: d.Message,
	})

	return persistedServer(r.Status().Update(ctx, srv))
}

// mirrorPlayerCount writes the in-memory count into the status, throttled.
// The control loop reads memory; the CR status is for observers.
func (r *ServerReconciler) mirrorPlayerCount(
	srv *spawneryv1alpha1.Server,
	snap agent.Snapshot,
	maxPlayers int32,
	specPlayable *int32,
	now metav1.Time,
) {
	if !snap.Known {
		return
	}
	// Clamped like the scaler's view: netstate carries the status into every
	// agent's picture. maxPlayers is zero on the fallback group.
	players, slots := snap.Players, snap.Slots
	playable := slots
	if maxPlayers > 0 {
		players, slots = clampReport(players, slots, maxPlayers)
		playable = playableSeats(snap.PlayableSlots, specPlayable, slots)
	}
	significant := players != srv.Status.Players || slots != srv.Status.Slots ||
		playable != srv.Status.PlayableSlots
	overdue := srv.Status.PlayersUpdatedAt == nil ||
		now.Sub(srv.Status.PlayersUpdatedAt.Time) >= r.PlayerStatusInterval
	if !significant && !overdue {
		return
	}
	srv.Status.Players = players
	srv.Status.Slots = slots
	srv.Status.PlayableSlots = playable
	srv.Status.PlayersUpdatedAt = &now
}

// syncOccupiedLabel keeps the label the group's PodDisruptionBudget selects on
// in step with isOccupied.
func (r *ServerReconciler) syncOccupiedLabel(
	ctx context.Context,
	srv *spawneryv1alpha1.Server,
	pod *corev1.Pod,
	snap agent.Snapshot,
) error {
	occupied := isOccupied(snap.Players, snap.PlayersStale, srv.Status.WasRegistered, podTerminal(pod))
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
