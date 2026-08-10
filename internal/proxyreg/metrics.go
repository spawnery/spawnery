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

package proxyreg

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// SessionsCut counts proxy sessions ended because their queue filled up. It is
// the only outward sign of a proxy that cannot keep up: the stream simply ends
// and the agent reconnects, which on its own looks like an ordinary reconnect.
var SessionsCut = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "spawnery_proxy_sessions_cut_total",
		Help: "Proxy sessions ended because the session fell too far behind.",
	},
)

func init() { metrics.Registry.MustRegister(SessionsCut) }
