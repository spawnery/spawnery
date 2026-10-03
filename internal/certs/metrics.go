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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// RotationPhase carries 1 for the active phase and 0 for the others.
	RotationPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spawnery_ca_rotation_phase",
		Help: "1 for the CA rotation phase currently in effect, 0 for the others.",
	}, []string{"phase"})

	RotationBlockedNamespaces = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "spawnery_ca_rotation_blocked_namespaces",
		Help: "Namespaces holding a Network whose CA ConfigMap does not yet carry the incoming CA.",
	})

	// CAExpiry: nothing schedules a CA rotation, so the ten-year CALifetime
	// would otherwise run out unseen. A timestamp rather than a remaining
	// duration, and no threshold here: the alert's margin belongs to whoever
	// runs the cluster.
	CAExpiry = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "spawnery_ca_expiry_timestamp_seconds",
		Help: "NotAfter of the CA currently signing the serving certificate, in Unix seconds.",
	})

	// ServingCertExpiry renews on its own (Bundle.NeedsRenewal); a value that
	// stops moving forward means renewal has stopped working.
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

// observeExpiry leaves a gauge alone on a parse failure: zero would read as
// "expired in 1970" to an alert.
func observeExpiry(b *Bundle) {
	if ca, _, err := b.parseCA(); err == nil {
		CAExpiry.Set(float64(ca.NotAfter.Unix()))
	}
	if serving, err := b.parseServing(); err == nil {
		ServingCertExpiry.Set(float64(serving.NotAfter.Unix()))
	}
}

// phaseNone is only a gauge label: drop-old and rollback delete the phase
// annotation rather than writing a third value.
const phaseNone = "none"

var rotationPhases = []string{PhaseDistributing, PhaseSwitched, phaseNone}

// setRotationPhase sets every known phase, the active one to 1. It runs on
// every tick that reads the phase, not only on changes, so a new leader
// repopulates the series. "" means no rotation.
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
