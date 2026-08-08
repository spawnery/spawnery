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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/spawnery/spawnery/internal/podspec"
)

// Bootstrapper makes sure a namespace holds what a Spawnery agent pod needs
// before it can start: the CA bundle it pins and the ServiceAccount whose
// projected token it presents to the operator's gRPC endpoint. The Server
// controller calls Ensure for a namespace right before it creates the first
// pod in it.
type Bootstrapper struct {
	// Client is the manager's cached client. Task 9 restricts that cache to
	// objects carrying podspec.LabelManagedBy, so both objects Ensure writes
	// must carry it too, or Ensure would never see them as already present.
	Client client.Client
	// Reader is an uncached client, used only to recover from the case where
	// the cached Client's view of the CA ConfigMap has fallen out of sync
	// with the cluster (see the AlreadyExists handling in ensureConfigMap).
	// ensureServiceAccount has no comparable repair path — see its comment.
	Reader client.Reader
	// CA returns the current CA bundle. It is nil until certs.Provider has
	// published one; Ensure refuses to run until it returns a non-empty
	// bundle, because a pod started with an empty ca.crt would fail its TLS
	// handshake instead of waiting.
	CA func() []byte
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create

// Ensure makes sure namespace holds an up-to-date CA ConfigMap and a
// ServiceAccount for the agent pods that will run there. It is idempotent
// and safe to call on every reconcile.
//
// Neither object gets an OwnerReference: they are meant to outlive the
// operator, so a pod restarting during an operator outage still finds a CA
// to trust and a ServiceAccount to authenticate with.
func (b *Bootstrapper) Ensure(ctx context.Context, namespace string) error {
	ca := b.CA()
	if len(ca) == 0 {
		return fmt.Errorf("bootstrap namespace %q: no CA bundle available yet", namespace)
	}

	if err := b.ensureConfigMap(ctx, namespace, ca); err != nil {
		return fmt.Errorf("bootstrap namespace %q: %w", namespace, err)
	}
	if err := b.ensureServiceAccount(ctx, namespace); err != nil {
		return fmt.Errorf("bootstrap namespace %q: %w", namespace, err)
	}
	return nil
}

// EnsureAll calls Ensure for every namespace, stopping at the first error.
func (b *Bootstrapper) EnsureAll(ctx context.Context, namespaces []string) error {
	for _, ns := range namespaces {
		if err := b.Ensure(ctx, ns); err != nil {
			return err
		}
	}
	return nil
}

// ensureConfigMap creates or updates the CA ConfigMap.
func (b *Bootstrapper) ensureConfigMap(ctx context.Context, namespace string, ca []byte) error {
	label := func(cm *corev1.ConfigMap) {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[podspec.LabelManagedBy] = podspec.ManagedByValue
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[podspec.CAConfigMapKey] = string(ca)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: podspec.CAConfigMapName, Namespace: namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, b.Client, cm, func() error {
		label(cm)
		return nil
	})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}

	// The cached Client's Get missed the object — most likely because it
	// lost podspec.LabelManagedBy and fell out of the manager's restricted
	// cache — so CreateOrUpdate tried a Create the API server rejected as a
	// duplicate. Read the real object with the uncached Reader, repair it,
	// and write it back directly; that also restores the label, which puts
	// it back in the cache for next time.
	current := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: namespace}
	if err := b.Reader.Get(ctx, key, current); err != nil {
		return fmt.Errorf("re-read ConfigMap after AlreadyExists: %w", err)
	}
	label(current)
	if err := b.Client.Update(ctx, current); err != nil {
		return fmt.Errorf("repair ConfigMap: %w", err)
	}
	return nil
}

// ensureServiceAccount creates the agent's ServiceAccount if it is missing,
// and otherwise leaves it alone.
//
// Unlike ensureConfigMap, this deliberately never issues a Client.Update,
// on purpose written as a plain Get-then-Create rather than
// controllerutil.CreateOrUpdate: CreateOrUpdate's own Get-mutate-Update path
// would write the label back the moment its Get can see the object, which
// makes "no write" a fact about whatever cache Client happens to be wired to
// today rather than a fact about this function. Get-then-Create can't do
// that by construction — there is no code path here that calls Update, so
// the guarantee holds no matter how Task 9 configures the manager's cache.
//
// The RBAC marker above grants get;list;watch;create on serviceaccounts —
// deliberately no update. Restoring podspec.LabelManagedBy on a ServiceAccount
// that exists but lost it would need that verb, and granting it clusterwide
// just to restore a cosmetic label is a far bigger grant than the problem it
// solves: nothing about the agent's token or the pod that mounts it depends
// on the label. A ConfigMap's content, by contrast, is not cosmetic — a
// stale ca.crt breaks every agent that reads it — which is why
// ensureConfigMap keeps its repair path and its update verb.
//
// The consequence: a hand-edited, unlabelled ServiceAccount stays invisible
// to the restricted cache (Task 9), so every future Ensure for that
// namespace will see NotFound on the cached Get, attempt a Create, and get
// AlreadyExists back — one wasted API call per pod creation, in a namespace
// someone edited by hand. Cheaper than the permission.
func (b *Bootstrapper) ensureServiceAccount(ctx context.Context, namespace string) error {
	key := types.NamespacedName{Name: podspec.ServerServiceAccountName, Namespace: namespace}
	existing := &corev1.ServiceAccount{}
	err := b.Client.Get(ctx, key, existing)
	if err == nil {
		// It exists, which is all Ensure needs: the pod references it by
		// name regardless of its labels. No write follows, ever.
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podspec.ServerServiceAccountName,
			Namespace: namespace,
			Labels:    map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue},
		},
	}
	// AlreadyExists here just means someone else created it between our Get
	// and this Create; that is success, not a conflict to resolve.
	if err := b.Client.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
