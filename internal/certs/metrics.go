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
)

func init() { metrics.Registry.MustRegister(RotationPhase, RotationBlockedNamespaces) }

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
