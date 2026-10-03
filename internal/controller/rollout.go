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

package controller

import (
	"sort"
	"time"
)

// ProxyView is not a corev1.Pod so the rollout rules stay testable without a
// cluster.
type ProxyView struct {
	Name         string
	Stale        bool
	Ready        bool
	Draining     bool
	Players      int32
	PlayersStale bool
	CreatedAt    time.Time
	// RetireRequested is an admin's retire request; such a pod is stale.
	RetireRequested bool
}

type RolloutDecision struct {
	Create int32
	Drain  []string
}

// DecideRollout rolls blue/green while the group may surge: every stale pod
// gets its replacement up front, and once replicas current pods are Ready
// every stale pod is marked at once. Until then a surplus drains from current
// pods only, since stale pods are still serving. Without surge a stale pod is
// replaced in place only when it serves nobody or ready capacity is spare.
func DecideRollout(pods []ProxyView, replicas int32, surgeAllowed bool) RolloutDecision {
	var stale, draining, currentReady int32
	var markable []string
	for _, p := range pods {
		if p.Stale {
			stale++
			if !p.Draining {
				markable = append(markable, p.Name)
			}
		} else if p.Ready && !p.Draining {
			currentReady++
		}
		if p.Draining {
			draining++
		}
	}

	var surge int32
	if surgeAllowed {
		surge = stale
	}
	target := replicas + surge
	total := int32(len(pods))

	if total < target {
		return RolloutDecision{Create: target - total}
	}

	if draining == 0 && total > target {
		candidates := pods
		if surgeAllowed && stale > 0 && currentReady < replicas {
			candidates = currentOnly(pods)
		}
		return RolloutDecision{Drain: pick(candidates, total-target)}
	}

	if surgeAllowed && len(markable) > 0 && currentReady >= replicas {
		return RolloutDecision{Drain: markable}
	}

	if idle := staleIdle(pods); len(idle) > 0 {
		return RolloutDecision{Drain: pick(idle, 1)}
	}

	if draining > 0 {
		return RolloutDecision{}
	}

	if !surgeAllowed && stale > 0 && readyBeyond(pods, replicas) {
		return RolloutDecision{Drain: pick(staleOnly(pods), 1)}
	}
	return RolloutDecision{}
}

// staleIdle returns the stale pods that serve nobody, so a crashlooping proxy
// cannot hold its own replacement back.
func staleIdle(pods []ProxyView) []ProxyView {
	var out []ProxyView
	for _, p := range pods {
		if p.Stale && !p.Ready && !p.Draining {
			out = append(out, p)
		}
	}
	return out
}

func readyBeyond(pods []ProxyView, replicas int32) bool {
	var readyTotal int32
	for _, p := range pods {
		if p.Ready && !p.Draining {
			readyTotal++
		}
	}
	return readyTotal > replicas
}

func currentOnly(pods []ProxyView) []ProxyView {
	out := make([]ProxyView, 0, len(pods))
	for _, p := range pods {
		if !p.Stale {
			out = append(out, p)
		}
	}
	return out
}

func staleOnly(pods []ProxyView) []ProxyView {
	out := make([]ProxyView, 0, len(pods))
	for _, p := range pods {
		if p.Stale {
			out = append(out, p)
		}
	}
	return out
}

// pick orders candidates for retirement: stale before current, then not-Ready
// before Ready (behind no Service endpoint, and its last player count is
// stale), then fewest players with an untrusted count as occupied, then newest
// first. Newest-first stands in for the occupancy the operator cannot see when
// every count is untrusted; do not flip it for symmetry.
//
// reconcileReplicas calls pick over standing surplus marks and keeps the marks
// on what comes back, so there the head means kept and the tail is released.
func pick(pods []ProxyView, n int32) []string {
	candidates := append([]ProxyView(nil), pods...)
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Stale != b.Stale {
			return a.Stale
		}
		if a.RetireRequested != b.RetireRequested {
			return a.RetireRequested
		}
		if a.Ready != b.Ready {
			return !a.Ready
		}
		if a.PlayersStale != b.PlayersStale {
			return !a.PlayersStale
		}
		if a.Players != b.Players {
			return a.Players < b.Players
		}
		return a.CreatedAt.After(b.CreatedAt)
	})
	out := make([]string, 0, n)
	for i := int32(0); i < n && int(i) < len(candidates); i++ {
		out = append(out, candidates[i].Name)
	}
	return out
}
