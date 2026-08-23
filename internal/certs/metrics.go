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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// RotationPhase carries 1 for the active phase and 0 for the others, so a
	// query can ask "is anything rotating" without knowing which phase to look
	// for.
	RotationPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spawnery_ca_rotation_phase",
		Help: "1 for the CA rotation phase currently in effect, 0 for the others.",
	}, []string{"phase"})

	// RotationBlockedNamespaces is the point of this file. "Stuck in
	// distributing for two days" is the failure this design most plausibly
	// produces, and it should be a query rather than something somebody happens
	// to notice.
	RotationBlockedNamespaces = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "spawnery_ca_rotation_blocked_namespaces",
		Help: "Namespaces holding a Network whose CA ConfigMap does not yet carry the incoming CA.",
	})

	// CAExpiry and ServingCertExpiry exist because a CA rotation is asked for
	// and never scheduled. CALifetime is ten years and nothing in the operator
	// watches what is left of it: the procedure runs only when a human
	// annotates the secret, and nothing anywhere says when that should happen.
	// A ten-year clock is not urgent, but it is invisible, and invisible is
	// what makes it arrive as a surprise rather than as a plan.
	//
	// Exported as an absolute timestamp rather than a remaining duration,
	// which is the convention for expiry metrics: a gauge counting down is
	// wrong the moment scraping stops, while a timestamp stays true and lets
	// the query say `... - time() < 90 * 24 * 3600`. That is also why there is
	// no threshold in this code -- the number of days that should worry
	// somebody belongs to whoever runs the cluster, not to the operator.
	CAExpiry = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "spawnery_ca_expiry_timestamp_seconds",
		Help: "NotAfter of the CA currently signing the serving certificate, in Unix seconds.",
	})

	// ServingCertExpiry is the cheaper sibling and guards a different thing.
	// The serving certificate renews on its own -- Bundle.NeedsRenewal fires
	// at a third of its life remaining -- so this gauge is not a countdown
	// anybody has to act on. It is a check on the renewal path itself: if this
	// stops moving forward, renewal has stopped working, and nothing else in
	// the operator would say so.
	ServingCertExpiry = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "spawnery_serving_cert_expiry_timestamp_seconds",
		Help: "NotAfter of the operator's serving certificate, in Unix seconds.",
	})
)

func init() {
	metrics.Registry.MustRegister(
		RotationPhase, RotationBlockedNamespaces, CAExpiry, ServingCertExpiry,
	)
}

// observeExpiry publishes both certificates' NotAfter from a bundle the
// operator has just accepted.
//
// A parse failure leaves the gauge at whatever it last held rather than
// zeroing it, and that is deliberate: zero is a timestamp in 1970, so an alert
// written the obvious way would read a parse failure as "expired fifty years
// ago" and page somebody at four in the morning for a bundle that is fine. A
// bundle that will not parse cannot reach here in the first place -- Provider.Set
// fails on it before this is called -- so the stale value is the honest one.
func observeExpiry(b *Bundle) {
	if ca, _, err := b.parseCA(); err == nil {
		CAExpiry.Set(float64(ca.NotAfter.Unix()))
	}
	if serving, err := b.parseServing(); err == nil {
		ServingCertExpiry.Set(float64(serving.NotAfter.Unix()))
	}
}

// phaseNone is RotationPhase's label value for "no rotation in progress" --
// the state a cluster that has never rotated starts in, and the state
// drop-old and rollback both return to. It is deliberately not one of
// PhaseDistributing/PhaseSwitched: those are annotation values written to the
// secret, and this one is never written there -- drop-old and rollback both
// delete the phase annotation outright rather than setting it to a third
// value, so this exists only as a label on the gauge.
const phaseNone = "none"

// rotationPhases is every label value RotationPhase carries. Walked in full
// on every call so that setting one phase to 1 always sets every other phase
// to 0 in the same call -- the gauge has no other way to "unset" a phase it
// previously reported.
var rotationPhases = []string{PhaseDistributing, PhaseSwitched, phaseNone}

// setRotationPhase sets RotationPhase for the given phase to 1 and every
// other known phase to 0. Called on every tick that reads the secret's phase
// annotation -- not only on the ticks that change it -- so the gauge is
// self-healing across a restart: the Prometheus client library starts a
// GaugeVec's series unset until the first Set, and only a call on the very
// next tick after a leader change repopulates them.
//
// active should be one of PhaseDistributing, PhaseSwitched, or "" for no
// rotation in progress; "" is mapped to phaseNone here so callers can pass
// the annotation's own (possibly empty) value straight through.
func setRotationPhase(active string) {
	if active == "" {
		active = phaseNone
	}
	for _, phase := range rotationPhases {
		v := 0.0
		if phase == active {
			v = 1
		}
		RotationPhase.WithLabelValues(phase).Set(v)
	}
}
