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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
)

const DefaultOrphanInterval = time.Minute

// OrphanReconciler reconciles the two directions no watch covers: a managed
// pod whose Server is gone, and a Server whose group is gone. It also drops
// registry entries of pods that no longer exist.
type OrphanReconciler struct {
	client.Client

	Agents *agent.Registry
	// Zero means DefaultOrphanInterval.
	Interval time.Duration
	Clock    func() time.Time
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=list;delete

func (r *OrphanReconciler) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultOrphanInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.Sweep(ctx); err != nil {
				log.FromContext(ctx).Error(err, "orphan sweep failed")
			}
		}
	}
}

func (r *OrphanReconciler) Sweep(ctx context.Context) error {
	logger := log.FromContext(ctx)

	// Managed-by alone, not role=server: the registry pruning below must see
	// proxy pods too.
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingLabels{
		podspec.LabelManagedBy: podspec.ManagedByValue,
	}); err != nil {
		return err
	}

	liveUIDs := make(map[string]bool, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		// Before the deletion skip: a draining pod still serves players, and
		// forgetting its registry entry loses its occupancy.
		liveUIDs[string(pod.UID)] = true

		if !pod.DeletionTimestamp.IsZero() {
			continue
		}

		var err error
		switch pod.Labels[podspec.LabelRole] {
		case podspec.RoleServer:
			err = r.sweepServerPod(ctx, logger, pod)
		case podspec.RoleProxy:
			err = r.sweepProxyPod(ctx, logger, pod)
		}
		if err != nil {
			return err
		}
	}

	servers := &spawneryv1alpha1.ServerList{}
	if err := r.List(ctx, servers); err != nil {
		return err
	}
	for i := range servers.Items {
		srv := &servers.Items[i]
		if !srv.DeletionTimestamp.IsZero() {
			continue
		}

		group := &spawneryv1alpha1.ServerGroup{}
		key := types.NamespacedName{Name: srv.Spec.GroupRef.Name, Namespace: srv.Namespace}
		err := r.Get(ctx, key, group)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}

		logger.Info("deleting a Server whose group is gone", "server", srv.Name, "namespace", srv.Namespace)
		if err := r.Delete(ctx, srv); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	for _, key := range r.Agents.Keys() {
		if !liveUIDs[key] {
			r.Agents.Forget(key)
		}
	}

	return r.sweepExpiredBoosts(ctx, logger)
}

// sweepExpiredBoosts only tidies expired ScaleBoosts away; liveBoost reads the
// clock, so a boost stops counting at its stated time, not at the next sweep.
func (r *OrphanReconciler) sweepExpiredBoosts(ctx context.Context, logger logr.Logger) error {
	boosts := &spawneryv1alpha1.ScaleBoostList{}
	if err := r.List(ctx, boosts); err != nil {
		return err
	}

	now := r.Clock()
	for i := range boosts.Items {
		b := &boosts.Items[i]
		if b.Spec.ExpiresAt == nil || b.Spec.ExpiresAt.After(now) {
			continue
		}
		logger.V(1).Info("removing an expired scale boost",
			"boost", b.Name, "namespace", b.Namespace, "group", b.Spec.GroupRef.Name)
		if err := r.Delete(ctx, b); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *OrphanReconciler) sweepServerPod(ctx context.Context, logger logr.Logger, pod *corev1.Pod) error {
	serverName := pod.Labels[podspec.LabelServer]
	if serverName == "" {
		return nil
	}

	srv := &spawneryv1alpha1.Server{}
	key := types.NamespacedName{Name: serverName, Namespace: pod.Namespace}
	err := r.Get(ctx, key, srv)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	logger.Info("deleting a managed pod whose Server is gone", "pod", pod.Name, "namespace", pod.Namespace)
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *OrphanReconciler) sweepProxyPod(ctx context.Context, logger logr.Logger, pod *corev1.Pod) error {
	groupName := pod.Labels[podspec.LabelGroup]
	if groupName == "" {
		return nil
	}

	group := &spawneryv1alpha1.ProxyGroup{}
	key := types.NamespacedName{Name: groupName, Namespace: pod.Namespace}
	err := r.Get(ctx, key, group)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	logger.Info("deleting a proxy pod whose ProxyGroup is gone", "pod", pod.Name, "namespace", pod.Namespace)
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
