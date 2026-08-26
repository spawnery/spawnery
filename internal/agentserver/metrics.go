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

// ExpectedAgents is how many agent connections the fleet ought to be holding:
// the count of managed pods, from the operator's own caches. It is the first
// number in this channel that says anything about the fleet rather than about
// one peer, and it exists because the comparison it enables cannot be done
// anywhere else -- OpenConnections above is a fact about the operator, the pod
// count is a fact about the cluster, and only their ratio says whether the
// connections open are the ones that ought to be.
//
// Read it as a ceiling on what ought to connect, not as a target. A pod that
// is Pending, or one whose agent has not started, is counted here and holds
// nothing; the count is deliberately the loose direction, because it bounds a
// refusal (see FleetConnectionsPerAgent) and a count that ran low would refuse
// an agent that had done nothing wrong.
//
// Absent, rather than zero, until the first count succeeds. Zero is a real
// answer -- a cluster with no groups -- and the two must not be confused by
// anything alerting on this.
var ExpectedAgents = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "spawnery_agents_expected",
		Help: "Managed pods that ought to hold an agent connection.",
	},
)

// ConnectionsRefused counts connections turned away for being over a bound
// (see PeerLimiter). A healthy fleet never moves it, so any increase is either
// a compromised pod or a bound set too low -- and the two are told apart by
// whether the pods that own the addresses are behaving, which is why the log
// line beside it names the peer.
//
// The bound label is "peer" or "fleet" and separates the two readings the
// refusal has. A peer refusal is one pod over MaxConnectionsPerPeer and says
// nothing about anybody else. A fleet refusal is the slack in that bound being
// withheld from every peer at once, because the connections open across the
// fleet had passed what its pod count can account for -- so it is a statement
// about the fleet, and a peer named in one may be entirely innocent.
//
// No peer label. The label values would be pod IPs, which is one new time
// series per address the cluster ever assigns, driven by whoever is attacking:
// a cardinality bomb the attacker aims. The bound label above is safe for the
// opposite reason -- it has two values and the code, not the peer, chooses
// them. The peer belongs in the log, where it is bounded by retention rather
// than by memory.
var ConnectionsRefused = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "spawnery_agent_connections_refused_total",
		Help: "Connections refused for exceeding a connection bound, by bound.",
	},
	[]string{"bound"},
)

// BoundPeer and BoundFleet are the values of ConnectionsRefused's bound label.
const (
	BoundPeer  = "peer"
	BoundFleet = "fleet"
)

func init() {
	metrics.Registry.MustRegister(
		OpenStreams, RejectedReports, OpenConnections, ExpectedAgents, ConnectionsRefused,
	)
	// Both series at zero from the start. A labelled counter does not exist
	// until something increments it, and a refusal counter that appears only
	// once there is trouble is the wrong shape twice over: a dashboard reads
	// the absence as a gap rather than as a healthy zero, and increase() over
	// a series born mid-window has nothing to subtract from.
	ConnectionsRefused.WithLabelValues(BoundPeer)
	ConnectionsRefused.WithLabelValues(BoundFleet)
}
