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
// bundle and the agent ServiceAccounts that the pods in it mount. Two
// controllers report it, and they mean it about different objects: the Server
// controller sets it as a condition on a Server whose pod it is therefore not
// creating, and the Network controller records it as an event on a Network
// whose namespace it could not keep current. One name, because an operator
// reading either is looking at the same obstacle in the same namespace.
const ReasonNamespaceNotBootstrapped = "NamespaceNotBootstrapped"

// ReasonPodNameTerminating marks a Server whose pod name is still held by the
// pod of an earlier server of the same name that has not finished terminating.
// Its neighbour ReasonPodNameConflict is the same question about a pod this
// Server does not control; this one is about a pod its own predecessor left.
const ReasonPodNameTerminating = "PodNameTerminating"

// ReasonServerPodRejected marks a Server whose pod the API server refused. It
// is the Server-level twin of ReasonProxyPodRejected on a ProxyGroup, and it
// exists for the same reason: the remedy is at the namespace's policy or
// quota, not at anything this operator can retry its way out of.
const ReasonServerPodRejected = "ServerPodRejected"

// defaultDrainTimeoutSeconds and defaultFailedRetentionSeconds mirror the
// kubebuilder defaults on ServerGroupSpec. They are what a Server falls back to
// when its group is gone, so drain and cleanup keep sane timings.
const (
	defaultDrainTimeoutSeconds    int32 = 60
	defaultFailedRetentionSeconds int32 = 3600
)

// resyncInterval is how often a Server is re-examined even without an event.
// The state machine has time-driven transitions (startup deadline, drain
// deadline, stream grace period) that no watch reports.
const resyncInterval = 5 * time.Second

// ServerReconciler drives one Server through the state machine.
type ServerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

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
	// Bootstrap puts the CA bundle and the agent ServiceAccount into a
	// namespace before the first pod is created there. Required.
	Bootstrap *Bootstrapper
	// AgentEndpoint is the address the in-game agent dials to reach the
	// operator's gRPC endpoint, e.g. "spawnery-operator.spawnery-system.svc:9443".
	AgentEndpoint string
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=servers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servers/status,verbs=update
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servers/finalizers,verbs=update
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups,verbs=get;list;watch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=networks,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;patch

