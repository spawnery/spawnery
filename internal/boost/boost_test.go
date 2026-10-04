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

package boost

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
	now := time.Unix(1000, 0)
	later := now.Add(time.Hour)

	got := Live([]spawneryv1alpha1.ScaleBoost{
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

	if got := Live([]spawneryv1alpha1.ScaleBoost{boostFor("lobby", 4, &past)}, "lobby", now); got != 0 {
		t.Errorf("boost = %d, want 0 for an expired one", got)
	}
}

func TestABoostWithNoExpiryCountsForever(t *testing.T) {
	now := time.Unix(1000, 0)

	if got := Live([]spawneryv1alpha1.ScaleBoost{boostFor("lobby", 2, nil)}, "lobby", now); got != 2 {
		t.Errorf("boost = %d, want 2: no expiry means no end", got)
	}
}

func TestAnotherGroupsBoostIsNotThisGroupsCapacity(t *testing.T) {
	now := time.Unix(1000, 0)
	later := now.Add(time.Hour)

	if got := Live([]spawneryv1alpha1.ScaleBoost{boostFor("arena", 9, &later)}, "lobby", now); got != 0 {
		t.Errorf("boost = %d, want 0: a boost names one group", got)
	}
}

func TestABoostExpiringExactlyNowHasExpired(t *testing.T) {
	// "Until 20:00" is over at 20:00.
	now := time.Unix(1000, 0)

	if got := Live([]spawneryv1alpha1.ScaleBoost{boostFor("lobby", 2, &now)}, "lobby", now); got != 0 {
		t.Errorf("boost = %d, want 0: expiring now means expired", got)
	}
}

func pinFor(group, name string, replicas int32, created time.Time, expires *time.Time) spawneryv1alpha1.ScaleBoost {
	b := boostFor(group, replicas, expires)
	b.Name = name
	b.Spec.Mode = spawneryv1alpha1.ScaleBoostExact
	b.CreationTimestamp = metav1.NewTime(created)
	return b
}

func TestTheNewestPinWins(t *testing.T) {
	now := time.Unix(10_000, 0)
	later := now.Add(time.Hour)

	got, ok := Exact([]spawneryv1alpha1.ScaleBoost{
		pinFor("lobby", "lobby-old", 5, now.Add(-time.Hour), &later),
		pinFor("lobby", "lobby-new", 0, now.Add(-time.Minute), &later),
	}, "lobby", now)

	if !ok || got.Replicas != 0 {
		t.Errorf("pin = %+v, %v, want the newer pin of 0: pins replace, they do not add up", got, ok)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Time.Equal(later) {
		t.Errorf("expiresAt = %v, want %v", got.ExpiresAt, later)
	}
}

func TestPinsCreatedInTheSameSecondFallToTheName(t *testing.T) {
	now := time.Unix(10_000, 0)
	at := now.Add(-time.Minute)

	got, _ := Exact([]spawneryv1alpha1.ScaleBoost{
		pinFor("lobby", "lobby-b", 2, at, nil),
		pinFor("lobby", "lobby-a", 7, at, nil),
	}, "lobby", now)

	if got.Replicas != 2 {
		t.Errorf("pin = %d, want lobby-b's 2: every reader has to pick the same one", got.Replicas)
	}
}

func TestAnExpiredPinIsNoPin(t *testing.T) {
	now := time.Unix(10_000, 0)
	past := now.Add(-time.Second)

	if _, ok := Exact([]spawneryv1alpha1.ScaleBoost{pinFor("lobby", "p", 0, now.Add(-time.Hour), &past)}, "lobby", now); ok {
		t.Error("an expired pin still holds")
	}
	if _, ok := Exact([]spawneryv1alpha1.ScaleBoost{pinFor("lobby", "p", 0, now.Add(-time.Hour), &now)}, "lobby", now); ok {
		t.Error("a pin expiring exactly now still holds")
	}
}

func TestAnAddBoostIsNoPinAndAPinAddsNothing(t *testing.T) {
	now := time.Unix(10_000, 0)
	later := now.Add(time.Hour)
	boosts := []spawneryv1alpha1.ScaleBoost{
		boostFor("lobby", 3, &later),
		pinFor("lobby", "p", 1, now.Add(-time.Minute), &later),
	}

	if got := Live(boosts, "lobby", now); got != 3 {
		t.Errorf("Live = %d, want 3: a pin is not extra capacity", got)
	}
	if _, ok := Exact([]spawneryv1alpha1.ScaleBoost{boostFor("lobby", 3, &later)}, "lobby", now); ok {
		t.Error("an Add boost read as a pin")
	}
}

func TestABoostWithAnEmptyModeIsAdd(t *testing.T) {
	now := time.Unix(10_000, 0)
	b := boostFor("lobby", 3, nil)
	b.Spec.Mode = ""

	if got := Live([]spawneryv1alpha1.ScaleBoost{b}, "lobby", now); got != 3 {
		t.Errorf("Live = %d, want 3: an object that never saw defaulting is an Add boost", got)
	}
	if _, ok := Exact([]spawneryv1alpha1.ScaleBoost{b}, "lobby", now); ok {
		t.Error("a boost with an empty mode read as a pin")
	}
}

func TestAnotherGroupsPinIsNotThisGroups(t *testing.T) {
	now := time.Unix(10_000, 0)
	if _, ok := Exact([]spawneryv1alpha1.ScaleBoost{pinFor("arena", "p", 0, now, nil)}, "lobby", now); ok {
		t.Error("a pin on arena held lobby")
	}
}

func TestABoostOwnedByAGroupThatIsGoneIsNotTheSuccessors(t *testing.T) {
	group := &spawneryv1alpha1.ServerGroup{ObjectMeta: metav1.ObjectMeta{Name: "lobby", UID: "new-uid"}}
	mine := pinFor("lobby", "mine", 2, time.Unix(1, 0), nil)
	mine.OwnerReferences = []metav1.OwnerReference{{Kind: "ServerGroup", Name: "lobby", UID: "new-uid"}}
	stale := pinFor("lobby", "stale", 0, time.Unix(2, 0), nil)
	stale.OwnerReferences = []metav1.OwnerReference{{Kind: "ServerGroup", Name: "lobby", UID: "old-uid"}}
	byHand := boostFor("lobby", 1, nil)

	got := Of([]spawneryv1alpha1.ScaleBoost{mine, stale, byHand, boostFor("arena", 1, nil)}, group)

	if len(got) != 2 || got[0].Name != "mine" || got[1].Spec.Mode != "" {
		t.Errorf("Of = %+v, want mine and the unowned boost: a predecessor's boost must not pin its successor", got)
	}
}
