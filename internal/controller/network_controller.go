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
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	"github.com/spawnery/spawnery/internal/podspec"
)

// NetworkReconciler enforces one network per namespace and publishes the
// aggregated network status.
type NetworkReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// SecretReader reads the Network's forwarding secret. It must be an
	// uncached reader — mgr.GetAPIReader(), which setup.go supplies: a cached
	// Secret would need an informer over every Secret in scope, and this
	// operator deliberately holds no list or watch on them
	// (internal/rbacaudit/required.go).
	SecretReader client.Reader

	// OperatorNamespace is where this operator runs, and it is the one value
	// the policy cannot derive from the Network it protects.
	OperatorNamespace string

	// Bootstrap puts the CA bundle and the agent ServiceAccounts into this
	// Network's namespace. It is the same instance ServerReconciler holds:
	// that one guarantees the objects exist before the first pod needs them,
	// and this one guarantees they stay current afterwards, in a namespace
	// where no pod is being created and nothing else would call Ensure at
	// all.
	Bootstrap *Bootstrapper
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=networks,verbs=get;list;watch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=networks/status,verbs=update
// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups,verbs=list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=list
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update

// Reconcile decides whether this network is the one that owns its namespace
// and, if so, sums up its groups.
func (r *NetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	network := &spawneryv1alpha1.Network{}
	if err := r.Get(ctx, req.NamespacedName, network); err != nil {
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
		// Counted even though this Network is being refused, because the
		// numbers are about what points at it and not about whether it is
		// serving. A refused Network used to report whatever it last counted,
		// which for the ordinary case -- created second, refused from its very
		// first pass -- was zero forever, however many groups were later
		// pointed at it. The count is how you see what is stranded behind the
		// refusal.
		//
		// A failure here is not allowed to swallow the refusal: the condition
		// below is the more important of the two things this pass has to say,
		// so a List error is logged and the refusal is written anyway.
		if err := r.countGroups(ctx, network); err != nil {
			log.FromContext(ctx).Error(err, "counting the groups behind a refused network")
		}
		message := fmt.Sprintf(
			"namespace %q is already served by network %q; put staging and production in separate namespaces",
			network.Namespace, owner)
		// The condition alone was the whole report until now, which meant a
		// Network created into an occupied namespace did nothing observable
		// unless somebody thought to describe that particular object. Everything
		// else this reconciler refuses -- a missing forwarding secret, a
		// namespace it could not bootstrap -- says so as an event too, and this
		// is the refusal a user is most likely to cause by hand.
		//
		// Gated on the transition, like the forwarding-secret events beside it:
		// this branch runs on every pass for as long as the duplicate stands,
		// and an event per minute forever is not a report, it is noise that
		// buries the one that mattered.
		if !hasConditionReason(network.Status.Conditions,
			spawneryv1alpha1.ConditionAccepted, spawneryv1alpha1.ReasonDuplicateNetwork) {
			r.Recorder.Eventf(network, nil, corev1.EventTypeWarning,
				spawneryv1alpha1.ReasonDuplicateNetwork, actionSyncStatus, "%s",
				eventNote("%s", message))
		}
		meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonDuplicateNetwork,
			Message: message,
		})
		return ctrl.Result{RequeueAfter: time.Minute}, r.Status().Update(ctx, network)
	}

	meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionAccepted,
		Status:  metav1.ConditionTrue,
		Reason:  spawneryv1alpha1.ReasonAccepted,
		Message: "this network owns its namespace",
	})

	// The policy, before anything else this reconcile does. A Forbidden here
	// is a security control failing to land, and it must not pass silently:
	// returning the error logs it and requeues. It deliberately does not
	// become a condition on the Network — the design's §2.4 argues that this
	// shape needs no report, and an error that appears only under an RBAC
	// misconfiguration is a fact about the installation rather than about
	// this object.
	if err := r.reconcileNetworkPolicy(ctx, network); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile the network policy: %w", err)
	}

	if err := r.countGroups(ctx, network); err != nil {
		return ctrl.Result{}, err
	}

	// The forwarding secret. This sits after the Accepted branch above returns,
	// so a Network that does not own its namespace never reads a secret it does
	// not manage.
	read := readForwardingSecret(ctx, r.SecretReader, network)
	if read.Hash != "" {
		if previous := network.Status.ForwardingSecretHash; previous != "" && previous != read.Hash {
			r.Recorder.Eventf(network, nil, corev1.EventTypeWarning,
				spawneryv1alpha1.EventForwardingSecretRotated, actionSyncStatus,
				"the forwarding secret changed; roll the server groups first, then the proxy groups — see %s",
				rotationRunbook)
		}
		// Only on a successful read: see NetworkStatus.ForwardingSecretHash.
		network.Status.ForwardingSecretHash = read.Hash
	}
	// Both events fire on entering a state, so the condition as it stands
	// before SetStatusCondition below is what says whether this is an entry.
	if read.Reason == spawneryv1alpha1.ReasonSecretNotFound &&
		!hasConditionReason(network.Status.Conditions,
			spawneryv1alpha1.ConditionForwardingSecretResolved, read.Reason) {
		r.Recorder.Eventf(network, nil, corev1.EventTypeWarning,
			spawneryv1alpha1.EventForwardingSecretNotFound, actionSyncStatus, "%s",
			eventNote("%s", read.Message))
	}
	meta.SetStatusCondition(&network.Status.Conditions, resolvedCondition(read))

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(network.Namespace),
		client.MatchingLabels(podspec.ManagedSelector(network.Name))); err != nil {
		return ctrl.Result{}, err
	}
	meta.SetStatusCondition(&network.Status.Conditions,
		rotationCondition(read, forwardingStamps(pods.Items)))

	if err := r.Status().Update(ctx, network); err != nil {
		return ctrl.Result{}, err
	}

	// The bootstrap runs after acceptance, after the policy, and last of all
	// -- after the status has been persisted.
	//
	// After acceptance and after the policy because a Network that lost its
	// namespace must write nothing into it, and because a namespace left
	// without its NetworkPolicy would be the worse trade if a ConfigMap write
	// were the thing blocking.
	//
	// Last because Ensure fails for two very different reasons and only one of
	// them passes on its own. An empty bundle -- the operator started, but
	// certs.Provider has not published yet -- clears itself within seconds. A
	// refused write does not: an admission webhook, a ResourceQuota on
	// ConfigMaps, a namespace policy stripping what it does not recognise.
	// Ahead of the status update, a reconcile in such a namespace would record
	// nothing at all for as long as the refusal stood. A new Network would
	// never persist Accepted, and both servergroup_controller.go and
	// proxygroup_controller.go gate on that condition, so every group in the
	// namespace would stop with a log line as the only trace. An accepted one
	// would keep a stale Accepted=True while its player counts, its group
	// counts and its forwarding-secret rotation condition all froze.
	//
	// This is not what ServerReconciler does on the same call, and the
	// difference is deliberate: there the bootstrap gates a pod creation that
	// has not happened yet, so it says so on the Server's own Accepted
	// condition and falls through to the status update. A Network has no such
	// verdict to record -- it owns its namespace either way -- so the event
	// carries the report and the returned error requeues.
	if err := r.Bootstrap.Ensure(ctx, network.Namespace); err != nil {
		r.Recorder.Eventf(network, nil, corev1.EventTypeWarning,
			ReasonNamespaceNotBootstrapped, actionBootstrapNamespace, "%s",
			eventNote("cannot bootstrap namespace %s: %v", network.Namespace, err))
		return ctrl.Result{}, fmt.Errorf("bootstrap the namespace: %w", err)
	}

	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// reconcileNetworkPolicy keeps the policy that admits only this network's own
