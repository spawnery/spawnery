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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
)

// DefaultOrphanInterval is how often the sweep runs.
const DefaultOrphanInterval = time.Minute

// OrphanReconciler reconciles the two directions no watch covers: a managed
// pod whose Server is gone, and a Server whose group is gone. It also drops
// registry entries of pods that no longer exist, so the map stays bounded.
//
// It is a manager Runnable rather than a controller, because it is a periodic
// full comparison and not a reaction to an event.
type OrphanReconciler struct {
	client.Client
	Recorder record.EventRecorder

	// Agents is the runtime state to be pruned.
	Agents *agent.Registry
	// Interval is how often Start runs the sweep. Zero means the default.
	Interval time.Duration
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=list;delete

// Start runs the sweep until the context is cancelled. It implements
// manager.Runnable.
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

// Sweep runs one full comparison.
func (r *OrphanReconciler) Sweep(ctx context.Context) error {
	logger := log.FromContext(ctx)

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingLabels{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelRole:      podspec.RoleServer,
	}); err != nil {
		return err
	}

	liveUIDs := make(map[string]bool, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		liveUIDs[string(pod.UID)] = true

		if !pod.DeletionTimestamp.IsZero() {
			continue
		}

		serverName := pod.Labels[podspec.LabelServer]
		if serverName == "" {
			continue
		}

		srv := &spawneryv1alpha1.Server{}
		key := types.NamespacedName{Name: serverName, Namespace: pod.Namespace}
		err := r.Get(ctx, key, srv)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}

		// No Server object: nobody owns this pod, so nobody would ever drain
		// it. Deleting it is the only way the count converges.
		logger.Info("deleting a managed pod whose Server is gone", "pod", pod.Name, "namespace", pod.Namespace)
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
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

	return nil
}
