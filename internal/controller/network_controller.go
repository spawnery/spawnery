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
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// NetworkReconciler enforces one network per namespace and publishes the
// aggregated network status.
type NetworkReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// SecretReader must be uncached: the operator holds no list or watch on
	// Secrets (internal/rbacaudit/required.go).
	SecretReader client.Reader

	OperatorNamespace string

	// Agents may be nil; the rescue-window condition then says Unknown.
	Agents *agent.Registry
	// ReportInterval zero means phase.RescueWindow's default.
	ReportInterval time.Duration

	// Bootstrap keeps the CA bundle and agent ServiceAccounts current in a
	// namespace where no pod is being created; ServerReconciler only ensures
	// them before a create.
	Bootstrap *Bootstrapper
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=networks,verbs=get;list;watch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=networks/status,verbs=update
// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups,verbs=list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=list
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update

func (r *NetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	network := &spawneryv1alpha1.Network{}
	if err := r.Get(ctx, req.NamespacedName, network); err != nil {
		if apierrors.IsNotFound(err) {
			// No finalizer, so a deletion is usually only ever seen as NotFound.
			ChangeoversInFlight.DeleteLabelValues(req.Namespace, req.Name)
			ChangeoversWaiting.DeleteLabelValues(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !network.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	owner, err := r.namespaceOwner(ctx, network.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if owner != network.Name {
		// Counted even when refused, to show what is stranded behind the refusal.
		// A List error must not keep the refusal from being written.
		if err := r.countGroups(ctx, network); err != nil {
			log.FromContext(ctx).Error(err, "counting the groups behind a refused network")
		}
		message := fmt.Sprintf(
			"namespace %q is already served by network %q; put staging and production in separate namespaces",
			network.Namespace, owner)
		entering := !hasConditionReason(network.Status.Conditions,
			spawneryv1alpha1.ConditionAccepted, spawneryv1alpha1.ReasonDuplicateNetwork)
		meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonDuplicateNetwork,
			Message: message,
		})
		if err := r.Status().Update(ctx, network); err != nil {
			return ctrl.Result{}, err
		}
		if entering {
			r.Recorder.Eventf(network, nil, corev1.EventTypeWarning,
				spawneryv1alpha1.ReasonDuplicateNetwork, actionSyncStatus, "%s",
				eventNote("%s", message))
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionAccepted,
		Status:  metav1.ConditionTrue,
		Reason:  spawneryv1alpha1.ReasonAccepted,
		Message: "this network owns its namespace",
	})

	// announce holds transition events until the status write recording the
	// transition lands; otherwise a failed write lets the retry announce it again.
	var announce []func()

	commit := func() error {
		if err := r.Status().Update(ctx, network); err != nil {
			return err
		}
		for _, fire := range announce {
			fire()
		}
		return nil
	}

	// record writes the status before requeuing on err, so a later failure does
	// not keep Accepted, which every group gates on, from being persisted.
	record := func(err error) (ctrl.Result, error) {
		if writeErr := commit(); writeErr != nil {
			log.FromContext(ctx).Error(writeErr,
				"recording the status of a network whose reconcile failed")
		}
		return ctrl.Result{}, err
	}

	// Fail-closed: Accepted=True would release the groups to create the pods the
	// policy was meant to fence.
	if err := r.reconcileNetworkPolicy(ctx, network); err != nil {
		meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
			Type:   spawneryv1alpha1.ConditionAccepted,
			Status: metav1.ConditionFalse,
			Reason: spawneryv1alpha1.ReasonNetworkPolicyNotWritten,
			Message: fmt.Sprintf(
				"this network is otherwise acceptable, but its NetworkPolicy could not be written, "+
					"so no group in this namespace may start: %s", err),
		})
		return record(fmt.Errorf("reconcile the network policy: %w", err))
	}

	if err := r.countGroups(ctx, network); err != nil {
		return record(err)
	}

	read := readForwardingSecret(ctx, r.SecretReader, network)
	if read.Hash != "" {
		if previous := network.Status.ForwardingSecretHash; previous != "" && previous != read.Hash {
			announce = append(announce, func() {
				r.Recorder.Eventf(network, nil, corev1.EventTypeWarning,
					spawneryv1alpha1.EventForwardingSecretRotated, actionSyncStatus,
					"the forwarding secret changed; roll the server groups first, then the proxy groups — see %s",
					rotationRunbook)
			})
		}
		network.Status.ForwardingSecretHash = read.Hash
	}
	entering := !hasConditionReason(network.Status.Conditions,
		spawneryv1alpha1.ConditionForwardingSecretResolved, read.Reason)
	if read.Reason == spawneryv1alpha1.ReasonSecretNotFound && entering {
		announce = append(announce, func() {
			r.Recorder.Eventf(network, nil, corev1.EventTypeWarning,
				spawneryv1alpha1.EventForwardingSecretNotFound, actionSyncStatus, "%s",
				eventNote("%s", read.Message))
		})
	}
	// Logged so the API server's own `is forbidden:` text reaches the log, which
	// test/e2e's theOperatorWasNeverDenied greps; the condition message does not
	// quote it.
	if read.Err != nil && entering {
		log.FromContext(ctx).Error(read.Err, "reading the forwarding secret",
			"network", network.Name, "namespace", network.Namespace)
		announce = append(announce, func() {
			r.Recorder.Eventf(network, nil, corev1.EventTypeWarning,
				read.Reason, actionSyncStatus, "%s",
				eventNote("%s", read.Message))
		})
	}
	meta.SetStatusCondition(&network.Status.Conditions, resolvedCondition(read))

	// On failure the rotation condition is left as it was: an empty stamp set
	// would report ForwardingSecretInSync with no pod examined.
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(network.Namespace),
		client.MatchingLabels(podspec.ManagedSelector(network.Name))); err != nil {
		return record(err)
	}
	meta.SetStatusCondition(&network.Status.Conditions,
		rotationCondition(read, forwardingStamps(pods.Items)))
	meta.SetStatusCondition(&network.Status.Conditions,
		r.rescueWindowCondition(network.Namespace))

	if err := commit(); err != nil {
		return ctrl.Result{}, err
	}

	// Last, after the status write: a refused ConfigMap write (webhook, quota)
	// can persist, and must not keep Accepted from being recorded.
	if err := r.Bootstrap.Ensure(ctx, network.Namespace); err != nil {
		r.Recorder.Eventf(network, nil, corev1.EventTypeWarning,
			ReasonNamespaceNotBootstrapped, actionBootstrapNamespace, "%s",
			eventNote("cannot bootstrap namespace %s: %v", network.Namespace, err))
		return ctrl.Result{}, fmt.Errorf("bootstrap the namespace: %w", err)
	}

	return ctrl.Result{RequeueAfter: ResyncInterval}, nil
}

