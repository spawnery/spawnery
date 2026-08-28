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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func boostFor(group string, replicas int32, expires *time.Time) spawneryv1alpha1.ScaleBoost {
	b := spawneryv1alpha1.ScaleBoost{
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: group},
			Replicas: replicas,
		},
	}
	if expires != nil {
		t := metav1.NewTime(*expires)
		b.Spec.ExpiresAt = &t
	}
	return b
}

func TestBoostsAddUpRatherThanReplacing(t *testing.T) {
	// Two people boosting the same group at once is a non-event, not a race.
	now := time.Unix(1000, 0)
	later := now.Add(time.Hour)

	got := liveBoost([]spawneryv1alpha1.ScaleBoost{
		boostFor("lobby", 2, &later),
		boostFor("lobby", 3, &later),
	}, "lobby", now)

	if got != 5 {
		t.Errorf("boost = %d, want 5: two boosts are two boosts", got)
	}
}

func TestAnExpiredBoostCountsForNothing(t *testing.T) {
	now := time.Unix(1000, 0)
	past := now.Add(-time.Second)

	if got := liveBoost([]spawneryv1alpha1.ScaleBoost{boostFor("lobby", 4, &past)}, "lobby", now); got != 0 {
		t.Errorf("boost = %d, want 0 for an expired one", got)
	}
}

func TestABoostWithNoExpiryCountsForever(t *testing.T) {
	now := time.Unix(1000, 0)

	if got := liveBoost([]spawneryv1alpha1.ScaleBoost{boostFor("lobby", 2, nil)}, "lobby", now); got != 2 {
		t.Errorf("boost = %d, want 2: no expiry means no end", got)
	}
}

func TestAnotherGroupsBoostIsNotThisGroupsCapacity(t *testing.T) {
	now := time.Unix(1000, 0)
	later := now.Add(time.Hour)

	if got := liveBoost([]spawneryv1alpha1.ScaleBoost{boostFor("arena", 9, &later)}, "lobby", now); got != 0 {
		t.Errorf("boost = %d, want 0: a boost names one group", got)
	}
}

func TestABoostExpiringExactlyNowHasExpired(t *testing.T) {
	// The boundary, asserted rather than left to whichever way the comparison
	// happened to be written. "Until 20:00" means it is over at 20:00.
	now := time.Unix(1000, 0)

	if got := liveBoost([]spawneryv1alpha1.ScaleBoost{boostFor("lobby", 2, &now)}, "lobby", now); got != 0 {
		t.Errorf("boost = %d, want 0: expiring now means expired", got)
	}
}
