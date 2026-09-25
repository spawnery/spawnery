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

	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
)

// setUpdatePolicy gives the fixture's group a floor of servers and an update
// policy before its first reconcile.
func (f *fixture) setUpdatePolicy(t *testing.T, minReplicas int32, update *spawneryv1alpha1.UpdateSpec) {
	t.Helper()
	g := f.serverGroup(t, f.group.Name)
	g.Spec.Scaling.MinReplicas = minReplicas
	g.Spec.Update = update
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update group: %v", err)
	}
}

func (f *fixture) reportPlayersOn(t *testing.T, name string, players int32) {
	t.Helper()
	pod, ok := f.pod(name)
	if !ok {
		t.Fatalf("no pod for %s", name)
	}
	if err := f.agents.ReportPlayers(string(pod.UID), players, 100); err != nil {
		t.Fatalf("report %d players on %s: %v", players, name, err)
	}
}

// bringUpCurrent readies every server of the group's current generation that
// is not Ready yet.
func (f *fixture) bringUpCurrent(t *testing.T, group string) {
	t.Helper()
	g := f.serverGroup(t, group)
	for _, name := range f.serverNamesOfGroup(t, group) {
		srv := f.server(name)
		if srv.Spec.GroupGeneration == g.Generation && srv.Status.Phase != string(phase.Ready) {
			bringUpNamed(t, f, name)
		}
	}
}

func TestWhenEmptyKeepsAnOccupiedServerThroughAChangeover(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setUpdatePolicy(t, 2, &spawneryv1alpha1.UpdateSpec{
		Strategy: spawneryv1alpha1.UpdateWhenEmpty, MaxUnavailable: 2,
	})
	f.reconcileNamedGroup(t, r, "lobby")
	f.readyAllServersOf(t, "lobby")
	names := f.serverNamesOfGroup(t, "lobby")
	if len(names) != 2 {
		t.Fatalf("servers = %v, want 2", names)
	}
	occupied, empty := names[0], names[1]
	f.reportPlayersOn(t, occupied, 5)

	f.setImage(t, "lobby", nextImage)
	f.reconcileNamedGroup(t, r, "lobby")
	f.bringUpCurrent(t, "lobby")
	for i := 0; i < 3; i++ {
		f.reconcileNamedGroup(t, r, "lobby")
	}

	if !f.server(empty).Spec.Retire {
		t.Errorf("%s is empty and stale, want it retired", empty)
	}
	if srv := f.server(occupied); srv.Spec.Retire || srv.Status.Phase != string(phase.Ready) {
		t.Fatalf("%s retire = %v phase = %s, want it left Ready while it has players",
			occupied, srv.Spec.Retire, srv.Status.Phase)
	}
	if got := f.serverGroup(t, "lobby").Status.Changeover; got != spawneryv1alpha1.ChangeoverDeferred {
		t.Errorf("status.changeover = %q, want Deferred", got)
	}

	f.reportPlayersOn(t, occupied, 0)
	f.reconcileNamedGroup(t, r, "lobby")
	if !f.server(occupied).Spec.Retire {
		t.Errorf("%s is empty now, want it retired", occupied)
	}
}

func TestMinAvailableBuildsAnExtraServerBeforeRetiring(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setUpdatePolicy(t, 2, &spawneryv1alpha1.UpdateSpec{MaxUnavailable: 2, MinAvailable: ptr.To[int32](2)})
	f.reconcileNamedGroup(t, r, "lobby")
	f.readyAllServersOf(t, "lobby")
	stale := f.serverNamesOfGroup(t, "lobby")

	f.setImage(t, "lobby", nextImage)
	f.reconcileNamedGroup(t, r, "lobby") // cold start
	f.bringUpCurrent(t, "lobby")
	f.reconcileNamedGroup(t, r, "lobby") // three joinable: the first stale one may go

	retired := 0
	for _, name := range stale {
		if f.server(name).Spec.Retire {
			retired++
		}
	}
	if retired != 1 {
		t.Fatalf("%d stale servers retired, want 1", retired)
	}

	f.reconcileNamedGroup(t, r, "lobby") // two joinable: the floor asks for an extra server
	if n := len(f.serverNamesOfGroup(t, "lobby")); n != 4 {
		t.Fatalf("group has %d servers, want 4: two stale, the cold start, and the extra server", n)
	}
	if c := f.progressing(t, "lobby"); c.Reason == spawneryv1alpha1.ReasonWaitingForMinAvailable {
		t.Errorf("Progressing = %s in the pass that builds the extra server", c.Reason)
	}
	f.reconcileNamedGroup(t, r, "lobby")
	if n := len(f.serverNamesOfGroup(t, "lobby")); n != 4 {
		t.Fatalf("group has %d servers, want still 4: one extra server at a time", n)
	}
	if c := f.progressing(t, "lobby"); c.Reason != spawneryv1alpha1.ReasonServersStarting {
		t.Errorf("Progressing = %s, want %s while the extra server starts", c.Reason, spawneryv1alpha1.ReasonServersStarting)
	}

	f.bringUpCurrent(t, "lobby")
	f.reconcileNamedGroup(t, r, "lobby")
	for _, name := range stale {
		if !f.server(name).Spec.Retire {
			t.Errorf("%s still not retired once the extra server is Ready", name)
		}
	}
}
