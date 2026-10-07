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

package worldsync

import "github.com/prometheus/client_golang/prometheus"

var (
	downloadSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "spawnery_worldsync_download_seconds", Help: "Time to download a world at publish.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 40, 80},
	})
	pendingUploads = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "spawnery_worldsync_pending_snapshots", Help: "Snapshots on this node not yet in the bucket.",
	})
	leaseConflicts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "spawnery_worldsync_lease_conflicts_total", Help: "Publishes refused because another node held the world.",
	})
	orphans = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "spawnery_worldsync_orphans_total", Help: "Local copies moved aside after the lease was lost.",
	})
)

// Collectors are registered by the binary, not here, so tests need no registry.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{downloadSeconds, pendingUploads, leaseConflicts, orphans}
}