// proxies to its own backends. It carries no delete: the owner reference on
// the object means the garbage collector removes it when the Network goes,
// which is why internal/rbacaudit's table has none either.
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

// namespaceOwner picks the network that owns the namespace out of everything
// that currently lives in it.
func (r *NetworkReconciler) namespaceOwner(ctx context.Context, namespace string) (string, error) {
	list := &spawneryv1alpha1.NetworkList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return "", err
	}
	return pickNamespaceOwner(list.Items), nil
}

// pickNamespaceOwner is the one-network-per-namespace rule: the oldest network
// owns the namespace, with the name as the tiebreaker so the choice is stable
// across reconciles when two were created within the same second. Networks on
// their way out are skipped, so deleting the owner hands the namespace to the
// next oldest rather than leaving every group in it unsized.
//
// Age has to decide before the name does. The reverse — newest wins — would
// mean one stray kubectl apply of a Network rejects the running one, and every
// ServerGroup in the namespace stops sizing until somebody deletes the
// newcomer. It is split out from namespaceOwner because that difference is only
// testable over a list with hand-set creation timestamps: two Networks created
// back to back against a real API server land in the same second, the tie-break
// decides, and the comparison direction never comes into it.
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

// SetupWithManager registers the controller.
//
// No Owns/Watches on ServerGroup: nothing sets an owner reference from a
// ServerGroup to its Network, so a watch keyed on that owner reference could
// never fire. The aggregated status is kept fresh by the resyncInterval poll
// in Reconcile instead. An event-driven refresh would need a mapping handler
// from a ServerGroup (or ProxyGroup) change to its Network's request, which is
// for Task 12's manager wiring to add if the poll interval turns out to be
// too coarse.
//
// Owns(&networkingv1.NetworkPolicy{}) is different: the policy does carry an
// owner reference, so a hand-deleted one comes back on the next watch event
// instead of waiting out resyncInterval.
func (r *NetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.Network{}).
		Owns(&networkingv1.NetworkPolicy{}).
		// For() enqueues the Network the event is about. This enqueues its
		// siblings, which is a different question and the one that was going
		// unanswered: pickNamespaceOwner decides between the Networks in a
		// namespace, so deleting the winner changes the verdict for every
		// loser -- and none of them is the object the delete event names.
		// Without this they waited out the one-minute requeue below.
		Watches(&spawneryv1alpha1.Network{},
			handler.EnqueueRequestsFromMapFunc(r.siblingNetworks)).
		Named("network").
		Complete(r)
}