// The two event grants are not the same right twice, and only one of them is
// cluster-wide.
//
// events.k8s.io is where every controller in this package writes, through
// events.EventRecorder, and it regards objects in whatever namespace a Network
// put its game servers -- so it has to be cluster-wide.
//
// The core group is not left over from before the migration off tools/record:
// controller-runtime's leader election still builds its resource lock with the
// deprecated GetEventRecorderFor, and that recorder writes core events. But
// leader election locks on a Lease in the operator's own namespace and its
// events regard that Lease, so the right it needs is namespaced -- the same
// argument, and the same spawnery-system placeholder, as the lease grant at
// internal/controller/setup.go:77. Granting it cluster-wide would let the
// operator write a core event into any namespace in the cluster for a lock it
// can only ever take in one.
//
// spawnery-system is not where the operator runs; it is the literal
// controller-gen requires to emit a namespaced Role at all, rewritten to
// Helm's release namespace by hack/chart-templates.sh.
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

	// Only pod creation needs the group and the network. Everything else — the
	// finalizer, the drain, the occupied label, releasing the object — has to
	// keep running without them, or a Server whose group was deleted would stay
	// Ready forever with its pod alive and its finalizer held, and the orphan
	// sweep of Task 11 would deadlock on that finalizer.
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

	// Last step before the status is touched. Everything from here down writes
	// to srv.Status, and nothing above this line does; see ensureFinalizer for
	// why the boundary exists.
	if err := r.ensureFinalizer(ctx, srv); err != nil {
		return ctrl.Result{}, err
	}

	// The clock starts when this operator began trying, which is this pass --
	// not when a pod appeared. Stamped only beside the pod, as it was until
	// 2026-08-24, a Server whose pod is refused had no clock at all: nothing
	// could fail it, so it sat in Pending occupying its group's slot for as
	// long as the refusal stood. The pod-creation branch below re-stamps it,
	// so the ordinary path still measures from the pod.
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

	// Recover from a status write lost between Create(pod) and Status().Update:
	// the pod is there but status.podName is empty, so without adoption the
	// creation branch would be skipped forever while the startup deadline could
	// never fire and PodLost could never be detected.
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
			// Someone else's pod holds this name. Adopting it would put this
			// Server in charge of a workload it never created, and deleting it
			// is not ours to do. Stand off and say so.
			r.Recorder.Eventf(srv, nil, corev1.EventTypeWarning, "PodNameConflict", actionAdoptPod,
				"pod %s exists but is not controlled by this Server", pod.Name)
			setAccepted(srv, false, ReasonPodNameConflict,
				fmt.Sprintf("pod %q exists but is not controlled by this Server", pod.Name))
			pod, podFound, nameConflict = nil, false, true
		}
	}

	// A pod of this name that the API server still holds, even one on its way
	// out. fetchPod reports a pod carrying a deletion timestamp as gone, and
	// that is right for every decision the state machine makes about a pod —
	// its players are leaving with it and nothing can bring it back. It is
	// wrong for the one decision below that is about the *name*, because the
	// object is still there and a Create against it returns AlreadyExists.
	//
	// A persistent server is what reaches this in practice. Its name is derived
	// from its ordinal and is therefore reused across every generation of the
	// Server object, so a recreated ordinal meets the pod its predecessor left
	// behind. An ephemeral name would have to have NewServerName's random
	// four-character suffix come up twice running for the same collision — not
	// the case this was written for, and covered anyway, since the test below
	// reads the name and not the type.
	// Without this test the create fires, the pod Create's AlreadyExists is
	// tolerated below, and the block goes on to record status.podName and emit
	// PodCreated for a pod this controller did not create — whose absence one
	// reconcile later is PodLost, which deletes this fresh Server. The group
	// then rebuilds the ordinal into the same collision, once per resync,
	// reaching Failed at no point and so counted by no backoff.
	nameStillHeld := !podFound && pod != nil

	// Create the pod once, and only for a server that has not been asked to go
	// away. status.podName is the record that a pod once existed; it is never
	// reused for a different pod, which is what makes PodLost detectable.
	createPod := groupFound && networkFound && !nameConflict && !nameStillHeld &&
		!podFound && srv.Status.PodName == "" && srv.DeletionTimestamp.IsZero()

	// Checked on every pass, not only the one that creates the pod: a
	// persistent server's pod usually already exists, so createPod is false
	// for the reconcile that actually has to notice spec.storage.size grew,
	// or the claim's FileSystemResizePending condition.
	if !group.IsEphemeral() {
		if err := r.growClaim(ctx, group, srv); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.readResizePending(ctx, srv); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Say so on the object, and only where nothing above has already put a
	// truer reason there: a Server whose group or network is missing carries
	// one from the switch above, and one that has itself been deleted is not
	// waiting for anything. status.podName being empty is what makes this the
	// fresh Server rather than the one whose pod is terminating. Nothing below
	// overwrites it while the wait lasts either — the bootstrap check is inside
	// `if createPod`, which this case is exactly what turns off.
	//
	// A condition and no event, deliberately. The wait is one termination
	// grace period in the ordinary case and unbounded when the pod cannot
	// finish terminating — a node gone NotReady — and it is the unbounded case
	// that needs a name: phase.Decide leaves the server Pending with "waiting
	// for the pod", which reads exactly like a slow image pull.
	// meta.SetStatusCondition writes no new transition while the reason and
	// message are unchanged, so a five-second resync stays quiet for the whole
	// wait, and an operator sees the two transitions that matter on the object
	// the rest of this server's state is already on. An event would announce an
	// ordinary pod replacement as a warning, once per pass.
	if nameStillHeld && groupFound && networkFound && !nameConflict &&
		srv.Status.PodName == "" && srv.DeletionTimestamp.IsZero() {
		setAccepted(srv, false, ReasonPodNameTerminating,
			fmt.Sprintf("pod %q is still terminating; this server's own pod is created "+
				"once that name is free", pod.Name))
	}

	// The namespace has to hold the CA and the ServiceAccount before the pod
	// does, not after: the kubelet mounts both at container start, and a pod
	// that comes up against a missing or empty ca.crt does not wait — it fails
	// its TLS handshake against the operator and burns the startup deadline.
	//
	// Ensure fails for as long as no CA is published, which is exactly the
	// window between process start and the leader's first certificate, so the
	// requeue below is a retry and not an error. But it fails just as well when
	// the write itself is refused — an admission webhook, a ResourceQuota on
	// ConfigMaps, a policy in a customer namespace — and that state does not
	// pass on its own. Nothing else here would ever say so: status.startedAt is
	// only set once a pod exists, so StartupDeadlineReached can never fire and
	// the Server would sit in Pending forever with an empty condition and no
	// event. Say it on the object, then fall through to the status update; the
	// requeue still lets it recover by itself once the obstacle is gone.
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

	if createPod {
		// The claim goes in before the pod that mounts it, and nothing here
		// waits for it to reach Bound. Under volumeBindingMode:
		// WaitForFirstConsumer — the default of most topology-aware storage
		// classes, and of the node-local ones this milestone's failure modes
		// are about — a volume binds only once a pod demands it, so waiting
		// for Bound would deadlock against the pod this block goes on to
		// create.
		//
		// AlreadyExists is the ordinary case rather than an error: an ordinal
		// recreated after its server was deleted is *supposed* to find the
		// claim it had before, and that is the whole point of the milestone.
		// growClaim above is what grows it; this call only ever creates.
		if !group.IsEphemeral() {
			claim := podspec.BuildDataClaim(group, srv)
			if err := r.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, err
			}
		}

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

		case apierrors.IsForbidden(err), apierrors.IsInvalid(err):
			// A create the API server refused is the Server's business and not
			// only the log's, and this is the same branch
			// proxygroup_controller.go takes one layer up for the same two
			// error kinds: IsForbidden covers a Pod Security profile that
			// forbids the pod's shape and an RBAC grant the operator does not
			// have, IsInvalid a pod the API server rejects outright, a quota
			// or a webhook among them.
			//
			// Returning the error alone left nothing on the object at all.
			// status.podName stays empty, so the pod-lost path never applies;
			// status.startedAt is only set beside a pod that exists, so
			// StartupDeadlineReached can never fire; and the Server therefore
			// sat in Pending with an empty condition and no event for as long
			// as the refusal stood, while still counting against its group's
			// replicas. That is the same silence the namespace-bootstrap
			// branch above was given a condition for, arriving through the
			// call one line further down.
			//
			// Falling through rather than returning: the tail writes the
			// status, and the resync requeue there lets this recover by itself
			// once the obstacle is gone.
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

// growClaim raises the claim's storage request to match spec.storage.size,
// and never lowers it. It is the only write this operator makes to an
// *existing* claim — the reconcile above creates one alongside the pod, and
// nothing anywhere deletes one — and the RBAC it needs is patch, not update,
// which would replace the whole object for one field, and never delete, which
// is the verb that destroys a world.
//
// A claim already at or above the size asked for is left untouched, byte for
// byte: that covers both the ordinary case (nothing to do) and the one a
// controller has no business correcting — a claim someone grew by hand, which
// the CRD's own shrink guard on spec.storage.size means this function will
// never be asked to shrink anyway.
//
// A resize can fail two different ways, and this function is where the
// choice was made to catch both rather than only the one Design §4 names.
// allowVolumeExpansion: false is the ordinary way the patch below fails
// synchronously, right here, with the API server's own admission error — but
// IsInvalid/IsForbidden covers other causes of the same shape too: Task 7's
// own report found "only dynamically provisioned pvc can be resized" from an
// unbound, class-less claim, which has nothing to do with
// allowVolumeExpansion. This function cannot tell those apart from the error
// alone, so the message it records below says what happened and names
// allowVolumeExpansion as the first thing to check, not as the established
// cause. A driver that accepts the resize and fails it later says so only on
// the claim itself, as a ControllerResizeError or NodeResizeError condition,
// with nothing synchronous to catch at all; resizeConditionError is what
// reads that, both below and on the pass where nothing needed to grow.
// Reporting only the synchronous half would leave the asynchronous one
// looking like a resize still in progress rather than one that failed. Both
// land on status.storageResizeError, and ServerGroupReconciler folds either
// into the group's StorageResize condition without needing to know which
// kind it was.
func (r *ServerReconciler) growClaim(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	srv *spawneryv1alpha1.Server,
) error {
	if group.Spec.Storage == nil {
		return nil
	}
	claim := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: podspec.DataClaimName(srv.Name), Namespace: srv.Namespace}
	if err := r.Get(ctx, key, claim); err != nil {
		if apierrors.IsNotFound(err) {
			srv.Status.StorageResizeError = ""
		}
		return client.IgnoreNotFound(err)
	}
	want := group.Spec.Storage.Size
	have := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if want.Cmp(have) <= 0 {
		srv.Status.StorageResizeError = resizeConditionError(claim)
		return nil
	}
	patched := claim.DeepCopy()
	patched.Spec.Resources.Requests[corev1.ResourceStorage] = want
	if err := r.Patch(ctx, patched, client.MergeFrom(claim)); err != nil {
		if !apierrors.IsInvalid(err) && !apierrors.IsForbidden(err) {
			return err
		}
		// Refused synchronously by the API server's own resize admission,
		// rather than returned as a reconcile error: returning it here would
		// fail this whole pass before readResizePending below ever ran, and
		// retrying an admission rejection every five seconds would not
		// change its outcome. Recording it on the status is what lets
		// ServerGroupReconciler say so on the group instead of this staying
		// a line in the reconcile log.
		//
		// The message names what happened and what to check, not why: see
		// this function's doc comment for why IsInvalid/IsForbidden alone
		// cannot tell an unexpandable storage class apart from Task 7's
		// unbound-claim rejection, or from any other cause the same two error
		// kinds cover. err is carried verbatim because it is the only part of
		// this that actually identifies the cause.
		className := "(the cluster default)"
		if claim.Spec.StorageClassName != nil {
			className = *claim.Spec.StorageClassName
		}
		srv.Status.StorageResizeError = fmt.Sprintf(
			"claim %s: the patch growing it to %s was refused by the API server: %v; "+
				"check storage class %q first, in particular whether it sets allowVolumeExpansion: true",
			claim.Name, want.String(), err, className)
		return nil
	}
	srv.Status.StorageResizeError = resizeConditionError(claim)
	return nil
}

