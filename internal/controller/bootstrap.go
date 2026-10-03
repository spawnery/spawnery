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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/spawnery/spawnery/internal/podspec"
)

// Bootstrapper makes sure a namespace holds the CA bundle and ServiceAccounts
// an agent pod needs before it can start.
type Bootstrapper struct {
	// Client's cache holds only objects carrying podspec.LabelManagedBy, so
	// every object Ensure writes must carry it.
	Client client.Client
	// Reader is uncached; ensureConfigMap uses it to repair a ConfigMap the
	// cache lost.
	Reader client.Reader
	// CA is empty until certs.Provider has published a bundle; Ensure refuses
	// to run until then, since an empty ca.crt fails the TLS handshake.
	CA func() []byte
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create

// These objects get no OwnerReference: a pod restarting during an operator
// outage must still find a CA and a ServiceAccount.
func (b *Bootstrapper) Ensure(ctx context.Context, namespace string) error {
	ca := b.CA()
	if len(ca) == 0 {
		return fmt.Errorf("bootstrap namespace %q: no CA bundle available yet", namespace)
	}

	if err := b.ensureConfigMap(ctx, namespace, ca); err != nil {
		return fmt.Errorf("bootstrap namespace %q: %w", namespace, err)
	}
	if err := b.ensureServiceAccounts(ctx, namespace); err != nil {
		return fmt.Errorf("bootstrap namespace %q: %w", namespace, err)
	}
	return nil
}

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

	// The cached Get missed an object that lost podspec.LabelManagedBy; repair
	// it through the uncached Reader, which also restores the label.
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

// ensureServiceAccounts never updates: the RBAC marker deliberately grants no
// update on serviceaccounts, as the label is cosmetic for a ServiceAccount.
// An unlabelled one costs a wasted Create per pod creation, cheaper than the
// permission. Both are created in every namespace, since the caller cannot know
// whether a ProxyGroup will appear there.
func (b *Bootstrapper) ensureServiceAccounts(ctx context.Context, namespace string) error {
	for _, name := range []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName} {
		key := types.NamespacedName{Name: name, Namespace: namespace}
		existing := &corev1.ServiceAccount{}
		err := b.Client.Get(ctx, key, existing)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}

		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue},
			},
		}
		if err := b.Client.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}
