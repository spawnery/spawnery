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
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	SecretName = "spawnery-agent-tls"
	// RenewCheckInterval is generous against a 90-day serving certificate.
	RenewCheckInterval = time.Hour

	keyNextCACert     = "ca-next.crt"
	keyNextCAKey      = "ca-next.key"
	keyPreviousCACert = "ca-previous.crt"
	keyPreviousCAKey  = "ca-previous.key"
)

// Store never caches: a stale bundle would be worse than a read.
type Store struct {
	Client    client.Client
	Namespace string
	Name      string
	DNSNames  []string
	Clock     func() time.Time

	// AgentSessionDeadline is the operator's --agent-session-deadline, half of
	// the overlap window: after it every agent has reconnected and re-read the
	// bundle. Zero makes a rotation refuse to time its window.
	AgentSessionDeadline time.Duration

	// Recorder is optional; nil records no events.
	Recorder events.EventRecorder
}

// Namespaced, so no cluster-wide secret write. No list or watch: Store uses an
// uncached client. spawnery-system is a placeholder that
// hack/chart-templates.sh replaces with the release namespace.
// +kubebuilder:rbac:groups="",namespace=spawnery-system,resources=secrets,verbs=get;create;update

// Ensure returns a usable bundle, creating or renewing it if needed.
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
		CACertPEM:         secret.Data["ca.crt"],
		CAKeyPEM:          secret.Data["ca.key"],
		ServingCertPEM:    secret.Data["tls.crt"],
		ServingKeyPEM:     secret.Data["tls.key"],
		NextCACertPEM:     secret.Data[keyNextCACert],
		NextCAKeyPEM:      secret.Data[keyNextCAKey],
		PreviousCACertPEM: secret.Data[keyPreviousCACert],
		PreviousCAKeyPEM:  secret.Data[keyPreviousCAKey],
	}

	validErr := bundle.Validate(now, s.DNSNames)
	switch {
	case validErr != nil:
		// Start over rather than guess what broke it.
		log.FromContext(ctx).Info("reissuing the TLS bundle", "reason", validErr.Error())
		fresh, err := s.reissueOrIssue(now, bundle)
		if err != nil {
			return nil, err
		}
		fresh = carryRotation(fresh, bundle)
		return fresh, s.write(ctx, secret, fresh)
	case bundle.NeedsRenewal(now):
		fresh, err := Reissue(now, bundle, s.DNSNames)
		if err != nil {
			return nil, err
		}
		fresh = carryRotation(fresh, bundle)
		return fresh, s.write(ctx, secret, fresh)
	}
	return bundle, nil
}

// carryRotation reattaches stale's rotation slots onto fresh, since Issue and
// Reissue know nothing about rotation. Also across a fallback to Issue, where
// the carried ca-next no longer relates to the new CA: dropping it would leave
// the phase annotation saying "distributing" with no slot to switch to.
func carryRotation(fresh, stale *Bundle) *Bundle {
	fresh.NextCACertPEM = stale.NextCACertPEM
	fresh.NextCAKeyPEM = stale.NextCAKeyPEM
	fresh.PreviousCACertPEM = stale.PreviousCACertPEM
	fresh.PreviousCAKeyPEM = stale.PreviousCAKeyPEM
	return fresh
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
	data := map[string][]byte{
		"ca.crt":  b.CACertPEM,
		"ca.key":  b.CAKeyPEM,
		"tls.crt": b.ServingCertPEM,
		"tls.key": b.ServingKeyPEM,
	}
	// Omitted when empty: write replaces Data wholesale, so omission is what
	// removes a slot.
	if len(b.NextCACertPEM) > 0 {
		data[keyNextCACert] = b.NextCACertPEM
		data[keyNextCAKey] = b.NextCAKeyPEM
	}
	if len(b.PreviousCACertPEM) > 0 {
		data[keyPreviousCACert] = b.PreviousCACertPEM
		data[keyPreviousCAKey] = b.PreviousCAKeyPEM
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data:       data,
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

// snapshot keeps the serving certificate and its CA bundle in one generation.
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

func NewProvider(s *Store) *Provider { return &Provider{store: s} }

// Set publishes a bundle. The next handshake uses it; running connections keep
// the one they negotiated.
func (p *Provider) Set(b *Bundle) error {
	cert, err := b.TLSCertificate()
	if err != nil {
		return err
	}
	p.current.Store(&snapshot{cert: cert, ca: b.PublishedCA()})
	// Every change to what the operator serves passes through here.
	observeExpiry(b)
	return nil
}

func (p *Provider) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s := p.current.Load()
	if s == nil {
		return nil, fmt.Errorf("no serving certificate yet")
	}
	return &s.cert, nil
}

// CABundle is what the agents pin.
func (p *Provider) CABundle() []byte {
	s := p.current.Load()
	if s == nil {
		return nil
	}
	return s.ca
}

// Start ensures a bundle once and then checks on a cadence that depends on
// whether a rotation is in flight.
func (p *Provider) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("certs")

	bundle, err := p.store.Ensure(ctx)
	if err != nil {
		return fmt.Errorf("ensure the TLS bundle: %w", err)
	}
	// Seeded true: if the first pass cannot tell, polling a timed window too
	// often is cheap and polling it hourly is not.
	inFlight := true
	if advanced, rotating, err := p.store.AdvanceRotation(ctx, bundle); err != nil {
		// Not fatal: the certificate in hand still works.
		logger.Error(err, "the CA rotation did not advance")
	} else {
		bundle, inFlight = advanced, rotating
	}
	if err := p.Set(bundle); err != nil {
		return err
	}
	logger.Info("serving certificate ready")

	// A timer, because the interval changes when a rotation starts or ends.
	timer := time.NewTimer(checkInterval(inFlight))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			inFlight = p.refresh(ctx, inFlight)
			timer.Reset(checkInterval(inFlight))
		}
	}
}

// refresh runs one renewal-and-rotation pass and reports whether a rotation is
// still in flight; on a failed read it returns wasInFlight unchanged.
func (p *Provider) refresh(ctx context.Context, wasInFlight bool) bool {
	logger := log.FromContext(ctx).WithName("certs")

	bundle, err := p.store.Ensure(ctx)
	if err != nil {
		// Renewal starts with a third of the lifetime left.
		logger.Error(err, "renewal failed, keeping the current certificate")
		return wasInFlight
	}

	inFlight := wasInFlight
	if advanced, rotating, err := p.store.AdvanceRotation(ctx, bundle); err != nil {
		logger.Error(err, "the CA rotation did not advance")
	} else {
		bundle, inFlight = advanced, rotating
	}
	if err := p.Set(bundle); err != nil {
		logger.Error(err, "the renewed bundle is unusable")
	}
	return inFlight
}

func checkInterval(inFlight bool) time.Duration {
	if inFlight {
		return RotationCheckInterval
	}
	return RenewCheckInterval
}

func (p *Provider) NeedLeaderElection() bool { return true }