// rescueWindowCondition reports whether the proxies give up on a silent
// backend before the operator's next resync could react.
func (r *NetworkReconciler) rescueWindowCondition(namespace string) metav1.Condition {
	unknown := metav1.Condition{
		Type:   spawneryv1alpha1.ConditionRescueWindowShort,
		Status: metav1.ConditionUnknown,
		Reason: spawneryv1alpha1.ReasonNoProxyReported,
		Message: "no proxy in this namespace has reported its read timeout, so how long the " +
			"operator has to move players off a backend whose node dies is not known here",
	}
	if r.Agents == nil {
		return unknown
	}
	timeout, known := r.Agents.ShortestReadTimeout(namespace)
	if !known {
		return unknown
	}

	window := phase.RescueWindow(r.ReportInterval, timeout)
	if window >= ResyncInterval {
		return metav1.Condition{
			Type:   spawneryv1alpha1.ConditionRescueWindowShort,
			Status: metav1.ConditionFalse,
			Reason: spawneryv1alpha1.ReasonRescueWindowSufficient,
			Message: fmt.Sprintf("%s to move players off a backend whose node dies, from a "+
				"proxy read timeout of %s", window, timeout),
		}
	}
	return metav1.Condition{
		Type:   spawneryv1alpha1.ConditionRescueWindowShort,
		Status: metav1.ConditionTrue,
		Reason: spawneryv1alpha1.ReasonRescueWindowTooShort,
		Message: fmt.Sprintf("a proxy here gives up on a silent backend after %s, which leaves "+
			"%s once a player count has gone stale — less than the %s the operator reconciles "+
			"on, so players on a backend whose node dies may be disconnected rather than moved",
			timeout, window, ResyncInterval),
	}
}

