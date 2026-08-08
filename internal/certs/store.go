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
	"crypto/tls"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// SecretName holds the CA and the serving certificate in the operator's
	// own namespace.
	SecretName = "spawnery-agent-tls"
	// RenewCheckInterval is how often the provider looks at the clock. The
	// serving certificate lives 90 days, so hourly is generous.
	RenewCheckInterval = time.Hour
)

// Store reads and writes the bundle. It never caches: the manager's client
// does that, and a stale bundle would be worse than a read.
type Store struct {
	Client    client.Client
	Namespace string
	Name      string
	DNSNames  []string
	Clock     func() time.Time
}

// The namespace qualifier keeps this out of the ClusterRole: the bundle lives in
// the operator's own namespace, and a cluster-wide write on secrets would be the
// wrong right to hand out. Note the absent list and watch — Store uses an
// uncached client precisely so those are not needed.
// +kubebuilder:rbac:groups="",namespace=spawnery-system,resources=secrets,verbs=get;create;update

// Ensure returns a usable bundle, creating or renewing it if needed. Safe to
// call repeatedly; only the leader ever does.
func (s *Store) Ensure(ctx context.Context) (*Bundle, error) {
	now := s.Clock()

	secret := &corev1.Secret{}
	err := s.Client.Get(ctx, types.NamespacedName{Name: s.Name, Namespace: s.Namespace}, secret)
	switch {
	case apierrors.IsNotFound(err):
		bundle, err := Issue(now, s.DNSNames)
		if err != nil {
			return nil, err
		}
		if err := s.Client.Create(ctx, s.secretFor(bundle)); err != nil {
			return nil, fmt.Errorf("create %s: %w", s.Name, err)
		}
		return bundle, nil
	case err != nil:
		return nil, fmt.Errorf("get %s: %w", s.Name, err)
	}

	bundle := &Bundle{
		CACertPEM:      secret.Data["ca.crt"],
		CAKeyPEM:       secret.Data["ca.key"],
		ServingCertPEM: secret.Data["tls.crt"],
		ServingKeyPEM:  secret.Data["tls.key"],
	}

	validErr := bundle.Validate(now, s.DNSNames)
	switch {
	case validErr != nil:
		// Unusable for whatever reason — a truncated write, a hand-edited
		// secret, an expired certificate. Start over rather than guess which.
		log.FromContext(ctx).Info("reissuing the TLS bundle", "reason", validErr.Error())
		fresh, err := s.reissueOrIssue(now, bundle)
		if err != nil {
			return nil, err
		}
		return fresh, s.write(ctx, secret, fresh)
	case bundle.NeedsRenewal(now):
		fresh, err := Reissue(now, bundle, s.DNSNames)
		if err != nil {
			return nil, err
		}
		return fresh, s.write(ctx, secret, fresh)
	}
	return bundle, nil
}

// reissueOrIssue keeps the CA if it is still intact, so agents that pinned it
// survive a damaged serving certificate.
func (s *Store) reissueOrIssue(now time.Time, broken *Bundle) (*Bundle, error) {
	if _, _, err := broken.parseCA(); err == nil {
		return Reissue(now, broken, s.DNSNames)
	}
	return Issue(now, s.DNSNames)
}

func (s *Store) secretFor(b *Bundle) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"ca.crt":  b.CACertPEM,
			"ca.key":  b.CAKeyPEM,
			"tls.crt": b.ServingCertPEM,
			"tls.key": b.ServingKeyPEM,
		},
	}
}

func (s *Store) write(ctx context.Context, existing *corev1.Secret, b *Bundle) error {
	existing.Type = corev1.SecretTypeTLS
	existing.Data = s.secretFor(b).Data
	if err := s.Client.Update(ctx, existing); err != nil {
		return fmt.Errorf("update %s: %w", s.Name, err)
	}
	return nil
}

// snapshot is one generation of the bundle: the serving certificate and the
// CA it chains to, published together so a reader never sees one half of a
// generation next to the other half of the next.
type snapshot struct {
	cert tls.Certificate
	ca   []byte
}

// Provider hands the current certificate to the TLS stack and renews it in the
// background.
type Provider struct {
	store   *Store
	current atomic.Pointer[snapshot]
}

// NewProvider wires a provider to a store.
func NewProvider(s *Store) *Provider { return &Provider{store: s} }

// Set publishes a bundle. The next handshake uses it; running connections keep
// the one they negotiated. The certificate and the CA bundle are published as
// one atomic snapshot, so a concurrent GetCertificate/CABundle pair always
// sees the same generation.
func (p *Provider) Set(b *Bundle) error {
	cert, err := b.TLSCertificate()
	if err != nil {
		return err
	}
	p.current.Store(&snapshot{cert: cert, ca: b.CACertPEM})
	return nil
}

// GetCertificate is the tls.Config callback.
func (p *Provider) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s := p.current.Load()
	if s == nil {
		return nil, fmt.Errorf("no serving certificate yet")
	}
	return &s.cert, nil
}

// CABundle is what the agents pin. It is a bundle, not a single certificate,
// so a later rotation can publish old and new side by side.
func (p *Provider) CABundle() []byte {
	s := p.current.Load()
	if s == nil {
		return nil
	}
	return s.ca
}

// Start ensures a bundle once and then checks hourly. It is a leader-bound
// Runnable: only the leader may write the secret.
func (p *Provider) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("certs")

	bundle, err := p.store.Ensure(ctx)
	if err != nil {
		return fmt.Errorf("ensure the TLS bundle: %w", err)
	}
	if err := p.Set(bundle); err != nil {
		return err
	}
	logger.Info("serving certificate ready")

	ticker := time.NewTicker(RenewCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			bundle, err := p.store.Ensure(ctx)
			if err != nil {
				// Keep serving the old certificate; it is still valid for a
				// third of its lifetime.
				logger.Error(err, "renewal failed, keeping the current certificate")
				continue
			}
			if err := p.Set(bundle); err != nil {
				logger.Error(err, "the renewed bundle is unusable")
			}
		}
	}
}

// NeedLeaderElection makes this a leader-bound runnable.
func (p *Provider) NeedLeaderElection() bool { return true }
