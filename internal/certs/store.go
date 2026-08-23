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
	"k8s.io/client-go/tools/events"
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

	keyNextCACert     = "ca-next.crt"
	keyNextCAKey      = "ca-next.key"
	keyPreviousCACert = "ca-previous.crt"
	keyPreviousCAKey  = "ca-previous.key"
)

// Store reads and writes the bundle. It never caches: the manager's client
// does that, and a stale bundle would be worse than a read.
type Store struct {
	Client    client.Client
	Namespace string
	Name      string
	DNSNames  []string
	Clock     func() time.Time

	// AgentSessionDeadline is the running operator's own
	// --agent-session-deadline. It is half of the overlap window: after it,
	// every agent stream that was open when the CA ConfigMap changed has been
	// closed and reopened, so every agent has re-read the bundle. Zero means
	// the flag never reached this store, and a rotation refuses to time its
	// window rather than compute one that is short by exactly the deadline.
	AgentSessionDeadline time.Duration

	// Recorder writes the rotation events (events.go). Optional: left nil, a
	// Store still rotates correctly and simply reports nothing on the secret
	// -- which is what every construction of a Store in this package's own
	// tests does, since only main.go wires mgr.GetEventRecorder("certs") in.
	Recorder events.EventRecorder
}

// The namespace qualifier keeps this out of the ClusterRole: the bundle lives in
// the operator's own namespace, and a cluster-wide write on secrets would be the
// wrong right to hand out. Note the absent list and watch — Store uses an
// uncached client precisely so those are not needed.
//
// spawnery-system below is a placeholder, not a claim about where the bundle
// actually lives: controller-gen needs some literal namespace to emit a
// namespaced Role at all, and hack/chart-templates.sh replaces it with Helm's
// release namespace on every `make manifests`.
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
		// Unusable for whatever reason — a truncated write, a hand-edited
		// secret, an expired certificate. Start over rather than guess which.
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

// carryRotation reattaches whatever rotation slots stale was holding onto
// fresh. Issue, Reissue and reissueOrIssue know nothing about rotation --
// that would make them a state machine, and Task 4 is where that sequence
// lives -- so their output always comes back with both slots empty, and
// Ensure is the one place positioned to put them back before the write.
//
// The renewal branch and the parseCA-succeeds half of a repair both keep the
// signing CA (Reissue signs a fresh serving certificate under the same CA
// key), so carrying the slots there only restores what Ensure already read
// out of the secret. The parseCA-fails half of a repair is different:
// reissueOrIssue falls back to Issue, which mints a CA with no relationship
// to anything -- so carrying ca-next across that fallback means
// PublishedCA() will publish that unrelated new CA next to a "next" CA left
// over from a rotation whose outgoing CA no longer exists. That is a choice,
// not an oversight, and it was weighed against dropping the slot instead:
// dropping would leave the stored phase saying a rotation is distributing
// while the one thing that proves it -- the slot -- is gone, so Task 4's
// SwitchToNext would find no next CA and the sequence would stall until a
// human noticed. Agents are already stranded the moment ca.key stops
// parsing; carrying doesn't fix that, but it is the one outcome that leaves
// the stored state internally consistent for whoever drives the rotation
// that follows.
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
	// Written only when occupied: drop-old has to be able to remove a slot,
	// and an empty value in a secret is still a key that a merge could keep
	// around forever. write below replaces Data wholesale, so omitting the
	// key here is what actually removes it.
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
	p.current.Store(&snapshot{cert: cert, ca: b.PublishedCA()})
	// Here rather than in the tick loop: every path that changes what the
	// operator serves goes through this one call -- the startup pass, each
	// renewal, and every step of a rotation -- so a gauge set here cannot fall
	// behind the certificate it describes.
	observeExpiry(b)
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

// Start ensures a bundle once and then checks on a cadence that depends on
// whether a rotation is in flight. It is a leader-bound Runnable: only the
// leader may write the secret.
func (p *Provider) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("certs")

	bundle, err := p.store.Ensure(ctx)
	if err != nil {
		return fmt.Errorf("ensure the TLS bundle: %w", err)
	}
	// Seeded true, not false: if this first pass cannot find out, the
	// conservative answer is the rotation cadence. A restart in the middle of
	// a window that then checked hourly would be an hour of silence in the
	// one phase that is timed in minutes, while a needless 30-second poll of
	// a single secret costs nothing. The first pass that succeeds corrects
	// it, and refresh carries the answer from there.
	inFlight := true
	if advanced, rotating, err := p.store.AdvanceRotation(ctx, bundle); err != nil {
		// Not fatal at startup either: a rotation that cannot advance is a
		// stalled procedure, while the certificate in hand still works, and
		// refusing to serve because of an annotation would be a worse outage
		// than the one it describes.
		logger.Error(err, "the CA rotation did not advance")
	} else {
		bundle, inFlight = advanced, rotating
	}
	if err := p.Set(bundle); err != nil {
		return err
	}
	logger.Info("serving certificate ready")

	// A timer rather than a ticker: the interval changes when a rotation
	// starts or ends, and the point of RotationCheckInterval is that the
	// window is a quarter of an hour, not an hour.
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
// still in flight. wasInFlight comes back unchanged whenever the pass could
// not find out, so a failed read never silently slows the cadence down in the
// middle of a window.
func (p *Provider) refresh(ctx context.Context, wasInFlight bool) bool {
	logger := log.FromContext(ctx).WithName("certs")

	bundle, err := p.store.Ensure(ctx)
	if err != nil {
		// Keep serving the old certificate; it is still valid for a third of
		// its lifetime.
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

// NeedLeaderElection makes this a leader-bound runnable.
func (p *Provider) NeedLeaderElection() bool { return true }