// resizeConditionError names the reason a claim's resize did not go through,
// read off the two conditions a CSI driver sets only after admission already
// let a resize patch through: PersistentVolumeClaimControllerResizeError and
// PersistentVolumeClaimNodeResizeError. This is the asynchronous half of what
// growClaim's own doc comment describes; growClaim calls this both after a
// patch it just made and on a pass where the claim already matched
// spec.storage.size, since a driver can fail a resize well after the pass
// that requested it.
//
// Returns "" when neither condition is set to True, which is the ordinary
// case: most CSI drivers that accept a resize also complete it.
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

// readResizePending mirrors the claim's FileSystemResizePending condition
// onto status.storageResizePending, which is what DecidePersistentSize's
// lowest-priority candidate class reads. Most CSI drivers expand a volume
// online and the pod never has to restart for it; the ones that do not set
// the condition until the resize actually needs a restart to take effect,
// so this is false for the ordinary case.
//
// A claim that no longer exists clears the flag rather than leaving a stale
// true behind -- there is nothing left asking for a restart.
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

// ensureFinalizer puts the drain finalizer on the object. It exists as a step
// of its own so that the order it has to run in is visible in Reconcile's call
// structure instead of only in a comment above an inline block.
//
// The finalizer must sit on the object before the pod exists, otherwise a
// deletion between pod creation and the next reconcile skips the drain.
//
// It must also run before anything writes to srv.Status, and that is the part
// worth a function: Update returns the persisted object, and the API server
// does not take the status from us because status is a subresource, so
// controller-runtime writes the persisted — on a first reconcile, empty —
// status back over srv. Every condition set before this call is silently lost,
// and the Status().Update at the end of applyDecision then persists an object
// that never had them. Nothing here reads or writes srv.Status; keep it that
// way, and keep the call ahead of the first status write in Reconcile.
func (r *ServerReconciler) ensureFinalizer(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	if !srv.DeletionTimestamp.IsZero() || slices.Contains(srv.Finalizers, ServerFinalizer) {
		return nil
	}
	srv.Finalizers = append(srv.Finalizers, ServerFinalizer)
	return r.Update(ctx, srv)
}

