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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// oneFailedRound leaves the fixture's group with a streak of one.
func oneFailedRound(t *testing.T, f *fixture, r *ServerGroupReconciler) {
	t.Helper()
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)
	f.failServer(t, f.oneServerName(t))
	f.reconcileGroup(t, r)
	if got := f.reloadGroup(t).Status.ConsecutiveFailures; got != 1 {
		t.Fatalf("consecutiveFailures = %d, want 1 to start from", got)
	}
}

func (f *fixture) editGroup(t *testing.T, edit func(*spawneryv1alpha1.ServerGroup)) {
	t.Helper()
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	edit(f.group)
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
}

func TestACapacityEditKeepsTheStreak(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	oneFailedRound(t, f, r)

	f.editGroup(t, func(g *spawneryv1alpha1.ServerGroup) { g.Spec.Scaling.MaxReplicas++ })
	f.reconcileGroup(t, r)

	if got := f.reloadGroup(t).Status.ConsecutiveFailures; got != 1 {
		t.Errorf("consecutiveFailures = %d after a capacity edit, want 1: the servers start with "+
			"exactly what failed", got)
	}
}

func TestAnAttributesEditKeepsTheStreak(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	oneFailedRound(t, f, r)

	f.editGroup(t, func(g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Attributes = spawneryv1alpha1.GroupAttributes{"game": "bedwars"}
	})
	f.reconcileGroup(t, r)

	if got := f.reloadGroup(t).Status.ConsecutiveFailures; got != 1 {
		t.Errorf("consecutiveFailures = %d after an attributes edit, want 1", got)
	}
}

func TestACorrectedOverlayClearsTheStreak(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	overlay := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-overlay", Namespace: f.ns},
		Data:       map[string]string{"server.properties": "no-such-key=1\n"},
	}
	if err := f.c.Create(f.ctx, overlay); err != nil {
		t.Fatalf("create overlay: %v", err)
	}
	f.editGroup(t, func(g *spawneryv1alpha1.ServerGroup) {
		g.Spec.ConfigOverlay = &spawneryv1alpha1.ObjectRef{Name: overlay.Name}
	})
	oneFailedRound(t, f, r)
	before := f.reloadGroup(t).Status.LastFailureAt

	overlay.Data["server.properties"] = "motd=fixed\n"
	if err := f.c.Update(f.ctx, overlay); err != nil {
		t.Fatalf("correct overlay: %v", err)
	}
	f.reconcileGroup(t, r)

	g := f.reloadGroup(t)
	if g.Status.ConsecutiveFailures != 0 {
		t.Errorf("consecutiveFailures = %d after the overlay was corrected, want 0; the "+
			"failed server started with the same pod hash, so its corpse must not count back",
			g.Status.ConsecutiveFailures)
	}
	if g.Status.LastFailureAt == nil || !g.Status.LastFailureAt.Equal(before) {
		t.Errorf("lastFailureAt = %v, want the watermark %v kept", g.Status.LastFailureAt, before)
	}
}

func TestTheRetryAnnotationClearsTheStreak(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	oneFailedRound(t, f, r)

	f.editGroup(t, func(g *spawneryv1alpha1.ServerGroup) {
		metav1.SetMetaDataAnnotation(&g.ObjectMeta, spawneryv1alpha1.AnnotationRetry, "1")
	})
	f.reconcileGroup(t, r)

	if got := f.reloadGroup(t).Status.ConsecutiveFailures; got != 0 {
		t.Errorf("consecutiveFailures = %d after the retry annotation, want 0", got)
	}
}

// An upgrade meets streaks recorded before the key existed. Adopting keeps a
// latched group latched rather than retrying every one of them at once.
func TestAStreakWithNoKeyIsAdoptedNotReset(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	oneFailedRound(t, f, r)

	g := f.reloadGroup(t)
	if g.Status.FailureStreakKey == "" {
		t.Fatal("a running streak recorded no key")
	}
	g.Status.FailureStreakKey = ""
	if err := f.c.Status().Update(f.ctx, g); err != nil {
		t.Fatalf("clear key: %v", err)
	}
	f.reconcileGroup(t, r)

	g = f.reloadGroup(t)
	if g.Status.ConsecutiveFailures != 1 {
		t.Errorf("consecutiveFailures = %d, want 1: an empty key is adopted, not a change", g.Status.ConsecutiveFailures)
	}
	if g.Status.FailureStreakKey == "" {
		t.Error("the key was not adopted")
	}
}

func TestASuccessClearsTheKeyWithTheStreak(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	oneFailedRound(t, f, r)

	f.clock.Advance(backoffCap)
	f.reconcileGroup(t, r)
	name, ok := f.newestServerName(t)
	if !ok {
		t.Fatal("no replacement was created once the window closed")
	}
	f.clock.Advance(1)
	bringUpNamed(t, f, name)
	f.reconcileGroup(t, r)

	g := f.reloadGroup(t)
	if g.Status.ConsecutiveFailures != 0 || g.Status.FailureStreakKey != "" {
		t.Errorf("consecutiveFailures = %d, failureStreakKey = %q after a success, want 0 and empty",
			g.Status.ConsecutiveFailures, g.Status.FailureStreakKey)
	}
}

// Before the key, the reset set lastFailureAt to nil and the persistent
// count, which is not filtered by attempt, counted the corpse straight back.
func TestAPersistentGroupsRetryLandsAtZero(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "outpost", 1)
	f.reconcilePersistentGroup(t, r, "outpost")
	f.failServerNeverReady(t, "outpost-0")
	f.reconcilePersistentGroup(t, r, "outpost")
	if got := f.persistentGroup(t, "outpost").Status.ConsecutiveFailures; got != 1 {
		t.Fatalf("consecutiveFailures = %d, want 1 to start from", got)
	}

	group := f.persistentGroup(t, "outpost")
	metav1.SetMetaDataAnnotation(&group.ObjectMeta, spawneryv1alpha1.AnnotationRetry, "1")
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("annotate: %v", err)
	}
	f.reconcilePersistentGroup(t, r, "outpost")

	if got := f.persistentGroup(t, "outpost").Status.ConsecutiveFailures; got != 0 {
		t.Errorf("consecutiveFailures = %d after the retry, want 0", got)
	}
}
