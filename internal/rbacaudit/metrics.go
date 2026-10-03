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

package rbacaudit

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// PermissionsMissing outlives the log line and is what the chart's
// SpawneryOperatorMissingPermissions alerts on. Absent until the first check
// answers and left alone when a check fails, so zero always means "asked,
// and nothing is missing".
var PermissionsMissing = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "spawnery_permissions_missing",
		Help: "Permissions the operator needs and the API server says it lacks, by scope.",
	},
	[]string{"scope"},
)

func init() { metrics.Registry.MustRegister(PermissionsMissing) }