// countGroups writes the three counts a Network's status carries: the groups
// in its namespace that name it, and the players those server groups report.
//
// Both directions of the filter matter. A namespace can hold more than one
// Network -- that is what the duplicate rule refuses, not what it prevents --
// so counting every group in the namespace would credit this Network with
// groups pointed at its rival.
func (r *NetworkReconciler) countGroups(ctx context.Context, network *spawneryv1alpha1.Network) error {
	serverGroups := &spawneryv1alpha1.ServerGroupList{}
	if err := r.List(ctx, serverGroups, client.InNamespace(network.Namespace)); err != nil {
		return err
	}
	proxyGroups := &spawneryv1alpha1.ProxyGroupList{}
	if err := r.List(ctx, proxyGroups, client.InNamespace(network.Namespace)); err != nil {
		return err
	}

	var serverGroupCount, players int32
	for _, g := range serverGroups.Items {
		if g.Spec.NetworkRef.Name != network.Name {
			continue
		}
		serverGroupCount++
		players += g.Status.OnlinePlayers
	}
	var proxyGroupCount int32
	for _, g := range proxyGroups.Items {
		if g.Spec.NetworkRef.Name == network.Name {
			proxyGroupCount++
		}
	}

	network.Status.ServerGroups = serverGroupCount
	network.Status.ProxyGroups = proxyGroupCount
	network.Status.OnlinePlayers = players
	return nil
}

// siblingNetworks maps a Network event onto the other Networks in its
// namespace.
//
// It excludes the object the event is about, because For() already has that
// one and enqueueing it twice buys nothing. A List error returns nothing
// rather than failing: this is an optimisation over the requeue, so losing it
// costs a minute of latency and never correctness.
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
