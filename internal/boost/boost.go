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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// Live is how many extra servers a group's unexpired Add boosts ask for. A
// boost stops counting when it expires, not when the sweep removes it. Add
// boosts on one group add up; Exact boosts are Exact's business.
func Live(boosts []spawneryv1alpha1.ScaleBoost, group string, now time.Time) int32 {
	var total int32
	for i := range boosts {
		b := &boosts[i]
		if b.Spec.GroupRef.Name != group || b.Spec.Mode == spawneryv1alpha1.ScaleBoostExact || expired(b, now) {
			continue
		}
		total += b.Spec.Replicas
	}
	return total
}

// Pin is the size an Exact boost holds a group at.
type Pin struct {
	Replicas int32
	// ExpiresAt is nil for a pin without an end.
	ExpiresAt *metav1.Time
}

// Exact is the group's newest unexpired Exact boost: pins replace each other
// rather than adding up. A tie on the creation second falls to the name, so
// every reader picks the same one.
func Exact(boosts []spawneryv1alpha1.ScaleBoost, group string, now time.Time) (Pin, bool) {
	var newest *spawneryv1alpha1.ScaleBoost
	for i := range boosts {
		b := &boosts[i]
		if b.Spec.GroupRef.Name != group || b.Spec.Mode != spawneryv1alpha1.ScaleBoostExact || expired(b, now) {
			continue
		}
		if newest == nil || newer(b, newest) {
			newest = b
		}
	}
	if newest == nil {
		return Pin{}, false
	}
	return Pin{Replicas: newest.Spec.Replicas, ExpiresAt: newest.Spec.ExpiresAt}, true
}

// Of is the boosts that belong to group: those naming it, minus those owned
// by another ServerGroup of the same name. A group deleted and created again
// keeps its name, and its predecessor's boosts stay until the garbage
// collector reaches them.
func Of(boosts []spawneryv1alpha1.ScaleBoost, group *spawneryv1alpha1.ServerGroup) []spawneryv1alpha1.ScaleBoost {
	var mine []spawneryv1alpha1.ScaleBoost
	for i := range boosts {
		b := boosts[i]
		if b.Spec.GroupRef.Name != group.Name || ownedByAnother(&b, group) {
			continue
		}
		mine = append(mine, b)
	}
	return mine
}

func ownedByAnother(b *spawneryv1alpha1.ScaleBoost, group *spawneryv1alpha1.ServerGroup) bool {
	for _, ref := range b.OwnerReferences {
		if ref.Kind == "ServerGroup" && ref.UID != group.UID {
			return true
		}
	}
	return false
}

// Expiring exactly now has expired: "until 20:00" is over at 20:00.
func expired(b *spawneryv1alpha1.ScaleBoost, now time.Time) bool {
	return b.Spec.ExpiresAt != nil && !b.Spec.ExpiresAt.After(now)
}

func newer(a, b *spawneryv1alpha1.ScaleBoost) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return b.CreationTimestamp.Before(&a.CreationTimestamp)
	}
	return a.Name > b.Name
}
