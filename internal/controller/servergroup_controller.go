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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
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
	Recorder record.EventRecorder

	// Agents is the runtime state reported by the in-game agents.
	Agents *agent.Registry
	// Clock is injectable so the time rules are testable.
	Clock func() time.Time
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

// Reconcile sizes the group and updates its status.
func (r *ServerGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !group.DeletionTimestamp.IsZero() {
		// The Server objects are owned by the group; Kubernetes garbage
		// collection cascades, and each Server drains through its finalizer.
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

	requeue := resyncInterval
	if networkFound {
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionTrue,
			Reason:  spawneryv1alpha1.ReasonAccepted,
			Message: fmt.Sprintf("managed as part of network %q", network.Name),
		})
	} else {
		logger.Info("network not found, no servers are created for this group",
			"group", group.Name, "network", group.Spec.NetworkRef.Name)
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonNetworkNotFound,
			Message: fmt.Sprintf("network %q does not exist in this namespace", group.Spec.NetworkRef.Name),
		})
		requeue = networkRetryInterval
	}

	if !group.IsEphemeral() {
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonNotImplemented,
			Message: "persistent groups arrive in milestone 5",
		})
		requeue = time.Minute
	}

	views, servers, err := r.collectViews(ctx, group)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Sizing is the only step that needs the Network resolved: a Server created
	// without one could never get a pod, would run into its startup deadline and
	// would be replaced over and over. Everything below this point — the
	// PodDisruptionBudget that keeps the eviction API off the occupied pods, and
	// the published status — has nothing to do with the Network, so a group
	// whose Network was deleted must keep doing both. Freezing them would leave
	// exactly the pods that still carry players unprotected.
	if networkFound && group.IsEphemeral() {
		if err := r.size(ctx, group, views, servers); err != nil {
			return ctrl.Result{}, err
		}
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

// size brings the number of servers that hold the group at its floor up or
// down to the desired count.
func (r *ServerGroupReconciler) size(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	views []ServerView,
	servers map[string]*spawneryv1alpha1.Server,
) error {
	logger := log.FromContext(ctx)

	desired := group.DesiredReplicas()
	alive := int32(0)
	for _, v := range views {
		if v.countsTowardSize() {
			alive++
		}
	}

	switch {
	case alive < desired:
		for i := alive; i < desired; i++ {
			if err := r.createServer(ctx, group); err != nil {
				return err
			}
		}
	case alive > desired:
		surplus := int(alive - desired)
		names := SelectDeletionCandidates(views, surplus)
		if len(names) < surplus {
			logger.Info("fewer free servers than the surplus, trying again later",
				"group", group.Name, "surplus", surplus, "free", len(names))
		}
		for _, name := range names {
			if err := r.deleteServer(ctx, group, servers, name,
				"ServerRemoved", "removing server %s"); err != nil {
				return err
			}
		}
	}
	return nil
}

// derivePhase maps the totals and conditions onto the group phase.
func derivePhase(group *spawneryv1alpha1.ServerGroup, totals GroupTotals) string {
	if meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		return "Degraded"
	}
	if totals.ReadyReplicas >= group.DesiredReplicas() && totals.ReadyReplicas > 0 {
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
		v := ServerView{
			Name:    srv.Name,
			Phase:   phase.Phase(srv.Status.Phase),
			Players: snap.Players,
			Slots:   snap.Slots,
			Stale:   snap.PlayersStale,
			// Read from the status, never guessed from the phase: a server that
			// lost its probe is in Starting with its players still connected.
			WasRegistered: srv.Status.WasRegistered,
			// A pod that once existed and is now gone took its sessions with it,
			// exactly like one that reached a terminal state.
			SessionsGone: srv.Status.PodName != "" && (!podFound || podTerminal(pod)),
			Generation:   srv.Spec.GroupGeneration,
			CreatedAt:    srv.CreationTimestamp.Time,
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

func (r *ServerGroupReconciler) createServer(ctx context.Context, group *spawneryv1alpha1.ServerGroup) error {
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NewServerName(group.Name),
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
		},
	}
	if err := controllerutil.SetControllerReference(group, srv, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, srv); err != nil {
		return err
	}
	r.Recorder.Eventf(group, corev1.EventTypeNormal, "ServerCreated", "created server %s", srv.Name)
	return nil
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
	r.Recorder.Eventf(group, corev1.EventTypeNormal, reason, message, name)
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
			"removing failed server %s, only the oldest failure is kept for diagnosis"); err != nil {
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
func (r *ServerGroupReconciler) reconcilePDB(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	views []ServerView,
) error {
	minAvailable := intstr.FromInt32(occupiedPods(views))

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: group.Name, Namespace: group.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Spec.MinAvailable = &minAvailable
		pdb.Spec.MaxUnavailable = nil
		pdb.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelGroup:     group.Name,
				podspec.LabelOccupied:  "true",
			},
		}
		return controllerutil.SetControllerReference(group, pdb, r.Scheme)
	})
	return err
}

// SetupWithManager registers the controller.
func (r *ServerGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.ServerGroup{}).
		Owns(&spawneryv1alpha1.Server{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("servergroup").
		Complete(r)
}
