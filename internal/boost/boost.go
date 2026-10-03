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

// Package boost holds the one rule for what a ScaleBoost is currently worth.
// Its own package because the ServerGroup controller and the agent endpoint
// both need it and must not import each other.
package boost

import (
	"time"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// Live is how many extra servers a group's unexpired boosts ask for. A boost
// stops counting when it expires, not when the sweep removes it. Boosts on one
// group add up.
func Live(boosts []spawneryv1alpha1.ScaleBoost, group string, now time.Time) int32 {
	var total int32
	for i := range boosts {
		b := &boosts[i]
		if b.Spec.GroupRef.Name != group {
			continue
		}
		// Expiring exactly now has expired: "until 20:00" is over at 20:00.
		if b.Spec.ExpiresAt != nil && !b.Spec.ExpiresAt.After(now) {
			continue
		}
		total += b.Spec.Replicas
	}
	return total
}
