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

// ReviewCacheHits and ReviewCacheMisses make the cache visible. Milestone 6a
// established that a mechanism reporting nothing is indistinguishable from an
// absent one, and a cache nobody can see cannot be shown to be working.
var (
	ReviewCacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "spawnery_agent_token_review_cache_hits_total",
		Help: "Token checks answered without asking the API server.",
	})
	ReviewCacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "spawnery_agent_token_review_cache_misses_total",
		Help: "Token checks that required a TokenReview.",
	})
)

// RateLimited counts token checks refused by the per-peer rate limit, so a
// throttled peer is visible rather than just quietly slower.
var RateLimited = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "spawnery_agent_rate_limited_total",
	Help: "Token checks refused by the per-peer rate limit.",
})

func init() {
	metrics.Registry.MustRegister(AuthFailures)
	metrics.Registry.MustRegister(ReviewCacheHits, ReviewCacheMisses)
	metrics.Registry.MustRegister(RateLimited)
}
