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

package agentserver

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// OpenStreams is how many agent streams are live right now. Compared against
// the number of running pods it is the fastest way to see agents that cannot
// reach the operator.
var OpenStreams = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "spawnery_agent_open_streams",
		Help: "Open agent streams, by role.",
	},
	[]string{"role"},
)

// RejectedReports counts reports the registry refused. They are discarded
// without dropping the stream, so without this counter a lying agent would be
// invisible outside the log.
var RejectedReports = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "spawnery_agent_rejected_reports_total",
		Help: "Agent reports discarded as implausible, by role.",
	},
	[]string{"role"},
)

// OpenConnections is how many connections the agent listener holds right now,
// across every peer. An agent opens one per session and a renewal overlaps two
// for the length of a handover, so a fleet's steady state is its pod count and
// anything durably above that is a channel somebody is not closing.
//
// This is also the count milestone 2c's blind spot was about: OpenStreams
// above counts what the operator was asked to serve, so an agent leaking a
// gRPC channel per reconnect moved this number and nothing else. Until this
// existed, nothing measured it.
var OpenConnections = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "spawnery_agent_open_connections",
		Help: "Connections open on the agent endpoint.",
	},
)

// ConnectionsRefused counts connections turned away for being over their
// peer's bound (see peerLimiter). A healthy fleet never moves it, so any
// increase is either a compromised pod or a bound set too low -- and the two
// are told apart by whether the pods that own the addresses are behaving,
// which is why the log line beside it names the peer.
//
// No peer label. The label values would be pod IPs, which is one new time
// series per address the cluster ever assigns, driven by whoever is attacking:
// a cardinality bomb the attacker aims. The peer belongs in the log, where it
// is bounded by retention rather than by memory.
var ConnectionsRefused = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "spawnery_agent_connections_refused_total",
		Help: "Connections refused for exceeding the per-peer connection limit.",
	},
)

func init() {
	metrics.Registry.MustRegister(OpenStreams, RejectedReports, OpenConnections, ConnectionsRefused)
}
