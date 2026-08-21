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

package certs

import (
	"context"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// namespacesMissingCA returns the namespaces that hold a Network but whose
// spawnery-ca ConfigMap does not yet carry the given certificate, sorted.
//
// The namespaces to check come from the Network objects, not from the CA
// ConfigMaps. Those are different sets: the ConfigMap deliberately carries no
// owner reference (see internal/controller/bootstrap.go) so that it outlives
// the operator, and a Network's ownership of its namespace is the
// one-per-namespace convention (pickNamespaceOwner in
// internal/controller/network_controller.go), never a Kubernetes
// OwnerReference -- the operator never creates or owns a namespace. So a
// namespace whose Network was deleted keeps whatever spawnery-ca it last
// received, forever. Listing ConfigMaps instead of Networks would make the
// gate wait on that dead namespace until a human cleaned it up by hand, and a
// rotation would never complete on any cluster where a Network had ever been
// deleted.
func (s *Store) namespacesMissingCA(ctx context.Context, caCertPEM []byte) ([]string, error) {
	target, err := fingerprintFirst(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("target certificate: %w", err)
	}

	list := &spawneryv1alpha1.NetworkList{}
	if err := s.Client.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}

	// A namespace can hold at most one Network (the one-per-namespace rule),
	// but nothing here depends on that; distinct namespaces is all this
	// needs, and it is cheap insurance against a namespace briefly holding
	// two while one is on its way out.
	namespaces := make(map[string]struct{}, len(list.Items))
	for i := range list.Items {
		namespaces[list.Items[i].Namespace] = struct{}{}
	}

	var missing []string
	for ns := range namespaces {
		ok, err := s.namespaceHasCA(ctx, ns, target)
		if err != nil {
			// A read that failed is not "everything is fine": surface it
			// rather than silently treating the namespace as caught up, or a
			// switch could go ahead while an agent in this namespace never
			// saw the new CA at all.
			return nil, err
		}
		if !ok {
			missing = append(missing, ns)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// namespaceHasCA reports whether the namespace's spawnery-ca ConfigMap
// already carries target among its (possibly two, while a rotation
// overlaps) certificates.
func (s *Store) namespaceHasCA(ctx context.Context, namespace string, target [sha256.Size]byte) (bool, error) {
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: namespace}
	err := s.Client.Get(ctx, key, cm)
	switch {
	case apierrors.IsNotFound(err):
		// The bootstrapper has not written the ConfigMap in this namespace
		// yet (or it raced with this check). Missing, not an error: the
		// caller's job is to say which namespaces still need the CA, and an
		// absent ConfigMap needs it same as a present-but-stale one.
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get configmap %s/%s: %w", namespace, podspec.CAConfigMapName, err)
	}

	rest := []byte(cm.Data[podspec.CAConfigMapKey])
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return false, nil
		}
		// SHA-256 of the DER, not the PEM bytes: a re-encoded PEM with
		// different line wrapping is the same certificate, and comparing PEM
		// text would be a subtler way of getting that wrong.
		if sha256.Sum256(block.Bytes) == target {
			return true, nil
		}
	}
}

// fingerprintFirst decodes the first PEM block and returns the SHA-256 of its
// DER bytes.
func fingerprintFirst(certPEM []byte) ([sha256.Size]byte, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return [sha256.Size]byte{}, fmt.Errorf("certificate is not PEM")
	}
	return sha256.Sum256(block.Bytes), nil
}