// fetchPod returns the pod of a server. A pod carrying a deletion timestamp
// counts as gone: it is on its way out, its players are leaving with it, and
// nothing we could decide would bring it back. Without this rule the Server
// object would wait for a pod that only the kubelet can finally remove — and
// in envtest, where no kubelet runs, it would wait forever.
//
// It still hands the caller that pod rather than nil, and the difference
// between the two returns is load-bearing in exactly one place: Reconcile's
// nameStillHeld, which asks whether the *name* is free rather than whether the
// pod is usable. Keep the object on this path.
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
	nameTaken bool,
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
		WasRegistered:       srv.Status.WasRegistered,
		RetirementRequested: srv.Spec.Retire,
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
	in.PlayersOnline = snap.Players
	in.PlayersStale = snap.PlayersStale
	in.Slots = snap.Slots

	// Only once a pod has existed. The stamp is written on acceptance now, so
	// without this guard the startup deadline would fire for a Server whose
	// pod was refused and report it as "did not become ready in time" -- a
	// server that never had a pod did not fail to become ready. That case is
	// the one below, with its own bound and its own reason.
	if srv.Status.StartedAt != nil && (podFound || srv.Status.PodName != "") {
		in.StartupDeadlineReached = now.Sub(srv.Status.StartedAt.Time) > r.StartupDeadline
	}
	// The wait a Server with no pod at all is allowed.
	//
	// Not while its pod's *name* is held by another pod -- a predecessor still
	// terminating, or somebody else's pod on a derived ordinal name. Failing
	// there makes things worse rather than better: for a persistent ordinal
	// the replacement is derived from the same name and hits the same wall, a
	// Failed server holds its ordinal (DecidePersistentSize's held map) and
	// pruneFailed does not run for a persistent group, so the object stays for
	// its full failedRetentionSeconds -- an hour by default -- even after
	// somebody force-deletes the stuck pod that caused it. Before this bound
	// existed, that Server recovered the moment the name came free. Both cases
	// already name the obstacle on the object, PodNameTerminating and
	// PodNameConflict, so nothing is silent here either.
	//
	// The bound itself is derived from the group rather than configured:
	// drain.timeoutSeconds is how long a predecessor's termination may take,
	// and the startup deadline on top is the slack every other attempt gets.
	// With the guard above that is margin rather than the mechanism -- the
	// clock does not run during the wait at all -- and it is kept as the
	// second line: if nameTaken were ever computed wrongly, a bound sized
	// below a legitimate termination would fail Servers that are doing the
	// right thing.
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
	// Measured from the wait in soft drain, not from the group's generation
	// change: a server still queued behind maxUnavailable is not failing to
	// empty, it has not been asked yet. Zero means never, which is the CRD
	// default and the promise that nobody is moved unless somebody asked for
	// it.
	if srv.Status.RetiringSince != nil {
		if window := group.UpdateMaxStale(); window > 0 {
			in.MaxStaleReached = now.Sub(srv.Status.RetiringSince.Time) >= window
		}
	}
	if srv.Status.FailedAt != nil {
		in.FailedRetentionElapsed = now.Sub(srv.Status.FailedAt.Time) >= group.FailedRetention()
	}

	return in
}

