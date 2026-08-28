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

package controller

import (
	"time"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// liveBoost is how many extra servers a group's unexpired boosts ask for.
//
// **The clock is the rule, and the sweep is not.** A boost stops counting the
// moment it expires, which is not the same as when the sweep next removes the
// object: otherwise a boost's effect would outlive its own stated end by up to
// a sweep interval, and the object would be telling the truth while the group
// did not. The sweep only tidies away what has already stopped counting.
//
// now is a parameter rather than a call, so a test asserts the boundary
// instead of racing it.
//
// Boosts add. Two on one group are two boosts and a second does not replace a
// first, which is what makes "somebody else already boosted this" a non-event
// rather than a race between two people typing.
func liveBoost(boosts []spawneryv1alpha1.ScaleBoost, group string, now time.Time) int32 {
	var total int32
	for i := range boosts {
		b := &boosts[i]
		if b.Spec.GroupRef.Name != group {
			continue
		}
		// Expiring exactly now has expired: "until 20:00" means it is over at
		// 20:00, and a boundary left to whichever way a comparison happened to
		// be written is a boundary somebody will read the other way.
		if b.Spec.ExpiresAt != nil && !b.Spec.ExpiresAt.After(now) {
			continue
		}
		total += b.Spec.Replicas
	}
	return total
}
