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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ChangeoversInFlight counts the groups AdmitChangeovers holds a place for;
// ChangeoversWaiting those it makes wait. NetworkReconciler.countGroups sets both.
var (
	ChangeoversInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spawnery_network_changeovers_in_flight",
		Help: "Groups of the network holding a changeover budget place.",
	}, []string{"namespace", "network"})

	ChangeoversWaiting = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spawnery_network_changeovers_waiting",
		Help: "Groups of the network waiting for a changeover budget place or an earlier stage.",
	}, []string{"namespace", "network"})
)

func init() {
	metrics.Registry.MustRegister(ChangeoversInFlight, ChangeoversWaiting)
}
