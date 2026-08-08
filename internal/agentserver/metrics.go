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

func init() { metrics.Registry.MustRegister(OpenStreams, RejectedReports) }