// fallbackGroup stands in for a ServerGroup that is gone. It carries the CRD
// defaults, so a Server that outlives its group still drains and cleans up on
// sane timings instead of freezing. It is never used to build a pod.
func fallbackGroup(srv *spawneryv1alpha1.Server) *spawneryv1alpha1.ServerGroup {
	// The type is read off the Server rather than assumed. spec.ordinal is set
	// by createPersistentServer and by nothing else, and Server's own API doc
	// says it is unset for ephemeral servers, so the ordinal is what identifies
	// the type of a group that is no longer there to ask.
	//
	// Stamping Ephemeral unconditionally was milestone 5's last open
	// precondition. Its stated reason -- that a persistent server would then
	// run on the wrong deadlines -- turned out not to be what the code does:
	// DrainTimeout, FailedRetention and UpdateMaxStale all read fields this
	// function fills with the CRD's own defaults, and none of the three looks
	// at the type. What the type actually decides on this path is the
	// !IsEphemeral() branch in Reconcile, so a persistent server whose group
	// had gone stopped refreshing status.storageResizeError -- growClaim
	// already returns on nil storage, and the branch that builds a claim needs
	// the group anyway, so there is nothing here for the truthful answer to
	// break.
	groupType := spawneryv1alpha1.ServerGroupEphemeral
	if srv.Spec.Ordinal != nil {
		groupType = spawneryv1alpha1.ServerGroupPersistent
	}
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      srv.Spec.GroupRef.Name,
			Namespace: srv.Namespace,
		},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Type:                   groupType,
			Drain:                  &spawneryv1alpha1.DrainSpec{TimeoutSeconds: defaultDrainTimeoutSeconds},
			FailedRetentionSeconds: defaultFailedRetentionSeconds,
			Update:                 &spawneryv1alpha1.UpdateSpec{MaxUnavailable: 1, MaxStaleSeconds: 0},
		},
	}
}