// No delete: the owner reference lets the garbage collector remove it.
func (r *NetworkReconciler) reconcileNetworkPolicy(
	ctx context.Context,
	network *spawneryv1alpha1.Network,
) error {
	desired := podspec.BuildNetworkPolicy(network, r.OperatorNamespace)
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		policy.Labels = desired.Labels
		policy.OwnerReferences = desired.OwnerReferences
		policy.Spec = desired.Spec
		return nil
	})
	return err
}

func (r *NetworkReconciler) namespaceOwner(ctx context.Context, namespace string) (string, error) {
	list := &spawneryv1alpha1.NetworkList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return "", err
	}
	return pickNamespaceOwner(list.Items), nil
}

// pickNamespaceOwner: the oldest wins, so a stray apply cannot unseat the
// running Network.
func pickNamespaceOwner(networks []spawneryv1alpha1.Network) string {
	owner := ""
	var ownerCreated metav1.Time
	for i := range networks {
		n := &networks[i]
		if !n.DeletionTimestamp.IsZero() {
			continue
		}
		switch {
		case owner == "",
			n.CreationTimestamp.Before(&ownerCreated),
			n.CreationTimestamp.Equal(&ownerCreated) && n.Name < owner:
			owner, ownerCreated = n.Name, n.CreationTimestamp
		}
	}
	return owner
}

// No watch on the groups: they carry no owner reference to their Network, and
// the ResyncInterval poll keeps the counts fresh.
func (r *NetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.Network{}).
		Owns(&networkingv1.NetworkPolicy{}).
		// Deleting the owner changes the verdict for its siblings.
		Watches(&spawneryv1alpha1.Network{},
			handler.EnqueueRequestsFromMapFunc(r.siblingNetworks)).
		Named("network").
		Complete(r)
}

func (r *NetworkReconciler) countGroups(ctx context.Context, network *spawneryv1alpha1.Network) error {
	serverGroups := &spawneryv1alpha1.ServerGroupList{}
	if err := r.List(ctx, serverGroups, client.InNamespace(network.Namespace)); err != nil {
		return err
	}
	proxyGroups := &spawneryv1alpha1.ProxyGroupList{}
	if err := r.List(ctx, proxyGroups, client.InNamespace(network.Namespace)); err != nil {
		return err
	}

	var serverGroupCount, players, inFlight, waiting int32
	for _, g := range serverGroups.Items {
		if g.Spec.NetworkRef.Name != network.Name {
			continue
		}
		serverGroupCount++
		players += g.Status.OnlinePlayers
		switch {
		case g.IsEphemeral() && g.Status.Changeover == spawneryv1alpha1.ChangeoverBegun && !changeoverFailing(g.Status.Conditions):
			inFlight++
		case g.Status.Changeover == spawneryv1alpha1.ChangeoverWaiting:
			waiting++
		}
	}
	var proxyGroupCount int32
	for _, g := range proxyGroups.Items {
		if g.Spec.NetworkRef.Name != network.Name {
			continue
		}
		proxyGroupCount++
		switch {
		case g.Status.Changeover == spawneryv1alpha1.ChangeoverBegun && !changeoverFailing(g.Status.Conditions):
			inFlight++
		case g.Status.Changeover == spawneryv1alpha1.ChangeoverWaiting:
			waiting++
		}
	}

	network.Status.ServerGroups = serverGroupCount
	network.Status.ProxyGroups = proxyGroupCount
	network.Status.OnlinePlayers = players
	ChangeoversInFlight.WithLabelValues(network.Namespace, network.Name).Set(float64(inFlight))
	ChangeoversWaiting.WithLabelValues(network.Namespace, network.Name).Set(float64(waiting))
	return nil
}

// A List error returns nothing: the one-minute requeue covers it.
func (r *NetworkReconciler) siblingNetworks(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &spawneryv1alpha1.NetworkList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Name == obj.GetName() {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].Namespace,
			Name:      list.Items[i].Name,
		}})
	}
	return out
}
