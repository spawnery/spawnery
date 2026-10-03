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

package agentserver

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var OpenStreams = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "spawnery_agent_open_streams",
		Help: "Open agent streams, by role.",
	},
	[]string{"role"},
)

var RejectedReports = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "spawnery_agent_rejected_reports_total",
		Help: "Agent reports discarded as implausible, by role.",
	},
	[]string{"role"},
)

// No pod, namespace or player label: cardinality, and a player's name does not
// belong in the monitoring stack's retention.
var RequestsRefused = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "spawnery_agent_requests_refused_total",
		Help: "Agent requests the operator declined, by reason.",
	},
	[]string{"reason"},
)

var OpenConnections = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "spawnery_agent_open_connections",
		Help: "Connections open on the agent endpoint.",
	},
)

// ExpectedAgents is a ceiling, not a target: Pending pods count. It is absent,
// not zero, until the first count succeeds.
var ExpectedAgents = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "spawnery_agents_expected",
		Help: "Managed pods that ought to hold an agent connection.",
	},
)

// No peer label: its values would be pod IPs, a cardinality the attacker
// chooses. The peer is in the log line instead.
var ConnectionsRefused = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "spawnery_agent_connections_refused_total",
		Help: "Connections refused for exceeding a connection bound, by bound.",
	},
	[]string{"bound"},
)

const (
	BoundPeer  = "peer"
	BoundFleet = "fleet"
)

func init() {
	metrics.Registry.MustRegister(
		RequestsRefused,
		OpenStreams, RejectedReports, OpenConnections, ExpectedAgents, ConnectionsRefused,
	)
	// A labelled counter does not exist until incremented, and increase() over
	// a series born mid-window has nothing to subtract from.
	ConnectionsRefused.WithLabelValues(BoundPeer)
	ConnectionsRefused.WithLabelValues(BoundFleet)
}