// setAccepted records whether the operator can fully manage this Server. It is
// written onto the object; applyDecision persists it with the rest of the
// status in a single update.
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

// podTerminal reports whether the pod is finished for good: the process is
// down and every session it held went with it. It is the single definition of
// that question — the state machine reads it to refuse a pointless drain, and
// the occupied label reads it to stop protecting a pod that has nobody on it.
func podTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodFailed ||
		pod.Status.Phase == corev1.PodSucceeded ||
		crashLooping(pod)
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
		// Persisted before the side effect, not after. Remembered for the life
		// of this pod: from here on a deletion has to drain, even if the server
		// falls back out of Ready first. Writing it afterwards means a lost
		// status update in this window makes a later deletion take the "never
		// registered, terminate immediately" branch — with players already on
		// the server, because the proxies were told about it a moment ago.
		//
		// This ordering has a cost of its own, and it is worth stating rather
		// than only the upside above: if Register itself fails after the flag
		// is already persisted, a later deletion takes the drain branch for a
		// server no proxy was actually told about. With a stale agent stream,
		// isOccupied still counts it occupied, so that drain runs out its full
		// deadline instead of terminating immediately. Bounded — it ends on its
		// own once the deadline passes — and the safe direction to err in, but
		// not free.
		//
		// One extra status write, at the single transition into Ready.
		if !srv.Status.WasRegistered {
			srv.Status.WasRegistered = true
			if err := r.Status().Update(ctx, srv); err != nil {
				return fmt.Errorf("persist the registration intent for %s: %w", srv.Name, err)
			}
		}
		if err := r.Registrar.Register(ctx, srv); err != nil {
			return fmt.Errorf("register %s: %w", srv.Name, err)
		}
		srv.Status.Registered = true
	}
	// The drain clock starts with the drain, not with phase Draining: a Failed
	// server is drained while staying Failed, and without this its deadline
	// would never be reached. Both the clock and the broadcast happen exactly
	// once — the Failed branch repeats StartDrain on every pass, and re-sending
	// the command to every proxy each resync would be pure noise. A proxy that
	// reconnects is re-synced from the phase in the CR status.
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

	// Phase bookkeeping. These timestamps are what the time-driven inputs are
	// derived from, so they have to survive an operator restart.
	if d.Next != current {
		r.Recorder.Eventf(srv, nil, corev1.EventTypeNormal, d.Reason, actionSyncStatus,
			"phase %s -> %s: %s", current, d.Next, d.Message)
	}
	switch d.Next {
	case phase.Ready:
		if current != phase.Ready || srv.Status.ReadySince == nil {
			srv.Status.ReadySince = &now
		}
	case phase.Starting:
		// Re-arm the startup deadline. It bounds the current attempt to become
		// playable, not the age of the pod: entering Starting from Pending arms
		// it, and entering it from Ready after a readiness loss re-arms it for
		// the recovery attempt. Without this a server older than the deadline
		// would be failed by the first blip; with it, one that cannot recover is
		// still failed a deadline later.
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
	r.mirrorPlayerCount(srv, snap, now)

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
		now.Sub(srv.Status.PlayersUpdatedAt.Time) >= r.PlayerStatusInterval
	if !significant && !overdue {
		return
	}
	srv.Status.Players = snap.Players
	srv.Status.Slots = snap.Slots
	srv.Status.PlayersUpdatedAt = &now
}

// syncOccupiedLabel keeps the label the group's PodDisruptionBudget selects on
// in step with reality. The label means "this pod may be carrying players",
// which is a narrower question than "is the count stale".
//
// The rule itself is isOccupied, shared with the ServerGroup controller, which
// sizes minAvailable from the same answer. This function only supplies the four
// facts from the Kubernetes side: the reported count, whether it is stale,
// whether the proxies ever routed to this server, and whether its pod is
// finished. A terminal pod counts as sessions gone — the state machine refuses
// to drain one for exactly that reason.
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
