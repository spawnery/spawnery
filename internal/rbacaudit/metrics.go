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

package rbacaudit

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// PermissionsMissing is how many of the permissions the operator needs the API
// server says it does not have, by scope. Checker sets it on every round.
//
// It is the durable half of what the check produces. The log line names the
// verbs and the call sites and is the thing to read, but a log line is gone
// from a node long before anyone looks; this survives, and it is what the
// chart's SpawneryOperatorMissingPermissions alerts on.
//
// Absent, rather than zero, until the first check answers -- and left where it
// was when a check fails. Zero has to mean "asked, and nothing is missing",
// because that is the reading an alert acts on. Nothing sets these series to
// zero to make them appear, for the same reason.
var PermissionsMissing = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "spawnery_permissions_missing",
		Help: "Permissions the operator needs and the API server says it lacks, by scope.",
	},
	[]string{"scope"},
)

func init() { metrics.Registry.MustRegister(PermissionsMissing) }
