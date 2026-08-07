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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// NetworkReconciler enforces one network per namespace and publishes the
// aggregated network status.
type NetworkReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Clock is injectable so the time rules are testable.
	Clock func() time.Time
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=networks,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=networks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups,verbs=get;list;watch

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
		meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
			Type:   spawneryv1alpha1.ConditionAccepted,
			Status: metav1.ConditionFalse,
			Reason: spawneryv1alpha1.ReasonDuplicateNetwork,
			Message: fmt.Sprintf(
				"namespace %q is already served by network %q; put staging and production in separate namespaces",
				network.Namespace, owner),
		})
		return ctrl.Result{RequeueAfter: time.Minute}, r.Status().Update(ctx, network)
	}

	meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionAccepted,
		Status:  metav1.ConditionTrue,
		Reason:  spawneryv1alpha1.ReasonAccepted,
		Message: "this network owns its namespace",
	})

	serverGroups := &spawneryv1alpha1.ServerGroupList{}
	if err := r.List(ctx, serverGroups, client.InNamespace(network.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	proxyGroups := &spawneryv1alpha1.ProxyGroupList{}
	if err := r.List(ctx, proxyGroups, client.InNamespace(network.Namespace)); err != nil {
		return ctrl.Result{}, err
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

	return ctrl.Result{RequeueAfter: resyncInterval}, r.Status().Update(ctx, network)
}

// namespaceOwner picks the network that owns the namespace: the oldest one,
// with the name as the tiebreaker so the choice is stable across reconciles.
func (r *NetworkReconciler) namespaceOwner(ctx context.Context, namespace string) (string, error) {
	list := &spawneryv1alpha1.NetworkList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return "", err
	}

	owner := ""
	var ownerCreated metav1.Time
	for i := range list.Items {
		n := &list.Items[i]
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
	return owner, nil
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
func (r *NetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.Network{}).
		Named("network").
		Complete(r)
}
