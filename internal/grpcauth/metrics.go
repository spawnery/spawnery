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

package grpcauth

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// AuthFailures counts refused streams. Without it a misconfigured agent is
// invisible outside the log.
var AuthFailures = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "spawnery_agent_auth_failures_total",
		Help: "Refused agent streams, by role.",
	},
	[]string{"role"},
)

func init() { metrics.Registry.MustRegister(AuthFailures) }
